package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"

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

// slogWriter adapts dbmate's io.Writer log output onto a *slog.Logger so
// migration progress (e.g. "Applying: 20260616000001_init.sql") is emitted
// through the project's standard structured logger.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			w.log.Info("dbmate: " + s)
		}
	}
	return len(p), nil
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
	db.Log = slogWriter{log: log}

	log.Info("running database migrations")
	if err := db.CreateAndMigrate(); err != nil {
		return fmt.Errorf("dbmate migrate: %w", err)
	}
	log.Info("database migrations complete")
	return nil
}
