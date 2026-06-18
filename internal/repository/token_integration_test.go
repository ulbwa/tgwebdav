package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

func TestTokenRepository_HappyPath(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	// Insert parent user directly.
	userID := uuid.New()
	err := sqlc.New(pool).CreateUser(ctx, sqlc.CreateUserParams{
		ID:           userID,
		Login:        "tokenuser",
		PasswordHash: "hash",
		IsAdmin:      false,
		QuotaBytes:   0,
		BandwidthBps: 0,
		RatePerMin:   0,
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("setup user: %v", err)
	}

	repo := NewTokenRepository(pool)

	tok := &model.APIToken{
		UserID:    userID,
		TokenHash: "abc123hash",
		Name:      "my-token",
	}

	// Create
	if err := repo.Create(ctx, tok); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tok.ID == uuid.Nil {
		t.Fatal("Create: id not assigned")
	}

	// GetByHash
	got, err := repo.GetByHash(ctx, tok.TokenHash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got.Name != tok.Name {
		t.Errorf("GetByHash name = %q, want %q", got.Name, tok.Name)
	}
	if got.LastUsedAt != nil {
		t.Errorf("GetByHash: LastUsedAt should be nil initially, got %v", got.LastUsedAt)
	}

	// ListByUser
	list, err := repo.ListByUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListByUser: want 1, got %d", len(list))
	}

	// TouchLastUsed
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.TouchLastUsed(ctx, tok.ID, at); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}
	got2, err := repo.GetByHash(ctx, tok.TokenHash)
	if err != nil {
		t.Fatalf("GetByHash after touch: %v", err)
	}
	if got2.LastUsedAt == nil {
		t.Fatal("TouchLastUsed: LastUsedAt still nil")
	}

	// Delete
	if err := repo.Delete(ctx, tok.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = repo.GetByHash(ctx, tok.TokenHash)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByHash after Delete: want ErrNotFound, got %v", err)
	}
}

func TestTokenRepository_NotFound(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewTokenRepository(pool)
	ctx := context.Background()

	_, err := repo.GetByHash(ctx, "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByHash missing: want ErrNotFound, got %v", err)
	}

	err = repo.Delete(ctx, uuid.New())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing: want ErrNotFound, got %v", err)
	}

	err = repo.TouchLastUsed(ctx, uuid.New(), time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("TouchLastUsed missing: want ErrNotFound, got %v", err)
	}
}
