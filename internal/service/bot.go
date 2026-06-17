// Package service implements the bot and channel management services: bot
// lifecycle, channel lifecycle, shared availability re-evaluation and
// round-robin upload selection. A channel is available when at least one
// enabled bot is a member of it, and that availability is propagated to the
// channel's blobs.
//
// Following the canon layering, every dependency is a tiny interface declared
// here (the consumer) listing only the methods this package uses, expressed in
// terms of the model types. The real repositories, the telegram client and the
// other services satisfy these interfaces structurally.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// ---- dependency interfaces -------------------------------------------------

// txManager runs a function inside a single database transaction. The
// repositories resolve the active transaction from the context automatically,
// so fn receives only a context.
type txManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// telegramClient is the narrow Bot API surface the bot and channel services
// need. The M3 telegram client satisfies it.
type telegramClient interface {
	// GetMe returns the bot's username (validates the token).
	GetMe(ctx context.Context, bot *model.Bot) (username string, err error)
	// GetChat reports whether the bot can access chatID and its title.
	GetChat(ctx context.Context, bot *model.Bot, chatID int64) (title string, member bool, err error)
}

// botStore persists Telegram bots. Only the methods the bot/channel services
// use are listed.
type botStore interface {
	Create(ctx context.Context, b *model.Bot) error
	Update(ctx context.Context, b *model.Bot) error
	Delete(ctx context.Context, id uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*model.Bot, error)
	GetByUsername(ctx context.Context, username string) (*model.Bot, error)
	List(ctx context.Context) ([]model.Bot, error)
}

// channelStore persists Telegram channels. Only the methods the bot/channel
// services use are listed.
type channelStore interface {
	Create(ctx context.Context, c *model.Channel) error
	Update(ctx context.Context, c *model.Channel) error
	GetByID(ctx context.Context, id uuid.UUID) (*model.Channel, error)
	GetByChatID(ctx context.Context, chatID int64) (*model.Channel, error)
	List(ctx context.Context) ([]model.Channel, error)
	SetAvailable(ctx context.Context, id uuid.UUID, available bool) error
}

// botChannelStore persists the bot↔channel membership matrix.
type botChannelStore interface {
	Upsert(ctx context.Context, bc *model.BotChannel) error
	ListByChannel(ctx context.Context, channelID uuid.UUID) ([]model.BotChannel, error)
}

// blobStore is the slice of the blob repository the channel-availability logic
// touches: flipping a channel's blobs to/from unavailable.
type blobStore interface {
	MarkChannelUnavailable(ctx context.Context, channelID uuid.UUID) error
	MarkChannelAvailable(ctx context.Context, channelID uuid.UUID) error
}

// eventLogger records audit/log events.
type eventLogger interface {
	Log(ctx context.Context, kind, message, ref string) error
}

// settingsGetter reads the current runtime settings.
type settingsGetter interface {
	Get(ctx context.Context) (model.Settings, error)
}

// ---- BotService ------------------------------------------------------------

// BotService encapsulates bot lifecycle, membership refresh,
// channel-availability rebalancing and round-robin upload-bot selection.
type BotService struct {
	bots     botStore
	channels channelStore
	bcs      botChannelStore
	blobs    blobStore
	events   eventLogger
	tx       txManager
	tg       telegramClient
	logger   *slog.Logger

	// rr is a monotonic counter used to round-robin PickForUpload across the
	// eligible bots for a channel.
	rr atomic.Uint64
}

// NewBotService wires a BotService from its dependencies.
func NewBotService(
	bots botStore,
	channels channelStore,
	bcs botChannelStore,
	blobs blobStore,
	events eventLogger,
	tx txManager,
	tg telegramClient,
	logger *slog.Logger,
) *BotService {
	return &BotService{
		bots:     bots,
		channels: channels,
		bcs:      bcs,
		blobs:    blobs,
		events:   events,
		tx:       tx,
		tg:       tg,
		logger:   logger,
	}
}

