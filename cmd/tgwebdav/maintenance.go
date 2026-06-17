package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

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

// runMaintenance runs periodic housekeeping: physically reaping unreferenced
// blobs from Telegram, marking channel-evicted blobs unavailable, and refreshing
// bot membership.
func runMaintenance(
	ctx context.Context,
	repos *domain.Repositories,
	tg domain.TelegramAPI,
	channels domain.ChannelService,
	bots domain.BotService,
	logger *slog.Logger,
) {
	log := logger.With("component", "maintenance")
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
			reapBlobs(ctx, repos, tg, bots, log)
		case <-evict.C:
			evictChannels(ctx, repos, log)
		case <-refresh.C:
			if err := bots.RefreshMembership(ctx); err != nil {
				log.Warn("refresh membership", "err", err)
			}
		}
	}
}

// reapBlobs physically deletes the Telegram message of each unreferenced
// (refcount 0) blob via an available member bot, then removes its row. It is
// gentle by design: a bounded batch, throttled between deletes, skipping blobs
// whose channel has no currently-usable bot, and stopping early on a rate limit
// so it never competes with uploads for the bots' budget.
func reapBlobs(ctx context.Context, repos *domain.Repositories, tg domain.TelegramAPI, bots domain.BotService, log *slog.Logger) {
	collectable, err := repos.Blobs.ListCollectable(ctx, reapBatch)
	if err != nil {
		log.Warn("list collectable blobs", "err", err)
		return
	}

	var deleted int
	for _, b := range collectable {
		if ctx.Err() != nil {
			return
		}
		channel, err := repos.Channels.GetByID(ctx, b.ChannelID)
		if err != nil {
			continue
		}
		bot, err := bots.PickForUpload(ctx, b.ChannelID)
		if err != nil {
			// No usable bot right now (e.g. all busy/parked by uploads) — leave
			// the blob for a later cycle so uploads keep priority.
			continue
		}

		err = tg.DeleteMessage(ctx, bot, channel.TGChatID, b.MessageID)
		switch {
		case err == nil, errors.Is(err, domain.ErrTelegramNotFound):
			// Deleted, or already gone — drop the row either way.
		default:
			var rl *domain.RateLimitError
			if errors.As(err, &rl) {
				until := time.Now().Add(rl.RetryAfter)
				_ = repos.Bots.SetUnavailableUntil(ctx, bot.ID, &until)
				return // back off; uploads get the budget
			}
			log.Warn("reap delete message", "blob", b.ID, "err", err)
			continue
		}

		if err := repos.Blobs.Delete(ctx, b.ID); err != nil {
			log.Warn("reap delete blob row", "blob", b.ID, "err", err)
			continue
		}
		_ = repos.Events.Log(ctx, domain.EventBlobReaped, "unreferenced blob message deleted from Telegram", b.ID.String())
		deleted++

		select {
		case <-ctx.Done():
			return
		case <-time.After(reapThrottle):
		}
	}
	if deleted > 0 {
		log.Info("reaped unreferenced blobs", "count", deleted)
	}
}

// evictChannels marks blobs whose channel messages fall outside the retention
// window (counter - eviction_threshold) as unavailable.
func evictChannels(ctx context.Context, repos *domain.Repositories, log *slog.Logger) {
	channels, err := repos.Channels.List(ctx)
	if err != nil {
		log.Warn("list channels for eviction", "err", err)
		return
	}
	for _, c := range channels {
		minSeq := c.MessageCounter - c.EvictionThreshold
		if minSeq <= 0 {
			continue
		}
		n, err := repos.Blobs.EvictOlderThan(ctx, c.ID, minSeq)
		if err != nil {
			log.Warn("evict channel blobs", "channel", c.ID, "err", err)
			continue
		}
		if n > 0 {
			_ = repos.Events.Log(ctx, domain.EventChannelEvicted, "blobs evicted past retention window", c.ID.String())
			log.Info("evicted channel blobs", "channel", c.ID, "count", n)
		}
	}
}
