package services

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// botService implements domain.BotService: bot lifecycle, membership refresh,
// channel-availability rebalancing and round-robin upload-bot selection.
type botService struct {
	repos  *domain.Repositories
	tx     domain.TxManager
	tg     domain.TelegramAPI
	logger *slog.Logger

	// rr is a monotonic counter used to round-robin PickForUpload across the
	// eligible bots for a channel.
	rr atomic.Uint64
}

// NewBotService returns a domain.BotService.
func NewBotService(r *domain.Repositories, tx domain.TxManager, tg domain.TelegramAPI, logger *slog.Logger) domain.BotService {
	return &botService{repos: r, tx: tx, tg: tg, logger: logger}
}

// Add validates the token via getMe, records (or refreshes) the bot identified
// by its Telegram username, checks the bot's membership of every known channel
// and re-evaluates channel availability. It is idempotent: re-adding a token
// for the same username updates that bot's token and re-enables it, reusing the
// existing id.
func (s *botService) Add(ctx context.Context, token string) (*domain.Bot, error) {
	bot := &domain.Bot{
		ID:      uuid.New(),
		Token:   token,
		Enabled: true,
	}

	username, err := s.tg.GetMe(ctx, bot)
	if err != nil {
		return nil, fmt.Errorf("validate bot token: %w", err)
	}
	bot.Username = username

	// Idempotency by username: reuse the existing bot's id and update its token.
	existing, err := s.repos.Bots.GetByUsername(ctx, username)
	switch {
	case err == nil:
		existing.Token = token
		existing.Enabled = true
		if err := s.repos.Bots.Update(ctx, existing); err != nil {
			return nil, fmt.Errorf("update existing bot %s: %w", username, err)
		}
		bot = existing
	case errorsIsNotFound(err):
		if err := s.repos.Bots.Create(ctx, bot); err != nil {
			return nil, fmt.Errorf("create bot %s: %w", username, err)
		}
	default:
		return nil, fmt.Errorf("lookup bot by username %s: %w", username, err)
	}

	channels, err := s.repos.Channels.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	for _, ch := range channels {
		_, member, err := s.tg.GetChat(ctx, bot, ch.TGChatID)
		if err != nil {
			return nil, fmt.Errorf("check membership of channel %d: %w", ch.TGChatID, err)
		}
		bc := &domain.BotChannel{
			BotID:     bot.ID,
			ChannelID: ch.ID,
			Member:    member,
			CheckedAt: time.Now(),
		}
		if err := s.repos.BotChannels.Upsert(ctx, bc); err != nil {
			return nil, fmt.Errorf("upsert bot_channel (%s,%s): %w", bot.ID, ch.ID, err)
		}
	}

	if err := reevaluateAvailability(ctx, s.repos, s.tx, s.logger); err != nil {
		return nil, fmt.Errorf("reevaluate availability: %w", err)
	}

	s.logger.InfoContext(ctx, "bot added",
		slog.String("bot_id", bot.ID.String()),
		slog.String("username", bot.Username),
	)
	return bot, nil
}

// Remove deletes the bot (the database cascades bot_channel and
// blob_bot_files), re-evaluates channel availability and logs the event.
func (s *botService) Remove(ctx context.Context, id uuid.UUID) error {
	if err := s.repos.Bots.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete bot %s: %w", id, err)
	}

	if err := reevaluateAvailability(ctx, s.repos, s.tx, s.logger); err != nil {
		return fmt.Errorf("reevaluate availability: %w", err)
	}

	if err := s.repos.Events.Log(ctx, domain.EventBotDisabled, "bot removed", id.String()); err != nil {
		return fmt.Errorf("log bot removed event: %w", err)
	}

	s.logger.InfoContext(ctx, "bot removed", slog.String("bot_id", id.String()))
	return nil
}