// Add validates the token via getMe, records (or refreshes) the bot identified
// by its Telegram username, checks the bot's membership of every known channel
// and re-evaluates channel availability. It is idempotent: re-adding a token
// for the same username updates that bot's token and re-enables it, reusing the
// existing id.
func (s *BotService) Add(ctx context.Context, token string) (*model.Bot, error) {
	bot := &model.Bot{
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
	existing, err := s.bots.GetByUsername(ctx, username)
	switch {
	case err == nil:
		existing.Token = token
		existing.Enabled = true
		if err := s.bots.Update(ctx, existing); err != nil {
			return nil, fmt.Errorf("update existing bot %s: %w", username, err)
		}
		bot = existing
	case errorsIsNotFound(err):
		if err := s.bots.Create(ctx, bot); err != nil {
			return nil, fmt.Errorf("create bot %s: %w", username, err)
		}
	default:
		return nil, fmt.Errorf("lookup bot by username %s: %w", username, err)
	}

	channels, err := s.channels.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	for _, ch := range channels {
		_, member, err := s.tg.GetChat(ctx, bot, ch.TGChatID)
		if err != nil {
			return nil, fmt.Errorf("check membership of channel %d: %w", ch.TGChatID, err)
		}
		bc := &model.BotChannel{
			BotID:     bot.ID,
			ChannelID: ch.ID,
			Member:    member,
			CheckedAt: time.Now(),
		}
		if err := s.bcs.Upsert(ctx, bc); err != nil {
			return nil, fmt.Errorf("upsert bot_channel (%s,%s): %w", bot.ID, ch.ID, err)
		}
	}

	if err := s.reevaluateAvailability(ctx); err != nil {
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
func (s *BotService) Remove(ctx context.Context, id uuid.UUID) error {
	if err := s.bots.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete bot %s: %w", id, err)
	}

	if err := s.reevaluateAvailability(ctx); err != nil {
		return fmt.Errorf("reevaluate availability: %w", err)
	}

	if err := s.events.Log(ctx, model.EventBotDisabled, "bot removed", id.String()); err != nil {
		return fmt.Errorf("log bot removed event: %w", err)
	}

	s.logger.InfoContext(ctx, "bot removed", slog.String("bot_id", id.String()))
	return nil
}

// SetEnabled toggles a bot's enabled flag and re-evaluates channel
// availability, since enabling/disabling a bot can change which channels have a
// member.
func (s *BotService) SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	bot, err := s.bots.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get bot %s: %w", id, err)
	}

	bot.Enabled = enabled
	if err := s.bots.Update(ctx, bot); err != nil {
		return fmt.Errorf("update bot %s: %w", id, err)
	}

	if err := s.reevaluateAvailability(ctx); err != nil {
		return fmt.Errorf("reevaluate availability: %w", err)
	}

	if !enabled {
		if err := s.events.Log(ctx, model.EventBotDisabled, "bot disabled", id.String()); err != nil {
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
func (s *BotService) List(ctx context.Context) ([]model.Bot, error) {
	bots, err := s.bots.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	return bots, nil
}

// Get returns the bot with the given id.
func (s *BotService) Get(ctx context.Context, id uuid.UUID) (*model.Bot, error) {
	bot, err := s.bots.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get bot %s: %w", id, err)
	}
	return bot, nil
}

// RefreshMembership re-checks every (bot, channel) pair via getChat, updates the
// membership matrix and re-evaluates channel availability.
func (s *BotService) RefreshMembership(ctx context.Context) error {
	bots, err := s.bots.List(ctx)
	if err != nil {
		return fmt.Errorf("list bots: %w", err)
	}
	channels, err := s.channels.List(ctx)
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
			bc := &model.BotChannel{
				BotID:     bot.ID,
				ChannelID: ch.ID,
				Member:    member,
				CheckedAt: now,
			}
			if err := s.bcs.Upsert(ctx, bc); err != nil {
				return fmt.Errorf("upsert bot_channel (%s,%s): %w", bot.ID, ch.ID, err)
			}
		}
	}

	if err := s.reevaluateAvailability(ctx); err != nil {
		return fmt.Errorf("reevaluate availability: %w", err)
	}
	return nil
}

// PickForUpload returns an available bot that is a member of channelID. It
// considers only bots that are members of the channel, enabled, and available
// at the current instant, and round-robins across them via an atomic counter.
// When no such bot exists it returns model.ErrNoBot.
func (s *BotService) PickForUpload(ctx context.Context, channelID uuid.UUID) (*model.Bot, error) {
	eligible, err := s.eligibleBots(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no member bot for channel %s: %w", channelID, model.ErrNoBot)
	}

	idx := int(s.rr.Add(1)-1) % len(eligible)
	bot := eligible[idx]
	return &bot, nil
}

