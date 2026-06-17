package service

import (
	"context"
	"fmt"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// settingsStore is the narrow repository interface SettingsService needs.
// The real *repository.SettingsRepository satisfies this structurally.
type settingsStore interface {
	Get(ctx context.Context) (model.Settings, error)
	Update(ctx context.Context, s model.Settings) error
}

// SettingsService reads and updates runtime settings.
type SettingsService struct {
	store settingsStore
}

// NewSettingsService returns a SettingsService backed by store.
func NewSettingsService(store settingsStore) *SettingsService {
	return &SettingsService{store: store}
}

// Get returns the current runtime settings.
func (s *SettingsService) Get(ctx context.Context) (model.Settings, error) {
	settings, err := s.store.Get(ctx)
	if err != nil {
		return model.Settings{}, fmt.Errorf("get settings: %w", err)
	}
	return settings, nil
}

// Update persists the provided runtime settings.
func (s *SettingsService) Update(ctx context.Context, settings model.Settings) error {
	if err := s.store.Update(ctx, settings); err != nil {
		return fmt.Errorf("update settings: %w", err)
	}
	return nil
}
