package service

import (
	"context"
	"errors"
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

// fakeStatWriter is an in-memory statWriter that records every Record call and
// can serve canned Query/Latest results for the read-path tests.
type fakeStatWriter struct {
	mu    sync.Mutex
	calls []recordCall

	// Read-path fixtures and capture.
	queryResult []model.StatSample
	queryErr    error
	queryCalls  []queryCall

	latestResult *model.StatSample
	latestErr    error
	latestCalls  []labelCall
}

type queryCall struct {
	metric string
	label  string
	from   time.Time
	to     time.Time
}

type labelCall struct {
	metric string
	label  string
}

func (f *fakeStatWriter) Record(_ context.Context, metric, label string, value float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordCall{metric: metric, label: label, value: value})
	return nil
}

func (f *fakeStatWriter) Query(_ context.Context, metric, label string, from, to time.Time) ([]model.StatSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryCalls = append(f.queryCalls, queryCall{metric: metric, label: label, from: from, to: to})
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.queryResult, nil
}

func (f *fakeStatWriter) Latest(_ context.Context, metric, label string) (*model.StatSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestCalls = append(f.latestCalls, labelCall{metric: metric, label: label})
	if f.latestErr != nil {
		return nil, f.latestErr
	}
	return f.latestResult, nil
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
	for countCalls(store.snapshot(), model.MetricReadOps, "") < 1 {
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

func TestStatsQueryDelegatesRangeToRepo(t *testing.T) {
	want := []model.StatSample{
		{Metric: model.MetricReadBytes, Label: "primary", Value: 10},
		{Metric: model.MetricReadBytes, Label: "primary", Value: 20},
	}
	store := &fakeStatWriter{queryResult: want}
	r := NewStatRecorder(store, time.Hour, nil)

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	got, err := r.Query(context.Background(), model.MetricReadBytes, "primary", from, to)
	if err != nil {
		t.Fatalf("Query: unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Query: got %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Query sample %d: got %+v, want %+v", i, got[i], want[i])
		}
	}

	if len(store.queryCalls) != 1 {
		t.Fatalf("expected exactly one Query delegation, got %d", len(store.queryCalls))
	}
	qc := store.queryCalls[0]
	if qc.metric != model.MetricReadBytes || qc.label != "primary" || !qc.from.Equal(from) || !qc.to.Equal(to) {
		t.Errorf("Query delegated wrong args: %+v", qc)
	}
}

func TestStatsQueryPropagatesError(t *testing.T) {
	store := &fakeStatWriter{queryErr: errors.New("boom")}
	r := NewStatRecorder(store, time.Hour, nil)

	_, err := r.Query(context.Background(), model.MetricReadBytes, "", time.Time{}, time.Now())
	if err == nil {
		t.Fatal("Query: expected error, got nil")
	}
}

func TestStatsLatestDelegatesToRepo(t *testing.T) {
	want := &model.StatSample{Metric: model.MetricStorageBytes, Label: "primary", Value: 4096}
	store := &fakeStatWriter{latestResult: want}
	r := NewStatRecorder(store, time.Hour, nil)

	got, err := r.Latest(context.Background(), model.MetricStorageBytes, "primary")
	if err != nil {
		t.Fatalf("Latest: unexpected error: %v", err)
	}
	if got == nil || *got != *want {
		t.Fatalf("Latest: got %+v, want %+v", got, want)
	}

	if len(store.latestCalls) != 1 {
		t.Fatalf("expected exactly one Latest delegation, got %d", len(store.latestCalls))
	}
	lc := store.latestCalls[0]
	if lc.metric != model.MetricStorageBytes || lc.label != "primary" {
		t.Errorf("Latest delegated wrong args: %+v", lc)
	}
}

func TestStatsLatestPropagatesNotFound(t *testing.T) {
	store := &fakeStatWriter{latestErr: model.ErrNotFound}
	r := NewStatRecorder(store, time.Hour, nil)

	_, err := r.Latest(context.Background(), model.MetricStorageBytes, "missing")
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Latest: expected ErrNotFound, got %v", err)
	}
}
