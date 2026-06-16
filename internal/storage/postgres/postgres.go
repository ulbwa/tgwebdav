// Package postgres provides GORM-backed implementations of every repository
// declared in internal/domain, plus a context-aware TxManager. The same
// *domain.Repositories value works inside and outside a transaction: each
// method resolves its *gorm.DB from the context (txFromCtx), so WithTx merely
// injects the transaction connection into the context before invoking fn.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open establishes a GORM connection to Postgres using the pgx driver and a
// production-tuned connection pool. The GORM logger is bridged to slog at Warn
// level. Open never runs migrations.
func Open(dsn string, log *slog.Logger) (*gorm.DB, error) {
	if log == nil {
		log = slog.Default()
	}
	gormLogger := logger.New(
		&slogWriter{log: log},
		logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:                 gormLogger,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("postgres: sql db handle: %w", err)
	}
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

// slogWriter adapts gorm's logger.Writer interface to an *slog.Logger.
type slogWriter struct{ log *slog.Logger }

// Printf implements gorm's logger.Writer; messages are emitted at Warn level.
func (w *slogWriter) Printf(format string, args ...any) {
	w.log.Warn(fmt.Sprintf(format, args...))
}

// New builds every repository implementation plus a TxManager, all bound to
// base. secretKey is the 32-byte AES-256-GCM key used to encrypt bot tokens at
// rest; it may be nil when no bots are configured (bot reads/writes will then
// fail explicitly). The returned repositories share the context-aware db
// resolution, so the same value is reused inside WithTx.
func New(db *gorm.DB, secretKey []byte) (*domain.Repositories, domain.TxManager, error) {
	if db == nil {
		return nil, nil, fmt.Errorf("postgres: New: nil db")
	}
	enc, err := newEncryptor(secretKey)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: New: %w", err)
	}
	repos := &domain.Repositories{
		Users:        &userRepo{base: db},
		Tokens:       &apiTokenRepo{base: db},
		Bots:         &botRepo{base: db, enc: enc},
		Channels:     &channelRepo{base: db},
		BotChannels:  &botChannelRepo{base: db},
		Blobs:        &blobRepo{base: db},
		BlobBotFiles: &blobBotFileRepo{base: db},
		Nodes:        &nodeRepo{base: db},
		Extents:      &extentRepo{base: db},
		WAL:          &walRepo{base: db},
		Events:       &eventRepo{base: db},
		Stats:        &statRepo{base: db},
		Settings:     &settingsRepo{base: db},
	}
	tx := &txManager{base: db, repos: repos}
	return repos, tx, nil
}

// txKey is the context key under which the active *gorm.DB transaction handle
// is stored by WithTx.
type txKey struct{}

// txFromCtx returns the transaction-scoped *gorm.DB stored in ctx, or base when
// no transaction is active. The returned handle is always bound to ctx via
// WithContext so query cancellation flows through.
func txFromCtx(ctx context.Context, base *gorm.DB) *gorm.DB {
	if db, ok := ctx.Value(txKey{}).(*gorm.DB); ok && db != nil {
		return db.WithContext(ctx)
	}
	return base.WithContext(ctx)
}

// txManager implements domain.TxManager. The repositories it carries are the
// very ones returned by New; because every method reads its db from the
// context, no rebinding is required — WithTx only injects the tx handle.
type txManager struct {
	base  *gorm.DB
	repos *domain.Repositories
}

// WithTx runs fn inside a single database transaction. The repos handed to fn
// are the shared repositories; they operate on the transaction because the
// transaction handle is injected into ctx. Returning an error (or panicking)
// rolls the transaction back.
func (t *txManager) WithTx(ctx context.Context, fn func(ctx context.Context, r *domain.Repositories) error) error {
	// Reuse an outer transaction if one is already active so nested WithTx calls
	// share the same atomic unit.
	if _, ok := ctx.Value(txKey{}).(*gorm.DB); ok {
		return fn(ctx, t.repos)
	}
	return t.base.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txCtx := context.WithValue(ctx, txKey{}, tx)
		return fn(txCtx, t.repos)
	})
}

// translateError maps storage-layer errors onto domain sentinels so callers
// never depend on GORM or pgx error types. gorm.ErrRecordNotFound becomes
// domain.ErrNotFound and a Postgres unique-violation (SQLSTATE 23505) becomes
// domain.ErrAlreadyExists. Anything else is wrapped verbatim.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, pgErr.ConstraintName)
	}
	return err
}
