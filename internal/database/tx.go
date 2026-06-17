// Package database provides the pgx-based transaction plumbing shared by every
// repository. Repositories never hold a connection directly; they resolve their
// executor per-call via FromContext, which returns the active transaction when
// one is in flight (TxManager.WithTx) or the pool otherwise. This keeps the same
// repository value correct both inside and outside a transaction.
package database

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is the pgx-compatible executor surface repositories use. Both
// *pgxpool.Pool and pgx.Tx satisfy it, so FromContext can return either. It
// matches the interface sqlc generates (including CopyFrom, used by :copyfrom
// queries) so the same value can be handed to sqlc.New.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}

// Compile-time proof that the two concrete executors satisfy DBTX.
var (
	_ DBTX = (*pgxpool.Pool)(nil)
	_ DBTX = (pgx.Tx)(nil)
)

// txKey is the context key under which the active transaction is stored.
type txKey struct{}

// FromContext returns the pgx.Tx stored in ctx by WithTx, or fallback when no
// transaction is active. Repositories call this with the pool as fallback so a
// single method body works both inside and outside a transaction.
func FromContext(ctx context.Context, fallback DBTX) DBTX {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok && tx != nil {
		return tx
	}
	return fallback
}

// TxManager opens transactions against a pgx pool and runs work inside them.
type TxManager struct {
	pool *pgxpool.Pool
}

// NewTxManager returns a TxManager bound to pool.
func NewTxManager(pool *pgxpool.Pool) *TxManager {
	return &TxManager{pool: pool}
}

// WithTx runs fn inside a single database transaction. The transaction is placed
// in the context handed to fn so repositories resolving via FromContext operate
// on it. fn returning an error rolls back and the error is returned; a panic in
// fn rolls back and re-panics; success commits.
func (m *TxManager) WithTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	// If a transaction is already active on this context, join it: nested
	// WithTx calls run as part of the outermost transaction, which owns the
	// commit/rollback. This keeps composed service calls atomic.
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			// Best-effort rollback, then propagate the panic.
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	txCtx := context.WithValue(ctx, txKey{}, tx)
	if err = fn(txCtx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}
