package limits

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAllowUnlimited verifies that a ratePerMin of 0 (or negative) never blocks.
func TestAllowUnlimited(t *testing.T) {
	l := New()
	uid := uuid.New()

	for i := 0; i < 1000; i++ {
		if !l.Allow(uid, 0) {
			t.Fatalf("Allow returned false for unlimited rate on call %d", i)
		}
	}
	if !l.Allow(uid, -5) {
		t.Fatal("Allow returned false for negative (unlimited) rate")
	}
	// No bucket should have been created for an unlimited user.
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected no buckets for unlimited rate, got %d", n)
	}
}

// TestAllowBlocksAfterBurst verifies that once the burst is exhausted, Allow
// returns false (the bucket refills too slowly to replenish within the test).
func TestAllowBlocksAfterBurst(t *testing.T) {
	l := New()
	uid := uuid.New()

	const ratePerMin = 6 // burst = max(1, 6) = 6, refill = 0.1 tokens/sec
	allowed := 0
	for i := 0; i < ratePerMin; i++ {
		if l.Allow(uid, ratePerMin) {
			allowed++
		}
	}
	if allowed != ratePerMin {
		t.Fatalf("expected first %d calls to be allowed, got %d", ratePerMin, allowed)
	}

	// The next call should be rejected: burst is spent and refill is ~0.1/sec.
	if l.Allow(uid, ratePerMin) {
		t.Fatal("expected Allow to block after burst exhausted")
	}
}

// TestAllowBurstOne verifies that a rate of 1/min yields a burst of exactly one
// token so the second immediate request is rejected.
func TestAllowBurstOne(t *testing.T) {
	l := New()
	uid := uuid.New()

	if !l.Allow(uid, 1) {
		t.Fatal("first request with rate 1/min should be allowed")
	}
	if l.Allow(uid, 1) {
		t.Fatal("second immediate request with rate 1/min should be blocked")
	}
}

// TestAllowPerUserIsolation verifies buckets are independent across users.
func TestAllowPerUserIsolation(t *testing.T) {
	l := New()
	a, b := uuid.New(), uuid.New()

	if !l.Allow(a, 1) {
		t.Fatal("user a first request should be allowed")
	}
	// b has its own fresh bucket and must not be affected by a's consumption.
	if !l.Allow(b, 1) {
		t.Fatal("user b first request should be allowed independently of a")
	}
}

// TestSweepEvictsIdleBuckets verifies the janitor removes buckets that have not
// been used within bucketIdleTTL while keeping recently used ones.
func TestSweepEvictsIdleBuckets(t *testing.T) {
	l := New()
	base := time.Now()
	l.now = func() time.Time { return base }

	idle := uuid.New()
	fresh := uuid.New()

	l.Allow(idle, 10) // recorded at base
	l.now = func() time.Time { return base.Add(bucketIdleTTL + sweepInterval) }
	l.Allow(fresh, 10) // recorded at base + TTL + interval

	l.sweep()

	l.mu.Lock()
	_, idleStillThere := l.buckets[idle]
	_, freshStillThere := l.buckets[fresh]
	l.mu.Unlock()

	if idleStillThere {
		t.Fatal("expected idle bucket to be evicted")
	}
	if !freshStillThere {
		t.Fatal("expected fresh bucket to survive sweep")
	}
}

// TestStartStopsOnContextCancel verifies Start returns promptly when ctx ends.
func TestStartStopsOnContextCancel(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Start(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

// TestThrottledReaderUnlimited verifies bps<=0 returns the reader unchanged.
func TestThrottledReaderUnlimited(t *testing.T) {
	l := New()
	src := bytes.NewReader([]byte("hello"))
	if got := l.ThrottledReader(src, 0); got != io.Reader(src) {
		t.Fatal("expected ThrottledReader to return the original reader when bps<=0")
	}
	if got := l.ThrottledReader(src, -100); got != io.Reader(src) {
		t.Fatal("expected ThrottledReader to return the original reader when bps<0")
	}
}

// TestThrottledReaderReturnsAllBytes verifies all bytes are delivered intact and
// that throughput is paced to roughly the configured rate.
func TestThrottledReaderReturnsAllBytes(t *testing.T) {
	l := New()

	payload := bytes.Repeat([]byte("abcdefghij"), 20) // 200 bytes
	const bps = 200                                   // ~1 second to drain after the initial burst

	src := bytes.NewReader(payload)
	tr := l.ThrottledReader(src, bps)

	start := time.Now()
	got, err := io.ReadAll(tr)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	// With burst == bps == 200 and a 200-byte payload, the initial burst covers
	// the whole payload, so it can complete near-instantly; just assert it did
	// not take absurdly long (generous upper bound, no flaky lower bound).
	if elapsed > 3*time.Second {
		t.Fatalf("throttled read took too long: %s", elapsed)
	}
}

// TestThrottledReaderPacesBeyondBurst verifies that reading more than one burst
// of data takes at least roughly the expected time, with generous slack.
func TestThrottledReaderPacesBeyondBurst(t *testing.T) {
	l := New()

	// 400 bytes at 200 bps with burst 200: first 200 are instant (burst), the
	// remaining 200 must wait ~1s for refill. Lower bound checked with slack.
	payload := bytes.Repeat([]byte("x"), 400)
	const bps = 200

	src := bytes.NewReader(payload)
	tr := l.ThrottledReader(src, bps)

	start := time.Now()
	got, err := io.ReadAll(tr)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	// Expect at least ~0.5s of pacing (true value ~1s); generous slack avoids
	// flakiness on slow CI. Upper bound guards against runaway delays.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("expected pacing to take at least 500ms, took %s", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("throttled read took too long: %s", elapsed)
	}
}
