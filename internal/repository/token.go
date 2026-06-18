package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/samber/lo"
	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// TokenRepository persists API tokens against a pgx pool.
type TokenRepository struct {
	pool *pgxpool.Pool
}

// NewTokenRepository returns a TokenRepository backed by pool.
func NewTokenRepository(pool *pgxpool.Pool) *TokenRepository {
	return &TokenRepository{pool: pool}
}

// Create inserts a new API token. If t.ID is zero, a new UUID is generated. If
// t.CreatedAt is zero, it is set to now.
func (r *TokenRepository) Create(ctx context.Context, t *model.APIToken) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).CreateAPIToken(ctx, sqlc.CreateAPITokenParams{
		ID:         t.ID,
		UserID:     t.UserID,
		TokenHash:  t.TokenHash,
		Name:       t.Name,
		CreatedAt:  pgtype.Timestamptz{Time: t.CreatedAt, Valid: true},
		LastUsedAt: ptrToTime(t.LastUsedAt),
	})
	return translateError(err)
}

// GetByHash loads a token by its sha256 hex hash.
func (r *TokenRepository) GetByHash(ctx context.Context, hash string) (*model.APIToken, error) {
	db := database.FromContext(ctx, r.pool)
	row, err := sqlc.New(db).GetAPITokenByHash(ctx, hash)
	if err != nil {
		return nil, translateError(err)
	}
	t := mapToken(row)
	return &t, nil
}

// ListByUser returns all tokens for a user, newest first.
func (r *TokenRepository) ListByUser(ctx context.Context, userID uuid.UUID) ([]model.APIToken, error) {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ListAPITokensByUser(ctx, userID)
	if err != nil {
		return nil, translateError(err)
	}
	return lo.Map(rows, func(row sqlc.ApiToken, _ int) model.APIToken { return mapToken(row) }), nil
}

// Delete removes a token by id. Returns ErrNotFound when no row with id exists.
func (r *TokenRepository) Delete(ctx context.Context, id uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).DeleteAPIToken(ctx, id)
	if err != nil {
		return translateError(err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLastUsed updates the last_used_at timestamp for a token.
// Returns ErrNotFound when no row with id exists.
func (r *TokenRepository) TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).TouchAPITokenLastUsed(ctx, sqlc.TouchAPITokenLastUsedParams{
		ID:         id,
		LastUsedAt: pgtype.Timestamptz{Time: at, Valid: true},
	})
	if err != nil {
		return translateError(err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// mapToken converts a sqlc.ApiToken row to a model.APIToken.
func mapToken(row sqlc.ApiToken) model.APIToken {
	return model.APIToken{
		ID:         row.ID,
		UserID:     row.UserID,
		TokenHash:  row.TokenHash,
		Name:       row.Name,
		CreatedAt:  row.CreatedAt.Time,
		LastUsedAt: timeToPtr(row.LastUsedAt),
	}
}