// SetEnabled toggles a bot's enabled flag and re-evaluates channel
// availability, since enabling/disabling a bot can change which channels have a
// member.
func (s *botService) SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	bot, err := s.repos.Bots.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get bot %s: %w", id, err)
	}

	bot.Enabled = enabled
	if err := s.repos.Bots.Update(ctx, bot); err != nil {
		return fmt.Errorf("update bot %s: %w", id, err)
	}

	if err := reevaluateAvailability(ctx, s.repos, s.tx, s.logger); err != nil {
		return fmt.Errorf("reevaluate availability: %w", err)
	}

	if !enabled {
		if err := s.repos.Events.Log(ctx, domain.EventBotDisabled, "bot disabled", id.String()); err != nil {
			return fmt.Errorf("log bot disabled event: %w", err)
		}
	}

	s.logger.InfoContext(ctx, "bot enabled state changed",
		slog.String("bot_id", id.String()),
		slog.Bool("enabled", enabled),
	)
	return nil
}

// List returns every bot.
func (s *botService) List(ctx context.Context) ([]domain.Bot, error) {
	bots, err := s.repos.Bots.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	return bots, nil
}

// Get returns the bot with the given id.
func (s *botService) Get(ctx context.Context, id uuid.UUID) (*domain.Bot, error) {
	bot, err := s.repos.Bots.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get bot %s: %w", id, err)
	}
	return bot, nil
}

// RefreshMembership re-checks every (bot, channel) pair via getChat, updates the
// membership matrix and re-evaluates channel availability.
func (s *botService) RefreshMembership(ctx context.Context) error {
	bots, err := s.repos.Bots.List(ctx)
	if err != nil {
		return fmt.Errorf("list bots: %w", err)
	}
	channels, err := s.repos.Channels.List(ctx)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}

	now := time.Now()
	for i := range bots {
		bot := &bots[i]
		for _, ch := range channels {
			_, member, err := s.tg.GetChat(ctx, bot, ch.TGChatID)
			if err != nil {
				return fmt.Errorf("check membership (bot %s, chat %d): %w", bot.ID, ch.TGChatID, err)
			}
			bc := &domain.BotChannel{
				BotID:     bot.ID,
				ChannelID: ch.ID,
				Member:    member,
				CheckedAt: now,
			}
			if err := s.repos.BotChannels.Upsert(ctx, bc); err != nil {
				return fmt.Errorf("upsert bot_channel (%s,%s): %w", bot.ID, ch.ID, err)
			}
		}
	}

	if err := reevaluateAvailability(ctx, s.repos, s.tx, s.logger); err != nil {
		return fmt.Errorf("reevaluate availability: %w", err)
	}
	return nil
}

// PickForUpload returns an available bot that is a member of channelID. It
// considers only bots that are members of the channel, enabled, and available
// at the current instant, and round-robins across them via an atomic counter.
// When no such bot exists it returns domain.ErrNoBot.
func (s *botService) PickForUpload(ctx context.Context, channelID uuid.UUID) (*domain.Bot, error) {
	eligible, err := s.eligibleBots(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no member bot for channel %s: %w", channelID, domain.ErrNoBot)
	}

	idx := int(s.rr.Add(1)-1) % len(eligible)
	bot := eligible[idx]
	return &bot, nil
}

// eligibleBots returns the bots that are members of channelID and are enabled
// and available right now, ordered deterministically by id so round-robin is
// stable across calls.
func (s *botService) eligibleBots(ctx context.Context, channelID uuid.UUID) ([]domain.Bot, error) {
	members, err := s.repos.BotChannels.ListByChannel(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("list bot channels for %s: %w", channelID, err)
	}
	memberIDs := make(map[uuid.UUID]struct{}, len(members))
	for _, m := range members {
		if m.Member {
			memberIDs[m.BotID] = struct{}{}
		}
	}
	if len(memberIDs) == 0 {
		return nil, nil
	}

	bots, err := s.repos.Bots.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}

	now := time.Now()
	var eligible []domain.Bot
	for _, b := range bots {
		if _, ok := memberIDs[b.ID]; !ok {
			continue
		}
		if b.Available(now) {
			eligible = append(eligible, b)
		}
	}
	return eligible, nil
}
