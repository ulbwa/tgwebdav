package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Repositories bundles every repository so services and the unit-of-work can
// be wired with a single value. Inside a transaction (see TxManager) the same
// struct is rebound to the transaction connection.
type Repositories struct {
	Users        UserRepository
	Tokens       APITokenRepository
	Bots         BotRepository
	Channels     ChannelRepository
	BotChannels  BotChannelRepository
	Blobs        BlobRepository
	BlobBotFiles BlobBotFileRepository
	Nodes        NodeRepository
	Extents      ExtentRepository
	WAL          WALRepository
	Events       EventRepository
	Stats        StatRepository
	Settings     SettingsRepository
}

// TxManager runs a function inside a single database transaction. The repos
// passed to fn operate on that transaction; an error (or panic) rolls back.
type TxManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, r *Repositories) error) error
}

// UserRepository persists users.
type UserRepository interface {
	Create(ctx context.Context, u *User) error
	Update(ctx context.Context, u *User) error
	Delete(ctx context.Context, id uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*User, error)
	GetByLogin(ctx context.Context, login string) (*User, error)
	List(ctx context.Context) ([]User, error)
	Count(ctx context.Context) (int64, error)
}

// APITokenRepository persists Management API bearer tokens.
type APITokenRepository interface {
	Create(ctx context.Context, t *APIToken) error
	GetByHash(ctx context.Context, hash string) (*APIToken, error)
	ListByUser(ctx context.Context, userID uuid.UUID) ([]APIToken, error)
	Delete(ctx context.Context, id uuid.UUID) error
	TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error
}

// BotRepository persists Telegram bots.
type BotRepository interface {
	Create(ctx context.Context, b *Bot) error
	Update(ctx context.Context, b *Bot) error
	Delete(ctx context.Context, id uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*Bot, error)
	GetByUsername(ctx context.Context, username string) (*Bot, error)
	List(ctx context.Context) ([]Bot, error)
	SetUnavailableUntil(ctx context.Context, id uuid.UUID, until *time.Time) error
}

// ChannelRepository persists Telegram channels.
type ChannelRepository interface {
	Create(ctx context.Context, c *Channel) error
	Update(ctx context.Context, c *Channel) error
	Delete(ctx context.Context, id uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*Channel, error)
	GetByChatID(ctx context.Context, chatID int64) (*Channel, error)
	List(ctx context.Context) ([]Channel, error)
	// IncrementCounter atomically adds delta and returns the new counter.
	IncrementCounter(ctx context.Context, id uuid.UUID, delta int64) (int64, error)
	SetAvailable(ctx context.Context, id uuid.UUID, available bool) error
}

// BotChannelRepository persists the bot↔channel membership matrix.
type BotChannelRepository interface {
	Upsert(ctx context.Context, bc *BotChannel) error
	Get(ctx context.Context, botID, channelID uuid.UUID) (*BotChannel, error)
	ListByChannel(ctx context.Context, channelID uuid.UUID) ([]BotChannel, error)
	ListByBot(ctx context.Context, botID uuid.UUID) ([]BotChannel, error)
	DeleteByBot(ctx context.Context, botID uuid.UUID) error
	DeleteByChannel(ctx context.Context, channelID uuid.UUID) error
}

// BlobRepository persists blobs (Telegram channel messages).
type BlobRepository interface {
	Create(ctx context.Context, b *Blob) error
	GetByID(ctx context.Context, id uuid.UUID) (*Blob, error)
	Update(ctx context.Context, b *Blob) error
	SetState(ctx context.Context, id uuid.UUID, state BlobState) error
	// AddRefcount atomically adds delta (may be negative) to the refcount.
	AddRefcount(ctx context.Context, id uuid.UUID, delta int64) error
	ListByChannel(ctx context.Context, channelID uuid.UUID) ([]Blob, error)
	ListByState(ctx context.Context, state BlobState) ([]Blob, error)
	// ListCollectable returns stored blobs with refcount <= 0.
	ListCollectable(ctx context.Context, limit int) ([]Blob, error)
	// MarkChannelUnavailable flips every blob of a channel to unavailable.
	MarkChannelUnavailable(ctx context.Context, channelID uuid.UUID) error
	// MarkChannelAvailable restores stored-eligible blobs of a channel.
	MarkChannelAvailable(ctx context.Context, channelID uuid.UUID) error
	// EvictOlderThan marks blobs with message_seq < minSeq unavailable.
	EvictOlderThan(ctx context.Context, channelID uuid.UUID, minSeq int64) (int64, error)
	Delete(ctx context.Context, id uuid.UUID) error
	Count(ctx context.Context) (int64, error)
}

