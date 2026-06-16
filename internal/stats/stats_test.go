package stats

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// recordCall captures one Record invocation.
type recordCall struct {
	metric string
	label  string
	value  float64
}

// fakeStatRepo is an in-memory domain.StatRepository that records every Record
// call. Query and Latest are unused by these tests.
type fakeStatRepo struct {
	mu    sync.Mutex
	calls []recordCall
}

func (f *fakeStatRepo) Record(_ context.Context, metric, label string, value float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordCall{metric: metric, label: label, value: value})
	return nil
}

func (f *fakeStatRepo) Query(_ context.Context, _, _ string, _, _ time.Time) ([]domain.StatSample, error) {
	return nil, nil
}

func (f *fakeStatRepo) Latest(_ context.Context, _, _ string) (*domain.StatSample, error) {
	return nil, nil
}

// snapshot returns a copy of recorded calls.
func (f *fakeStatRepo) snapshot() []recordCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// find returns the single call for metric/label, requiring exactly one match.
func find(t *testing.T, calls []recordCall, metric, label string) recordCall {
	t.Helper()
	var matches []recordCall
	for _, c := range calls {
		if c.metric == metric && c.label == label {
			matches = append(matches, c)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one call for metric=%q label=%q, got %d (%+v)",
			metric, label, len(matches), calls)
	}
	return matches[0]
}

// countFor returns how many calls matched metric/label.
func countFor(calls []recordCall, metric, label string) int {
	n := 0
	for _, c := range calls {
		if c.metric == metric && c.label == label {
			n++
		}
	}
	return n
}

func TestCountersAccumulateThenFlushRecordsDeltaAndResets(t *testing.T) {
	repo := &fakeStatRepo{}
	r := NewRecorder(repo, time.Hour, nil)

	// Accumulate across several calls per counter.
	r.AddReadBytes(100)
	r.AddReadBytes(50)
	r.AddWriteBytes(200)
	r.IncReadOps()
	r.IncReadOps()
	r.IncReadOps()
	r.IncWriteOps()
	r.IncCacheHit()
	r.IncCacheHit()
	r.IncCacheMiss()
	r.IncTelegramReq()

	r.flush(context.Background())

	calls := repo.snapshot()

	want := map[string]float64{
		domain.MetricReadBytes:   150,
		domain.MetricWriteBytes:  200,
		domain.MetricReadOps:     3,
		domain.MetricWriteOps:    1,
		domain.MetricCacheHit:    2,
		domain.MetricCacheMiss:   1,
		domain.MetricTelegramReq: 1,
	}
	for metric, value := range want {
		got := find(t, calls, metric, "")
		if got.value != value {
			t.Errorf("metric %q: got value %v, want %v", metric, got.value, value)
		}
		if got.label != "" {
			t.Errorf("metric %q: got label %q, want empty", metric, got.label)
		}
	}
	if len(calls) != len(want) {
		t.Fatalf("expected %d recorded calls, got %d (%+v)", len(want), len(calls), calls)
	}

	// A second flush with no further activity must record nothing: counters were
	// reset to zero by the first flush and zero deltas are skipped.
	r.flush(context.Background())
	if got := len(repo.snapshot()); got != len(want) {
		t.Fatalf("second flush should record nothing; total calls = %d, want %d", got, len(want))
	}

	// New activity after a flush is recorded as a fresh delta, not a running total.
	r.AddReadBytes(7)
	r.flush(context.Background())
	calls = repo.snapshot()
	// Two read_bytes calls now exist (150 then 7); assert the most recent value.
	last := calls[len(calls)-1]
	if last.metric != domain.MetricReadBytes || last.value != 7 {
		t.Fatalf("expected last call to be read_bytes=7, got %+v", last)
	}
	if countFor(calls, domain.MetricReadBytes, "") != 2 {
		t.Fatalf("expected 2 read_bytes calls total, got %d", countFor(calls, domain.MetricReadBytes, ""))
	}
}

func TestZeroCountersAreNotRecorded(t *testing.T) {
	repo := &fakeStatRepo{}
	r := NewRecorder(repo, time.Hour, nil)

	// No counter activity at all.
	r.flush(context.Background())

	if calls := repo.snapshot(); len(calls) != 0 {
		t.Fatalf("flush with no activity should record nothing, got %+v", calls)
	}
}

func TestGaugeSampledOnFlush(t *testing.T) {
	repo := &fakeStatRepo{}
	r := NewRecorder(repo, time.Hour, nil)

	var current float64 = 1024
	var samples int
	r.RegisterGauge(domain.MetricStorageBytes, "primary", func() float64 {
		samples++
		return current
	})

	r.flush(context.Background())
	if samples != 1 {
		t.Fatalf("expected gauge fn sampled once, got %d", samples)
	}
	got := find(t, repo.snapshot(), domain.MetricStorageBytes, "primary")
	if got.value != 1024 {
		t.Errorf("gauge value: got %v, want 1024", got.value)
	}

	// The gauge is resampled on each flush, reflecting the current value, and is
	// recorded even when the underlying value is unchanged or zero.
	current = 2048
	r.flush(context.Background())
	if samples != 2 {
		t.Fatalf("expected gauge fn sampled twice, got %d", samples)
	}
	calls := repo.snapshot()
	if countFor(calls, domain.MetricStorageBytes, "primary") != 2 {
		t.Fatalf("expected 2 gauge calls, got %d", countFor(calls, domain.MetricStorageBytes, "primary"))
	}
	last := calls[len(calls)-1]
	if last.value != 2048 {
		t.Errorf("second gauge sample: got %v, want 2048", last.value)
	}
}

func TestStartFlushesPeriodicallyAndOnExit(t *testing.T) {
	repo := &fakeStatRepo{}
	r := NewRecorder(repo, 10*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Start(ctx)
		close(done)
	}()

	// Record activity and wait for at least one periodic flush to persist it.
	r.IncReadOps()
	deadline := time.After(2 * time.Second)
	for {
		if countFor(repo.snapshot(), domain.MetricReadOps, "") >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for a periodic flush")
		case <-time.After(2 * time.Millisecond):
		}
	}

	// Activity recorded just before cancel must survive thanks to the final flush.
	r.AddWriteBytes(42)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}

	got := find(t, repo.snapshot(), domain.MetricWriteBytes, "")
	if got.value != 42 {
		t.Errorf("final-flush write_bytes: got %v, want 42", got.value)
	}
}

// compile-time assertion that *Recorder satisfies the domain contract.
var _ domain.StatRecorder = (*Recorder)(nil)
