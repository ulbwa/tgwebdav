// Package cache implements a disk-backed LRU over whole blobs, keyed by blob
// UUID. It satisfies domain.BlobCache: each blob is stored as a single file
// <dir>/<uuid>.blob and an in-memory index tracks size, last-access time and
// LRU ordering. The total on-disk size is bounded by maxBytes (least-recently
// used entries are evicted on Put), and a background janitor drops entries that
// have not been touched within idleTTL.
package cache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// blobExt is the on-disk filename suffix for cached blobs.
const blobExt = ".blob"

// entry is the in-memory bookkeeping for one cached blob. It is referenced from
// both the index map (by id) and the LRU list (front = most recently used).
type entry struct {
	id         uuid.UUID
	size       int64
	lastAccess time.Time
	elem       *list.Element // position in Cache.lru; value is *entry
}

// Cache is a disk-backed, size-bounded LRU cache of whole blobs. It implements
// domain.BlobCache and is safe for concurrent use.
type Cache struct {
	dir      string
	maxBytes int64
	idleTTL  time.Duration
	logger   *slog.Logger

	mu    sync.Mutex
	index map[uuid.UUID]*entry
	lru   *list.List // front = most recently used; values are *entry
	total int64

	// now is the clock used for last-access stamping and idle eviction. It is a
	// field so tests can inject a deterministic time source.
	now func() time.Time
	// tick is the janitor sweep interval. Exposed as a field so tests can drive
	// the janitor faster than the production default.
	tick time.Duration
}

// New creates a Cache rooted at dir, bounded to maxBytes of on-disk content,
// evicting entries idle for longer than idleTTL. The directory is created if
// missing and the in-memory index is rebuilt from any pre-existing *.blob files
// (size from os.Stat, last-access from the file's modification time). If the
// rebuilt set already exceeds maxBytes, the least-recently used files are
// evicted immediately so the cache opens within its bound.
func New(dir string, maxBytes int64, idleTTL time.Duration, logger *slog.Logger) (*Cache, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache: create dir %q: %w", dir, err)
	}
	c := &Cache{
		dir:      dir,
		maxBytes: maxBytes,
		idleTTL:  idleTTL,
		logger:   logger,
		index:    make(map[uuid.UUID]*entry),
		lru:      list.New(),
		now:      time.Now,
		tick:     time.Minute,
	}
	if err := c.rebuild(); err != nil {
		return nil, err
	}
	return c, nil
}

// rebuild scans the cache directory and reconstructs the in-memory index from
// existing *.blob files, ordering the LRU by modification time (oldest at the
// back) and evicting down to maxBytes if necessary.
func (c *Cache) rebuild() error {
	dirents, err := os.ReadDir(c.dir)
	if err != nil {
		return fmt.Errorf("cache: read dir %q: %w", c.dir, err)
	}

	type scanned struct {
		id   uuid.UUID
		size int64
		mod  time.Time
	}
	var found []scanned
	for _, de := range dirents {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, blobExt) {
			continue
		}
		id, err := uuid.Parse(strings.TrimSuffix(name, blobExt))
		if err != nil {
			// Not a managed file; leave it untouched.
			continue
		}
		fi, err := de.Info()
		if err != nil {
			c.logger.Warn("cache: stat during rebuild failed", "file", name, "err", err)
			continue
		}
		found = append(found, scanned{id: id, size: fi.Size(), mod: fi.ModTime()})
	}

	// Sort oldest-first so the most recently modified ends up at the LRU front.
	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && found[j-1].mod.After(found[j].mod); j-- {
			found[j-1], found[j] = found[j], found[j-1]
		}
	}

	for _, s := range found {
		e := &entry{id: s.id, size: s.size, lastAccess: s.mod}
		e.elem = c.lru.PushFront(e)
		c.index[s.id] = e
		c.total += s.size
	}

	c.evictLocked()
	return nil
}

// Get returns the cached bytes for id and reports whether it was present. On a
// hit the entry is promoted to most-recently-used and its access time updated.
func (c *Cache) Get(id uuid.UUID) ([]byte, bool) {
	c.mu.Lock()
	e, ok := c.index[id]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	path := c.pathFor(id)
	c.mu.Unlock()

	// Read outside the lock to avoid holding it across disk I/O.
	data, err := os.ReadFile(path)
	if err != nil {
		// The file vanished underneath us; treat as a miss and drop the index
		// entry so it can be repopulated.
		c.logger.Warn("cache: read failed, dropping entry", "id", id, "err", err)
		c.mu.Lock()
		if cur, ok := c.index[id]; ok && cur == e {
			c.removeLocked(e)
		}
		c.mu.Unlock()
		return nil, false
	}

	c.mu.Lock()
	if cur, ok := c.index[id]; ok && cur == e {
		e.lastAccess = c.now()
		c.lru.MoveToFront(e.elem)
	}
	c.mu.Unlock()
	return data, true
}

