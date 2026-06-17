package repository

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// schemaPath returns the absolute path to db/schema.sql by walking up from this
// source file's directory to the module root, regardless of the test's working
// directory.
func schemaPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("setup_test: runtime.Caller failed")
	}
	// thisFile = <root>/internal/repository/setup_test.go
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(root, "db", "schema.sql")
}

// setupTestPool spins a postgres:17-alpine container, applies db/schema.sql via
// the postgres init-script entrypoint (which runs it through psql, so pg_dump
// meta-commands like \restrict are honored), and returns a ready *pgxpool.Pool.
// The container is terminated and the pool closed via t.Cleanup. The test is
// skipped when Docker is unavailable so non-Docker environments do not hard-fail.
func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithInitScripts(schemaPath(t)),
		tcpostgres.WithDatabase("tgwebdav"),
		tcpostgres.WithUsername("tgwebdav"),
		tcpostgres.WithPassword("tgwebdav"),
		// BasicWaitStrategies waits for the "ready to accept connections" log to
		// appear twice; the postgres entrypoint restarts after running init
		// scripts, so the second occurrence guarantees the schema is applied.
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("docker unavailable, skipping integration test: %v", err)
		}
		t.Fatalf("setup_test: start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("setup_test: terminate container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("setup_test: connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("setup_test: open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("setup_test: ping: %v", err)
	}
	return pool
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

// TestSetupTestPool is a smoke test proving the helper yields a usable pool.
func TestSetupTestPool(t *testing.T) {
	pool := setupTestPool(t)

	var got int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&got); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if got != 1 {
		t.Fatalf("SELECT 1 = %d, want 1", got)
	}

	// The schema must have been applied: the nodes table should exist.
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'nodes')",
	).Scan(&exists); err != nil {
		t.Fatalf("schema check query: %v", err)
	}
	if !exists {
		t.Fatal("schema not applied: nodes table missing")
	}
}
