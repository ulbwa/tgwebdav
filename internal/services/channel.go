package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// channelService implements domain.ChannelService: channel lifecycle,
// availability re-evaluation and round-robin upload-channel selection.
type channelService struct {
	repos    *domain.Repositories
	tx       domain.TxManager
	tg       domain.TelegramAPI
	settings domain.SettingsService
	logger   *slog.Logger

	// rr is a monotonic counter used to round-robin PickForUpload across the
	// eligible channels.
	rr atomic.Uint64
}

// NewChannelService returns a domain.ChannelService.
func NewChannelService(r *domain.Repositories, tx domain.TxManager, tg domain.TelegramAPI, settings domain.SettingsService, logger *slog.Logger) domain.ChannelService {
	return &channelService{repos: r, tx: tx, tg: tg, settings: settings, logger: logger}
}

// chatIDFromBareID applies the Telegram "-100" supergroup/channel prefix to a
// bare channel id and returns the full chat id passed to the Bot API.
func chatIDFromBareID(bareID int64) (int64, error) {
	chatID, err := strconv.ParseInt("-100"+strconv.FormatInt(bareID, 10), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("compute chat id from bare id %d: %w", bareID, err)
	}
	return chatID, nil
}

// Add registers a channel by its bare id (the "-100" prefix is applied
// internally). It is idempotent by chat id: re-adding an existing channel
// reuses its row. New channels seed their eviction threshold from settings and
// start available. The bot membership matrix is refreshed for every known bot
// and channel availability is re-evaluated.
func (s *channelService) Add(ctx context.Context, bareID int64) (*domain.Channel, error) {
	chatID, err := chatIDFromBareID(bareID)
	if err != nil {
		return nil, err
	}

	channel, err := s.repos.Channels.GetByChatID(ctx, chatID)
	switch {
	case err == nil:
		// Idempotent: reuse the existing channel row.
	case errorsIsNotFound(err):
		settings, err := s.settings.Get(ctx)
		if err != nil {
			return nil, fmt.Errorf("load settings: %w", err)
		}
		channel = &domain.Channel{
			ID:                uuid.New(),
			TGChatID:          chatID,
			EvictionThreshold: settings.DefaultEvictionThreshold,
			Available:         true,
		}
		if err := s.repos.Channels.Create(ctx, channel); err != nil {
			return nil, fmt.Errorf("create channel %d: %w", chatID, err)
		}
	default:
		return nil, fmt.Errorf("lookup channel by chat id %d: %w", chatID, err)
	}

	bots, err := s.repos.Bots.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	now := time.Now()
	for i := range bots {
		bot := &bots[i]
		title, member, err := s.tg.GetChat(ctx, bot, chatID)
		if err != nil {
			return nil, fmt.Errorf("check membership (bot %s, chat %d): %w", bot.ID, chatID, err)
		}
		// Record the channel title from the first bot that can see it.
		if member && title != "" && channel.Title == "" {
			channel.Title = title
			if err := s.repos.Channels.Update(ctx, channel); err != nil {
				return nil, fmt.Errorf("update channel title: %w", err)
			}
		}
		bc := &domain.BotChannel{
			BotID:     bot.ID,
			ChannelID: channel.ID,
			Member:    member,
			CheckedAt: now,
		}
		if err := s.repos.BotChannels.Upsert(ctx, bc); err != nil {
			return nil, fmt.Errorf("upsert bot_channel (%s,%s): %w", bot.ID, channel.ID, err)
		}
	}

	if err := reevaluateAvailability(ctx, s.repos, s.tx, s.logger); err != nil {
		return nil, fmt.Errorf("reevaluate availability: %w", err)
	}

	s.logger.InfoContext(ctx, "channel added",
		slog.String("channel_id", channel.ID.String()),
		slog.Int64("tg_chat_id", channel.TGChatID),
	)
	return channel, nil
}

// Remove decommissions a channel: it marks the channel unavailable and flips
// its blobs to unavailable, then logs the event. The row is NOT deleted because
// blobs FK-reference it.
func (s *channelService) Remove(ctx context.Context, id uuid.UUID) error {
	err := s.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		if err := r.Channels.SetAvailable(ctx, id, false); err != nil {
			return fmt.Errorf("set channel unavailable: %w", err)
		}
		if err := r.Blobs.MarkChannelUnavailable(ctx, id); err != nil {
			return fmt.Errorf("mark channel blobs unavailable: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("remove channel %s: %w", id, err)
	}

	if err := s.repos.Events.Log(ctx, domain.EventChannelDisabled, "channel disabled", id.String()); err != nil {
		return fmt.Errorf("log channel disabled event: %w", err)
	}

	s.logger.InfoContext(ctx, "channel removed", slog.String("channel_id", id.String()))
	return nil
}

// SetEvictionThreshold updates a channel's eviction threshold.
func (s *channelService) SetEvictionThreshold(ctx context.Context, id uuid.UUID, threshold int64) error {
	channel, err := s.repos.Channels.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get channel %s: %w", id, err)
	}

	channel.EvictionThreshold = threshold
	if err := s.repos.Channels.Update(ctx, channel); err != nil {
		return fmt.Errorf("update channel %s: %w", id, err)
	}
	return nil
}

// List returns every channel.
func (s *channelService) List(ctx context.Context) ([]domain.Channel, error) {
	channels, err := s.repos.Channels.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	return channels, nil
}

// Get returns the channel with the given id.
func (s *channelService) Get(ctx context.Context, id uuid.UUID) (*domain.Channel, error) {
	channel, err := s.repos.Channels.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get channel %s: %w", id, err)
	}
	return channel, nil
}

// ReevaluateAvailability recomputes channel availability from bot membership
// and propagates the result to each channel's blobs.
func (s *channelService) ReevaluateAvailability(ctx context.Context) error {
	if err := reevaluateAvailability(ctx, s.repos, s.tx, s.logger); err != nil {
		return fmt.Errorf("reevaluate availability: %w", err)
	}
	return nil
}

// PickForUpload returns an available channel that has at least one enabled
// member bot, round-robining across the eligible channels via an atomic
// counter. When no such channel exists it returns domain.ErrNoBot.
func (s *channelService) PickForUpload(ctx context.Context) (*domain.Channel, error) {
	eligible, err := s.eligibleChannels(ctx)
	if err != nil {
		return nil, err
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no channel with an available member bot: %w", domain.ErrNoBot)
	}

	idx := int(s.rr.Add(1)-1) % len(eligible)
	channel := eligible[idx]
	return &channel, nil
}

// eligibleChannels returns the channels that are available and have at least one
// enabled member bot.
func (s *channelService) eligibleChannels(ctx context.Context) ([]domain.Channel, error) {
	channels, err := s.repos.Channels.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}

	bots, err := s.repos.Bots.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	enabled := make(map[uuid.UUID]bool, len(bots))
	for _, b := range bots {
		enabled[b.ID] = b.Enabled
	}

	var eligible []domain.Channel
	for _, ch := range channels {
		if !ch.Available {
			continue
		}
		hasMember, err := channelHasEnabledMember(ctx, s.repos, ch.ID, enabled)
		if err != nil {
			return nil, err
		}
		if hasMember {
			eligible = append(eligible, ch)
		}
	}
	return eligible, nil
}

// errorsIsNotFound reports whether err wraps domain.ErrNotFound.
func errorsIsNotFound(err error) bool {
	return errors.Is(err, domain.ErrNotFound)
}
