package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// recordCall captures one Record invocation.
type recordCall struct {
	metric string
	label  string
	value  float64
}

// fakeStatWriter is an in-memory statWriter that records every Record call.
type fakeStatWriter struct {
	mu    sync.Mutex
	calls []recordCall
}

func (f *fakeStatWriter) Record(_ context.Context, metric, label string, value float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordCall{metric: metric, label: label, value: value})
	return nil
}

var _ statWriter = (*fakeStatWriter)(nil)

// snapshot returns a copy of recorded calls.
func (f *fakeStatWriter) snapshot() []recordCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// findCall returns the single call for metric/label, requiring exactly one match.
func findCall(t *testing.T, calls []recordCall, metric, label string) recordCall {
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

// countCalls returns how many calls matched metric/label.
func countCalls(calls []recordCall, metric, label string) int {
	n := 0
	for _, c := range calls {
		if c.metric == metric && c.label == label {
			n++
		}
	}
	return n
}

func TestStatsCountersAccumulateThenFlushRecordsDeltaAndResets(t *testing.T) {
	store := &fakeStatWriter{}
	r := NewStatRecorder(store, time.Hour, nil)

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

	calls := store.snapshot()

	want := map[string]float64{
		model.MetricReadBytes:   150,
		model.MetricWriteBytes:  200,
		model.MetricReadOps:     3,
		model.MetricWriteOps:    1,
		model.MetricCacheHit:    2,
		model.MetricCacheMiss:   1,
		model.MetricTelegramReq: 1,
	}
	for metric, value := range want {
		got := findCall(t, calls, metric, "")
		if got.value != value {
			t.Errorf("metric %q: got value %v, want %v", metric, got.value, value)
		}
	}
	if len(calls) != len(want) {
		t.Fatalf("expected %d recorded calls, got %d (%+v)", len(want), len(calls), calls)
	}

	// Second flush with no activity must record nothing.
	r.flush(context.Background())
	if got := len(store.snapshot()); got != len(want) {
		t.Fatalf("second flush should record nothing; total calls = %d, want %d", got, len(want))
	}

	// New activity is recorded as a fresh delta, not a running total.
	r.AddReadBytes(7)
	r.flush(context.Background())
	calls = store.snapshot()
	last := calls[len(calls)-1]
	if last.metric != model.MetricReadBytes || last.value != 7 {
		t.Fatalf("expected last call to be read_bytes=7, got %+v", last)
	}
	if countCalls(calls, model.MetricReadBytes, "") != 2 {
		t.Fatalf("expected 2 read_bytes calls total, got %d", countCalls(calls, model.MetricReadBytes, ""))
	}
}

func TestStatsZeroCountersAreNotRecorded(t *testing.T) {
	store := &fakeStatWriter{}
	r := NewStatRecorder(store, time.Hour, nil)

	r.flush(context.Background())

	if calls := store.snapshot(); len(calls) != 0 {
		t.Fatalf("flush with no activity should record nothing, got %+v", calls)
	}
}

func TestStatsGaugeSampledOnFlush(t *testing.T) {
	store := &fakeStatWriter{}
	r := NewStatRecorder(store, time.Hour, nil)

	var current float64 = 1024
	var samples int
	r.RegisterGauge(model.MetricStorageBytes, "primary", func() float64 {
		samples++
		return current
	})

	r.flush(context.Background())
	if samples != 1 {
		t.Fatalf("expected gauge fn sampled once, got %d", samples)
	}
	got := findCall(t, store.snapshot(), model.MetricStorageBytes, "primary")
	if got.value != 1024 {
		t.Errorf("gauge value: got %v, want 1024", got.value)
	}

	current = 2048
	r.flush(context.Background())
	if samples != 2 {
		t.Fatalf("expected gauge fn sampled twice, got %d", samples)
	}
	calls := store.snapshot()
	if countCalls(calls, model.MetricStorageBytes, "primary") != 2 {
		t.Fatalf("expected 2 gauge calls, got %d", countCalls(calls, model.MetricStorageBytes, "primary"))
	}
	last := calls[len(calls)-1]
	if last.value != 2048 {
		t.Errorf("second gauge sample: got %v, want 2048", last.value)
	}
}

func TestStatsStartFlushesPeriodicallyAndOnExit(t *testing.T) {
	store := &fakeStatWriter{}
	r := NewStatRecorder(store, 10*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Start(ctx)
		close(done)
	}()

	r.IncReadOps()
	deadline := time.After(2 * time.Second)
	for {
		if countCalls(store.snapshot(), model.MetricReadOps, "") >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for a periodic flush")
		case <-time.After(2 * time.Millisecond):
		}
	}

	r.AddWriteBytes(42)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}

	got := findCall(t, store.snapshot(), model.MetricWriteBytes, "")
	if got.value != 42 {
		t.Errorf("final-flush write_bytes: got %v, want 42", got.value)
	}
}
