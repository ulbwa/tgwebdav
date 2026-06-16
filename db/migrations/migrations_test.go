package migrations

import (
	"database/sql"
	"io"
	"log/slog"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestRun applies the embedded migrations against a real PostgreSQL instance.
// It is skipped unless TGWEBDAV_TEST_DSN is set.
func TestRun(t *testing.T) {
	dsn := os.Getenv("TGWEBDAV_TEST_DSN")
	if dsn == "" {
		t.Skip("set TGWEBDAV_TEST_DSN to run migration integration test")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Run(dsn, logger); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Idempotency: running again must be a no-op.
	if err := Run(dsn, logger); err != nil {
		t.Fatalf("Run (second): %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	for _, table := range []string{
		"users", "api_tokens", "bots", "channels", "bot_channel", "blobs",
		"blob_bot_files", "nodes", "extents", "wal_chunks", "events",
		"stat_samples", "settings", "schema_migrations",
	} {
		var n int
		q := "SELECT 1 FROM information_schema.tables WHERE table_name = $1"
		if err := db.QueryRow(q, table).Scan(&n); err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}

	var blobMax int64
	if err := db.QueryRow("SELECT blob_max_size FROM settings WHERE id = 1").Scan(&blobMax); err != nil {
		t.Fatalf("settings row: %v", err)
	}
	if blobMax != 19922944 {
		t.Errorf("blob_max_size = %d, want 19922944", blobMax)
	}
}
