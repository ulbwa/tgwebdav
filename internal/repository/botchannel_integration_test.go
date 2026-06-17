package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/model"
)

// insertBot inserts a minimal bot row directly (token columns are NOT NULL) and
// returns its id, for use as a FK parent in membership tests.
func insertBot(t *testing.T, pool *pgxpool.Pool, username string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO bots (id, username, token_sha, token_enc, enabled, created_at)
		 VALUES ($1, $2, $3, $4, true, now())`,
		id, username, tokenSHA(username), []byte("enc-"+username),
	)
	if err != nil {
		t.Fatalf("insertBot: %v", err)
	}
	return id
}

// insertChannel inserts a minimal channel row directly and returns its id.
func insertChannel(t *testing.T, pool *pgxpool.Pool, chatID int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO channels (id, tg_chat_id, title, created_at)
		 VALUES ($1, $2, '', now())`,
		id, chatID,
	)
	if err != nil {
		t.Fatalf("insertChannel: %v", err)
	}
	return id
}

func TestBotChannelRepository_UpsertAndGet(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotChannelRepository(pool)

	botID := insertBot(t, pool, "bc-bot")
	chanID := insertChannel(t, pool, -100100)

	bc := &model.BotChannel{BotID: botID, ChannelID: chanID, Member: true}
	if err := repo.Upsert(ctx, bc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if bc.CheckedAt.IsZero() {
		t.Fatal("Upsert did not default CheckedAt")
	}

	got, err := repo.Get(ctx, botID, chanID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Member {
		t.Fatal("Member not persisted")
	}
	if got.CheckedAt.IsZero() {
		t.Fatal("CheckedAt not persisted")
	}

	// Upsert again toggling member; the same PK row must be updated, not duped.
	later := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	bc2 := &model.BotChannel{BotID: botID, ChannelID: chanID, Member: false, CheckedAt: later}
	if err := repo.Upsert(ctx, bc2); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	got, err = repo.Get(ctx, botID, chanID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Member {
		t.Fatal("Upsert did not update member")
	}
	if !got.CheckedAt.Equal(later) {
		t.Fatalf("CheckedAt = %v, want %v", got.CheckedAt, later)
	}
}

func TestBotChannelRepository_ListAndDelete(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotChannelRepository(pool)

	bot1 := insertBot(t, pool, "list-bot1")
	bot2 := insertBot(t, pool, "list-bot2")
	chanA := insertChannel(t, pool, -100201)
	chanB := insertChannel(t, pool, -100202)

	pairs := []model.BotChannel{
		{BotID: bot1, ChannelID: chanA, Member: true},
		{BotID: bot1, ChannelID: chanB, Member: true},
		{BotID: bot2, ChannelID: chanA, Member: false},
	}
	for i := range pairs {
		if err := repo.Upsert(ctx, &pairs[i]); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	byBot, err := repo.ListByBot(ctx, bot1)
	if err != nil {
		t.Fatalf("ListByBot: %v", err)
	}
	if len(byBot) != 2 {
		t.Fatalf("ListByBot len = %d, want 2", len(byBot))
	}

	byChan, err := repo.ListByChannel(ctx, chanA)
	if err != nil {
		t.Fatalf("ListByChannel: %v", err)
	}
	if len(byChan) != 2 {
		t.Fatalf("ListByChannel len = %d, want 2", len(byChan))
	}

	if err := repo.DeleteByBot(ctx, bot1); err != nil {
		t.Fatalf("DeleteByBot: %v", err)
	}
	byBot, err = repo.ListByBot(ctx, bot1)
	if err != nil {
		t.Fatalf("ListByBot after delete: %v", err)
	}
	if len(byBot) != 0 {
		t.Fatalf("ListByBot after DeleteByBot len = %d, want 0", len(byBot))
	}
	// bot2/chanA must survive DeleteByBot(bot1).
	if _, err := repo.Get(ctx, bot2, chanA); err != nil {
		t.Fatalf("Get bot2/chanA after DeleteByBot(bot1): %v", err)
	}

	if err := repo.DeleteByChannel(ctx, chanA); err != nil {
		t.Fatalf("DeleteByChannel: %v", err)
	}
	byChan, err = repo.ListByChannel(ctx, chanA)
	if err != nil {
		t.Fatalf("ListByChannel after delete: %v", err)
	}
	if len(byChan) != 0 {
		t.Fatalf("ListByChannel after DeleteByChannel len = %d, want 0", len(byChan))
	}
}

func TestBotChannelRepository_GetNotFound(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotChannelRepository(pool)

	if _, err := repo.Get(ctx, uuid.New(), uuid.New()); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}

	// DeleteBy* on absent rows is a no-op (no error), matching legacy behavior.
	if err := repo.DeleteByBot(ctx, uuid.New()); err != nil {
		t.Fatalf("DeleteByBot missing = %v, want nil", err)
	}
	if err := repo.DeleteByChannel(ctx, uuid.New()); err != nil {
		t.Fatalf("DeleteByChannel missing = %v, want nil", err)
	}
}
