// Package limits implements domain.Limiter: per-user request rate limiting and
// per-read bandwidth throttling, both built on golang.org/x/time/rate. The
// request limiter keeps one token bucket per user in a mutex-guarded map and
// evicts buckets that have been idle for a while; the bandwidth limiter wraps an
// io.Reader and paces reads through a rate.Limiter.
package limits

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

const (
	// bucketIdleTTL is how long a per-user bucket may sit unused before the
	// janitor evicts it. A user whose bucket is evicted simply gets a fresh one
	// on their next request, which is harmless for rate limiting.
	bucketIdleTTL = 10 * time.Minute
	// sweepInterval is how often the idle-bucket janitor runs.
	sweepInterval = time.Minute
)

// bucket is a per-user token bucket plus the wall-clock time it was last used,
// so the janitor can evict idle entries.
type bucket struct {
	limiter    *rate.Limiter
	ratePerMin int
	lastSeen   time.Time
}

// Limiter enforces per-user request rate and per-read bandwidth limits. It is
// safe for concurrent use. The zero value is not usable; call New.
type Limiter struct {
	mu      sync.Mutex
	buckets map[uuid.UUID]*bucket
	now     func() time.Time // injectable clock for tests
}

// New returns a ready-to-use Limiter. Call Start to run the idle-bucket janitor.
func New() *Limiter {
	return &Limiter{
		buckets: make(map[uuid.UUID]*bucket),
		now:     time.Now,
	}
}

// Start runs the idle-bucket janitor until ctx is cancelled. Buckets untouched
// for longer than bucketIdleTTL are removed so the map does not grow without
// bound. It blocks, so callers typically run it in its own goroutine.
func (l *Limiter) Start(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.sweep()
		}
	}
}

// sweep evicts buckets that have been idle for longer than bucketIdleTTL.
func (l *Limiter) sweep() {
	cutoff := l.now().Add(-bucketIdleTTL)
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, id)
		}
	}
}

// Allow consumes one token from the user's per-minute bucket. A ratePerMin of
// zero (or negative) means unlimited and always returns true. Otherwise the
// per-user bucket refills at ratePerMin/60 tokens per second with a burst of
// max(1, ratePerMin); Allow reports false when the bucket is empty.
func (l *Limiter) Allow(userID uuid.UUID, ratePerMin int) bool {
	if ratePerMin <= 0 {
		return true
	}

	l.mu.Lock()
	b, ok := l.buckets[userID]
	if !ok || b.ratePerMin != ratePerMin {
		// Create (or recreate, if the user's configured rate changed) the bucket.
		b = &bucket{
			limiter:    rate.NewLimiter(rate.Limit(float64(ratePerMin)/60.0), maxInt(1, ratePerMin)),
			ratePerMin: ratePerMin,
		}
		l.buckets[userID] = b
	}
	b.lastSeen = l.now()
	limiter := b.limiter
	l.mu.Unlock()

	return limiter.Allow()
}

// ThrottledReader wraps r so reads are paced to at most bps bytes per second. A
// bps of zero (or negative) means unlimited and r is returned unchanged.
func (l *Limiter) ThrottledReader(r io.Reader, bps int64) io.Reader {
	if bps <= 0 {
		return r
	}
	// burst must be at least 1 and is also the largest chunk we may request from
	// the underlying limiter in a single WaitN call, so cap reads to it.
	burst := int(bps)
	if burst < 1 {
		burst = 1
	}
	return &throttledReader{
		r:       r,
		limiter: rate.NewLimiter(rate.Limit(float64(bps)), burst),
		burst:   burst,
		ctx:     context.Background(),
	}
}

// throttledReader paces reads from an underlying reader through a rate.Limiter.
type throttledReader struct {
	r       io.Reader
	limiter *rate.Limiter
	burst   int
	ctx     context.Context
}

// Read reads from the underlying reader in chunks no larger than the limiter's
// burst, blocking via WaitN so the average throughput stays at or below the
// configured bytes-per-second. It returns the bytes read from one underlying
// Read call, so callers should loop (io.Copy does) to drain the stream.
func (t *throttledReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Never ask the underlying reader for more than the limiter's burst, since
	// WaitN cannot wait for more tokens than the burst allows.
	if len(p) > t.burst {
		p = p[:t.burst]
	}

	n, err := t.r.Read(p)
	if n > 0 {
		// Pace based on the bytes actually produced. WaitN blocks until enough
		// tokens are available; n <= burst by construction so it cannot fail for
		// an exceeded-burst reason, and the background context never cancels.
		if waitErr := t.limiter.WaitN(t.ctx, n); waitErr != nil && err == nil {
			err = waitErr
		}
	}
	return n, err
}

// maxInt returns the larger of a and b.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
