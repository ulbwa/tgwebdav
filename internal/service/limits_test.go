package service

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLimiterAllowUnlimited(t *testing.T) {
	l := NewLimiter()
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

func TestLimiterAllowBlocksAfterBurst(t *testing.T) {
	l := NewLimiter()
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

func TestLimiterAllowBurstOne(t *testing.T) {
	l := NewLimiter()
	uid := uuid.New()

	if !l.Allow(uid, 1) {
		t.Fatal("first request with rate 1/min should be allowed")
	}
	if l.Allow(uid, 1) {
		t.Fatal("second immediate request with rate 1/min should be blocked")
	}
}

func TestLimiterAllowPerUserIsolation(t *testing.T) {
	l := NewLimiter()
	a, b := uuid.New(), uuid.New()

	if !l.Allow(a, 1) {
		t.Fatal("user a first request should be allowed")
	}
	if !l.Allow(b, 1) {
		t.Fatal("user b first request should be allowed independently of a")
	}
}

func TestLimiterSweepEvictsIdleBuckets(t *testing.T) {
	l := NewLimiter()
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

func TestLimiterStartStopsOnContextCancel(t *testing.T) {
	l := NewLimiter()
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

func TestLimiterThrottledReaderUnlimited(t *testing.T) {
	l := NewLimiter()
	src := bytes.NewReader([]byte("hello"))
	if got := l.ThrottledReader(src, 0); got != io.Reader(src) {
		t.Fatal("expected ThrottledReader to return the original reader when bps<=0")
	}
	if got := l.ThrottledReader(src, -100); got != io.Reader(src) {
		t.Fatal("expected ThrottledReader to return the original reader when bps<0")
	}
}

func TestLimiterThrottledReaderReturnsAllBytes(t *testing.T) {
	l := NewLimiter()

	payload := bytes.Repeat([]byte("abcdefghij"), 20) // 200 bytes
	const bps = 200                                   // burst == bps, so whole payload can be served in first burst

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
	if elapsed > 3*time.Second {
		t.Fatalf("throttled read took too long: %s", elapsed)
	}
}

func TestLimiterThrottledReaderPacesBeyondBurst(t *testing.T) {
	l := NewLimiter()

	// 400 bytes at 200 bps with burst 200: first 200 are instant (burst), the
	// remaining 200 must wait ~1s for refill.
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
	if elapsed < 500*time.Millisecond {
		t.Fatalf("expected pacing to take at least 500ms, took %s", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("throttled read took too long: %s", elapsed)
	}
}
