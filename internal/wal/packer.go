// Package wal contains the background packer worker. The WebDAV write path
// appends file content as chunks into wal_chunks (via the WAL repository); the
// packer accumulates buffered files into immutable Telegram blobs (< 20 MiB),
// splitting oversized files across several blobs and packing multiple small
// files into one, then records blobs + extents and removes the WAL rows.
//
// Crash-safety: WAL rows are deleted only inside the same transaction that
// records the blob and extents and marks the node stored. If the process dies
// after a successful upload but before that commit, the node simply stays
// buffered (its packer lease expires) and is re-packed later — producing an
// orphaned, unreferenced Telegram message but never losing or corrupting data.
package wal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// Packer is the background worker that flushes buffered nodes into blobs.
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
		leaseFor:     2 * time.Minute,
		pollInterval: 250 * time.Millisecond,
		batchLimit:   64,
	}
}

// segment is one contiguous (node → current blob) span awaiting an extent.
type segment struct {
	nodeID     uuid.UUID
	seq        int64
	fileOffset int64
	blobOffset int64
	length     int64
}

// run accumulates bytes for the blob currently being built.
type run struct {
	buf  []byte
	segs []segment
}

// Run executes the packer loop until ctx is cancelled.
func (p *Packer) Run(ctx context.Context) {
	p.log.Info("packer started", "lease_owner", p.leaseOwner)
	cur := &run{}
	var pending []domain.Node // fully-appended nodes awaiting the next flush
	lastActivity := time.Time{}

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Best-effort final flush so a clean shutdown doesn't strand bytes.
			if len(cur.buf) > 0 {
				flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if err := p.flush(flushCtx, cur, pending); err != nil {
					p.releaseLeases(flushCtx, cur, pending)
				}
				cancel()
			}
			p.log.Info("packer stopped")
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
			// Idle flush: emit a partially-filled blob once nothing new arrives.
			if len(cur.buf) > 0 && !lastActivity.IsZero() && time.Since(lastActivity) >= set.WALIdleTimeout {
				if err := p.flush(ctx, cur, pending); err != nil {
					p.log.Warn("idle flush", "err", err)
					p.releaseLeases(ctx, cur, pending)
				}
				cur, pending = &run{}, nil
			}
			continue
		}

		for i := range nodes {
			node := nodes[i]
			if node.Size == 0 {
				// Zero-byte files involve no blob; finalize immediately.
				if err := p.finalizeEmpty(ctx, node); err != nil {
					p.log.Warn("finalize empty node", "node", node.ID, "err", err)
					_ = p.repos.Nodes.ReleaseLease(ctx, node.ID)
				}
				continue
			}
			if _, err := p.appendNode(ctx, &cur, &pending, node, blobMax); err != nil {
				p.log.Warn("append node", "node", node.ID, "err", err)
				// Drop everything in flight; leases expire and it is retried.
				p.releaseLeases(ctx, cur, pending)
				_ = p.repos.Nodes.ReleaseLease(ctx, node.ID)
				cur, pending = &run{}, nil
				break
			}
			pending = append(pending, node)
			lastActivity = time.Now()
		}
	}
}

// appendNode streams node's WAL bytes into cur, flushing whenever a blob fills.
// cur/pending are pointers-to-pointers so a mid-node flush can reset them.
func (p *Packer) appendNode(ctx context.Context, cur **run, pending *[]domain.Node, node domain.Node, blobMax int64) (bool, error) {
	flushedAny := false
	var (
		seq     int64
		fileOff int64
		openIdx = -1
	)
	err := p.repos.WAL.EachChunk(ctx, node.ID, func(c domain.WALChunk) error {
		data := c.Data
		off := 0
		for off < len(data) {
			space := int(blobMax) - len((*cur).buf)
			if space == 0 {
				// Current blob is full: flush it (emits all open segments).
				if err := p.flush(ctx, *cur, *pending); err != nil {
					return err
				}
				flushedAny = true
				*cur = &run{}
				*pending = nil
				openIdx = -1
				space = int(blobMax)
			}
			take := min(space, len(data)-off)
			if openIdx == -1 {
				(*cur).segs = append((*cur).segs, segment{
					nodeID:     node.ID,
					seq:        seq,
					fileOffset: fileOff,
					blobOffset: int64(len((*cur).buf)),
					length:     0,
				})
				openIdx = len((*cur).segs) - 1
				seq++
			}
			(*cur).buf = append((*cur).buf, data[off:off+take]...)
			(*cur).segs[openIdx].length += int64(take)
			fileOff += int64(take)
			off += take
			if len((*cur).buf) == int(blobMax) {
				if err := p.flush(ctx, *cur, *pending); err != nil {
					return err
				}
				flushedAny = true
				*cur = &run{}
				*pending = nil
				openIdx = -1
			}
		}
		return nil
	})
	return flushedAny, err
}

