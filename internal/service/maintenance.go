package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/client/telegram"
	"github.com/ulbwa/tgwebdav/internal/model"
)

// ---- tiny dependency interfaces (Rule 5) -----------------------------------
//
// The maintenance loop declares only the methods it calls on each collaborator,
// in model.* types. They are maint-prefixed to avoid colliding with the
// identically-shaped interfaces other services in this package already declare.
// The real repositories, the telegram client and the bot service satisfy these
// structurally.

// maintBlobStore is the slice of the blob repository maintenance needs: listing
// unreferenced blobs to reap, deleting their rows and evicting blobs past a
// channel's retention window.
type maintBlobStore interface {
	ListCollectable(ctx context.Context, limit int) ([]model.Blob, error)
	Delete(ctx context.Context, id uuid.UUID) error
	EvictOlderThan(ctx context.Context, channelID uuid.UUID, minSeq int64) (int64, error)
}

// maintChannelStore is the slice of the channel repository maintenance needs.
type maintChannelStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*model.Channel, error)
	List(ctx context.Context) ([]model.Channel, error)
}

// maintBotStore is the slice of the bot repository maintenance needs (to park a
// rate-limited bot so uploads keep priority).
type maintBotStore interface {
	SetUnavailableUntil(ctx context.Context, id uuid.UUID, until *time.Time) error
}

// maintTelegram is the slice of the Telegram Bot API maintenance needs.
type maintTelegram interface {
	DeleteMessage(ctx context.Context, bot *model.Bot, chatID, messageID int64) error
}

// maintBotPicker selects an available member bot for a channel. Satisfied by
// the bot service.
type maintBotPicker interface {
	PickForUpload(ctx context.Context, channelID uuid.UUID) (*model.Bot, error)
}

// maintBotRefresher re-checks bot↔channel membership. Satisfied by the bot
// service.
type maintBotRefresher interface {
	RefreshMembership(ctx context.Context) error
}

// maintEventLogger records audit/log events.
type maintEventLogger interface {
	Log(ctx context.Context, kind, message, ref string) error
}

// Reaper tuning: a blob whose refcount has dropped to 0 has its Telegram message
// physically deleted, then its row removed. This runs in the background at low
// priority — a small batch per cycle, spaced by reapThrottle, using the same
// availability-aware bot selection as uploads (so a bot busy/parked by uploads
// is simply skipped) and backing off immediately on a rate limit. Uploads of new
// files therefore always win the bots' rate budget.
const (
	reapBatch    = 50
	reapThrottle = 250 * time.Millisecond
)

// MaintenanceService runs periodic housekeeping: physically reaping
// unreferenced blobs from Telegram, evicting channel blobs past their retention
// window, and refreshing bot membership. Construct it with
// NewMaintenanceService and drive its lifecycle with Run.
type MaintenanceService struct {
	blobs     maintBlobStore
	channels  maintChannelStore
	bots      maintBotStore
	picker    maintBotPicker
	refresher maintBotRefresher
	tg        maintTelegram
	events    maintEventLogger
	logger    *slog.Logger
}

// NewMaintenanceService wires a MaintenanceService from its dependencies. logger
// may be nil (the package default is substituted).
func NewMaintenanceService(
	blobs maintBlobStore,
	channels maintChannelStore,
	bots maintBotStore,
	picker maintBotPicker,
	refresher maintBotRefresher,
	tg maintTelegram,
	events maintEventLogger,
	logger *slog.Logger,
) *MaintenanceService {
	if logger == nil {
		logger = slog.Default()
	}
	return &MaintenanceService{
		blobs:     blobs,
		channels:  channels,
		bots:      bots,
		picker:    picker,
		refresher: refresher,
		tg:        tg,
		events:    events,
		logger:    logger.With("component", "maintenance"),
	}
}

// Run runs the periodic housekeeping loop until ctx is cancelled: reaping
// unreferenced blobs every minute, evicting channel blobs past retention every
// 10 minutes, and refreshing bot membership every 30 minutes.
func (s *MaintenanceService) Run(ctx context.Context) {
	reap := time.NewTicker(time.Minute)
	evict := time.NewTicker(10 * time.Minute)
	refresh := time.NewTicker(30 * time.Minute)
	defer reap.Stop()
	defer evict.Stop()
	defer refresh.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-reap.C:
			s.reap(ctx)
		case <-evict.C:
			s.evict(ctx)
		case <-refresh.C:
			if err := s.refresher.RefreshMembership(ctx); err != nil {
				s.logger.Warn("refresh membership", "err", err)
			}
		}
	}
}

// reap physically deletes the Telegram message of each unreferenced (refcount 0)
// blob via an available member bot, then removes its row. It is gentle by
// design: a bounded batch, throttled between deletes, skipping blobs whose
// channel has no currently-usable bot, and stopping early on a rate limit so it
// never competes with uploads for the bots' budget.
func (s *MaintenanceService) reap(ctx context.Context) {
	collectable, err := s.blobs.ListCollectable(ctx, reapBatch)
	if err != nil {
		s.logger.Warn("list collectable blobs", "err", err)
		return
	}

	var deleted int
	for _, b := range collectable {
		if ctx.Err() != nil {
			return
		}
		channel, err := s.channels.GetByID(ctx, b.ChannelID)
		if err != nil {
			continue
		}
		bot, err := s.picker.PickForUpload(ctx, b.ChannelID)
		if err != nil {
			// No usable bot right now (e.g. all busy/parked by uploads) — leave
			// the blob for a later cycle so uploads keep priority.
			continue
		}

		err = s.tg.DeleteMessage(ctx, bot, channel.TGChatID, b.MessageID)
		switch {
		case err == nil, errors.Is(err, telegram.ErrMessageNotFound):
			// Deleted, or already gone — drop the row either way.
		default:
			var rl *telegram.RateLimitError
			if errors.As(err, &rl) {
				until := time.Now().Add(rl.RetryAfter)
				_ = s.bots.SetUnavailableUntil(ctx, bot.ID, &until)
				return // back off; uploads get the budget
			}
			s.logger.Warn("reap delete message", "blob", b.ID, "err", err)
			continue
		}

		if err := s.blobs.Delete(ctx, b.ID); err != nil {
			s.logger.Warn("reap delete blob row", "blob", b.ID, "err", err)
			continue
		}
		_ = s.events.Log(ctx, model.EventBlobReaped, "unreferenced blob message deleted from Telegram", b.ID.String())
		deleted++

		select {
		case <-ctx.Done():
			return
		case <-time.After(reapThrottle):
		}
	}
	if deleted > 0 {
		s.logger.Info("reaped unreferenced blobs", "count", deleted)
	}
}

// evict marks blobs whose channel messages fall outside the retention window
// (counter - eviction_threshold) as unavailable.
func (s *MaintenanceService) evict(ctx context.Context) {
	channels, err := s.channels.List(ctx)
	if err != nil {
		s.logger.Warn("list channels for eviction", "err", err)
		return
	}
	for _, c := range channels {
		minSeq := c.MessageCounter - c.EvictionThreshold
		if minSeq <= 0 {
			continue
		}
		n, err := s.blobs.EvictOlderThan(ctx, c.ID, minSeq)
		if err != nil {
			s.logger.Warn("evict channel blobs", "channel", c.ID, "err", err)
			continue
		}
		if n > 0 {
			_ = s.events.Log(ctx, model.EventChannelEvicted, "blobs evicted past retention window", c.ID.String())
			s.logger.Info("evicted channel blobs", "channel", c.ID, "count", n)
		}
	}
}
