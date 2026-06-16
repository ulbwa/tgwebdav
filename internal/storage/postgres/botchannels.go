package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// botChannelRepo implements domain.BotChannelRepository.
type botChannelRepo struct{ base *gorm.DB }

// Upsert inserts or updates a (bot, channel) membership record.
func (r *botChannelRepo) Upsert(ctx context.Context, bc *domain.BotChannel) error {
	if bc.CheckedAt.IsZero() {
		bc.CheckedAt = time.Now()
	}
	err := txFromCtx(ctx, r.base).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "bot_id"}, {Name: "channel_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"member", "checked_at"}),
	}).Create(botChannelToModel(bc)).Error
	if err != nil {
		return fmt.Errorf("upsert bot_channel: %w", translateError(err))
	}
	return nil
}

// Get loads a single membership record.
func (r *botChannelRepo) Get(ctx context.Context, botID, channelID uuid.UUID) (*domain.BotChannel, error) {
	var m botChannelModel
	if err := txFromCtx(ctx, r.base).
		Where("bot_id = ? AND channel_id = ?", botID, channelID).
		First(&m).Error; err != nil {
		return nil, fmt.Errorf("get bot_channel: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// ListByChannel returns every membership record for a channel.
func (r *botChannelRepo) ListByChannel(ctx context.Context, channelID uuid.UUID) ([]domain.BotChannel, error) {
	var ms []botChannelModel
	if err := txFromCtx(ctx, r.base).Where("channel_id = ?", channelID).Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list bot_channel by channel: %w", translateError(err))
	}
	return botChannelsToDomain(ms), nil
}

// ListByBot returns every membership record for a bot.
func (r *botChannelRepo) ListByBot(ctx context.Context, botID uuid.UUID) ([]domain.BotChannel, error) {
	var ms []botChannelModel
	if err := txFromCtx(ctx, r.base).Where("bot_id = ?", botID).Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list bot_channel by bot: %w", translateError(err))
	}
	return botChannelsToDomain(ms), nil
}

// DeleteByBot removes all membership records for a bot.
func (r *botChannelRepo) DeleteByBot(ctx context.Context, botID uuid.UUID) error {
	if err := txFromCtx(ctx, r.base).Where("bot_id = ?", botID).Delete(&botChannelModel{}).Error; err != nil {
		return fmt.Errorf("delete bot_channel by bot: %w", translateError(err))
	}
	return nil
}

// DeleteByChannel removes all membership records for a channel.
func (r *botChannelRepo) DeleteByChannel(ctx context.Context, channelID uuid.UUID) error {
	if err := txFromCtx(ctx, r.base).Where("channel_id = ?", channelID).Delete(&botChannelModel{}).Error; err != nil {
		return fmt.Errorf("delete bot_channel by channel: %w", translateError(err))
	}
	return nil
}

func botChannelsToDomain(ms []botChannelModel) []domain.BotChannel {
	out := make([]domain.BotChannel, len(ms))
	for i := range ms {
		out[i] = *ms[i].toDomain()
	}
	return out
}