// finalizeEmpty stores a zero-length node immediately (no blob involved).
func (p *Packer) finalizeEmpty(ctx context.Context, node domain.Node) error {
	return p.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		n, err := r.Nodes.GetByID(ctx, node.ID)
		if err != nil {
			return err
		}
		if n.State == domain.NodeStored {
			return nil
		}
		n.State = domain.NodeStored
		if err := r.Nodes.Update(ctx, n); err != nil {
			return err
		}
		if err := r.WAL.DeleteByNode(ctx, node.ID); err != nil {
			return err
		}
		return r.Nodes.ReleaseLease(ctx, node.ID)
	})
}

// flush uploads cur's bytes as one blob and, in a single transaction, records
// the blob + extents and finalizes every pending node.
func (p *Packer) flush(ctx context.Context, cur *run, pending []domain.Node) error {
	if len(cur.buf) == 0 {
		return p.finalizePendingOnly(ctx, pending)
	}

	channel, err := p.channels.PickForUpload(ctx)
	if err != nil {
		return fmt.Errorf("pick channel: %w", err)
	}

	res, bot, err := p.upload(ctx, channel, cur.buf)
	if err != nil {
		return err
	}

	seq, err := p.repos.Channels.IncrementCounter(ctx, channel.ID, 1)
	if err != nil {
		return fmt.Errorf("increment counter: %w", err)
	}

	blobID := uuid.New()
	now := time.Now()
	err = p.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		blob := &domain.Blob{
			ID:         blobID,
			ChannelID:  channel.ID,
			MessageID:  res.MessageID,
			MessageSeq: seq,
			Size:       int64(len(cur.buf)),
			State:      domain.BlobStored,
			Refcount:   int64(len(cur.segs)),
			CreatedAt:  now,
			SealedAt:   &now,
		}
		if err := r.Blobs.Create(ctx, blob); err != nil {
			return err
		}
		if err := r.BlobBotFiles.Upsert(ctx, &domain.BlobBotFile{
			BlobID:       blobID,
			BotID:        bot.ID,
			FileID:       res.FileID,
			FileUniqueID: res.FileUniqueID,
			FetchedAt:    now,
		}); err != nil {
			return err
		}
		extents := make([]domain.Extent, 0, len(cur.segs))
		for _, s := range cur.segs {
			extents = append(extents, domain.Extent{
				ID:         uuid.New(),
				NodeID:     s.nodeID,
				Seq:        s.seq,
				FileOffset: s.fileOffset,
				Length:     s.length,
				BlobID:     blobID,
				BlobOffset: s.blobOffset,
			})
		}
		if err := r.Extents.CreateBatch(ctx, extents); err != nil {
			return err
		}
		return finalizeNodes(ctx, r, pending)
	})
	if err != nil {
		return fmt.Errorf("persist blob: %w", err)
	}

	p.stats.AddWriteBytes(int64(len(cur.buf)))
	p.log.Debug("flushed blob", "blob", blobID, "channel", channel.ID, "bot", bot.ID,
		"bytes", len(cur.buf), "extents", len(cur.segs), "nodes", len(pending))
	return nil
}

// upload sends the bytes, rotating bots on rate-limit/forbidden errors.
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

// finalizePendingOnly finalizes pending nodes when there is no blob to write
// (e.g. all pending nodes were zero-byte).
func (p *Packer) finalizePendingOnly(ctx context.Context, pending []domain.Node) error {
	if len(pending) == 0 {
		return nil
	}
	return p.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		return finalizeNodes(ctx, r, pending)
	})
}

// finalizeNodes marks each pending node stored, removes its WAL rows and clears
// its packer lease. Already-stored nodes (e.g. zero-byte, finalized early) are
// skipped so this is idempotent.
func finalizeNodes(ctx context.Context, r *domain.Repositories, pending []domain.Node) error {
	for i := range pending {
		n, err := r.Nodes.GetByID(ctx, pending[i].ID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				continue // deleted while buffered
			}
			return err
		}
		if n.State == domain.NodeStored {
			continue
		}
		n.State = domain.NodeStored
		if err := r.Nodes.Update(ctx, n); err != nil {
			return err
		}
		if err := r.WAL.DeleteByNode(ctx, n.ID); err != nil {
			return err
		}
		if err := r.Nodes.ReleaseLease(ctx, n.ID); err != nil {
			return err
		}
	}
	return nil
}

// releaseLeases clears packer leases for every node referenced by a failed run
// so they are re-claimed promptly instead of waiting for lease expiry.
func (p *Packer) releaseLeases(ctx context.Context, cur *run, pending []domain.Node) {
	seen := map[uuid.UUID]struct{}{}
	release := func(id uuid.UUID) {
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		_ = p.repos.Nodes.ReleaseLease(ctx, id)
	}
	for i := range pending {
		release(pending[i].ID)
	}
	if cur != nil {
		for _, s := range cur.segs {
			release(s.nodeID)
		}
	}
}
