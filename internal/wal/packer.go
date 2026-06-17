// Package wal contains the background packer worker. The WebDAV write path
// appends file content as chunks into wal_chunks (via the WAL repository); the
// packer reads buffered files, splits each into immutable blobs (< 20 MiB),
// uploads those blobs to Telegram CONCURRENTLY across the available bots, and
// records the blob + extents and removes the WAL rows once a node's blobs have
// all landed.
//
// Parallelism: a builder goroutine plans blobs (splitting large files into
// blob-sized pieces and packing small files together) and feeds them to a pool
// of upload workers; each worker uploads a different blob through a different
// bot at the same time, so a large file saturates all bots instead of trickling
// through one. A node is finalized only after every one of its blobs is stored.
//
// Crash-safety / correctness: a blob is created with refcount 0; its extents are
// written and refcounts bumped only when the owning node is finalized, in one
// transaction guarded by MarkStoredIfOwner (atomic buffered→stored iff this
// worker still holds the lease). So a partially-uploaded or lost-lease node
// never leaves committed extents behind — it is re-packed from the still-present
// WAL, and the already-uploaded blobs become orphans (refcount 0) that the GC
// reclaims after a grace period. WAL rows are deleted only at finalization.
package wal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// Packer is the background worker that uploads buffered nodes into blobs.
type Packer struct {
	repos    *domain.Repositories
	tx       domain.TxManager
	tg       domain.TelegramAPI
	channels domain.ChannelService
	bots     domain.BotService
	settings domain.SettingsService
	stats    domain.StatRecorder
	log      *slog.Logger

	leaseOwner   string
	leaseFor     time.Duration
	pollInterval time.Duration
	batchLimit   int
	// uploadConcurrency caps parallel blob uploads. 0 = auto (one per enabled bot).
	uploadConcurrency int
}

// NewPacker builds a packer. leaseOwner identifies this worker for crash-safe
// lease ownership.
func NewPacker(
	r *domain.Repositories,
	tx domain.TxManager,
	tg domain.TelegramAPI,
	channels domain.ChannelService,
	bots domain.BotService,
	settings domain.SettingsService,
	stats domain.StatRecorder,
	logger *slog.Logger,
) *Packer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Packer{
		repos:        r,
		tx:           tx,
		tg:           tg,
		channels:     channels,
		bots:         bots,
		settings:     settings,
		stats:        stats,
		log:          logger.With("component", "wal-packer"),
		leaseOwner:   uuid.NewString(),
		leaseFor:     10 * time.Minute,
		pollInterval: 250 * time.Millisecond,
		batchLimit:   64,
	}
}

// track holds the in-flight state of one node being packed. remaining counts the
// node's blobs not yet stored; planned is set once the builder has emitted all
// of them; extents accumulate (one per blob the node touches) and are written at
// finalization. All fields are guarded by packState.mu.
type track struct {
	node      domain.Node
	remaining int
	planned   bool
	failed    bool
	extents   []domain.Extent
}

// blobJob is one blob ready to upload: its bytes, the target channel (chosen at
// plan time so every blob of one file lands in the SAME channel — losing one
// channel then loses whole files rather than corrupting many), and the distinct
// tracks whose data it contains.
type blobJob struct {
	blobID  uuid.UUID
	data    []byte
	channel *domain.Channel
	tracks  []*track
}

// pendingExtent is one (node → current blob) span accumulated in the small-file
// builder buffer before the blob is sealed.
type pendingExtent struct {
	track      *track
	seq        int64
	fileOffset int64
	blobOffset int64
	length     int64
}

// run accumulates bytes for the small-file blob currently being built.
type run struct {
	buf  []byte
	segs []pendingExtent
}

// packState is the shared, mutex-guarded coordination between the builder and
// the upload workers.
type packState struct {
	mu sync.Mutex
	// pendingSmall are small-file tracks whose bytes sit in the current builder
	// buffer; they become planned when that buffer is sealed.
	pendingSmall []*track
}

// Run executes the packer until ctx is cancelled.
func (p *Packer) Run(ctx context.Context) {
	n := p.workerCount(ctx)
	p.log.Info("packer started", "lease_owner", p.leaseOwner, "upload_workers", n)

	// Uploads/finalization run on a background context so a clean shutdown can
	// drain queued blobs; the builder stops claiming when ctx is cancelled.
	uploadCtx, cancelUpload := context.WithCancel(context.Background())
	jobs := make(chan blobJob, n)
	st := &packState{}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.uploadWorker(uploadCtx, jobs, st)
		}()
	}

	p.builder(ctx, uploadCtx, jobs, st) // seals the final buffer and closes jobs

	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(90 * time.Second):
		p.log.Warn("packer drain timed out, cancelling uploads")
		cancelUpload()
		<-drained
	}
	cancelUpload()
	p.log.Info("packer stopped")
}