// BlobBotFileRepository caches per-bot file_ids for blobs.
type BlobBotFileRepository interface {
	Upsert(ctx context.Context, f *BlobBotFile) error
	Get(ctx context.Context, blobID, botID uuid.UUID) (*BlobBotFile, error)
	ListByBlob(ctx context.Context, blobID uuid.UUID) ([]BlobBotFile, error)
	DeleteByBlob(ctx context.Context, blobID uuid.UUID) error
	DeleteByBot(ctx context.Context, botID uuid.UUID) error
}

// NodeRepository persists filesystem nodes.
type NodeRepository interface {
	Create(ctx context.Context, n *Node) error
	Update(ctx context.Context, n *Node) error
	Delete(ctx context.Context, id uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*Node, error)
	GetByPath(ctx context.Context, userID uuid.UUID, path string) (*Node, error)
	ListChildren(ctx context.Context, userID uuid.UUID, parentID uuid.UUID) ([]Node, error)
	// ListSubtree returns the node at prefix plus all descendants (path = prefix
	// or path LIKE prefix/%), ordered by path.
	ListSubtree(ctx context.Context, userID uuid.UUID, prefix string) ([]Node, error)
	CountChildren(ctx context.Context, parentID uuid.UUID) (int64, error)
	// SumSizeByUser returns the total logical size of a user's file nodes.
	SumSizeByUser(ctx context.Context, userID uuid.UUID) (int64, error)
	// ClaimBufferedForPacking atomically leases up to limit buffered nodes to a
	// packer worker and returns them. Leases expire after leaseFor.
	ClaimBufferedForPacking(ctx context.Context, leaseOwner string, leaseFor time.Duration, limit int) ([]Node, error)
	// ReleaseLease clears the packer lease on a node (e.g. on failure).
	ReleaseLease(ctx context.Context, id uuid.UUID) error
}

// ExtentRepository persists extents (file-range → blob-range mappings).
type ExtentRepository interface {
	CreateBatch(ctx context.Context, extents []Extent) error
	ListByNode(ctx context.Context, nodeID uuid.UUID) ([]Extent, error)
	DeleteByNode(ctx context.Context, nodeID uuid.UUID) error
	// ListBlobIDsByNode returns the distinct blob ids a node references.
	ListBlobIDsByNode(ctx context.Context, nodeID uuid.UUID) ([]uuid.UUID, error)
	// CopyForNode duplicates srcNode's extents onto dstNode (for COPY).
	CopyForNode(ctx context.Context, srcNodeID, dstNodeID uuid.UUID) error
	// ListNodesSolelyOnBlob returns node ids whose every extent references only
	// the given blob (used for MESSAGE_DELETED cascade deletion).
	ListNodesSolelyOnBlob(ctx context.Context, blobID uuid.UUID) ([]uuid.UUID, error)
}

// WALRepository persists append-only file content awaiting packing.
type WALRepository interface {
	AppendChunk(ctx context.Context, c *WALChunk) error
	// EachChunk streams a node's chunks in seq order (memory-friendly for the
	// packer). Returning an error from fn stops iteration with that error.
	EachChunk(ctx context.Context, nodeID uuid.UUID, fn func(c WALChunk) error) error
	// ReadRange assembles up to length bytes from offset for a buffered node.
	ReadRange(ctx context.Context, nodeID uuid.UUID, offset, length int64) ([]byte, error)
	SizeByNode(ctx context.Context, nodeID uuid.UUID) (int64, error)
	DeleteByNode(ctx context.Context, nodeID uuid.UUID) error
}

// EventRepository persists audit/log events.
type EventRepository interface {
	Log(ctx context.Context, kind, message, ref string) error
	List(ctx context.Context, kind string, limit, offset int) ([]Event, int64, error)
}

// StatRepository persists time-series samples for the dashboard.
type StatRepository interface {
	Record(ctx context.Context, metric, label string, value float64) error
	Query(ctx context.Context, metric, label string, from, to time.Time) ([]StatSample, error)
	Latest(ctx context.Context, metric, label string) (*StatSample, error)
}

// SettingsRepository persists the single runtime Settings row, applying
// DefaultSettings for any missing values.
type SettingsRepository interface {
	Get(ctx context.Context) (Settings, error)
	Update(ctx context.Context, s Settings) error
}
