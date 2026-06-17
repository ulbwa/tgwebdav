package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// SettingsRepository persists the single runtime Settings row against a pgx pool.
// The idle timeout is stored as integer milliseconds (wal_idle_timeout_ms).
type SettingsRepository struct {
	pool *pgxpool.Pool
}

// NewSettingsRepository returns a SettingsRepository backed by pool.
func NewSettingsRepository(pool *pgxpool.Pool) *SettingsRepository {
	return &SettingsRepository{pool: pool}
}

// Get loads the settings row (id = 1), falling back to DefaultSettings for any
// field left at its zero value and for a wholly-missing row. MaxFileSize == 0 is
// meaningful (unlimited) and is never replaced with a default.
func (r *SettingsRepository) Get(ctx context.Context) (model.Settings, error) {
	db := database.FromContext(ctx, r.pool)
	row, err := sqlc.New(db).GetSettings(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.DefaultSettings(), nil
		}
		return model.Settings{}, translateError(err)
	}

	def := model.DefaultSettings()
	s := model.Settings{
		BlobMaxSize:              row.BlobMaxSize,
		WALIdleTimeout:           time.Duration(row.WalIdleTimeoutMs) * time.Millisecond,
		MaxFileSize:              row.MaxFileSize,
		DefaultEvictionThreshold: row.DefaultEvictionThreshold,
		UpdatedAt:                row.UpdatedAt.Time,
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
	// MaxFileSize == 0 means unlimited; never overwrite with default.
	return s, nil
}

// Update upserts the single settings row (id = 1), storing the idle timeout in
// milliseconds and stamping updated_at to now.
func (r *SettingsRepository) Update(ctx context.Context, s model.Settings) error {
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).UpsertSettings(ctx, sqlc.UpsertSettingsParams{
		BlobMaxSize:              s.BlobMaxSize,
		WalIdleTimeoutMs:         s.WALIdleTimeout.Milliseconds(),
		MaxFileSize:              s.MaxFileSize,
		DefaultEvictionThreshold: s.DefaultEvictionThreshold,
		UpdatedAt:                pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	return translateError(err)
}
