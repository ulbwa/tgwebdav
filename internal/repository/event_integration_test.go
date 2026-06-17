package repository

import (
	"context"
	"testing"
)

func TestEventRepository_HappyPath(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewEventRepository(pool)
	ctx := context.Background()

	// Log several events of different kinds.
	for i := 0; i < 5; i++ {
		if err := repo.Log(ctx, "test_kind", "message", "ref"); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	if err := repo.Log(ctx, "other_kind", "msg2", "ref2"); err != nil {
		t.Fatalf("Log other: %v", err)
	}

	// List all (no kind filter).
	all, total, err := repo.List(ctx, "", 0, 0)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if total != 6 {
		t.Errorf("List all total = %d, want 6", total)
	}
	if len(all) != 6 {
		t.Errorf("List all rows = %d, want 6", len(all))
	}

	// List with kind filter.
	filtered, fTotal, err := repo.List(ctx, "test_kind", 0, 0)
	if err != nil {
		t.Fatalf("List filtered: %v", err)
	}
	if fTotal != 5 {
		t.Errorf("List filtered total = %d, want 5", fTotal)
	}
	if len(filtered) != 5 {
		t.Errorf("List filtered rows = %d, want 5", len(filtered))
	}

	// Newest-first ordering.
	if len(all) >= 2 {
		if all[0].TS.Before(all[1].TS) {
			t.Error("List: events not newest-first")
		}
	}
}

func TestEventRepository_Pagination(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewEventRepository(pool)
	ctx := context.Background()

	// Insert 10 events.
	for i := 0; i < 10; i++ {
		if err := repo.Log(ctx, "page_kind", "msg", ""); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}

	// Page 1: limit=4, offset=0.
	page1, total, err := repo.List(ctx, "page_kind", 4, 0)
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	if len(page1) != 4 {
		t.Errorf("page1 len = %d, want 4", len(page1))
	}

	// Page 2: limit=4, offset=4.
	page2, total2, err := repo.List(ctx, "page_kind", 4, 4)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if total2 != 10 {
		t.Errorf("page2 total = %d, want 10", total2)
	}
	if len(page2) != 4 {
		t.Errorf("page2 len = %d, want 4", len(page2))
	}

	// Nil limit means all rows.
	all, allTotal, err := repo.List(ctx, "page_kind", 0, 0)
	if err != nil {
		t.Fatalf("List nil limit: %v", err)
	}
	if allTotal != 10 {
		t.Errorf("all total = %d, want 10", allTotal)
	}
	if len(all) != 10 {
		t.Errorf("all len = %d, want 10", len(all))
	}
}
