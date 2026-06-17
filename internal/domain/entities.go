// Package domain holds the core entities, sentinel errors and port
// interfaces (repositories and services) for tgwebdav. It depends on nothing
// outside the standard library and a UUID helper, so every other package can
// import it without creating cycles. Implementations live in their own
// packages and depend only on these contracts.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// NodeState describes the lifecycle of a filesystem node's content.
type NodeState string

const (
	// NodeWriting means a WAL session is open and bytes are still arriving.
	NodeWriting NodeState = "writing"
	// NodeBuffered means the content lives in the WAL and is readable, but has
	// not yet been packed into a Telegram blob.
	NodeBuffered NodeState = "buffered"
	// NodeStored means the content has been packed into one or more blobs and
	// the WAL rows have been removed; reads assemble from extents.
	NodeStored NodeState = "stored"
)

// BlobState describes the lifecycle of a blob (a Telegram channel message).
type BlobState string

const (
	// BlobOpen means the packer is still accumulating bytes for this blob.
	BlobOpen BlobState = "open"
	// BlobSealed means the blob is full/flushed and ready to upload.
	BlobSealed BlobState = "sealed"
	// BlobUploading means an upload is in progress (lease held).
	BlobUploading BlobState = "uploading"
	// BlobStored means the blob is uploaded and downloadable.
	BlobStored BlobState = "stored"
	// BlobUnavailable means the blob is temporarily unreadable (channel gone,
	// evicted, or no bot has access). May recover later.
	BlobUnavailable BlobState = "unavailable"
	// BlobPermUnavailable means the underlying message was deleted; never
	// recoverable. Files referencing only such blobs are cascade-deleted.
	BlobPermUnavailable BlobState = "perm_unavailable"
)

// Readable reports whether a blob in this state can be downloaded.
func (s BlobState) Readable() bool { return s == BlobStored }

// User is an authenticated principal with an isolated WebDAV namespace.
type User struct {
	ID           uuid.UUID
	Login        string
	PasswordHash string // argon2id encoded hash
	IsAdmin      bool
	QuotaBytes   int64 // 0 means unlimited
	BandwidthBPS int64 // 0 means unlimited (bytes/sec for reads)
	RatePerMin   int   // 0 means unlimited (requests/min)
	CreatedAt    time.Time
}

// APIToken is a hashed, revocable bearer token for the Management API.
type APIToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  string // sha-256 hex of the presented token
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// Bot is a Telegram bot used to upload/download blobs. Token is the decrypted
// API token; the postgres layer encrypts it at rest with TGWEBDAV_SECRET_KEY.
type Bot struct {
	ID               uuid.UUID
	Username         string
	Token            string
	Enabled          bool
	UnavailableUntil *time.Time // set from Telegram retry_after
	CreatedAt        time.Time
}

// Available reports whether the bot may be used at instant now.
func (b Bot) Available(now time.Time) bool {
	if !b.Enabled {
		return false
	}
	if b.UnavailableUntil != nil && now.Before(*b.UnavailableUntil) {
		return false
	}
	return true
}

// Channel is a Telegram channel used as blob storage.
type Channel struct {
	ID                uuid.UUID
	TGChatID          int64 // -100… form passed to the Bot API
	Title             string
	MessageCounter    int64 // monotonic count of messages we have sent
	EvictionThreshold int64 // blobs older than counter-threshold are evicted
	Available         bool
	CreatedAt         time.Time
}

// BotChannel records whether a bot is a member/admin of a channel.
type BotChannel struct {
	BotID     uuid.UUID
	ChannelID uuid.UUID
	Member    bool
	CheckedAt time.Time
}

// Blob is an immutable, reference-counted unit of stored bytes living as a
// single Telegram channel message.
type Blob struct {
	ID         uuid.UUID
	ChannelID  uuid.UUID
	MessageID  int64
	MessageSeq int64 // channel.message_counter snapshot at send time (for eviction)
	Size       int64
	State      BlobState
	Refcount   int64
	CreatedAt  time.Time
	SealedAt   *time.Time
}

// BlobBotFile caches a per-bot file_id for a blob (file_ids are bot-specific).
type BlobBotFile struct {
	BlobID       uuid.UUID
	BotID        uuid.UUID
	FileID       string
	FileUniqueID string
	FetchedAt    time.Time
}

// Node is a filesystem entry (file or directory) in a user's namespace.
type Node struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	ParentID    *uuid.UUID
	Name        string
	Path        string // normalized, slash-rooted, no trailing slash (root = "/")
	IsDir       bool
	Size        int64
	ContentHash string // sha-256 hex of content (files only)
	ETag        string
	ContentType string
	State       NodeState
	CreatedAt   time.Time
	ModifiedAt  time.Time
}

// Extent maps a contiguous byte range of a file onto a region of a blob.
type Extent struct {
	ID         uuid.UUID
	NodeID     uuid.UUID
	Seq        int64
	FileOffset int64 // offset within the file
	Length     int64
	BlobID     uuid.UUID
	BlobOffset int64 // offset within the blob
}

// WALChunk is an append-only fragment of a file's content held in Postgres
// until the packer flushes it into a blob.
type WALChunk struct {
	ID        uuid.UUID
	NodeID    uuid.UUID
	Seq       int64
	Data      []byte
	CreatedAt time.Time
}

// Event is an audit/log record surfaced through the Management API.
type Event struct {
	ID      uuid.UUID
	TS      time.Time
	Kind    string
	Message string
	Ref     string
}

// Common event kinds.
const (
	EventBlobUnavailable = "blob_unavailable"
	EventBlobPermDeleted = "blob_perm_deleted"
	EventBlobReaped      = "blob_reaped"
	EventCascadeDelete   = "cascade_delete"
	EventBotDisabled     = "bot_disabled"
	EventBotUnavailable  = "bot_unavailable"
	EventChannelEvicted  = "channel_evicted"
	EventChannelDisabled = "channel_disabled"
	EventUploadFailed    = "upload_failed"
)

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
