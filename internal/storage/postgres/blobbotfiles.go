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

// blobBotFileRepo implements domain.BlobBotFileRepository.
type blobBotFileRepo struct{ base *gorm.DB }

// Upsert inserts or updates a cached per-bot file_id for a blob.
func (r *blobBotFileRepo) Upsert(ctx context.Context, f *domain.BlobBotFile) error {
	if f.FetchedAt.IsZero() {
		f.FetchedAt = time.Now()
	}
	err := txFromCtx(ctx, r.base).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "blob_id"}, {Name: "bot_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"file_id", "file_unique_id", "fetched_at"}),
	}).Create(blobBotFileToModel(f)).Error
	if err != nil {
		return fmt.Errorf("upsert blob_bot_file: %w", translateError(err))
	}
	return nil
}

// Get loads the cached file_id for a (blob, bot) pair.
func (r *blobBotFileRepo) Get(ctx context.Context, blobID, botID uuid.UUID) (*domain.BlobBotFile, error) {
	var m blobBotFileModel
	if err := txFromCtx(ctx, r.base).
		Where("blob_id = ? AND bot_id = ?", blobID, botID).
		First(&m).Error; err != nil {
		return nil, fmt.Errorf("get blob_bot_file: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// ListByBlob returns every cached file_id for a blob.
func (r *blobBotFileRepo) ListByBlob(ctx context.Context, blobID uuid.UUID) ([]domain.BlobBotFile, error) {
	var ms []blobBotFileModel
	if err := txFromCtx(ctx, r.base).Where("blob_id = ?", blobID).Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list blob_bot_files by blob: %w", translateError(err))
	}
	out := make([]domain.BlobBotFile, len(ms))
	for i := range ms {
		out[i] = *ms[i].toDomain()
	}
	return out, nil
}

// DeleteByBlob removes all cached file_ids for a blob.
func (r *blobBotFileRepo) DeleteByBlob(ctx context.Context, blobID uuid.UUID) error {
	if err := txFromCtx(ctx, r.base).Where("blob_id = ?", blobID).Delete(&blobBotFileModel{}).Error; err != nil {
		return fmt.Errorf("delete blob_bot_files by blob: %w", translateError(err))
	}
	return nil
}

// DeleteByBot removes all cached file_ids belonging to a bot. Used when a stale
// file_id is detected: the row is purged so the next read forces recovery.
func (r *blobBotFileRepo) DeleteByBot(ctx context.Context, botID uuid.UUID) error {
	if err := txFromCtx(ctx, r.base).Where("bot_id = ?", botID).Delete(&blobBotFileModel{}).Error; err != nil {
		return fmt.Errorf("delete blob_bot_files by bot: %w", translateError(err))
	}
	return nil
}
