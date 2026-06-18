package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/samber/lo"

	"github.com/ulbwa/tgwebdav/internal/client/telegram"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
)

// ---- tiny dependency interfaces (Rule 5) -----------------------------------
//
// Each interface declares only the methods the blob reader actually calls,
// mirroring the old internal/domain repository/port shapes but expressed in
// model.* types. The real repositories, the telegram client, the disk cache,
// the stats recorder and the event repository satisfy these structurally.

// readerBlobStore is the slice of the blob repository the reader needs.
type readerBlobStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*model.Blob, error)
	SetState(ctx context.Context, id uuid.UUID, state model.BlobState) error
}

// readerChannelStore is the slice of the channel repository the reader needs.
type readerChannelStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*model.Channel, error)
}

// readerBotStore is the slice of the bot repository the reader needs.
type readerBotStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*model.Bot, error)
	SetUnavailableUntil(ctx context.Context, id uuid.UUID, until *time.Time) error
}

// readerBotChannelStore is the slice of the bot↔channel membership repository
// the reader needs.
type readerBotChannelStore interface {
	ListByChannel(ctx context.Context, channelID uuid.UUID) ([]model.BotChannel, error)
	Upsert(ctx context.Context, bc *model.BotChannel) error
}

// readerBlobBotFiles is the slice of the per-bot file_id cache repository the
// reader needs.
type readerBlobBotFiles interface {
	ListByBlob(ctx context.Context, blobID uuid.UUID) ([]model.BlobBotFile, error)
	Upsert(ctx context.Context, f *model.BlobBotFile) error
	DeleteByBlob(ctx context.Context, blobID uuid.UUID) error
}

// readerNodeStore is the slice of the node repository the cascade delete needs.
type readerNodeStore interface {
	Delete(ctx context.Context, id uuid.UUID) error
}

// readerExtentStore is the slice of the extent repository the cascade delete
// needs.
type readerExtentStore interface {
	ListNodesSolelyOnBlob(ctx context.Context, blobID uuid.UUID) ([]uuid.UUID, error)
}

// readerTelegram is the slice of the Telegram Bot API the reader needs. It
// returns model.TGSendResult and the typed errors *telegram.RateLimitError,
// telegram.ErrMessageNotFound and telegram.ErrForbidden.
type readerTelegram interface {
	DownloadFile(ctx context.Context, bot *model.Bot, fileID string) ([]byte, error)
	ForwardMessage(ctx context.Context, bot *model.Bot, toChatID, fromChatID, messageID int64) (model.TGSendResult, error)
	DeleteMessage(ctx context.Context, bot *model.Bot, chatID, messageID int64) error
}

// readerBlobCache is the disk-backed LRU over whole blobs the reader uses.
type readerBlobCache interface {
	Get(id uuid.UUID) ([]byte, bool)
	Put(id uuid.UUID, data []byte) error
	Has(id uuid.UUID) bool
	Capacity() int64
	Remove(id uuid.UUID)
}

// readerStatRecorder is the slice of the stats recorder the reader touches.
type readerStatRecorder interface {
	IncCacheHit()
	IncCacheMiss()
	IncTelegramReq()
	AddReadBytes(n int64)
}

// readerEventLogger logs audit/log events (best-effort or transactional).
type readerEventLogger interface {
	Log(ctx context.Context, kind, message, ref string) error
}

// readerTxManager runs a function inside a single database transaction. Unlike
// the old domain.TxManager (which re-bound a *Repositories), the canon
// TxManager puts the active tx on the context; repositories resolve it via
// database.FromContext, so the held stores are correct inside the closure.
type readerTxManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// BlobReader resolves the full bytes of a stored Telegram blob, transparently
// using the disk cache, multi-bot selection, the cached per-bot file_id fast
// path, cross-bot forward-recovery, rate-limit / forbidden handling, and the
// permanent-not-found cascade that marks a blob perm_unavailable and deletes the
// nodes that lived solely on it. Construct it with NewBlobReader.
type BlobReader struct {
	blobs    readerBlobStore
	channels readerChannelStore
	bots     readerBotStore
	botChans readerBotChannelStore
	files    readerBlobBotFiles
	nodes    readerNodeStore
	extents  readerExtentStore
	tx       readerTxManager
	tg       readerTelegram
	cache    readerBlobCache
	stats    readerStatRecorder
	events   readerEventLogger
	logger   *slog.Logger

	// prefetchConcurrency caps parallel read-ahead downloads.
	prefetchConcurrency int

	// now is overridable in tests; defaults to time.Now.
	now func() time.Time
}

