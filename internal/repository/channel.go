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

// ChannelRepository persists Telegram channels used as blob storage.
type ChannelRepository struct {
	pool *pgxpool.Pool
}

// NewChannelRepository builds a ChannelRepository bound to pool.
func NewChannelRepository(pool *pgxpool.Pool) *ChannelRepository {
	return &ChannelRepository{pool: pool}
}

// channelToModel maps a sqlc.Channel row into a model.Channel.
func channelToModel(m sqlc.Channel) *model.Channel {
	return &model.Channel{
		ID:                m.ID,
		TGChatID:          m.TgChatID,
		Title:             m.Title,
		MessageCounter:    m.MessageCounter,
		EvictionThreshold: m.EvictionThreshold,
		Available:         m.Available,
		CreatedAt:         m.CreatedAt.Time,
	}
}

// Create inserts a new channel.
func (r *ChannelRepository) Create(ctx context.Context, c *model.Channel) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).CreateChannel(ctx, sqlc.CreateChannelParams{
		ID:                c.ID,
		TgChatID:          c.TGChatID,
		Title:             c.Title,
		MessageCounter:    c.MessageCounter,
		EvictionThreshold: c.EvictionThreshold,
		Available:         c.Available,
		CreatedAt:         ptrToTime(&c.CreatedAt),
	})
	if err != nil {
		return fmt.Errorf("create channel: %w", translateError(err))
	}
	return nil
}

// Update saves the mutable columns of a channel.
func (r *ChannelRepository) Update(ctx context.Context, c *model.Channel) error {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).UpdateChannel(ctx, sqlc.UpdateChannelParams{
		ID:                c.ID,
		TgChatID:          c.TGChatID,
		Title:             c.Title,
		MessageCounter:    c.MessageCounter,
		EvictionThreshold: c.EvictionThreshold,
		Available:         c.Available,
	})
	if err != nil {
		return fmt.Errorf("update channel: %w", translateError(err))
	}
	if rows == 0 {
		return fmt.Errorf("update channel: %w", model.ErrNotFound)
	}
	return nil
}

// Delete removes a channel by id. Note the blobs FK is RESTRICT, so this fails
// while blobs reference the channel; decommission via SetAvailable instead.
func (r *ChannelRepository) Delete(ctx context.Context, id uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).DeleteChannel(ctx, id)
	if err != nil {
		return fmt.Errorf("delete channel: %w", translateError(err))
	}
	if rows == 0 {
		return fmt.Errorf("delete channel: %w", model.ErrNotFound)
	}
	return nil
}

// GetByID loads a channel by primary key.
func (r *ChannelRepository) GetByID(ctx context.Context, id uuid.UUID) (*model.Channel, error) {
	db := database.FromContext(ctx, r.pool)
	m, err := sqlc.New(db).GetChannelByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get channel by id: %w", translateError(err))
	}
	return channelToModel(m), nil
}

// GetByChatID loads a channel by its Telegram chat id.
func (r *ChannelRepository) GetByChatID(ctx context.Context, chatID int64) (*model.Channel, error) {
	db := database.FromContext(ctx, r.pool)
	m, err := sqlc.New(db).GetChannelByChatID(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("get channel by chat id: %w", translateError(err))
	}
	return channelToModel(m), nil
}

// List returns all channels ordered by creation time.
func (r *ChannelRepository) List(ctx context.Context) ([]model.Channel, error) {
	db := database.FromContext(ctx, r.pool)
	ms, err := sqlc.New(db).ListChannels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", translateError(err))
	}
	out := make([]model.Channel, len(ms))
	for i := range ms {
		out[i] = *channelToModel(ms[i])
	}
	return out, nil
}

// IncrementCounter atomically adds delta to message_counter and returns the new
// value using an UPDATE ... RETURNING.
func (r *ChannelRepository) IncrementCounter(ctx context.Context, id uuid.UUID, delta int64) (int64, error) {
	db := database.FromContext(ctx, r.pool)
	counter, err := sqlc.New(db).IncrementChannelCounter(ctx, sqlc.IncrementChannelCounterParams{
		ID:             id,
		MessageCounter: delta,
	})
	if err != nil {
		return 0, fmt.Errorf("increment channel counter: %w", translateError(err))
	}
	return counter, nil
}

// SetAvailable flips a channel's availability flag.
func (r *ChannelRepository) SetAvailable(ctx context.Context, id uuid.UUID, available bool) error {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).SetChannelAvailable(ctx, sqlc.SetChannelAvailableParams{
		ID:        id,
		Available: available,
	})
	if err != nil {
		return fmt.Errorf("set channel available: %w", translateError(err))
	}
	if rows == 0 {
		return fmt.Errorf("set channel available: %w", model.ErrNotFound)
	}
	return nil
}
