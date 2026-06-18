package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// UserRepository persists users against a pgx pool.
type UserRepository struct {
	pool *pgxpool.Pool
}

// NewUserRepository returns a UserRepository backed by pool.
func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{pool: pool}
}

// Create inserts a new user. If u.ID is zero, a new UUID is generated. If
// u.CreatedAt is zero, it is set to now.
func (r *UserRepository) Create(ctx context.Context, u *model.User) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).CreateUser(ctx, sqlc.CreateUserParams{
		ID:           u.ID,
		Login:        u.Login,
		PasswordHash: u.PasswordHash,
		IsAdmin:      u.IsAdmin,
		QuotaBytes:   u.QuotaBytes,
		BandwidthBps: u.BandwidthBPS,
		RatePerMin:   int32(u.RatePerMin),
		CreatedAt:    pgtype.Timestamptz{Time: u.CreatedAt, Valid: true},
	})
	return translateError(err)
}

// Update saves mutable fields of an existing user. Returns ErrNotFound when
// no row with u.ID exists.
func (r *UserRepository) Update(ctx context.Context, u *model.User) error {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).UpdateUser(ctx, sqlc.UpdateUserParams{
		ID:           u.ID,
		Login:        u.Login,
		PasswordHash: u.PasswordHash,
		IsAdmin:      u.IsAdmin,
		QuotaBytes:   u.QuotaBytes,
		BandwidthBps: u.BandwidthBPS,
		RatePerMin:   int32(u.RatePerMin),
	})
	if err != nil {
		return translateError(err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a user by id. Returns ErrNotFound when no row with id exists.
func (r *UserRepository) Delete(ctx context.Context, id uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).DeleteUser(ctx, id)
	if err != nil {
		return translateError(err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetByID loads a user by primary key.
func (r *UserRepository) GetByID(ctx context.Context, id uuid.UUID) (*model.User, error) {
	db := database.FromContext(ctx, r.pool)
	row, err := sqlc.New(db).GetUserByID(ctx, id)
	if err != nil {
		return nil, translateError(err)
	}
	u := mapUser(row)
	return &u, nil
}

// GetByLogin loads a user by their unique login.
func (r *UserRepository) GetByLogin(ctx context.Context, login string) (*model.User, error) {
	db := database.FromContext(ctx, r.pool)
	row, err := sqlc.New(db).GetUserByLogin(ctx, login)
	if err != nil {
		return nil, translateError(err)
	}
	u := mapUser(row)
	return &u, nil
}

// List returns all users ordered by creation time ascending.
func (r *UserRepository) List(ctx context.Context) ([]model.User, error) {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ListUsers(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]model.User, len(rows))
	for i, row := range rows {
		out[i] = mapUser(row)
	}
	return out, nil
}

// Count returns the total number of users.
func (r *UserRepository) Count(ctx context.Context) (int64, error) {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).CountUsers(ctx)
	if err != nil {
		return 0, translateError(err)
	}
	return n, nil
}

// mapUser converts a sqlc.User row to a model.User.
func mapUser(row sqlc.User) model.User {
	return model.User{
		ID:           row.ID,
		Login:        row.Login,
		PasswordHash: row.PasswordHash,
		IsAdmin:      row.IsAdmin,
		QuotaBytes:   row.QuotaBytes,
		BandwidthBPS: row.BandwidthBps,
		RatePerMin:   int(row.RatePerMin),
		CreatedAt:    row.CreatedAt.Time,
	}
}