// NewBlobReader builds a *BlobReader. All collaborators are required; logger may
// be nil (a discard logger is substituted).
func NewBlobReader(
	blobs readerBlobStore,
	channels readerChannelStore,
	bots readerBotStore,
	botChans readerBotChannelStore,
	files readerBlobBotFiles,
	nodes readerNodeStore,
	extents readerExtentStore,
	tx readerTxManager,
	tg readerTelegram,
	cache readerBlobCache,
	stats readerStatRecorder,
	events readerEventLogger,
	logger *slog.Logger,
) *BlobReader {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &BlobReader{
		blobs:               blobs,
		channels:            channels,
		bots:                bots,
		botChans:            botChans,
		files:               files,
		nodes:               nodes,
		extents:             extents,
		tx:                  tx,
		tg:                  tg,
		cache:               cache,
		stats:               stats,
		events:              events,
		logger:              logger,
		prefetchConcurrency: 8,
		now:                 time.Now,
	}
}

// Prefetch warms the cache by downloading blobIDs concurrently, bounded so the
// prefetched data does not exceed the cache capacity (older entries the reader
// still needs would otherwise be evicted). Already-cached blobs are skipped.
// It blocks until the bounded set is fetched; run it in a goroutine.
func (r *BlobReader) Prefetch(ctx context.Context, blobIDs []uuid.UUID) {
	capacity := r.cache.Capacity()
	if capacity <= 0 || len(blobIDs) == 0 {
		return
	}
	// Conservative cap on how many blobs to hold ahead: blobs are < 20 MiB, so
	// capacity/20MiB pieces are guaranteed to fit. Leave at least one slot.
	const maxBlob = 20 << 20
	limit := int(capacity / maxBlob)
	if limit < 1 {
		limit = 1
	}

	conc := r.prefetchConcurrency
	if conc < 1 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	fetched := 0
	for _, id := range blobIDs {
		if fetched >= limit {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if r.cache.Has(id) {
			continue
		}
		fetched++
		wg.Add(1)
		sem <- struct{}{}
		go func(id uuid.UUID) {
			defer wg.Done()
			defer func() { <-sem }()
			// ReadBlob downloads (via bot selection + recovery) and caches the
			// blob; the bytes are discarded here — we only want it warm.
			if _, err := r.ReadBlob(ctx, id); err != nil {
				r.logger.DebugContext(ctx, "blob: prefetch failed (ignored)",
					slog.String("blob", id.String()), slog.Any("err", err))
			}
		}(id)
	}
	wg.Wait()
}

// candidate is a bot we may try, together with an optional already-cached
// per-bot file_id for the blob (the fast path).
type candidate struct {
	bot    *model.Bot
	fileID string // "" when no cached file_id is known for this bot/blob
}

// ReadBlob resolves the full bytes of the blob identified by blobID.
//
// The blob must be in a readable state; otherwise ErrBlobUnavailable is
// returned. A cache hit short-circuits everything. On a miss the reader builds
// an ordered list of candidate bots — first any bot that already has a cached
// file_id for this blob, then the remaining enabled+available member bots of
// the blob's channel — and tries each in turn:
//
//   - A cached file_id is downloaded directly. If Telegram reports the file as
//     not found the cached file_id is STALE: its blob_bot_files row is deleted
//     and the candidate falls through to forward-recovery (we do NOT conclude
//     the message is gone yet).
//   - Forward-recovery forwards the blob's message within its channel to mint a
//     fresh file_id, downloads it, best-effort deletes the forwarded copy and
//     caches the new file_id.
//   - A *telegram.RateLimitError parks the bot (SetUnavailableUntil) and moves on.
//   - telegram.ErrForbidden records the bot as a non-member and moves on.
//
// Only when forward-recovery itself returns telegram.ErrMessageNotFound do we
// conclude the underlying message is permanently gone: the blob is marked
// perm_unavailable and every node that referenced solely this blob is
// cascade-deleted, inside a single transaction, with EventBlobPermDeleted and
// EventCascadeDelete logged. ErrBlobUnavailable is then returned.
//
// On success the bytes are cached, read-byte stats recorded, and the bytes
// returned.
func (r *BlobReader) ReadBlob(ctx context.Context, blobID uuid.UUID) ([]byte, error) {
	blob, err := r.blobs.GetByID(ctx, blobID)
	if err != nil {
		return nil, fmt.Errorf("blob: get blob %s: %w", blobID, err)
	}
	if !blob.State.Readable() {
		return nil, fmt.Errorf("blob %s in state %s: %w", blobID, blob.State, ErrBlobUnavailable)
	}

	// Cache hit short-circuits all network work.
	if data, ok := r.cache.Get(blobID); ok {
		r.stats.IncCacheHit()
		return data, nil
	}
	r.stats.IncCacheMiss()

	channel, err := r.channels.GetByID(ctx, blob.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("blob: get channel %s: %w", blob.ChannelID, err)
	}

	candidates, err := r.candidates(ctx, blob)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("blob %s: no member bot: %w", blobID, ErrBlobUnavailable)
	}

	// permGone records whether any candidate proved (via forward-recovery) that
	// the underlying message is definitively gone. Stale cached file_ids do NOT
	// set this — only a not-found from forwarding does. We do NOT stop at the
	// first such signal: a different member bot may still be able to forward the
	// message (cross-bot recovery), so we try every candidate before concluding
	// the message is permanently gone.
	permGone := false

	for _, c := range candidates {
		data, gone, err := r.tryCandidate(ctx, blob, channel, c)
		if err == nil {
			if putErr := r.cache.Put(blobID, data); putErr != nil {
				r.logger.WarnContext(ctx, "blob: cache put failed",
					slog.String("blob", blobID.String()), slog.Any("err", putErr))
			}
			r.stats.AddReadBytes(int64(len(data)))
			return data, nil
		}
		if gone {
			permGone = true
		}
		// Transient (rate limit / forbidden / transport) or a single bot's
		// not-found: move to the next bot.
		r.logger.WarnContext(ctx, "blob: candidate bot failed, trying next",
			slog.String("blob", blobID.String()),
			slog.String("bot", c.bot.Username),
			slog.Any("err", err))
	}

	// Only after every member bot failed to recover the message — and at least
	// one of them got a definitive not-found from forwarding — do we conclude the
	// message is permanently gone.
	if permGone {
		r.markPermUnavailable(ctx, blob)
		return nil, fmt.Errorf("blob %s message gone: %w", blobID, ErrBlobUnavailable)
	}

	return nil, fmt.Errorf("blob %s: no usable bot: %w", blobID, ErrBlobUnavailable)
}

