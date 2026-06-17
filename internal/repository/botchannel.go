package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// BotChannelRepository persists the bot↔channel membership matrix.
type BotChannelRepository struct {
	pool *pgxpool.Pool
}

// NewBotChannelRepository builds a BotChannelRepository bound to pool.
func NewBotChannelRepository(pool *pgxpool.Pool) *BotChannelRepository {
	return &BotChannelRepository{pool: pool}
}

// botChannelToModel maps a sqlc.BotChannel row into a model.BotChannel.
func botChannelToModel(m sqlc.BotChannel) *model.BotChannel {
	return &model.BotChannel{
		BotID:     m.BotID,
		ChannelID: m.ChannelID,
		Member:    m.Member,
		CheckedAt: m.CheckedAt.Time,
	}
}

// Upsert inserts or updates a (bot, channel) membership record.
func (r *BotChannelRepository) Upsert(ctx context.Context, bc *model.BotChannel) error {
	if bc.CheckedAt.IsZero() {
		bc.CheckedAt = time.Now()
	}
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).UpsertBotChannel(ctx, sqlc.UpsertBotChannelParams{
		BotID:     bc.BotID,
		ChannelID: bc.ChannelID,
		Member:    bc.Member,
		CheckedAt: ptrToTime(&bc.CheckedAt),
	})
	if err != nil {
		return fmt.Errorf("upsert bot_channel: %w", translateError(err))
	}
	return nil
}

// Get loads a single membership record.
func (r *BotChannelRepository) Get(ctx context.Context, botID, channelID uuid.UUID) (*model.BotChannel, error) {
	db := database.FromContext(ctx, r.pool)
	m, err := sqlc.New(db).GetBotChannel(ctx, sqlc.GetBotChannelParams{
		BotID:     botID,
		ChannelID: channelID,
	})
	if err != nil {
		return nil, fmt.Errorf("get bot_channel: %w", translateError(err))
	}
	return botChannelToModel(m), nil
}

// ListByChannel returns every membership record for a channel.
func (r *BotChannelRepository) ListByChannel(ctx context.Context, channelID uuid.UUID) ([]model.BotChannel, error) {
	db := database.FromContext(ctx, r.pool)
	ms, err := sqlc.New(db).ListBotChannelsByChannel(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("list bot_channel by channel: %w", translateError(err))
	}
	return botChannelsToModel(ms), nil
}

// ListByBot returns every membership record for a bot.
func (r *BotChannelRepository) ListByBot(ctx context.Context, botID uuid.UUID) ([]model.BotChannel, error) {
	db := database.FromContext(ctx, r.pool)
	ms, err := sqlc.New(db).ListBotChannelsByBot(ctx, botID)
	if err != nil {
		return nil, fmt.Errorf("list bot_channel by bot: %w", translateError(err))
	}
	return botChannelsToModel(ms), nil
}

// DeleteByBot removes all membership records for a bot.
func (r *BotChannelRepository) DeleteByBot(ctx context.Context, botID uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	if err := sqlc.New(db).DeleteBotChannelsByBot(ctx, botID); err != nil {
		return fmt.Errorf("delete bot_channel by bot: %w", translateError(err))
	}
	return nil
}

// DeleteByChannel removes all membership records for a channel.
func (r *BotChannelRepository) DeleteByChannel(ctx context.Context, channelID uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	if err := sqlc.New(db).DeleteBotChannelsByChannel(ctx, channelID); err != nil {
		return fmt.Errorf("delete bot_channel by channel: %w", translateError(err))
	}
	return nil
}

// botChannelsToModel maps a slice of sqlc rows into model values.
func botChannelsToModel(ms []sqlc.BotChannel) []model.BotChannel {
	out := make([]model.BotChannel, len(ms))
	for i := range ms {
		out[i] = *botChannelToModel(ms[i])
	}
	return out
}