// workerCount picks the upload concurrency: the configured value, else one per
// enabled bot, clamped to [1, 16].
func (p *Packer) workerCount(ctx context.Context) int {
	n := p.uploadConcurrency
	if n <= 0 {
		bots, err := p.bots.List(ctx)
		if err == nil {
			for _, b := range bots {
				if b.Enabled {
					n++
				}
			}
		}
	}
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	return n
}

// builder claims buffered nodes and emits blob jobs: large files are split into
// independent blob-sized pieces (each its own job, uploaded in parallel), small
// files are packed together into shared blobs.
func (p *Packer) builder(ctx, uploadCtx context.Context, jobs chan<- blobJob, st *packState) {
	cur := &run{}
	lastActivity := time.Time{}
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.seal(uploadCtx, cur, jobs, st)
			close(jobs)
			return
		case <-ticker.C:
		}

		set, err := p.settings.Get(ctx)
		if err != nil {
			p.log.Warn("read settings", "err", err)
			continue
		}
		blobMax := set.BlobMaxSize
		if blobMax <= 0 {
			blobMax = domain.DefaultSettings().BlobMaxSize
		}

		nodes, err := p.repos.Nodes.ClaimBufferedForPacking(ctx, p.leaseOwner, p.leaseFor, p.batchLimit)
		if err != nil {
			p.log.Warn("claim buffered nodes", "err", err)
			continue
		}

		if len(nodes) == 0 {
			if len(cur.buf) > 0 && !lastActivity.IsZero() && time.Since(lastActivity) >= set.WALIdleTimeout {
				p.seal(uploadCtx, cur, jobs, st)
				cur = &run{}
			}
			continue
		}

		for i := range nodes {
			node := nodes[i]
			if node.Size == 0 {
				if err := p.finalizeEmpty(uploadCtx, node); err != nil {
					p.log.Warn("finalize empty node", "node", node.ID, "err", err)
					_ = p.repos.Nodes.ReleaseLease(uploadCtx, node.ID)
				}
				continue
			}
			t := &track{node: node}
			if node.Size > blobMax {
				p.splitLarge(uploadCtx, t, blobMax, jobs, st)
			} else {
				p.packSmall(uploadCtx, t, node, blobMax, &cur, jobs, st)
			}
			lastActivity = time.Now()
		}
	}
}

// splitLarge emits one independent blob job per blob-sized piece of a large
// file. The jobs upload in parallel; the node is finalized once all are stored.
func (p *Packer) splitLarge(ctx context.Context, t *track, blobMax int64, jobs chan<- blobJob, st *packState) {
	// One channel for the whole file: all its blobs land together.
	channel, err := p.channels.PickForUpload(ctx)
	if err != nil {
		p.log.Warn("pick channel for file", "node", t.node.ID, "err", err)
		p.failTrack(ctx, t, st)
		return
	}
	var off, seq int64
	for off < t.node.Size {
		n := min(blobMax, t.node.Size-off)
		data, err := p.repos.WAL.ReadRange(ctx, t.node.ID, off, n)
		if err != nil {
			p.log.Warn("read wal range", "node", t.node.ID, "err", err)
			p.failTrack(ctx, t, st)
			return
		}
		blobID := uuid.New()
		st.mu.Lock()
		t.extents = append(t.extents, domain.Extent{
			ID: uuid.New(), NodeID: t.node.ID, Seq: seq,
			FileOffset: off, Length: int64(len(data)), BlobID: blobID, BlobOffset: 0,
		})
		t.remaining++
		st.mu.Unlock()
		jobs <- blobJob{blobID: blobID, data: data, channel: channel, tracks: []*track{t}}
		off += int64(len(data))
		seq++
	}
	// Every piece is enqueued → the node is fully planned.
	st.mu.Lock()
	t.planned = true
	ready := t.remaining == 0 && !t.failed
	st.mu.Unlock()
	if ready {
		p.finalize(ctx, t)
	}
}

