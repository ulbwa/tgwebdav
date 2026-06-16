package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// runMaintenance runs periodic housekeeping: collecting unreferenced blob rows,
// marking channel-evicted blobs unavailable, and refreshing bot membership.
func runMaintenance(
	ctx context.Context,
	repos *domain.Repositories,
	channels domain.ChannelService,
	bots domain.BotService,
	logger *slog.Logger,
) {
	log := logger.With("component", "maintenance")
	gc := time.NewTicker(5 * time.Minute)
	evict := time.NewTicker(10 * time.Minute)
	refresh := time.NewTicker(30 * time.Minute)
	defer gc.Stop()
	defer evict.Stop()
	defer refresh.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gc.C:
			collectBlobs(ctx, repos, log)
		case <-evict.C:
			evictChannels(ctx, repos, log)
		case <-refresh.C:
			if err := bots.RefreshMembership(ctx); err != nil {
				log.Warn("refresh membership", "err", err)
			}
		}
	}
}

// collectBlobs deletes blob rows that no extent references any more. The
// Telegram message itself is intentionally never deleted (blobs are shared and
// deletion is irreversible).
func collectBlobs(ctx context.Context, repos *domain.Repositories, log *slog.Logger) {
	collectable, err := repos.Blobs.ListCollectable(ctx, 500)
	if err != nil {
		log.Warn("list collectable blobs", "err", err)
		return
	}
	var n int
	for _, b := range collectable {
		if err := repos.Blobs.Delete(ctx, b.ID); err != nil {
			log.Warn("delete collectable blob", "blob", b.ID, "err", err)
			continue
		}
		n++
	}
	if n > 0 {
		log.Info("collected unreferenced blobs", "count", n)
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
