// Package migrations embeds the dbmate SQL migrations and runs them on boot.
// dbmate is used purely as a library; GORM never performs AutoMigrate.
package migrations

import (
	"embed"
	"fmt"
	"io"
	"net/url"

	"github.com/amacneil/dbmate/v2/pkg/dbmate"
	// Register only the PostgreSQL driver to keep the binary lean.
	_ "github.com/amacneil/dbmate/v2/pkg/driver/postgres"
)

//go:embed *.sql
var migrationsFS embed.FS

// Run applies all pending migrations against the database identified by dsn.
// It creates the database if it does not yet exist and fails fast on error.
func Run(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}

	db := dbmate.New(u)
	db.FS = migrationsFS
	db.MigrationsDir = []string{"."}
	db.AutoDumpSchema = false
	db.Log = io.Discard

	if err := db.CreateAndMigrate(); err != nil {
		return fmt.Errorf("dbmate migrate: %w", err)
	}
	return nil
}
