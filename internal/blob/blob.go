// Package blob implements domain.BlobReader: it resolves the full bytes of a
// stored Telegram blob, transparently using the disk cache, multi-bot
// selection, the cached per-bot file_id fast path, cross-bot forward-recovery,
// rate-limit / forbidden handling, and the permanent-not-found cascade that
// marks a blob perm_unavailable and deletes the nodes that lived solely on it.
//
// The package depends only on domain interfaces (repositories, TelegramAPI,
// BlobCache, StatRecorder) so it can be wired without touching storage or
// transport implementations.
package blob

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// Reader implements domain.BlobReader. Construct it with NewReader.
type Reader struct {
	repos  *domain.Repositories
	tx     domain.TxManager
	tg     domain.TelegramAPI
	cache  domain.BlobCache
	stats  domain.StatRecorder
	logger *slog.Logger

	// now is overridable in tests; defaults to time.Now.
	now func() time.Time
}

// NewReader builds a *Reader. All collaborators are required; logger may be nil
// (a discard logger is substituted).
func NewReader(
	r *domain.Repositories,
	tx domain.TxManager,
	tg domain.TelegramAPI,
	cache domain.BlobCache,
	stats domain.StatRecorder,
	logger *slog.Logger,
) *Reader {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Reader{
		repos:  r,
		tx:     tx,
		tg:     tg,
		cache:  cache,
		stats:  stats,
		logger: logger,
		now:    time.Now,
	}
}

// candidate is a bot we may try, together with an optional already-cached
// per-bot file_id for the blob (the fast path).
type candidate struct {
	bot    *domain.Bot
	fileID string // "" when no cached file_id is known for this bot/blob
}

// ReadBlob resolves the full bytes of the blob identified by blobID.
//
// The blob must be in a readable state; otherwise domain.ErrBlobUnavailable is
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
//   - A *domain.RateLimitError parks the bot (SetUnavailableUntil) and moves on.
//   - domain.ErrTelegramForbidden records the bot as a non-member and moves on.
//
// Only when forward-recovery itself returns domain.ErrTelegramNotFound do we
// conclude the underlying message is permanently gone: the blob is marked
// perm_unavailable and every node that referenced solely this blob is
// cascade-deleted, inside a single transaction, with EventBlobPermDeleted and
// EventCascadeDelete logged. domain.ErrBlobUnavailable is then returned.
//
// On success the bytes are cached, read-byte stats recorded, and the bytes
// returned.
func (r *Reader) ReadBlob(ctx context.Context, blobID uuid.UUID) ([]byte, error) {
	blob, err := r.repos.Blobs.GetByID(ctx, blobID)
	if err != nil {
		return nil, fmt.Errorf("blob: get blob %s: %w", blobID, err)
	}
	if !blob.State.Readable() {
		return nil, fmt.Errorf("blob %s in state %s: %w", blobID, blob.State, domain.ErrBlobUnavailable)
	}

	// Cache hit short-circuits all network work.
	if data, ok := r.cache.Get(blobID); ok {
		r.stats.IncCacheHit()
		return data, nil
	}
	r.stats.IncCacheMiss()

	channel, err := r.repos.Channels.GetByID(ctx, blob.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("blob: get channel %s: %w", blob.ChannelID, err)
	}

	candidates, err := r.candidates(ctx, blob)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("blob %s: no member bot: %w", blobID, domain.ErrBlobUnavailable)
	}

	// permGone records whether any candidate proved (via forward-recovery) that
	// the underlying message is definitively gone. Stale cached file_ids do NOT
	// set this — only a not-found from forwarding does.
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
			// The message is gone; no other bot can recover it. Stop trying.
			break
		}
		// Transient (rate limit / forbidden / transport): move to the next bot.
		r.logger.WarnContext(ctx, "blob: candidate bot failed, trying next",
			slog.String("blob", blobID.String()),
			slog.String("bot", c.bot.Username),
			slog.Any("err", err))
	}

	if permGone {
		r.markPermUnavailable(ctx, blob)
		return nil, fmt.Errorf("blob %s message gone: %w", blobID, domain.ErrBlobUnavailable)
	}

	return nil, fmt.Errorf("blob %s: no usable bot: %w", blobID, domain.ErrBlobUnavailable)
}

