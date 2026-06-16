package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// channelRepo implements domain.ChannelRepository.
type channelRepo struct{ base *gorm.DB }

// Create inserts a new channel.
func (r *channelRepo) Create(ctx context.Context, c *domain.Channel) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	if err := txFromCtx(ctx, r.base).Create(channelToModel(c)).Error; err != nil {
		return fmt.Errorf("create channel: %w", translateError(err))
	}
	return nil
}

// Update saves the mutable columns of a channel.
func (r *channelRepo) Update(ctx context.Context, c *domain.Channel) error {
	res := txFromCtx(ctx, r.base).Model(&channelModel{}).
		Where("id = ?", c.ID).
		Updates(map[string]any{
			"tg_chat_id":         c.TGChatID,
			"title":              c.Title,
			"message_counter":    c.MessageCounter,
			"eviction_threshold": c.EvictionThreshold,
			"available":          c.Available,
		})
	if res.Error != nil {
		return fmt.Errorf("update channel: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update channel: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes a channel by id. Note the blobs FK is RESTRICT, so this fails
// while blobs reference the channel; decommission via SetAvailable instead.
func (r *channelRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res := txFromCtx(ctx, r.base).Where("id = ?", id).Delete(&channelModel{})
	if res.Error != nil {
		return fmt.Errorf("delete channel: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete channel: %w", domain.ErrNotFound)
	}
	return nil
}

// GetByID loads a channel by primary key.
func (r *channelRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Channel, error) {
	var m channelModel
	if err := txFromCtx(ctx, r.base).Where("id = ?", id).First(&m).Error; err != nil {
		return nil, fmt.Errorf("get channel by id: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// GetByChatID loads a channel by its Telegram chat id.
func (r *channelRepo) GetByChatID(ctx context.Context, chatID int64) (*domain.Channel, error) {
	var m channelModel
	if err := txFromCtx(ctx, r.base).Where("tg_chat_id = ?", chatID).First(&m).Error; err != nil {
		return nil, fmt.Errorf("get channel by chat id: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// List returns all channels ordered by creation time.
func (r *channelRepo) List(ctx context.Context) ([]domain.Channel, error) {
	var ms []channelModel
	if err := txFromCtx(ctx, r.base).Order("created_at").Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list channels: %w", translateError(err))
	}
	out := make([]domain.Channel, len(ms))
	for i := range ms {
		out[i] = *ms[i].toDomain()
	}
	return out, nil
}

// IncrementCounter atomically adds delta to message_counter and returns the new
// value using an UPDATE ... RETURNING.
func (r *channelRepo) IncrementCounter(ctx context.Context, id uuid.UUID, delta int64) (int64, error) {
	var counter int64
	row := txFromCtx(ctx, r.base).Raw(
		`UPDATE channels SET message_counter = message_counter + ? WHERE id = ? RETURNING message_counter`,
		delta, id,
	).Row()
	if err := row.Scan(&counter); err != nil {
		return 0, fmt.Errorf("increment channel counter: %w", translateError(err))
	}
	return counter, nil
}

// SetAvailable flips a channel's availability flag.
func (r *channelRepo) SetAvailable(ctx context.Context, id uuid.UUID, available bool) error {
	res := txFromCtx(ctx, r.base).Model(&channelModel{}).
		Where("id = ?", id).
		Update("available", available)
	if res.Error != nil {
		return fmt.Errorf("set channel available: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set channel available: %w", domain.ErrNotFound)
	}
	return nil
}
