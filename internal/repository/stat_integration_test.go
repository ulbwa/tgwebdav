package repository

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStatRepository_HappyPath(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewStatRepository(pool)
	ctx := context.Background()

	// Record some samples.
	if err := repo.Record(ctx, "req_count", "global", 1.0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := repo.Record(ctx, "req_count", "global", 2.0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := repo.Record(ctx, "req_count", "global", 3.0); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Latest returns the most recent one (value 3.0 was last inserted).
	latest, err := repo.Latest(ctx, "req_count", "global")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.Value != 3.0 {
		t.Errorf("Latest value = %v, want 3.0", latest.Value)
	}
	if latest.Metric != "req_count" {
		t.Errorf("Latest metric = %q, want req_count", latest.Metric)
	}
}

func TestStatRepository_Query(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewStatRepository(pool)
	ctx := context.Background()

	before := time.Now().Add(-time.Second)

	if err := repo.Record(ctx, "bytes", "read", 100); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := repo.Record(ctx, "bytes", "read", 200); err != nil {
		t.Fatalf("Record: %v", err)
	}

	after := time.Now().Add(time.Second)

	samples, err := repo.Query(ctx, "bytes", "read", before, after)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(samples) != 2 {
		t.Errorf("Query: want 2 samples, got %d", len(samples))
	}
	// Ordered oldest-first.
	if len(samples) == 2 && samples[0].TS.After(samples[1].TS) {
		t.Error("Query: samples not oldest-first")
	}

	// Outside the window.
	empty, err := repo.Query(ctx, "bytes", "read", after, after.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query empty window: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("Query empty window: want 0, got %d", len(empty))
	}
}

func TestStatRepository_LatestNotFound(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewStatRepository(pool)
	ctx := context.Background()

	_, err := repo.Latest(ctx, "nonexistent_metric", "nonexistent_label")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Latest missing: want ErrNotFound, got %v", err)
	}
}
