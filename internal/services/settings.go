package services

import (
	"context"
	"fmt"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// settingsService implements domain.SettingsService by delegating to the
// settings repository.
type settingsService struct {
	repo domain.SettingsRepository
}

// NewSettingsService returns a domain.SettingsService backed by r.Settings.
func NewSettingsService(r *domain.Repositories) domain.SettingsService {
	return &settingsService{repo: r.Settings}
}

// Get returns the current runtime settings.
func (s *settingsService) Get(ctx context.Context) (domain.Settings, error) {
	settings, err := s.repo.Get(ctx)
	if err != nil {
		return domain.Settings{}, fmt.Errorf("get settings: %w", err)
	}
	return settings, nil
}

// Update persists the provided runtime settings.
func (s *settingsService) Update(ctx context.Context, settings domain.Settings) error {
	if err := s.repo.Update(ctx, settings); err != nil {
		return fmt.Errorf("update settings: %w", err)
	}
	return nil
}