// candidates builds the ordered candidate list: bots with a cached file_id for
// this blob first (fast path), then the remaining enabled+available member bots
// of the blob's channel.
func (r *BlobReader) candidates(ctx context.Context, blob *model.Blob) ([]candidate, error) {
	now := r.now()

	// Member bots of the channel, in a stable order.
	bcs, err := r.botChans.ListByChannel(ctx, blob.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("blob: list bot-channels for %s: %w", blob.ChannelID, err)
	}

	// Resolve member, enabled, available bots keyed by id.
	usable := make(map[uuid.UUID]*model.Bot)
	order := make([]uuid.UUID, 0, len(bcs))
	for _, bc := range bcs {
		if !bc.Member {
			continue
		}
		bot, err := r.bots.GetByID(ctx, bc.BotID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("blob: get bot %s: %w", bc.BotID, err)
		}
		if !bot.Available(now) {
			continue
		}
		if _, seen := usable[bot.ID]; seen {
			continue
		}
		usable[bot.ID] = bot
		order = append(order, bot.ID)
	}

	// Cached file_ids for this blob give the fast-path bots.
	files, err := r.files.ListByBlob(ctx, blob.ID)
	if err != nil {
		return nil, fmt.Errorf("blob: list cached file_ids for %s: %w", blob.ID, err)
	}
	fileIDByBot := lo.Associate(files, func(f model.BlobBotFile) (uuid.UUID, string) {
		return f.BotID, f.FileID
	})

	candidates := make([]candidate, 0, len(order))
	added := make(map[uuid.UUID]bool, len(order))

	// 1. Fast path: usable bots that already hold a cached file_id, in member order.
	for _, id := range order {
		if fileID, ok := fileIDByBot[id]; ok {
			candidates = append(candidates, candidate{bot: usable[id], fileID: fileID})
			added[id] = true
		}
	}
	// 2. Remaining usable member bots (forward-recovery only).
	for _, id := range order {
		if added[id] {
			continue
		}
		candidates = append(candidates, candidate{bot: usable[id]})
		added[id] = true
	}

	return candidates, nil
}

