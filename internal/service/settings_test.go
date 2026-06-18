package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// fakeSettingsStore is an in-memory settingsStore for tests.
type fakeSettingsStore struct {
	settings  model.Settings
	getErr    error
	updateErr error
	updated   int
}

func (f *fakeSettingsStore) Get(_ context.Context) (model.Settings, error) {
	return f.settings, f.getErr
}

func (f *fakeSettingsStore) Update(_ context.Context, s model.Settings) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.settings = s
	f.updated++
	return nil
}

var _ settingsStore = (*fakeSettingsStore)(nil)

func TestSettingsGet(t *testing.T) {
	want := model.Settings{
		BlobMaxSize:              10 * 1024 * 1024,
		WALIdleTimeout:           3 * time.Second,
		MaxFileSize:              0,
		DefaultEvictionThreshold: 500_000,
		UpdatedAt:                time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	store := &fakeSettingsStore{settings: want}
	svc := NewSettingsService(store)

	got, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != want {
		t.Fatalf("Get returned wrong settings: got %+v, want %+v", got, want)
	}
}

func TestSettingsGetError(t *testing.T) {
	sentinel := errors.New("db down")
	store := &fakeSettingsStore{getErr: sentinel}
	svc := NewSettingsService(store)

	_, err := svc.Get(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestSettingsUpdate(t *testing.T) {
	store := &fakeSettingsStore{settings: model.DefaultSettings()}
	svc := NewSettingsService(store)

	updated := model.Settings{
		BlobMaxSize:              5 * 1024 * 1024,
		WALIdleTimeout:           10 * time.Second,
		MaxFileSize:              100 * 1024 * 1024,
		DefaultEvictionThreshold: 200_000,
	}

	if err := svc.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if store.updated != 1 {
		t.Fatalf("expected 1 update call, got %d", store.updated)
	}
	if store.settings != updated {
		t.Fatalf("store has wrong settings: got %+v, want %+v", store.settings, updated)
	}
}

func TestSettingsUpdateError(t *testing.T) {
	sentinel := errors.New("write failed")
	store := &fakeSettingsStore{updateErr: sentinel}
	svc := NewSettingsService(store)

	err := svc.Update(context.Background(), model.Settings{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestSettingsGetDefaults(t *testing.T) {
	// Verify that DefaultSettings returns sensible values and that Get passes
	// them through unchanged.
	def := model.DefaultSettings()
	store := &fakeSettingsStore{settings: def}
	svc := NewSettingsService(store)

	got, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BlobMaxSize != 19*1024*1024 {
		t.Errorf("default BlobMaxSize: got %d, want %d", got.BlobMaxSize, 19*1024*1024)
	}
	if got.WALIdleTimeout != 60*time.Second {
		t.Errorf("default WALIdleTimeout: got %v, want 60s", got.WALIdleTimeout)
	}
	if got.DefaultEvictionThreshold != 900_000 {
		t.Errorf("default DefaultEvictionThreshold: got %d, want 900000", got.DefaultEvictionThreshold)
	}
}
