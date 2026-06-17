package repository

import (
	"context"
	"testing"
	"time"

	"github.com/ulbwa/tgwebdav/internal/model"
)

func TestSettingsRepository_DefaultsOnMissingRow(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewSettingsRepository(pool)
	ctx := context.Background()

	// No row exists yet — must return DefaultSettings.
	got, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get (no row): %v", err)
	}
	def := model.DefaultSettings()
	if got.BlobMaxSize != def.BlobMaxSize {
		t.Errorf("BlobMaxSize = %d, want %d", got.BlobMaxSize, def.BlobMaxSize)
	}
	if got.WALIdleTimeout != def.WALIdleTimeout {
		t.Errorf("WALIdleTimeout = %v, want %v", got.WALIdleTimeout, def.WALIdleTimeout)
	}
	if got.DefaultEvictionThreshold != def.DefaultEvictionThreshold {
		t.Errorf("DefaultEvictionThreshold = %d, want %d", got.DefaultEvictionThreshold, def.DefaultEvictionThreshold)
	}
}

func TestSettingsRepository_UpdateAndGet(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewSettingsRepository(pool)
	ctx := context.Background()

	s := model.Settings{
		BlobMaxSize:              5 * 1024 * 1024,
		WALIdleTimeout:           10 * time.Second,
		MaxFileSize:              100 * 1024 * 1024,
		DefaultEvictionThreshold: 500_000,
	}

	// First upsert.
	if err := repo.Update(ctx, s); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if got.BlobMaxSize != s.BlobMaxSize {
		t.Errorf("BlobMaxSize = %d, want %d", got.BlobMaxSize, s.BlobMaxSize)
	}
	if got.WALIdleTimeout != s.WALIdleTimeout {
		t.Errorf("WALIdleTimeout = %v, want %v", got.WALIdleTimeout, s.WALIdleTimeout)
	}
	if got.MaxFileSize != s.MaxFileSize {
		t.Errorf("MaxFileSize = %d, want %d", got.MaxFileSize, s.MaxFileSize)
	}
	if got.DefaultEvictionThreshold != s.DefaultEvictionThreshold {
		t.Errorf("DefaultEvictionThreshold = %d, want %d", got.DefaultEvictionThreshold, s.DefaultEvictionThreshold)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should not be zero after Update")
	}

	// Update again (tests ON CONFLICT path).
	s2 := model.Settings{
		BlobMaxSize:              8 * 1024 * 1024,
		WALIdleTimeout:           2 * time.Second,
		MaxFileSize:              0, // unlimited
		DefaultEvictionThreshold: 100_000,
	}
	if err := repo.Update(ctx, s2); err != nil {
		t.Fatalf("Update second: %v", err)
	}
	got2, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get after second Update: %v", err)
	}
	if got2.BlobMaxSize != s2.BlobMaxSize {
		t.Errorf("second BlobMaxSize = %d, want %d", got2.BlobMaxSize, s2.BlobMaxSize)
	}
	// MaxFileSize == 0 must not be replaced by default (it means unlimited).
	if got2.MaxFileSize != 0 {
		t.Errorf("MaxFileSize = %d, want 0 (unlimited)", got2.MaxFileSize)
	}
}

func TestSettingsRepository_ZeroFieldsUseDefaults(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewSettingsRepository(pool)
	ctx := context.Background()

	// Persist a row with zero BlobMaxSize and zero WALIdleTimeout.
	s := model.Settings{
		BlobMaxSize:              0, // should be replaced by default on Get
		WALIdleTimeout:           0, // should be replaced by default on Get
		MaxFileSize:              0,
		DefaultEvictionThreshold: 0, // should be replaced by default on Get
	}
	if err := repo.Update(ctx, s); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	def := model.DefaultSettings()
	if got.BlobMaxSize != def.BlobMaxSize {
		t.Errorf("BlobMaxSize = %d, want default %d", got.BlobMaxSize, def.BlobMaxSize)
	}
	if got.WALIdleTimeout != def.WALIdleTimeout {
		t.Errorf("WALIdleTimeout = %v, want default %v", got.WALIdleTimeout, def.WALIdleTimeout)
	}
	if got.DefaultEvictionThreshold != def.DefaultEvictionThreshold {
		t.Errorf("DefaultEvictionThreshold = %d, want default %d", got.DefaultEvictionThreshold, def.DefaultEvictionThreshold)
	}
}
