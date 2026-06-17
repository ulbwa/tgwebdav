package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/model"
)

func newChannel(chatID int64, title string) *model.Channel {
	return &model.Channel{
		TGChatID:          chatID,
		Title:             title,
		EvictionThreshold: 900000,
		Available:         true,
	}
}

func TestChannelRepository_CRUD(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewChannelRepository(pool)

	c := newChannel(-1001234567890, "Storage A")
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == uuid.Nil {
		t.Fatal("Create did not assign an ID")
	}

	got, err := repo.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.TGChatID != -1001234567890 || got.Title != "Storage A" {
		t.Fatalf("GetByID mismatch: %+v", got)
	}
	if got.EvictionThreshold != 900000 || !got.Available {
		t.Fatalf("GetByID defaults lost: %+v", got)
	}

	byChat, err := repo.GetByChatID(ctx, -1001234567890)
	if err != nil {
		t.Fatalf("GetByChatID: %v", err)
	}
	if byChat.ID != c.ID {
		t.Fatalf("GetByChatID returned wrong row: %v", byChat.ID)
	}

	c.Title = "Storage A renamed"
	c.MessageCounter = 42
	c.EvictionThreshold = 5
	c.Available = false
	if err := repo.Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = repo.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.Title != "Storage A renamed" || got.MessageCounter != 42 ||
		got.EvictionThreshold != 5 || got.Available {
		t.Fatalf("Update did not persist: %+v", got)
	}

	if err := repo.Delete(ctx, c.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, c.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetByID after delete = %v, want ErrNotFound", err)
	}
}

func TestChannelRepository_List(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewChannelRepository(pool)

	a := newChannel(-100111, "a")
	a.CreatedAt = time.Now().Add(-2 * time.Hour)
	b := newChannel(-100222, "b")
	b.CreatedAt = time.Now().Add(-1 * time.Hour)
	for _, c := range []*model.Channel{a, b} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
	if list[0].Title != "a" || list[1].Title != "b" {
		t.Fatalf("List order = %s,%s", list[0].Title, list[1].Title)
	}
}

func TestChannelRepository_DuplicateChatID(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewChannelRepository(pool)

	if err := repo.Create(ctx, newChannel(-100999, "first")); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	err := repo.Create(ctx, newChannel(-100999, "second"))
	if !errors.Is(err, model.ErrAlreadyExists) {
		t.Fatalf("duplicate tg_chat_id = %v, want ErrAlreadyExists", err)
	}
}

func TestChannelRepository_IncrementCounter(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewChannelRepository(pool)

	c := newChannel(-100777, "counter")
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	n, err := repo.IncrementCounter(ctx, c.ID, 1)
	if err != nil {
		t.Fatalf("IncrementCounter: %v", err)
	}
	if n != 1 {
		t.Fatalf("IncrementCounter returned %d, want 1", n)
	}
	n, err = repo.IncrementCounter(ctx, c.ID, 10)
	if err != nil {
		t.Fatalf("IncrementCounter: %v", err)
	}
	if n != 11 {
		t.Fatalf("IncrementCounter returned %d, want 11", n)
	}

	// Concurrent increments must be atomic: 50 goroutines x +1 = 61 total.
	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if _, err := repo.IncrementCounter(ctx, c.ID, 1); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent IncrementCounter: %v", err)
	}

	final, err := repo.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if final.MessageCounter != 11+workers {
		t.Fatalf("final counter = %d, want %d", final.MessageCounter, 11+workers)
	}
}

func TestChannelRepository_SetAvailable(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewChannelRepository(pool)

	c := newChannel(-100555, "avail")
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.SetAvailable(ctx, c.ID, false); err != nil {
		t.Fatalf("SetAvailable: %v", err)
	}
	got, err := repo.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Available {
		t.Fatal("SetAvailable(false) did not persist")
	}
}

func TestChannelRepository_NotFound(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewChannelRepository(pool)

	missing := uuid.New()
	if _, err := repo.GetByID(ctx, missing); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetByID missing = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByChatID(ctx, -100000); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetByChatID missing = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, missing); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Delete missing = %v, want ErrNotFound", err)
	}
	if err := repo.SetAvailable(ctx, missing, true); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("SetAvailable missing = %v, want ErrNotFound", err)
	}
	upd := newChannel(-100123, "x")
	upd.ID = missing
	if err := repo.Update(ctx, upd); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Update missing = %v, want ErrNotFound", err)
	}
	// IncrementCounter on a missing row hits no row → pgx.ErrNoRows → ErrNotFound.
	if _, err := repo.IncrementCounter(ctx, missing, 1); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("IncrementCounter missing = %v, want ErrNotFound", err)
	}
}
