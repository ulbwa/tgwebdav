// Package migrations embeds the dbmate SQL migrations (db/migrations/*.sql) and
// runs them on boot. dbmate is the only schema authority — there is no runtime
// auto-migration; the schema changes only through these versioned SQL files. The
// embedded files live alongside this file so `go:embed` can reach them while the
// migrations stay out of the internal tree.
package migrations

import (
	"embed"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/amacneil/dbmate/v2/pkg/dbmate"
	// Register only the PostgreSQL driver to keep the binary lean.
	_ "github.com/amacneil/dbmate/v2/pkg/driver/postgres"
)

//go:embed *.sql
var migrationsFS embed.FS

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

// Run applies all pending migrations against the database identified by dsn,
// creating the database if it does not yet exist and failing fast on error.
// Progress is logged through logger (slog); a nil logger uses slog.Default.
func Run(dsn string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "migrations")

	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}

	db := dbmate.New(u)
	db.FS = migrationsFS
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
