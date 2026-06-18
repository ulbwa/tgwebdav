package cache

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestCache(t *testing.T, maxBytes int64, idleTTL time.Duration) *Cache {
	t.Helper()
	c, err := New(t.TempDir(), maxBytes, idleTTL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestPutGetRoundtrip(t *testing.T) {
	c := newTestCache(t, 0, 0)
	id := uuid.New()
	want := []byte("hello blob world")

	if err := c.Put(id, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := c.Get(id)
	if !ok {
		t.Fatal("Get: expected hit, got miss")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Get: got %q, want %q", got, want)
	}

	// File must exist on disk at the expected path.
	if _, err := os.Stat(c.pathFor(id)); err != nil {
		t.Fatalf("expected blob file on disk: %v", err)
	}
}

func TestGetMiss(t *testing.T) {
	c := newTestCache(t, 0, 0)
	if _, ok := c.Get(uuid.New()); ok {
		t.Fatal("Get on empty cache: expected miss")
	}
}

// TestDirAndFilePermissions asserts the cache is owner-only at rest: the
// directory is 0700 (even when it pre-exists with looser perms) and blob files
// are 0600. Cached blobs are decrypted user content, so they must not be
// world-readable or the directory world-listable.
func TestDirAndFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	// Pre-create the directory world-traversable to prove New tightens it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-create dir: %v", err)
	}

	c, err := New(dir, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("cache dir perm = %v, want 0700", got)
	}

	id := uuid.New()
	if err := c.Put(id, []byte("secret bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fi, err := os.Stat(c.pathFor(id))
	if err != nil {
		t.Fatalf("stat blob file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("blob file perm = %v, want 0600", got)
	}
}

func TestPutOverwriteAdjustsTotal(t *testing.T) {
	c := newTestCache(t, 0, 0)
	id := uuid.New()

	if err := c.Put(id, bytes.Repeat([]byte("a"), 100)); err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	if err := c.Put(id, bytes.Repeat([]byte("b"), 30)); err != nil {
		t.Fatalf("Put 2: %v", err)
	}

	gotBytes, entries := c.Stats()
	if entries != 1 {
		t.Fatalf("entries: got %d, want 1", entries)
	}
	if gotBytes != 30 {
		t.Fatalf("bytes: got %d, want 30", gotBytes)
	}
	got, ok := c.Get(id)
	if !ok || len(got) != 30 {
		t.Fatalf("Get after overwrite: ok=%v len=%d", ok, len(got))
	}
}

func TestLRUSizeEviction(t *testing.T) {
	// maxBytes holds at most two 10-byte blobs.
	c := newTestCache(t, 20, 0)
	a, b, d := uuid.New(), uuid.New(), uuid.New()
	blob := bytes.Repeat([]byte("x"), 10)

	if err := c.Put(a, blob); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := c.Put(b, blob); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	// Touch a so b becomes the least-recently used.
	if _, ok := c.Get(a); !ok {
		t.Fatal("Get a: expected hit")
	}
	// Adding d pushes total to 30 > 20: the LRU entry (b) must be evicted.
	if err := c.Put(d, blob); err != nil {
		t.Fatalf("Put d: %v", err)
	}

	if _, ok := c.Get(b); ok {
		t.Fatal("b should have been evicted as least-recently used")
	}
	if _, ok := c.Get(a); !ok {
		t.Fatal("a should still be cached")
	}
	if _, ok := c.Get(d); !ok {
		t.Fatal("d should be cached")
	}

	gotBytes, entries := c.Stats()
	if entries != 2 || gotBytes != 20 {
		t.Fatalf("after eviction: bytes=%d entries=%d, want 20/2", gotBytes, entries)
	}

	// The evicted blob's file must be gone from disk.
	if _, err := os.Stat(c.pathFor(b)); !os.IsNotExist(err) {
		t.Fatalf("evicted blob file should be removed, stat err = %v", err)
	}
}

func TestIdleEviction(t *testing.T) {
	c := newTestCache(t, 0, time.Hour)

	// Drive the clock manually so the test is deterministic.
	base := time.Now()
	c.now = func() time.Time { return base }

	old, fresh := uuid.New(), uuid.New()
	if err := c.Put(old, []byte("old")); err != nil {
		t.Fatalf("Put old: %v", err)
	}

	// Advance the clock past the TTL, then add a fresh entry.
	c.now = func() time.Time { return base.Add(2 * time.Hour) }
	if err := c.Put(fresh, []byte("fresh")); err != nil {
		t.Fatalf("Put fresh: %v", err)
	}

	// Manually trigger the janitor sweep (test seam).
	c.sweepIdle()

	if _, ok := c.Get(old); ok {
		t.Fatal("old entry should have been idle-evicted")
	}
	if _, ok := c.Get(fresh); !ok {
		t.Fatal("fresh entry should survive idle sweep")
	}
}

func TestStartJanitorViaTicker(t *testing.T) {
	c := newTestCache(t, 0, time.Millisecond)
	c.tick = 5 * time.Millisecond

	id := uuid.New()
	if err := c.Put(id, []byte("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx)

	deadline := time.After(2 * time.Second)
	for {
		if _, n := c.Stats(); n == 0 {
			return // janitor evicted the idle entry
		}
		select {
		case <-deadline:
			t.Fatal("janitor did not evict idle entry within deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRemove(t *testing.T) {
	c := newTestCache(t, 0, 0)
	id := uuid.New()
	if err := c.Put(id, []byte("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	c.Remove(id)
	if _, ok := c.Get(id); ok {
		t.Fatal("Get after Remove: expected miss")
	}
	if b, n := c.Stats(); b != 0 || n != 0 {
		t.Fatalf("Stats after Remove: bytes=%d entries=%d, want 0/0", b, n)
	}
	if _, err := os.Stat(c.pathFor(id)); !os.IsNotExist(err) {
		t.Fatalf("file should be deleted, stat err = %v", err)
	}

	// Removing an absent id is a no-op.
	c.Remove(uuid.New())
}

func TestStats(t *testing.T) {
	c := newTestCache(t, 0, 0)
	if b, n := c.Stats(); b != 0 || n != 0 {
		t.Fatalf("empty Stats: got %d/%d, want 0/0", b, n)
	}
	if err := c.Put(uuid.New(), bytes.Repeat([]byte("z"), 7)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Put(uuid.New(), bytes.Repeat([]byte("z"), 3)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if b, n := c.Stats(); b != 10 || n != 2 {
		t.Fatalf("Stats: got %d/%d, want 10/2", b, n)
	}
}

func TestRebuildIndexFromExistingDir(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	id1, id2 := uuid.New(), uuid.New()
	data1 := bytes.Repeat([]byte("1"), 12)
	data2 := bytes.Repeat([]byte("2"), 8)

	// Seed the directory directly, plus a stray non-managed file.
	if err := os.WriteFile(filepath.Join(dir, id1.String()+blobExt), data1, 0o644); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id2.String()+blobExt), data2, 0o644); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-a-blob.txt"), []byte("ignore"), 0o644); err != nil {
		t.Fatalf("seed stray: %v", err)
	}

	c, err := New(dir, 0, 0, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	b, n := c.Stats()
	if n != 2 || b != 20 {
		t.Fatalf("rebuilt Stats: got %d/%d, want 20/2", b, n)
	}
	if got, ok := c.Get(id1); !ok || !bytes.Equal(got, data1) {
		t.Fatalf("rebuilt Get id1: ok=%v match=%v", ok, bytes.Equal(got, data1))
	}
	if got, ok := c.Get(id2); !ok || !bytes.Equal(got, data2) {
		t.Fatalf("rebuilt Get id2: ok=%v match=%v", ok, bytes.Equal(got, data2))
	}
}

func TestRebuildEvictsOverBudget(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	older, newer := uuid.New(), uuid.New()
	blob := bytes.Repeat([]byte("q"), 10)
	if err := os.WriteFile(filepath.Join(dir, older.String()+blobExt), blob, 0o644); err != nil {
		t.Fatalf("seed older: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, newer.String()+blobExt), blob, 0o644); err != nil {
		t.Fatalf("seed newer: %v", err)
	}
	// Make `older` genuinely older by mod time.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, older.String()+blobExt), past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// maxBytes fits only one of the two 10-byte blobs.
	c, err := New(dir, 10, 0, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, n := c.Stats()
	if n != 1 || b != 10 {
		t.Fatalf("rebuilt over-budget Stats: got %d/%d, want 10/1", b, n)
	}
	if _, ok := c.Get(older); ok {
		t.Fatal("older entry should have been evicted on rebuild")
	}
	if _, ok := c.Get(newer); !ok {
		t.Fatal("newer entry should survive rebuild eviction")
	}
}

func TestNewCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cache")
	c, err := New(dir, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir should be created: %v", err)
	}
	if b, n := c.Stats(); b != 0 || n != 0 {
		t.Fatalf("fresh cache Stats: got %d/%d, want 0/0", b, n)
	}
}
