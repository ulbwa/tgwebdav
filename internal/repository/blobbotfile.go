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

// BlobBotFileRepository caches per-bot file_ids for blobs (file_ids are
// bot-specific in the Telegram Bot API).
type BlobBotFileRepository struct {
	pool *pgxpool.Pool
}

// NewBlobBotFileRepository builds a BlobBotFileRepository bound to pool.
func NewBlobBotFileRepository(pool *pgxpool.Pool) *BlobBotFileRepository {
	return &BlobBotFileRepository{pool: pool}
}

// blobBotFileToModel maps a sqlc.BlobBotFile row into a model.BlobBotFile.
func blobBotFileToModel(m sqlc.BlobBotFile) *model.BlobBotFile {
	return &model.BlobBotFile{
		BlobID:       m.BlobID,
		BotID:        m.BotID,
		FileID:       m.FileID,
		FileUniqueID: m.FileUniqueID,
		FetchedAt:    m.FetchedAt.Time,
	}
}

// Upsert inserts or updates a cached per-bot file_id for a blob.
func (r *BlobBotFileRepository) Upsert(ctx context.Context, f *model.BlobBotFile) error {
	if f.FetchedAt.IsZero() {
		f.FetchedAt = time.Now()
	}
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).UpsertBlobBotFile(ctx, sqlc.UpsertBlobBotFileParams{
		BlobID:       f.BlobID,
		BotID:        f.BotID,
		FileID:       f.FileID,
		FileUniqueID: f.FileUniqueID,
		FetchedAt:    ptrToTime(&f.FetchedAt),
	})
	if err != nil {
		return fmt.Errorf("upsert blob_bot_file: %w", translateError(err))
	}
	return nil
}

// Get loads the cached file_id for a (blob, bot) pair.
func (r *BlobBotFileRepository) Get(ctx context.Context, blobID, botID uuid.UUID) (*model.BlobBotFile, error) {
	db := database.FromContext(ctx, r.pool)
	m, err := sqlc.New(db).GetBlobBotFile(ctx, sqlc.GetBlobBotFileParams{
		BlobID: blobID,
		BotID:  botID,
	})
	if err != nil {
		return nil, fmt.Errorf("get blob_bot_file: %w", translateError(err))
	}
	return blobBotFileToModel(m), nil
}

// ListByBlob returns every cached file_id for a blob.
func (r *BlobBotFileRepository) ListByBlob(ctx context.Context, blobID uuid.UUID) ([]model.BlobBotFile, error) {
	db := database.FromContext(ctx, r.pool)
	ms, err := sqlc.New(db).ListBlobBotFilesByBlob(ctx, blobID)
	if err != nil {
		return nil, fmt.Errorf("list blob_bot_files by blob: %w", translateError(err))
	}
	out := make([]model.BlobBotFile, len(ms))
	for i := range ms {
		out[i] = *blobBotFileToModel(ms[i])
	}
	return out, nil
}

// DeleteByBlob removes all cached file_ids for a blob.
func (r *BlobBotFileRepository) DeleteByBlob(ctx context.Context, blobID uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	if err := sqlc.New(db).DeleteBlobBotFilesByBlob(ctx, blobID); err != nil {
		return fmt.Errorf("delete blob_bot_files by blob: %w", translateError(err))
	}
	return nil
}

// DeleteByBot removes all cached file_ids belonging to a bot. Used when a stale
// file_id is detected: the rows are purged so the next read forces recovery.
func (r *BlobBotFileRepository) DeleteByBot(ctx context.Context, botID uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	if err := sqlc.New(db).DeleteBlobBotFilesByBot(ctx, botID); err != nil {
		return fmt.Errorf("delete blob_bot_files by bot: %w", translateError(err))
	}
	return nil
}
