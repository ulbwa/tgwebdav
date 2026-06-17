package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// newTestPool spins a bare postgres:17-alpine container (no schema) and returns a
// ready pool. The container and pool are cleaned up via t.Cleanup. The test is
// skipped when Docker is unavailable.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

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
		if dockerUnavailable(err) {
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
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func dockerUnavailable(err error) bool {
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

func TestFromContext_FallbackWhenNoTx(t *testing.T) {
	pool := newTestPool(t)
	if got := FromContext(context.Background(), pool); got != DBTX(pool) {
		t.Fatalf("FromContext with no tx = %v, want pool fallback", got)
	}
}

func TestWithTx_CommitPersists(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	m := NewTxManager(pool)

	if _, err := pool.Exec(ctx, "CREATE TABLE t_commit (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	err := m.WithTx(ctx, func(txCtx context.Context) error {
		exec := FromContext(txCtx, pool)
		// Inside WithTx, FromContext must yield the tx, not the pool.
		if exec == DBTX(pool) {
			t.Fatal("FromContext returned pool inside WithTx; want tx")
		}
		_, err := exec.Exec(txCtx, "INSERT INTO t_commit (id) VALUES (1)")
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM t_commit").Scan(&n); err != nil {
		t.Fatalf("count after commit: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count after commit = %d, want 1", n)
	}
}

func TestWithTx_RollbackOnError(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	m := NewTxManager(pool)

	if _, err := pool.Exec(ctx, "CREATE TABLE t_rollback (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	sentinel := errors.New("boom")
	err := m.WithTx(ctx, func(txCtx context.Context) error {
		exec := FromContext(txCtx, pool)
		if _, err := exec.Exec(txCtx, "INSERT INTO t_rollback (id) VALUES (1)"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error = %v, want sentinel", err)
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM t_rollback").Scan(&n); err != nil {
		t.Fatalf("count after rollback: %v", err)
	}
	if n != 0 {
		t.Fatalf("row count after rollback = %d, want 0", n)
	}
}

func TestWithTx_NestedReusesOuterTx(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	m := NewTxManager(pool)

	if _, err := pool.Exec(ctx, "CREATE TABLE t_nested (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// The inner WithTx must join the outer transaction rather than open a second
	// one: the executor resolved inside the nested closure is the very same
	// pgx.Tx the outer closure sees.
	var innerRan bool
	err := m.WithTx(ctx, func(outerCtx context.Context) error {
		outerExec := FromContext(outerCtx, pool)
		if outerExec == DBTX(pool) {
			t.Fatal("outer FromContext returned pool; want tx")
		}
		return m.WithTx(outerCtx, func(innerCtx context.Context) error {
			innerRan = true
			innerExec := FromContext(innerCtx, pool)
			if innerExec != outerExec {
				t.Fatal("nested WithTx began a new tx; want it to reuse the outer tx")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if !innerRan {
		t.Fatal("inner closure did not run")
	}
}

func TestWithTx_NestedErrorRollsBackOuter(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	m := NewTxManager(pool)

	if _, err := pool.Exec(ctx, "CREATE TABLE t_nested_rollback (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// An error returned from the INNER closure propagates out of the nested
	// WithTx (which only ran fn, owning nothing) up to the OUTER WithTx, which
	// owns the transaction and rolls it back. A row written in the outer closure
	// before the nested call must therefore NOT be persisted.
	sentinel := errors.New("inner boom")
	err := m.WithTx(ctx, func(outerCtx context.Context) error {
		exec := FromContext(outerCtx, pool)
		if _, err := exec.Exec(outerCtx, "INSERT INTO t_nested_rollback (id) VALUES (1)"); err != nil {
			return err
		}
		return m.WithTx(outerCtx, func(innerCtx context.Context) error {
			return sentinel
		})
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error = %v, want sentinel", err)
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM t_nested_rollback").Scan(&n); err != nil {
		t.Fatalf("count after rollback: %v", err)
	}
	if n != 0 {
		t.Fatalf("row count after nested-error rollback = %d, want 0", n)
	}
}

func TestWithTx_RollbackOnPanic(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	m := NewTxManager(pool)

	if _, err := pool.Exec(ctx, "CREATE TABLE t_panic (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("WithTx did not re-panic")
			}
		}()
		_ = m.WithTx(ctx, func(txCtx context.Context) error {
			exec := FromContext(txCtx, pool)
			if _, err := exec.Exec(txCtx, "INSERT INTO t_panic (id) VALUES (1)"); err != nil {
				t.Errorf("insert: %v", err)
			}
			panic("kaboom")
		})
	}()

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM t_panic").Scan(&n); err != nil {
		t.Fatalf("count after panic: %v", err)
	}
	if n != 0 {
		t.Fatalf("row count after panic = %d, want 0", n)
	}
}