// tryCandidate attempts to read the blob with one bot. It returns the bytes on
// success. The bool result reports a definitive "message gone" outcome (only
// set when forward-recovery returns ErrMessageNotFound); on a transient
// failure it is false and err is the transient error.
func (r *BlobReader) tryCandidate(
	ctx context.Context,
	blob *model.Blob,
	channel *model.Channel,
	c candidate,
) (data []byte, gone bool, err error) {
	// Fast path: try the cached file_id first.
	if c.fileID != "" {
		r.stats.IncTelegramReq()
		data, err = r.tg.DownloadFile(ctx, c.bot, c.fileID)
		switch {
		case err == nil:
			return data, false, nil
		case errors.Is(err, telegram.ErrMessageNotFound):
			// STALE cached file_id: drop just this row and fall through to
			// recovery. Do NOT perm-delete the blob on this signal.
			r.logger.InfoContext(ctx, "blob: cached file_id stale, recovering",
				slog.String("blob", blob.ID.String()),
				slog.String("bot", c.bot.Username))
			r.deleteStaleFileID(ctx, blob.ID, c.bot.ID)
			// fall through to recovery below
		case isRateLimit(err):
			r.parkBot(ctx, c.bot, err)
			return nil, false, err
		case errors.Is(err, telegram.ErrForbidden):
			r.recordNonMember(ctx, c.bot, channel)
			return nil, false, err
		default:
			// Transport / unexpected error: treat as transient, try next bot.
			return nil, false, fmt.Errorf("blob: download cached file_id: %w", err)
		}
	}

	// Forward-recovery: mint a fresh file_id by forwarding the message within
	// its own channel, download it, then best-effort delete the forwarded copy.
	r.stats.IncTelegramReq()
	res, err := r.tg.ForwardMessage(ctx, c.bot, channel.TGChatID, channel.TGChatID, blob.MessageID)
	switch {
	case err == nil:
		// proceed to download
	case errors.Is(err, telegram.ErrMessageNotFound):
		// The message itself is gone — definitive.
		return nil, true, err
	case isRateLimit(err):
		r.parkBot(ctx, c.bot, err)
		return nil, false, err
	case errors.Is(err, telegram.ErrForbidden):
		r.recordNonMember(ctx, c.bot, channel)
		return nil, false, err
	default:
		return nil, false, fmt.Errorf("blob: forward message: %w", err)
	}

	r.stats.IncTelegramReq()
	data, err = r.tg.DownloadFile(ctx, c.bot, res.FileID)
	if err != nil {
		// Best-effort cleanup of the forwarded copy even on download failure.
		r.bestEffortDelete(ctx, c.bot, channel.TGChatID, res.MessageID)
		switch {
		case errors.Is(err, telegram.ErrMessageNotFound):
			// We just forwarded and got a fresh file_id; a not-found here is a
			// transient/odd state, not proof the original is gone. Try next bot.
			return nil, false, fmt.Errorf("blob: download recovered file_id: %w", err)
		case isRateLimit(err):
			r.parkBot(ctx, c.bot, err)
			return nil, false, err
		case errors.Is(err, telegram.ErrForbidden):
			r.recordNonMember(ctx, c.bot, channel)
			return nil, false, err
		default:
			return nil, false, fmt.Errorf("blob: download recovered file_id: %w", err)
		}
	}

	// Success: clean up the forwarded copy and cache the fresh file_id.
	r.bestEffortDelete(ctx, c.bot, channel.TGChatID, res.MessageID)
	if upErr := r.files.Upsert(ctx, &model.BlobBotFile{
		BlobID:       blob.ID,
		BotID:        c.bot.ID,
		FileID:       res.FileID,
		FileUniqueID: res.FileUniqueID,
		FetchedAt:    r.now(),
	}); upErr != nil {
		r.logger.WarnContext(ctx, "blob: upsert recovered file_id failed",
			slog.String("blob", blob.ID.String()),
			slog.String("bot", c.bot.Username),
			slog.Any("err", upErr))
	}
	return data, false, nil
}

