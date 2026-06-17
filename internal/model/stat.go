package model

import (
	"time"

	"github.com/google/uuid"
)

// StatSample is one point of a named time series.
type StatSample struct {
	ID     uuid.UUID
	TS     time.Time
	Metric string
	Label  string
	Value  float64
}

// Common stat metrics.
const (
	MetricStorageBytes = "storage_bytes"
	MetricReadBytes    = "read_bytes"
	MetricWriteBytes   = "write_bytes"
	MetricReadOps      = "read_ops"
	MetricWriteOps     = "write_ops"
	MetricWALBytes     = "wal_bytes"
	MetricCacheBytes   = "cache_bytes"
	MetricCacheHit     = "cache_hit"
	MetricCacheMiss    = "cache_miss"
	MetricTelegramReq  = "telegram_req"
)
