package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// settingsRepo implements domain.SettingsRepository. Settings live in a single
// row pinned to id = 1. The idle timeout is persisted as integer milliseconds.
type settingsRepo struct{ base *gorm.DB }

// Get loads the settings row, falling back to DefaultSettings for any field
// left at its zero value (and for a wholly-missing row).
func (r *settingsRepo) Get(ctx context.Context) (domain.Settings, error) {
	var m settingsModel
	err := txFromCtx(ctx, r.base).Where("id = ?", 1).First(&m).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.DefaultSettings(), nil
		}
		return domain.Settings{}, fmt.Errorf("get settings: %w", translateError(err))
	}

	def := domain.DefaultSettings()
	s := domain.Settings{
		BlobMaxSize:              m.BlobMaxSize,
		WALIdleTimeout:           time.Duration(m.WALIdleTimeoutMS) * time.Millisecond,
		MaxFileSize:              m.MaxFileSize,
		DefaultEvictionThreshold: m.DefaultEvictionThreshold,
		UpdatedAt:                m.UpdatedAt,
	}
	if s.BlobMaxSize == 0 {
		s.BlobMaxSize = def.BlobMaxSize
	}
	if s.WALIdleTimeout == 0 {
		s.WALIdleTimeout = def.WALIdleTimeout
	}
	if s.DefaultEvictionThreshold == 0 {
		s.DefaultEvictionThreshold = def.DefaultEvictionThreshold
	}
	// MaxFileSize == 0 is meaningful (unlimited), so it is never defaulted.
	return s, nil
}

// Update upserts the single settings row (id = 1), storing the idle timeout in
// milliseconds and stamping updated_at.
func (r *settingsRepo) Update(ctx context.Context, s domain.Settings) error {
	m := &settingsModel{
		ID:                       1,
		BlobMaxSize:              s.BlobMaxSize,
		WALIdleTimeoutMS:         s.WALIdleTimeout.Milliseconds(),
		MaxFileSize:              s.MaxFileSize,
		DefaultEvictionThreshold: s.DefaultEvictionThreshold,
		UpdatedAt:                time.Now(),
	}
	err := txFromCtx(ctx, r.base).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"blob_max_size", "wal_idle_timeout_ms", "max_file_size",
			"default_eviction_threshold", "updated_at",
		}),
	}).Create(m).Error
	if err != nil {
		return fmt.Errorf("update settings: %w", translateError(err))
	}
	return nil
}