// deleteStaleFileID drops the stale cached file_id for this blob. The contract
// exposes only DeleteByBlob (scoped to one blob, all bots) and DeleteByBot
// (scoped to one bot, all blobs). DeleteByBlob is the correct narrowest
// primitive: it removes only this blob's cached file_ids without touching other
// blobs. Any other bot's entry for this blob is simply re-recovered and
// re-cached on demand.
func (r *BlobReader) deleteStaleFileID(ctx context.Context, blobID, botID uuid.UUID) {
	if err := r.files.DeleteByBlob(ctx, blobID); err != nil {
		r.logger.WarnContext(ctx, "blob: delete stale file_id failed",
			slog.String("blob", blobID.String()),
			slog.String("bot", botID.String()),
			slog.Any("err", err))
	}
}

// markPermUnavailable marks the blob perm_unavailable and cascade-deletes every
// node whose extents reference solely this blob, all within a single
// transaction, logging EventBlobPermDeleted and EventCascadeDelete.
func (r *BlobReader) markPermUnavailable(ctx context.Context, blob *model.Blob) {
	err := r.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := r.blobs.SetState(ctx, blob.ID, model.BlobStatePermUnavailable); err != nil {
			return fmt.Errorf("set blob %s perm_unavailable: %w", blob.ID, err)
		}
		if err := r.events.Log(ctx, model.EventBlobPermDeleted,
			"blob message gone, marked perm_unavailable", blob.ID.String()); err != nil {
			return fmt.Errorf("log perm-deleted event: %w", err)
		}

		nodeIDs, err := r.extents.ListNodesSolelyOnBlob(ctx, blob.ID)
		if err != nil {
			return fmt.Errorf("list nodes solely on blob %s: %w", blob.ID, err)
		}
		for _, nodeID := range nodeIDs {
			if err := r.nodes.Delete(ctx, nodeID); err != nil {
				return fmt.Errorf("cascade-delete node %s: %w", nodeID, err)
			}
			if err := r.events.Log(ctx, model.EventCascadeDelete,
				"node deleted: only content lived on a now-gone blob", nodeID.String()); err != nil {
				return fmt.Errorf("log cascade-delete event: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		r.logger.ErrorContext(ctx, "blob: perm-unavailable cascade failed",
			slog.String("blob", blob.ID.String()), slog.Any("err", err))
	}
}

// parkBot records a Telegram rate-limit by marking the bot unavailable until
// now+RetryAfter and logging an availability event (best-effort).
func (r *BlobReader) parkBot(ctx context.Context, bot *model.Bot, err error) {
	var rl *telegram.RateLimitError
	if !errors.As(err, &rl) {
		return
	}
	until := r.now().Add(rl.RetryAfter)
	if serr := r.bots.SetUnavailableUntil(ctx, bot.ID, &until); serr != nil {
		r.logger.WarnContext(ctx, "blob: set bot unavailable failed",
			slog.String("bot", bot.Username), slog.Any("err", serr))
	}
	if lerr := r.events.Log(ctx, model.EventBotUnavailable,
		fmt.Sprintf("bot rate limited, unavailable for %s", rl.RetryAfter), bot.ID.String()); lerr != nil {
		r.logger.WarnContext(ctx, "blob: log bot-unavailable event failed",
			slog.String("bot", bot.Username), slog.Any("err", lerr))
	}
}

// recordNonMember marks the bot as a non-member of the channel after a 403
// (best-effort).
func (r *BlobReader) recordNonMember(ctx context.Context, bot *model.Bot, channel *model.Channel) {
	if err := r.botChans.Upsert(ctx, &model.BotChannel{
		BotID:     bot.ID,
		ChannelID: channel.ID,
		Member:    false,
		CheckedAt: r.now(),
	}); err != nil {
		r.logger.WarnContext(ctx, "blob: record non-member failed",
			slog.String("bot", bot.Username),
			slog.String("channel", channel.ID.String()),
			slog.Any("err", err))
	}
}

// bestEffortDelete removes a forwarded copy, logging but ignoring failures.
func (r *BlobReader) bestEffortDelete(ctx context.Context, bot *model.Bot, chatID, messageID int64) {
	if messageID == 0 {
		return
	}
	if err := r.tg.DeleteMessage(ctx, bot, chatID, messageID); err != nil {
		r.logger.DebugContext(ctx, "blob: delete forwarded copy failed (ignored)",
			slog.String("bot", bot.Username),
			slog.Int64("message", messageID),
			slog.Any("err", err))
	}
}

// isRateLimit reports whether err is (or wraps) a *telegram.RateLimitError.
func isRateLimit(err error) bool {
	var rl *telegram.RateLimitError
	return errors.As(err, &rl)
}
