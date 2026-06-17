package model

import "time"

// Settings holds runtime-tunable parameters stored in the database and edited
// through the Management API (everything not needed to bootstrap the process).
type Settings struct {
	// BlobMaxSize is the working blob size in bytes (< 20 MiB getFile limit).
	BlobMaxSize int64
	// WALIdleTimeout is how long the packer waits after the last append before
	// flushing a partially-filled blob.
	WALIdleTimeout time.Duration
	// MaxFileSize caps a single uploaded file (0 = unlimited, split as needed).
	MaxFileSize int64
	// DefaultEvictionThreshold seeds new channels' eviction threshold.
	DefaultEvictionThreshold int64
	UpdatedAt                time.Time
}

// DefaultSettings returns the built-in defaults applied when no row exists.
func DefaultSettings() Settings {
	return Settings{
		BlobMaxSize:              19 * 1024 * 1024,
		WALIdleTimeout:           5 * time.Second,
		MaxFileSize:              0,
		DefaultEvictionThreshold: 900_000,
	}
}
