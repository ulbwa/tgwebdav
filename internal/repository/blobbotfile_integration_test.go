package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/model"
)

// insertBlob inserts a minimal blob row (channel_id FK is RESTRICT) and returns
// its id, for use as a FK parent in blob_bot_files tests.
func insertBlob(t *testing.T, pool *pgxpool.Pool, channelID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	hash := sha256.Sum256(id[:])
	_, err := pool.Exec(context.Background(),
		`INSERT INTO blobs (id, channel_id, message_id, size, content_hash, state, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now())`,
		id, channelID, int64(1), int64(0), hash[:], int32(model.BlobStateStored),
	)
	if err != nil {
		t.Fatalf("insertBlob: %v", err)
	}
	return id
}

func TestBlobBotFileRepository_UpsertAndGet(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobBotFileRepository(pool)

	chanID := insertChannel(t, pool, -100300)
	blobID := insertBlob(t, pool, chanID)
	botID := insertBot(t, pool, "bbf-bot")

	f := &model.BlobBotFile{
		BlobID:       blobID,
		BotID:        botID,
		FileID:       "FILEID_ABC",
		FileUniqueID: "UNIQ_ABC",
	}
	if err := repo.Upsert(ctx, f); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if f.FetchedAt.IsZero() {
		t.Fatal("Upsert did not default FetchedAt")
	}

	got, err := repo.Get(ctx, blobID, botID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FileID != "FILEID_ABC" || got.FileUniqueID != "UNIQ_ABC" {
		t.Fatalf("Get mismatch: %+v", got)
	}
	if got.FetchedAt.IsZero() {
		t.Fatal("FetchedAt not persisted")
	}

	// Upsert again with a new file_id on the same PK must update, not insert.
	later := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	f2 := &model.BlobBotFile{
		BlobID:       blobID,
		BotID:        botID,
		FileID:       "FILEID_NEW",
		FileUniqueID: "UNIQ_NEW",
		FetchedAt:    later,
	}
	if err := repo.Upsert(ctx, f2); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	got, err = repo.Get(ctx, blobID, botID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.FileID != "FILEID_NEW" || got.FileUniqueID != "UNIQ_NEW" {
		t.Fatalf("Upsert did not update: %+v", got)
	}
	if !got.FetchedAt.Equal(later) {
		t.Fatalf("FetchedAt = %v, want %v", got.FetchedAt, later)
	}

	list, err := repo.ListByBlob(ctx, blobID)
	if err != nil {
		t.Fatalf("ListByBlob: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListByBlob len = %d, want 1", len(list))
	}
}

func TestBlobBotFileRepository_ListAndDelete(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobBotFileRepository(pool)

	chanID := insertChannel(t, pool, -100400)
	blob1 := insertBlob(t, pool, chanID)
	blob2 := insertBlob(t, pool, chanID)
	bot1 := insertBot(t, pool, "bbf-list-bot1")
	bot2 := insertBot(t, pool, "bbf-list-bot2")

	entries := []model.BlobBotFile{
		{BlobID: blob1, BotID: bot1, FileID: "a"},
		{BlobID: blob1, BotID: bot2, FileID: "b"},
		{BlobID: blob2, BotID: bot1, FileID: "c"},
	}
	for i := range entries {
		if err := repo.Upsert(ctx, &entries[i]); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	byBlob, err := repo.ListByBlob(ctx, blob1)
	if err != nil {
		t.Fatalf("ListByBlob: %v", err)
	}
	if len(byBlob) != 2 {
		t.Fatalf("ListByBlob len = %d, want 2", len(byBlob))
	}

	// DeleteByBot removes bot1 rows across all blobs.
	if err := repo.DeleteByBot(ctx, bot1); err != nil {
		t.Fatalf("DeleteByBot: %v", err)
	}
	if _, err := repo.Get(ctx, blob1, bot1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get blob1/bot1 after DeleteByBot = %v, want ErrNotFound", err)
	}
	if _, err := repo.Get(ctx, blob2, bot1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get blob2/bot1 after DeleteByBot = %v, want ErrNotFound", err)
	}
	// blob1/bot2 must survive.
	if _, err := repo.Get(ctx, blob1, bot2); err != nil {
		t.Fatalf("Get blob1/bot2 after DeleteByBot(bot1): %v", err)
	}

	// DeleteByBlob removes the remaining blob1 rows.
	if err := repo.DeleteByBlob(ctx, blob1); err != nil {
		t.Fatalf("DeleteByBlob: %v", err)
	}
	byBlob, err = repo.ListByBlob(ctx, blob1)
	if err != nil {
		t.Fatalf("ListByBlob after DeleteByBlob: %v", err)
	}
	if len(byBlob) != 0 {
		t.Fatalf("ListByBlob after DeleteByBlob len = %d, want 0", len(byBlob))
	}
}

func TestBlobBotFileRepository_GetNotFound(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobBotFileRepository(pool)

	if _, err := repo.Get(ctx, uuid.New(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	// DeleteBy* on absent rows is a no-op (no error).
	if err := repo.DeleteByBlob(ctx, uuid.New()); err != nil {
		t.Fatalf("DeleteByBlob missing = %v, want nil", err)
	}
	if err := repo.DeleteByBot(ctx, uuid.New()); err != nil {
		t.Fatalf("DeleteByBot missing = %v, want nil", err)
	}
}
