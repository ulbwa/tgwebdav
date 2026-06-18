package repository

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Sentinel errors raised by the repository layer. Callers map them to protocol
// responses via errors.Is without depending on pgx error types. Error identity
// is part of the package's public contract, so other layers may import this
// package solely to errors.Is-check these sentinels.
var (
	// ErrNotFound is returned when an entity does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists is returned on a unique-constraint conflict.
	ErrAlreadyExists = errors.New("already exists")
)

// translateError maps storage-layer errors onto repository sentinels so callers
// never depend on pgx error types. pgx.ErrNoRows becomes ErrNotFound and a
// Postgres unique-violation (SQLSTATE 23505) becomes ErrAlreadyExists.
// Anything else (including nil) is returned unchanged.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrAlreadyExists
	}
	return err
}
