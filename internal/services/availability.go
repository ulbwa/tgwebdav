// Package services implements the bot, channel and settings management services
// defined by the domain port interfaces. Each service coordinates the
// repositories, the Telegram port and (for channels and bots) shared
// availability re-evaluation: a channel is available when at least one enabled
// bot is a member, and that availability is propagated to the channel's blobs.
package services

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// reevaluateAvailability recomputes every channel's availability from the
// current bot membership matrix and propagates the result to the channel's
// blobs. A channel is available iff at least one enabled bot is a member of it.
//
// For each channel the function flips Channels.SetAvailable and then, depending
// on the new value, calls Blobs.MarkChannelAvailable (restoring stored-eligible
// blobs) or Blobs.MarkChannelUnavailable (flipping every blob to unavailable).
// Because the per-channel update touches both the channel row and its blobs, it
// runs inside a single transaction so the two mutations stay consistent.
func reevaluateAvailability(ctx context.Context, r *domain.Repositories, tx domain.TxManager, logger *slog.Logger) error {
	channels, err := r.Channels.List(ctx)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}

	bots, err := r.Bots.List(ctx)
	if err != nil {
		return fmt.Errorf("list bots: %w", err)
	}

	// enabled[botID] reports whether a bot is currently enabled.
	enabled := make(map[uuid.UUID]bool, len(bots))
	for _, b := range bots {
		enabled[b.ID] = b.Enabled
	}

	for _, ch := range channels {
		available, err := channelHasEnabledMember(ctx, r, ch.ID, enabled)
		if err != nil {
			return err
		}

		if err := applyChannelAvailability(ctx, r, tx, ch, available, logger); err != nil {
			return err
		}
	}

	return nil
}

// channelHasEnabledMember reports whether the channel has at least one enabled
// bot that is recorded as a member.
func channelHasEnabledMember(ctx context.Context, r *domain.Repositories, channelID uuid.UUID, enabled map[uuid.UUID]bool) (bool, error) {
	members, err := r.BotChannels.ListByChannel(ctx, channelID)
	if err != nil {
		return false, fmt.Errorf("list bot channels for %s: %w", channelID, err)
	}
	for _, m := range members {
		if m.Member && enabled[m.BotID] {
			return true, nil
		}
	}
	return false, nil
}

// applyChannelAvailability persists the channel's new availability and the
// matching blob transition inside a single transaction.
func applyChannelAvailability(ctx context.Context, r *domain.Repositories, tx domain.TxManager, ch domain.Channel, available bool, logger *slog.Logger) error {
	err := tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		if err := r.Channels.SetAvailable(ctx, ch.ID, available); err != nil {
			return fmt.Errorf("set channel available: %w", err)
		}
		if available {
			if err := r.Blobs.MarkChannelAvailable(ctx, ch.ID); err != nil {
				return fmt.Errorf("mark channel blobs available: %w", err)
			}
		} else {
			if err := r.Blobs.MarkChannelUnavailable(ctx, ch.ID); err != nil {
				return fmt.Errorf("mark channel blobs unavailable: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("apply availability for channel %s: %w", ch.ID, err)
	}

	if available != ch.Available {
		logger.InfoContext(ctx, "channel availability changed",
			slog.String("channel_id", ch.ID.String()),
			slog.Int64("tg_chat_id", ch.TGChatID),
			slog.Bool("available", available),
		)
	}
	return nil
}
