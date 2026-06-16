package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// apiTokenRepo implements domain.APITokenRepository.
type apiTokenRepo struct{ base *gorm.DB }

// Create inserts a new API token.
func (r *apiTokenRepo) Create(ctx context.Context, t *domain.APIToken) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	if err := txFromCtx(ctx, r.base).Create(apiTokenToModel(t)).Error; err != nil {
		return fmt.Errorf("create api token: %w", translateError(err))
	}
	return nil
}

// GetByHash loads a token by its sha256 hex hash.
func (r *apiTokenRepo) GetByHash(ctx context.Context, hash string) (*domain.APIToken, error) {
	var m apiTokenModel
	if err := txFromCtx(ctx, r.base).Where("token_hash = ?", hash).First(&m).Error; err != nil {
		return nil, fmt.Errorf("get api token by hash: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// ListByUser returns a user's tokens, newest first.
func (r *apiTokenRepo) ListByUser(ctx context.Context, userID uuid.UUID) ([]domain.APIToken, error) {
	var ms []apiTokenModel
	if err := txFromCtx(ctx, r.base).Where("user_id = ?", userID).Order("created_at DESC").Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list api tokens: %w", translateError(err))
	}
	out := make([]domain.APIToken, len(ms))
	for i := range ms {
		out[i] = *ms[i].toDomain()
	}
	return out, nil
}

// Delete removes a token by id.
func (r *apiTokenRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res := txFromCtx(ctx, r.base).Where("id = ?", id).Delete(&apiTokenModel{})
	if res.Error != nil {
		return fmt.Errorf("delete api token: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete api token: %w", domain.ErrNotFound)
	}
	return nil
}

// TouchLastUsed records the last-used timestamp for a token.
func (r *apiTokenRepo) TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	res := txFromCtx(ctx, r.base).Model(&apiTokenModel{}).
		Where("id = ?", id).
		Update("last_used_at", at)
	if res.Error != nil {
		return fmt.Errorf("touch api token: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("touch api token: %w", domain.ErrNotFound)
	}
	return nil
}