// candidates builds the ordered candidate list: bots with a cached file_id for
// this blob first (fast path), then the remaining enabled+available member bots
// of the blob's channel.
func (r *Reader) candidates(ctx context.Context, blob *domain.Blob) ([]candidate, error) {
	now := r.now()

	// Member bots of the channel, in a stable order.
	bcs, err := r.repos.BotChannels.ListByChannel(ctx, blob.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("blob: list bot-channels for %s: %w", blob.ChannelID, err)
	}

	// Resolve member, enabled, available bots keyed by id.
	usable := make(map[uuid.UUID]*domain.Bot)
	order := make([]uuid.UUID, 0, len(bcs))
	for _, bc := range bcs {
		if !bc.Member {
			continue
		}
		bot, err := r.repos.Bots.GetByID(ctx, bc.BotID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
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
	files, err := r.repos.BlobBotFiles.ListByBlob(ctx, blob.ID)
	if err != nil {
		return nil, fmt.Errorf("blob: list cached file_ids for %s: %w", blob.ID, err)
	}
	fileIDByBot := make(map[uuid.UUID]string, len(files))
	for _, f := range files {
		fileIDByBot[f.BotID] = f.FileID
	}

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
// set when forward-recovery returns ErrTelegramNotFound); on a transient
// failure it is false and err is the transient error.
func (r *Reader) tryCandidate(
	ctx context.Context,
	blob *domain.Blob,
	channel *domain.Channel,
	c candidate,
) (data []byte, gone bool, err error) {
	// Fast path: try the cached file_id first.
	if c.fileID != "" {
		r.stats.IncTelegramReq()
		data, err = r.tg.DownloadFile(ctx, c.bot, c.fileID)
		switch {
		case err == nil:
			return data, false, nil
		case errors.Is(err, domain.ErrTelegramNotFound):
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
		case errors.Is(err, domain.ErrTelegramForbidden):
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
	case errors.Is(err, domain.ErrTelegramNotFound):
		// The message itself is gone — definitive.
		return nil, true, err
	case isRateLimit(err):
		r.parkBot(ctx, c.bot, err)
		return nil, false, err
	case errors.Is(err, domain.ErrTelegramForbidden):
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
		case errors.Is(err, domain.ErrTelegramNotFound):
			// We just forwarded and got a fresh file_id; a not-found here is a
			// transient/odd state, not proof the original is gone. Try next bot.
			return nil, false, fmt.Errorf("blob: download recovered file_id: %w", err)
		case isRateLimit(err):
			r.parkBot(ctx, c.bot, err)
			return nil, false, err
		case errors.Is(err, domain.ErrTelegramForbidden):
			r.recordNonMember(ctx, c.bot, channel)
			return nil, false, err
		default:
			return nil, false, fmt.Errorf("blob: download recovered file_id: %w", err)
		}
	}

	// Success: clean up the forwarded copy and cache the fresh file_id.
	r.bestEffortDelete(ctx, c.bot, channel.TGChatID, res.MessageID)
	if upErr := r.repos.BlobBotFiles.Upsert(ctx, &domain.BlobBotFile{
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
func (r *Reader) deleteStaleFileID(ctx context.Context, blobID, botID uuid.UUID) {
	if err := r.repos.BlobBotFiles.DeleteByBlob(ctx, blobID); err != nil {
		r.logger.WarnContext(ctx, "blob: delete stale file_id failed",
			slog.String("blob", blobID.String()),
			slog.String("bot", botID.String()),
			slog.Any("err", err))
	}
}

// markPermUnavailable marks the blob perm_unavailable and cascade-deletes every
// node whose extents reference solely this blob, all within a single
// transaction, logging EventBlobPermDeleted and EventCascadeDelete.
func (r *Reader) markPermUnavailable(ctx context.Context, blob *domain.Blob) {
	err := r.tx.WithTx(ctx, func(ctx context.Context, tr *domain.Repositories) error {
		if err := tr.Blobs.SetState(ctx, blob.ID, domain.BlobPermUnavailable); err != nil {
			return fmt.Errorf("set blob %s perm_unavailable: %w", blob.ID, err)
		}
		if err := tr.Events.Log(ctx, domain.EventBlobPermDeleted,
			"blob message gone, marked perm_unavailable", blob.ID.String()); err != nil {
			return fmt.Errorf("log perm-deleted event: %w", err)
		}

		nodeIDs, err := tr.Extents.ListNodesSolelyOnBlob(ctx, blob.ID)
		if err != nil {
			return fmt.Errorf("list nodes solely on blob %s: %w", blob.ID, err)
		}
		for _, nodeID := range nodeIDs {
			if err := tr.Nodes.Delete(ctx, nodeID); err != nil {
				return fmt.Errorf("cascade-delete node %s: %w", nodeID, err)
			}
			if err := tr.Events.Log(ctx, domain.EventCascadeDelete,
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
func (r *Reader) parkBot(ctx context.Context, bot *domain.Bot, err error) {
	var rl *domain.RateLimitError
	if !errors.As(err, &rl) {
		return
	}
	until := r.now().Add(rl.RetryAfter)
	if serr := r.repos.Bots.SetUnavailableUntil(ctx, bot.ID, &until); serr != nil {
		r.logger.WarnContext(ctx, "blob: set bot unavailable failed",
			slog.String("bot", bot.Username), slog.Any("err", serr))
	}
	if lerr := r.repos.Events.Log(ctx, domain.EventBotUnavailable,
		fmt.Sprintf("bot rate limited, unavailable for %s", rl.RetryAfter), bot.ID.String()); lerr != nil {
		r.logger.WarnContext(ctx, "blob: log bot-unavailable event failed",
			slog.String("bot", bot.Username), slog.Any("err", lerr))
	}
}

// recordNonMember marks the bot as a non-member of the channel after a 403
// (best-effort).
func (r *Reader) recordNonMember(ctx context.Context, bot *domain.Bot, channel *domain.Channel) {
	if err := r.repos.BotChannels.Upsert(ctx, &domain.BotChannel{
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
func (r *Reader) bestEffortDelete(ctx context.Context, bot *domain.Bot, chatID, messageID int64) {
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

// isRateLimit reports whether err is (or wraps) a *domain.RateLimitError.
func isRateLimit(err error) bool {
	var rl *domain.RateLimitError
	return errors.As(err, &rl)
}
