package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/model"
)

func TestUserRepository_HappyPath(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewUserRepository(pool)
	ctx := context.Background()

	u := &model.User{
		Login:        "alice",
		PasswordHash: "$argon2id$v=19$...",
		IsAdmin:      false,
		QuotaBytes:   1024 * 1024,
		BandwidthBPS: 0,
		RatePerMin:   60,
		CreatedAt:    time.Now().UTC().Truncate(time.Microsecond),
	}

	// Create
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.ID == uuid.Nil {
		t.Fatal("Create: id not assigned")
	}

	// GetByID
	got, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Login != u.Login {
		t.Errorf("GetByID login = %q, want %q", got.Login, u.Login)
	}

	// GetByLogin
	got2, err := repo.GetByLogin(ctx, u.Login)
	if err != nil {
		t.Fatalf("GetByLogin: %v", err)
	}
	if got2.ID != u.ID {
		t.Errorf("GetByLogin id = %v, want %v", got2.ID, u.ID)
	}

	// Update
	u.Login = "alice2"
	u.IsAdmin = true
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got3, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID after Update: %v", err)
	}
	if got3.Login != "alice2" || !got3.IsAdmin {
		t.Errorf("Update: got login=%q isAdmin=%v, want login=alice2 isAdmin=true", got3.Login, got3.IsAdmin)
	}

	// List
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("List: empty")
	}

	// Count
	n, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n == 0 {
		t.Fatal("Count: zero")
	}

	// Delete
	if err := repo.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Gone
	if _, err := repo.GetByID(ctx, u.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("GetByID after Delete: want ErrNotFound, got %v", err)
	}
}

func TestUserRepository_NotFound(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewUserRepository(pool)
	ctx := context.Background()

	_, err := repo.GetByID(ctx, uuid.New())
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("GetByID missing: want ErrNotFound, got %v", err)
	}

	_, err = repo.GetByLogin(ctx, "nobody")
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("GetByLogin missing: want ErrNotFound, got %v", err)
	}

	err = repo.Update(ctx, &model.User{ID: uuid.New(), Login: "x"})
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Update missing: want ErrNotFound, got %v", err)
	}

	err = repo.Delete(ctx, uuid.New())
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Delete missing: want ErrNotFound, got %v", err)
	}
}

func TestUserRepository_DuplicateLogin(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewUserRepository(pool)
	ctx := context.Background()

	u1 := &model.User{Login: "bob", PasswordHash: "hash"}
	if err := repo.Create(ctx, u1); err != nil {
		t.Fatalf("Create first: %v", err)
	}

	u2 := &model.User{Login: "bob", PasswordHash: "hash2"}
	err := repo.Create(ctx, u2)
	if !errors.Is(err, model.ErrAlreadyExists) {
		t.Errorf("Create duplicate login: want ErrAlreadyExists, got %v", err)
	}
}