// packSmall reads a small file fully and appends it to the current builder
// buffer, sealing the buffer first if the file would overflow it.
func (p *Packer) packSmall(ctx context.Context, t *track, node domain.Node, blobMax int64, cur **run, jobs chan<- blobJob, st *packState) {
	data, err := p.repos.WAL.ReadRange(ctx, node.ID, 0, node.Size)
	if err != nil {
		p.log.Warn("read wal range", "node", node.ID, "err", err)
		_ = p.repos.Nodes.ReleaseLease(ctx, node.ID)
		return
	}
	if int64(len((*cur).buf))+int64(len(data)) > blobMax && len((*cur).buf) > 0 {
		p.seal(ctx, *cur, jobs, st)
		*cur = &run{}
	}
	(*cur).segs = append((*cur).segs, pendingExtent{
		track: t, seq: 0, fileOffset: 0, blobOffset: int64(len((*cur).buf)), length: int64(len(data)),
	})
	(*cur).buf = append((*cur).buf, data...)
	st.mu.Lock()
	st.pendingSmall = append(st.pendingSmall, t)
	st.mu.Unlock()
}

// seal turns the current small-file buffer into a blob job, records its extents
// on each contributing track, and marks those tracks planned.
func (p *Packer) seal(ctx context.Context, cur *run, jobs chan<- blobJob, st *packState) {
	if len(cur.buf) == 0 {
		return
	}
	channel, err := p.channels.PickForUpload(ctx)
	if err != nil {
		p.log.Warn("pick channel for blob", "err", err)
		st.mu.Lock()
		pending := st.pendingSmall
		st.pendingSmall = nil
		st.mu.Unlock()
		for _, t := range pending {
			_ = p.repos.Nodes.ReleaseLease(ctx, t.node.ID)
		}
		return
	}
	blobID := uuid.New()

	st.mu.Lock()
	seen := map[*track]bool{}
	var distinct []*track
	for _, s := range cur.segs {
		s.track.extents = append(s.track.extents, domain.Extent{
			ID: uuid.New(), NodeID: s.track.node.ID, Seq: s.seq,
			FileOffset: s.fileOffset, Length: s.length, BlobID: blobID, BlobOffset: s.blobOffset,
		})
		if !seen[s.track] {
			seen[s.track] = true
			s.track.remaining++
			distinct = append(distinct, s.track)
		}
	}
	pending := st.pendingSmall
	st.pendingSmall = nil
	st.mu.Unlock()

	jobs <- blobJob{blobID: blobID, data: cur.buf, channel: channel, tracks: distinct}

	// All buffered small files are now in a sealed blob → planned.
	st.mu.Lock()
	var ready []*track
	for _, t := range pending {
		t.planned = true
		if t.remaining == 0 && !t.failed {
			ready = append(ready, t)
		}
	}
	st.mu.Unlock()
	for _, t := range ready {
		p.finalize(ctx, t)
	}
}

// uploadWorker uploads blobs and, once a blob is stored, decrements its tracks
// and finalizes any node whose blobs have all landed.
func (p *Packer) uploadWorker(ctx context.Context, jobs <-chan blobJob, st *packState) {
	for job := range jobs {
		// The channel was chosen at plan time so all of a file's blobs share it;
		// the worker only picks a (member) bot, which still spreads uploads of one
		// file across the channel's bots in parallel.
		channel := job.channel
		res, bot, err := p.upload(ctx, channel, job.data)
		if err != nil {
			p.failJob(ctx, job, st, err)
			continue
		}
		if err := p.persistBlob(ctx, job, channel, bot, res); err != nil {
			p.failJob(ctx, job, st, fmt.Errorf("persist blob: %w", err))
			continue
		}
		p.stats.AddWriteBytes(int64(len(job.data)))

		st.mu.Lock()
		var ready []*track
		for _, t := range job.tracks {
			t.remaining--
			if t.planned && t.remaining == 0 && !t.failed {
				ready = append(ready, t)
			}
		}
		st.mu.Unlock()
		for _, t := range ready {
			p.finalize(ctx, t)
		}
	}
}

// persistBlob records the uploaded blob (refcount 0; extents are written at node
// finalization) and its per-bot file_id, incrementing the channel counter.
func (p *Packer) persistBlob(ctx context.Context, job blobJob, channel *domain.Channel, bot *domain.Bot, res domain.TGSendResult) error {
	now := time.Now()
	return p.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		seq, err := r.Channels.IncrementCounter(ctx, channel.ID, 1)
		if err != nil {
			return err
		}
		if err := r.Blobs.Create(ctx, &domain.Blob{
			ID: job.blobID, ChannelID: channel.ID, MessageID: res.MessageID,
			MessageSeq: seq, Size: int64(len(job.data)), State: domain.BlobStored,
			Refcount: 0, CreatedAt: now, SealedAt: &now,
		}); err != nil {
			return err
		}
		return r.BlobBotFiles.Upsert(ctx, &domain.BlobBotFile{
			BlobID: job.blobID, BotID: bot.ID, FileID: res.FileID,
			FileUniqueID: res.FileUniqueID, FetchedAt: now,
		})
	})
}

