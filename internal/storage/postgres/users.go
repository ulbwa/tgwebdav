package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// userRepo implements domain.UserRepository.
type userRepo struct{ base *gorm.DB }

// Create inserts a new user. The id is expected to be set by the caller;
// CreatedAt defaults to now when zero.
func (r *userRepo) Create(ctx context.Context, u *domain.User) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	if err := txFromCtx(ctx, r.base).Create(userToModel(u)).Error; err != nil {
		return fmt.Errorf("create user: %w", translateError(err))
	}
	return nil
}

// Update saves every column of an existing user.
func (r *userRepo) Update(ctx context.Context, u *domain.User) error {
	res := txFromCtx(ctx, r.base).Model(&userModel{}).
		Where("id = ?", u.ID).
		Updates(map[string]any{
			"login":         u.Login,
			"password_hash": u.PasswordHash,
			"is_admin":      u.IsAdmin,
			"quota_bytes":   u.QuotaBytes,
			"bandwidth_bps": u.BandwidthBPS,
			"rate_per_min":  u.RatePerMin,
		})
	if res.Error != nil {
		return fmt.Errorf("update user: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update user: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes a user by id (cascades to tokens and nodes).
func (r *userRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res := txFromCtx(ctx, r.base).Where("id = ?", id).Delete(&userModel{})
	if res.Error != nil {
		return fmt.Errorf("delete user: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete user: %w", domain.ErrNotFound)
	}
	return nil
}

// GetByID loads a user by primary key.
func (r *userRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	var m userModel
	if err := txFromCtx(ctx, r.base).Where("id = ?", id).First(&m).Error; err != nil {
		return nil, fmt.Errorf("get user by id: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// GetByLogin loads a user by their unique login.
func (r *userRepo) GetByLogin(ctx context.Context, login string) (*domain.User, error) {
	var m userModel
	if err := txFromCtx(ctx, r.base).Where("login = ?", login).First(&m).Error; err != nil {
		return nil, fmt.Errorf("get user by login: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// List returns all users ordered by creation time.
func (r *userRepo) List(ctx context.Context) ([]domain.User, error) {
	var ms []userModel
	if err := txFromCtx(ctx, r.base).Order("created_at").Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list users: %w", translateError(err))
	}
	out := make([]domain.User, len(ms))
	for i := range ms {
		out[i] = *ms[i].toDomain()
	}
	return out, nil
}

// Count returns the number of users.
func (r *userRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := txFromCtx(ctx, r.base).Model(&userModel{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count users: %w", translateError(err))
	}
	return n, nil
}
