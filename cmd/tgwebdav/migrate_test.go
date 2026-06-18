package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestRunMigrations applies the embedded migrations against a real PostgreSQL
// instance started in a throwaway container, then asserts the schema is present
// and that re-running is a no-op. The test is skipped when Docker is unavailable
// so non-Docker environments do not hard-fail.
func TestRunMigrations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("tgwebdav"),
		tcpostgres.WithUsername("tgwebdav"),
		tcpostgres.WithPassword("tgwebdav"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("docker unavailable, skipping integration test: %v", err)
		}
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := runMigrations(dsn, logger); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	// Idempotency: running again must be a no-op.
	if err := runMigrations(dsn, logger); err != nil {
		t.Fatalf("runMigrations (second): %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

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

// isDockerUnavailable reports whether err indicates Docker could not be reached,
// so the test can be skipped rather than failed on machines without Docker.
func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker daemon",
		"failed to find a working docker",
		"rootless docker not found",
		"no such host",
		"connection refused",
		"dial unix",
		"dial tcp",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