// finalize atomically marks a node stored (iff this worker still owns the
// lease), writes all its extents, bumps each blob's refcount and deletes its WAL
// rows. Lost-lease/already-finalized nodes are skipped, so a re-pack can never
// produce duplicate extents.
func (p *Packer) finalize(ctx context.Context, t *track) {
	st := t // alias for clarity
	err := p.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		owned, err := r.Nodes.MarkStoredIfOwner(ctx, st.node.ID, p.leaseOwner)
		if err != nil {
			return err
		}
		if !owned {
			return nil
		}
		if len(st.extents) > 0 {
			if err := r.Extents.CreateBatch(ctx, st.extents); err != nil {
				return err
			}
			for _, e := range st.extents {
				if err := r.Blobs.AddRefcount(ctx, e.BlobID, 1); err != nil {
					return err
				}
			}
		}
		return r.WAL.DeleteByNode(ctx, st.node.ID)
	})
	if err != nil {
		p.log.Warn("finalize node", "node", t.node.ID, "err", err)
		// Leave it buffered with its lease; it will be re-packed.
	}
}

// finalizeEmpty stores a zero-length node immediately (no blob involved).
func (p *Packer) finalizeEmpty(ctx context.Context, node domain.Node) error {
	return p.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		owned, err := r.Nodes.MarkStoredIfOwner(ctx, node.ID, p.leaseOwner)
		if err != nil {
			return err
		}
		if !owned {
			return nil
		}
		return r.WAL.DeleteByNode(ctx, node.ID)
	})
}

// failTrack marks a single track failed and releases its node's lease so it is
// re-packed; already-uploaded blobs for it become refcount-0 orphans.
func (p *Packer) failTrack(ctx context.Context, t *track, st *packState) {
	st.mu.Lock()
	already := t.failed
	t.failed = true
	st.mu.Unlock()
	if !already {
		_ = p.repos.Nodes.ReleaseLease(ctx, t.node.ID)
	}
}

// failJob fails every track in a job (upload/persist error) and releases their
// leases for re-packing.
func (p *Packer) failJob(ctx context.Context, job blobJob, st *packState, cause error) {
	p.log.Warn("blob upload failed", "blob", job.blobID, "err", cause)
	for _, t := range job.tracks {
		p.failTrack(ctx, t, st)
	}
}

// upload sends the bytes, rotating bots on rate-limit/forbidden errors. Distinct
// concurrent calls pick distinct bots (round-robin), so uploads run in parallel.
func (p *Packer) upload(ctx context.Context, channel *domain.Channel, data []byte) (domain.TGSendResult, *domain.Bot, error) {
	filename := uuid.NewString() + ".bin"
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		bot, err := p.bots.PickForUpload(ctx, channel.ID)
		if err != nil {
			if lastErr != nil {
				return domain.TGSendResult{}, nil, lastErr
			}
			return domain.TGSendResult{}, nil, fmt.Errorf("pick bot: %w", err)
		}
		p.stats.IncTelegramReq()
		res, err := p.tg.SendDocument(ctx, bot, channel.TGChatID, filename, data)
		if err == nil {
			return res, bot, nil
		}
		lastErr = err
		var rl *domain.RateLimitError
		switch {
		case errors.As(err, &rl):
			until := time.Now().Add(rl.RetryAfter)
			_ = p.repos.Bots.SetUnavailableUntil(ctx, bot.ID, &until)
			_ = p.repos.Events.Log(ctx, domain.EventBotUnavailable, "rate limited on upload", bot.ID.String())
		case errors.Is(err, domain.ErrTelegramForbidden):
			_ = p.repos.BotChannels.Upsert(ctx, &domain.BotChannel{BotID: bot.ID, ChannelID: channel.ID, Member: false, CheckedAt: time.Now()})
		default:
			_ = p.repos.Events.Log(ctx, domain.EventUploadFailed, err.Error(), channel.ID.String())
		}
	}
	return domain.TGSendResult{}, nil, fmt.Errorf("upload failed after retries: %w", lastErr)
}
