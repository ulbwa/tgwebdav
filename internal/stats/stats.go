// Package stats provides an in-memory StatRecorder that accumulates counters
// and periodically flushes them — together with registered gauges — to a
// domain.StatRepository as time-series samples.
//
// Counters (bytes read/written, ops, cache hit/miss, telegram requests) are
// updated on the hot path via atomic operations. A background flush loop swaps
// each counter to zero on a fixed interval and records the accumulated delta as
// a single sample, so the repository sees per-interval rates rather than
// monotonic totals. Gauges are point-in-time values sampled by calling their
// provided function on every flush.
package stats

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// gauge pairs a metric/label with the function sampled on each flush.
type gauge struct {
	metric string
	label  string
	fn     func() float64
}

// Recorder implements domain.StatRecorder. It is safe for concurrent use: the
// counter methods only touch atomic fields, and the gauge registry is guarded
// by a mutex. A single Start goroutine owns the flush loop.
type Recorder struct {
	repo          domain.StatRepository
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

// NewRecorder returns a Recorder that flushes accumulated counters and sampled
// gauges to repo every flushInterval. The returned Recorder does nothing until
// Start is called. logger may be nil, in which case slog.Default is used.
func NewRecorder(repo domain.StatRepository, flushInterval time.Duration, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{
		repo:          repo,
		flushInterval: flushInterval,
		logger:        logger,
	}
}

// AddReadBytes adds n to the read_bytes counter.
func (r *Recorder) AddReadBytes(n int64) { r.readBytes.Add(n) }

// AddWriteBytes adds n to the write_bytes counter.
func (r *Recorder) AddWriteBytes(n int64) { r.writeBytes.Add(n) }

// IncReadOps increments the read_ops counter.
func (r *Recorder) IncReadOps() { r.readOps.Add(1) }

// IncWriteOps increments the write_ops counter.
func (r *Recorder) IncWriteOps() { r.writeOps.Add(1) }

// IncCacheHit increments the cache_hit counter.
func (r *Recorder) IncCacheHit() { r.cacheHit.Add(1) }

// IncCacheMiss increments the cache_miss counter.
func (r *Recorder) IncCacheMiss() { r.cacheMiss.Add(1) }

// IncTelegramReq increments the telegram_req counter.
func (r *Recorder) IncTelegramReq() { r.telegramReq.Add(1) }

// RegisterGauge registers a gauge sampled on every flush by calling fn and
// recording (metric, label, fn()). It is safe to call concurrently and at any
// time, including after Start.
func (r *Recorder) RegisterGauge(metric, label string, fn func() float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges = append(r.gauges, gauge{metric: metric, label: label, fn: fn})
}

// Start runs the flush loop until ctx is done, flushing every flushInterval. A
// final flush is performed on exit so the last partial interval is not lost.
// Start blocks; run it in its own goroutine.
func (r *Recorder) Start(ctx context.Context) {
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

// flush atomically drains each counter to zero, recording any non-zero delta as
// a sample, then samples every registered gauge. Recording errors are logged
// and otherwise ignored so a transient repository failure does not stall the
// loop.
func (r *Recorder) flush(ctx context.Context) {
	counters := []struct {
		metric string
		v      *atomic.Int64
	}{
		{domain.MetricReadBytes, &r.readBytes},
		{domain.MetricWriteBytes, &r.writeBytes},
		{domain.MetricReadOps, &r.readOps},
		{domain.MetricWriteOps, &r.writeOps},
		{domain.MetricCacheHit, &r.cacheHit},
		{domain.MetricCacheMiss, &r.cacheMiss},
		{domain.MetricTelegramReq, &r.telegramReq},
	}

	for _, c := range counters {
		delta := c.v.Swap(0)
		if delta == 0 {
			continue
		}
		if err := r.repo.Record(ctx, c.metric, "", float64(delta)); err != nil {
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
		if err := r.repo.Record(ctx, g.metric, g.label, value); err != nil {
			r.logger.WarnContext(ctx, "stats: record gauge failed",
				"metric", g.metric, "label", g.label, "error", err)
		}
	}
}
