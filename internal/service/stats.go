package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// statWriter is the narrow repository interface StatRecorder needs. Besides
// recording samples (the flush loop) it also reads them back for the Management
// API's stats endpoints (Query/Latest). The real *repository.StatRepository
// satisfies this structurally.
type statWriter interface {
	Record(ctx context.Context, metric, label string, value float64) error
	Query(ctx context.Context, metric, label string, from, to time.Time) ([]model.StatSample, error)
	Latest(ctx context.Context, metric, label string) (*model.StatSample, error)
}

// gauge pairs a metric/label with the function sampled on each flush.
type gauge struct {
	metric string
	label  string
	fn     func() float64
}

// StatRecorder implements in-memory counters and periodic flush to a stat
// repository. It is safe for concurrent use: the counter methods only touch
// atomic fields, and the gauge registry is guarded by a mutex. A single
// Start goroutine owns the flush loop.
type StatRecorder struct {
	store         statWriter
	flushInterval time.Duration
	logger        *slog.Logger

	readBytes   atomic.Int64
	writeBytes  atomic.Int64
	readOps     atomic.Int64
	writeOps    atomic.Int64
	cacheHit    atomic.Int64
	cacheMiss   atomic.Int64
	telegramReq atomic.Int64

	mu     sync.Mutex
	gauges []gauge
}

// NewStatRecorder returns a StatRecorder that flushes accumulated counters and
// sampled gauges to store every flushInterval. The returned recorder does
// nothing until Start is called. logger may be nil, in which case
// slog.Default is used.
func NewStatRecorder(store statWriter, flushInterval time.Duration, logger *slog.Logger) *StatRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &StatRecorder{
		store:         store,
		flushInterval: flushInterval,
		logger:        logger,
	}
}

// AddReadBytes adds n to the read_bytes counter.
func (r *StatRecorder) AddReadBytes(n int64) { r.readBytes.Add(n) }

// AddWriteBytes adds n to the write_bytes counter.
func (r *StatRecorder) AddWriteBytes(n int64) { r.writeBytes.Add(n) }

// IncReadOps increments the read_ops counter.
func (r *StatRecorder) IncReadOps() { r.readOps.Add(1) }

// IncWriteOps increments the write_ops counter.
func (r *StatRecorder) IncWriteOps() { r.writeOps.Add(1) }

// IncCacheHit increments the cache_hit counter.
func (r *StatRecorder) IncCacheHit() { r.cacheHit.Add(1) }

// IncCacheMiss increments the cache_miss counter.
func (r *StatRecorder) IncCacheMiss() { r.cacheMiss.Add(1) }

// IncTelegramReq increments the telegram_req counter.
func (r *StatRecorder) IncTelegramReq() { r.telegramReq.Add(1) }

// RegisterGauge registers a gauge sampled on every flush by calling fn and
// recording (metric, label, fn()). It is safe to call concurrently and at
// any time, including after Start.
func (r *StatRecorder) RegisterGauge(metric, label string, fn func() float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges = append(r.gauges, gauge{metric: metric, label: label, fn: fn})
}

// Start runs the flush loop until ctx is done, flushing every flushInterval.
// A final flush is performed on exit so the last partial interval is not
// lost. Start blocks; run it in its own goroutine.
func (r *StatRecorder) Start(ctx context.Context) {
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Use a detached context for the final flush so an already-cancelled
			// ctx does not abort persisting the last interval's samples.
			r.flush(context.WithoutCancel(ctx))
			return
		case <-ticker.C:
			r.flush(ctx)
		}
	}
}

// flush atomically drains each counter to zero, recording any non-zero delta
// as a sample, then samples every registered gauge. Recording errors are
// logged and otherwise ignored so a transient repository failure does not
// stall the loop.
func (r *StatRecorder) flush(ctx context.Context) {
	counters := []struct {
		metric string
		v      *atomic.Int64
	}{
		{model.MetricReadBytes, &r.readBytes},
		{model.MetricWriteBytes, &r.writeBytes},
		{model.MetricReadOps, &r.readOps},
		{model.MetricWriteOps, &r.writeOps},
		{model.MetricCacheHit, &r.cacheHit},
		{model.MetricCacheMiss, &r.cacheMiss},
		{model.MetricTelegramReq, &r.telegramReq},
	}

	for _, c := range counters {
		delta := c.v.Swap(0)
		if delta == 0 {
			continue
		}
		if err := r.store.Record(ctx, c.metric, "", float64(delta)); err != nil {
			r.logger.WarnContext(ctx, "stats: record counter failed",
				"metric", c.metric, "error", err)
		}
	}

	// Snapshot the gauge list under the lock, then sample outside it so a slow
	// fn does not block RegisterGauge or the counter methods.
	r.mu.Lock()
	gauges := make([]gauge, len(r.gauges))
	copy(gauges, r.gauges)
	r.mu.Unlock()

	for _, g := range gauges {
		value := g.fn()
		if err := r.store.Record(ctx, g.metric, g.label, value); err != nil {
			r.logger.WarnContext(ctx, "stats: record gauge failed",
				"metric", g.metric, "label", g.label, "error", err)
		}
	}
}

// Query returns the persisted samples for metric/label within the closed
// [from, to] window, oldest-first, by delegating to the stat repository.
func (r *StatRecorder) Query(ctx context.Context, metric, label string, from, to time.Time) ([]model.StatSample, error) {
	samples, err := r.store.Query(ctx, metric, label, from, to)
	if err != nil {
		return nil, fmt.Errorf("query stats %q/%q: %w", metric, label, err)
	}
	return samples, nil
}

// Latest returns the most recent persisted sample for metric/label, or
// repository.ErrNotFound when none exists, by delegating to the stat repository.
func (r *StatRecorder) Latest(ctx context.Context, metric, label string) (*model.StatSample, error) {
	sample, err := r.store.Latest(ctx, metric, label)
	if err != nil {
		return nil, fmt.Errorf("latest stat %q/%q: %w", metric, label, err)
	}
	return sample, nil
}
