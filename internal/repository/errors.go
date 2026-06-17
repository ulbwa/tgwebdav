package repository

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/ulbwa/tgwebdav/internal/model"
)

// translateError maps storage-layer errors onto model sentinels so callers never
// depend on pgx error types. pgx.ErrNoRows becomes model.ErrNotFound and a
// Postgres unique-violation (SQLSTATE 23505) becomes model.ErrAlreadyExists.
// Anything else (including nil) is returned unchanged.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return model.ErrAlreadyExists
	}
	return err
}