// Put writes data under id, atomically replacing any previous content, then
// evicts least-recently-used entries until the total on-disk size is within
// maxBytes. The just-written entry is most-recently-used and is never the first
// to be evicted (unless it alone exceeds maxBytes).
func (c *Cache) Put(id uuid.UUID, data []byte) error {
	path := c.pathFor(id)
	tmp, err := os.CreateTemp(c.dir, "tmp-*"+blobExt)
	if err != nil {
		return fmt.Errorf("cache: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("cache: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("cache: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("cache: rename temp into place: %w", err)
	}

	size := int64(len(data))
	c.mu.Lock()
	if e, ok := c.index[id]; ok {
		// Replace existing entry, adjusting the running total by the delta.
		c.total += size - e.size
		e.size = size
		e.lastAccess = c.now()
		c.lru.MoveToFront(e.elem)
	} else {
		e := &entry{id: id, size: size, lastAccess: c.now()}
		e.elem = c.lru.PushFront(e)
		c.index[id] = e
		c.total += size
	}
	c.evictLocked()
	c.mu.Unlock()
	return nil
}

// Remove deletes id from the cache (both the file and the index entry). It is a
// no-op if id is not present.
func (c *Cache) Remove(id uuid.UUID) {
	c.mu.Lock()
	e, ok := c.index[id]
	if !ok {
		c.mu.Unlock()
		return
	}
	c.removeLocked(e)
	c.mu.Unlock()
}

// Stats reports the current total on-disk size in bytes and the number of
// cached entries.
func (c *Cache) Stats() (bytes int64, entries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total, len(c.index)
}

// Has reports whether a blob is cached, without reading its bytes from disk.
func (c *Cache) Has(id uuid.UUID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.index[id]
	return ok
}

// Capacity returns the maximum cache size in bytes.
func (c *Cache) Capacity() int64 { return c.maxBytes }

// Start runs the idle janitor until ctx is cancelled. Every c.tick it removes
// every entry whose last access is older than idleTTL. If idleTTL is
// non-positive, idle eviction is disabled and Start simply waits for ctx.
func (c *Cache) Start(ctx context.Context) {
	if c.idleTTL <= 0 {
		<-ctx.Done()
		return
	}
	t := time.NewTicker(c.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweepIdle()
		}
	}
}

// sweepIdle removes every entry untouched for longer than idleTTL. It is
// exported to tests via the package-internal seam (called directly) so the
// janitor logic can be exercised without waiting on the ticker.
func (c *Cache) sweepIdle() {
	cutoff := c.now().Add(-c.idleTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Walk from the back (least recently used); once we reach an entry newer
	// than the cutoff, everything ahead of it is newer too, so we can stop.
	for el := c.lru.Back(); el != nil; {
		e := el.Value.(*entry)
		prev := el.Prev()
		if e.lastAccess.Before(cutoff) {
			c.removeLocked(e)
		} else {
			break
		}
		el = prev
	}
}

// evictLocked removes least-recently-used entries until total <= maxBytes. A
// non-positive maxBytes means unbounded and disables size eviction. Callers
// must hold c.mu.
func (c *Cache) evictLocked() {
	if c.maxBytes <= 0 {
		return
	}
	for c.total > c.maxBytes {
		el := c.lru.Back()
		if el == nil {
			return
		}
		c.removeLocked(el.Value.(*entry))
	}
}

// removeLocked deletes e's file and drops it from the index and LRU list.
// Callers must hold c.mu.
func (c *Cache) removeLocked(e *entry) {
	if err := os.Remove(c.pathFor(e.id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.logger.Warn("cache: remove file failed", "id", e.id, "err", err)
	}
	c.lru.Remove(e.elem)
	delete(c.index, e.id)
	c.total -= e.size
	if c.total < 0 {
		c.total = 0
	}
}

// pathFor returns the on-disk path for a blob id.
func (c *Cache) pathFor(id uuid.UUID) string {
	return filepath.Join(c.dir, id.String()+blobExt)
}
