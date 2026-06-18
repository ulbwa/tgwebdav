package main

import (
	"fmt"
	"log/slog"
	"net/url"

	"github.com/amacneil/dbmate/v2/pkg/dbmate"
	"github.com/spf13/cobra"

	// Register only the PostgreSQL driver to keep the binary lean.
	_ "github.com/amacneil/dbmate/v2/pkg/driver/postgres"

	"github.com/ulbwa/tgwebdav/db/migrations"
)

// migrateCmd applies all pending database migrations and exits. It uses the same
// embedded migration runner the server runs on boot, so `tgwebdav migrate` and
// the auto-migrate-on-start path are identical. It reads only the DSN.
var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database migrations",
	Long:  "Apply all pending embedded database migrations against the configured DSN and exit.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		dsn, err := loadMigrateConfig(cmd)
		if err != nil {
			return err
		}
		resolveLogLevel(cmd)
		return runMigrations(dsn, slog.Default())
	},
}

// runMigrations applies all pending embedded migrations (migrations.FS) against
// the database identified by dsn, creating the database if it does not yet exist
// and failing fast on error. Progress is logged through logger (slog); a nil
// logger uses slog.Default. Both the migrate command and the server's
// auto-migrate-on-boot path call this.
func runMigrations(dsn string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "migrations")

	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}

	db := dbmate.New(u)
	db.FS = migrations.FS
	db.MigrationsDir = []string{"."}
	db.AutoDumpSchema = false
	// dbmate writes progress to an io.Writer; route it to slog at INFO via the
	// stdlib bridge (each line becomes a record carrying the migrations attrs).
	db.Log = slog.NewLogLogger(log.Handler(), slog.LevelInfo).Writer()

	log.Info("running database migrations")
	if err := db.CreateAndMigrate(); err != nil {
		return fmt.Errorf("dbmate migrate: %w", err)
	}
	log.Info("database migrations complete")
	return nil
}