// eligibleBots returns the bots that are members of channelID and are enabled
// and available right now, ordered deterministically by id so round-robin is
// stable across calls.
func (s *BotService) eligibleBots(ctx context.Context, channelID uuid.UUID) ([]model.Bot, error) {
	members, err := s.bcs.ListByChannel(ctx, channelID)
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

	bots, err := s.bots.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}

	now := time.Now()
	var eligible []model.Bot
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

// ---- shared availability re-evaluation -------------------------------------

// reevaluateAvailability recomputes every channel's availability from the
// current bot membership matrix and propagates the result to the channel's
// blobs. A channel is available iff at least one enabled bot is a member of it.
//
// For each channel it flips channels.SetAvailable and then, depending on the
// new value, calls blobs.MarkChannelAvailable (restoring stored-eligible blobs)
// or blobs.MarkChannelUnavailable (flipping every blob to unavailable). Because
// the per-channel update touches both the channel row and its blobs, it runs
// inside a single transaction so the two mutations stay consistent.
func (s *BotService) reevaluateAvailability(ctx context.Context) error {
	return reevaluateAvailability(ctx, s.channels, s.bcs, s.blobs, s.bots, s.tx, s.logger)
}

// reevaluateAvailability is the package-shared implementation used by both
// services. It is a free function so BotService and ChannelService can call it
// with their own (structurally identical) dependencies.
func reevaluateAvailability(
	ctx context.Context,
	channels channelStore,
	bcs botChannelStore,
	blobs blobStore,
	bots botStore,
	tx txManager,
	logger *slog.Logger,
) error {
	chs, err := channels.List(ctx)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}

	bs, err := bots.List(ctx)
	if err != nil {
		return fmt.Errorf("list bots: %w", err)
	}

	// enabled[botID] reports whether a bot is currently enabled.
	enabled := make(map[uuid.UUID]bool, len(bs))
	for _, b := range bs {
		enabled[b.ID] = b.Enabled
	}

	for _, ch := range chs {
		available, err := channelHasEnabledMember(ctx, bcs, ch.ID, enabled)
		if err != nil {
			return err
		}

		if err := applyChannelAvailability(ctx, channels, blobs, tx, ch, available, logger); err != nil {
			return err
		}
	}

	return nil
}

// channelHasEnabledMember reports whether the channel has at least one enabled
// bot that is recorded as a member.
func channelHasEnabledMember(ctx context.Context, bcs botChannelStore, channelID uuid.UUID, enabled map[uuid.UUID]bool) (bool, error) {
	members, err := bcs.ListByChannel(ctx, channelID)
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

// channelHasUsableMember reports whether the channel has at least one member bot
// that is usable right now, i.e. usable[botID] is true. Unlike
// channelHasEnabledMember (which only checks the enabled flag, used to decide
// long-lived availability), this is used by upload selection so a channel whose
// only member bots are all rate-limited is not picked.
func channelHasUsableMember(ctx context.Context, bcs botChannelStore, channelID uuid.UUID, usable map[uuid.UUID]bool) (bool, error) {
	members, err := bcs.ListByChannel(ctx, channelID)
	if err != nil {
		return false, fmt.Errorf("list bot channels for %s: %w", channelID, err)
	}
	for _, m := range members {
		if m.Member && usable[m.BotID] {
			return true, nil
		}
	}
	return false, nil
}

// applyChannelAvailability persists the channel's new availability and the
// matching blob transition inside a single transaction. The repositories
// resolve the active transaction from the context, so the closure receives only
// the context and uses the held stores directly.
func applyChannelAvailability(
	ctx context.Context,
	channels channelStore,
	blobs blobStore,
	tx txManager,
	ch model.Channel,
	available bool,
	logger *slog.Logger,
) error {
	err := tx.WithTx(ctx, func(ctx context.Context) error {
		if err := channels.SetAvailable(ctx, ch.ID, available); err != nil {
			return fmt.Errorf("set channel available: %w", err)
		}
		if available {
			if err := blobs.MarkChannelAvailable(ctx, ch.ID); err != nil {
				return fmt.Errorf("mark channel blobs available: %w", err)
			}
		} else {
			if err := blobs.MarkChannelUnavailable(ctx, ch.ID); err != nil {
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

// errorsIsNotFound reports whether err wraps model.ErrNotFound.
func errorsIsNotFound(err error) bool {
	return errors.Is(err, model.ErrNotFound)
}
