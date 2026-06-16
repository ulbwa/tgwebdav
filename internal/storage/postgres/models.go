package postgres

import (
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
)

// The model structs below are the GORM persistence shapes. They are kept
// separate from the domain entities so that storage concerns (encrypted
// columns, lease bookkeeping, ms-encoded durations) never leak into the
// domain layer. Column names and types mirror the init migration exactly;
// inserts never rely on database DEFAULTs (ids/timestamps are produced in Go).

// userModel maps the users table.
type userModel struct {
	ID           uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	Login        string    `gorm:"column:login"`
	PasswordHash string    `gorm:"column:password_hash"`
	IsAdmin      bool      `gorm:"column:is_admin"`
	QuotaBytes   int64     `gorm:"column:quota_bytes"`
	BandwidthBPS int64     `gorm:"column:bandwidth_bps"`
	RatePerMin   int       `gorm:"column:rate_per_min"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

// TableName returns the SQL table name for userModel.
func (userModel) TableName() string { return "users" }

func userToModel(u *domain.User) *userModel {
	return &userModel{
		ID:           u.ID,
		Login:        u.Login,
		PasswordHash: u.PasswordHash,
		IsAdmin:      u.IsAdmin,
		QuotaBytes:   u.QuotaBytes,
		BandwidthBPS: u.BandwidthBPS,
		RatePerMin:   u.RatePerMin,
		CreatedAt:    u.CreatedAt,
	}
}

func (m *userModel) toDomain() *domain.User {
	return &domain.User{
		ID:           m.ID,
		Login:        m.Login,
		PasswordHash: m.PasswordHash,
		IsAdmin:      m.IsAdmin,
		QuotaBytes:   m.QuotaBytes,
		BandwidthBPS: m.BandwidthBPS,
		RatePerMin:   m.RatePerMin,
		CreatedAt:    m.CreatedAt,
	}
}

// apiTokenModel maps the api_tokens table.
type apiTokenModel struct {
	ID         uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID     uuid.UUID  `gorm:"column:user_id;type:uuid"`
	TokenHash  string     `gorm:"column:token_hash"`
	Name       string     `gorm:"column:name"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	LastUsedAt *time.Time `gorm:"column:last_used_at"`
}

// TableName returns the SQL table name for apiTokenModel.
func (apiTokenModel) TableName() string { return "api_tokens" }

func apiTokenToModel(t *domain.APIToken) *apiTokenModel {
	return &apiTokenModel{
		ID:         t.ID,
		UserID:     t.UserID,
		TokenHash:  t.TokenHash,
		Name:       t.Name,
		CreatedAt:  t.CreatedAt,
		LastUsedAt: t.LastUsedAt,
	}
}

func (m *apiTokenModel) toDomain() *domain.APIToken {
	return &domain.APIToken{
		ID:         m.ID,
		UserID:     m.UserID,
		TokenHash:  m.TokenHash,
		Name:       m.Name,
		CreatedAt:  m.CreatedAt,
		LastUsedAt: m.LastUsedAt,
	}
}

// botModel maps the bots table. The plaintext token is never stored: token_enc
// holds the AES-256-GCM ciphertext and token_sha the sha256 hex of the token.
type botModel struct {
	ID               uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	Username         string     `gorm:"column:username"`
	TokenSHA         string     `gorm:"column:token_sha"`
	TokenEnc         []byte     `gorm:"column:token_enc;type:bytea"`
	Enabled          bool       `gorm:"column:enabled"`
	UnavailableUntil *time.Time `gorm:"column:unavailable_until"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
}

// TableName returns the SQL table name for botModel.
func (botModel) TableName() string { return "bots" }

// channelModel maps the channels table.
type channelModel struct {
	ID                uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	TGChatID          int64     `gorm:"column:tg_chat_id"`
	Title             string    `gorm:"column:title"`
	MessageCounter    int64     `gorm:"column:message_counter"`
	EvictionThreshold int64     `gorm:"column:eviction_threshold"`
	Available         bool      `gorm:"column:available"`
	CreatedAt         time.Time `gorm:"column:created_at"`
}

// TableName returns the SQL table name for channelModel.
func (channelModel) TableName() string { return "channels" }

func channelToModel(c *domain.Channel) *channelModel {
	return &channelModel{
		ID:                c.ID,
		TGChatID:          c.TGChatID,
		Title:             c.Title,
		MessageCounter:    c.MessageCounter,
		EvictionThreshold: c.EvictionThreshold,
		Available:         c.Available,
		CreatedAt:         c.CreatedAt,
	}
}

func (m *channelModel) toDomain() *domain.Channel {
	return &domain.Channel{
		ID:                m.ID,
		TGChatID:          m.TGChatID,
		Title:             m.Title,
		MessageCounter:    m.MessageCounter,
		EvictionThreshold: m.EvictionThreshold,
		Available:         m.Available,
		CreatedAt:         m.CreatedAt,
	}
}

// botChannelModel maps the bot_channel table.
type botChannelModel struct {
	BotID     uuid.UUID `gorm:"column:bot_id;type:uuid;primaryKey"`
	ChannelID uuid.UUID `gorm:"column:channel_id;type:uuid;primaryKey"`
	Member    bool      `gorm:"column:member"`
	CheckedAt time.Time `gorm:"column:checked_at"`
}

// TableName returns the SQL table name for botChannelModel.
func (botChannelModel) TableName() string { return "bot_channel" }

func botChannelToModel(bc *domain.BotChannel) *botChannelModel {
	return &botChannelModel{
		BotID:     bc.BotID,
		ChannelID: bc.ChannelID,
		Member:    bc.Member,
		CheckedAt: bc.CheckedAt,
	}
}

func (m *botChannelModel) toDomain() *domain.BotChannel {
	return &domain.BotChannel{
		BotID:     m.BotID,
		ChannelID: m.ChannelID,
		Member:    m.Member,
		CheckedAt: m.CheckedAt,
	}
}

// blobModel maps the blobs table.
type blobModel struct {
	ID         uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	ChannelID  uuid.UUID  `gorm:"column:channel_id;type:uuid"`
	MessageID  int64      `gorm:"column:message_id"`
	MessageSeq int64      `gorm:"column:message_seq"`
	Size       int64      `gorm:"column:size"`
	State      string     `gorm:"column:state"`
	Refcount   int64      `gorm:"column:refcount"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	SealedAt   *time.Time `gorm:"column:sealed_at"`
}

// TableName returns the SQL table name for blobModel.
func (blobModel) TableName() string { return "blobs" }

func blobToModel(b *domain.Blob) *blobModel {
	return &blobModel{
		ID:         b.ID,
		ChannelID:  b.ChannelID,
		MessageID:  b.MessageID,
		MessageSeq: b.MessageSeq,
		Size:       b.Size,
		State:      string(b.State),
		Refcount:   b.Refcount,
		CreatedAt:  b.CreatedAt,
		SealedAt:   b.SealedAt,
	}
}

func (m *blobModel) toDomain() *domain.Blob {
	return &domain.Blob{
		ID:         m.ID,
		ChannelID:  m.ChannelID,
		MessageID:  m.MessageID,
		MessageSeq: m.MessageSeq,
		Size:       m.Size,
		State:      domain.BlobState(m.State),
		Refcount:   m.Refcount,
		CreatedAt:  m.CreatedAt,
		SealedAt:   m.SealedAt,
	}
}

// blobBotFileModel maps the blob_bot_files table.
type blobBotFileModel struct {
	BlobID       uuid.UUID `gorm:"column:blob_id;type:uuid;primaryKey"`
	BotID        uuid.UUID `gorm:"column:bot_id;type:uuid;primaryKey"`
	FileID       string    `gorm:"column:file_id"`
	FileUniqueID string    `gorm:"column:file_unique_id"`
	FetchedAt    time.Time `gorm:"column:fetched_at"`
}

// TableName returns the SQL table name for blobBotFileModel.
func (blobBotFileModel) TableName() string { return "blob_bot_files" }

func blobBotFileToModel(f *domain.BlobBotFile) *blobBotFileModel {
	return &blobBotFileModel{
		BlobID:       f.BlobID,
		BotID:        f.BotID,
		FileID:       f.FileID,
		FileUniqueID: f.FileUniqueID,
		FetchedAt:    f.FetchedAt,
	}
}

func (m *blobBotFileModel) toDomain() *domain.BlobBotFile {
	return &domain.BlobBotFile{
		BlobID:       m.BlobID,
		BotID:        m.BotID,
		FileID:       m.FileID,
		FileUniqueID: m.FileUniqueID,
		FetchedAt:    m.FetchedAt,
	}
}

// nodeModel maps the nodes table. The packer-lease columns have no domain
// counterpart; they are managed entirely inside the repository.
type nodeModel struct {
	ID               uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID           uuid.UUID  `gorm:"column:user_id;type:uuid"`
	ParentID         *uuid.UUID `gorm:"column:parent_id;type:uuid"`
	Name             string     `gorm:"column:name"`
	Path             string     `gorm:"column:path"`
	IsDir            bool       `gorm:"column:is_dir"`
	Size             int64      `gorm:"column:size"`
	ContentHash      string     `gorm:"column:content_hash"`
	ETag             string     `gorm:"column:etag"`
	ContentType      string     `gorm:"column:content_type"`
	State            string     `gorm:"column:state"`
	PackerLeaseOwner string     `gorm:"column:packer_lease_owner"`
	PackerLeaseUntil *time.Time `gorm:"column:packer_lease_until"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	ModifiedAt       time.Time  `gorm:"column:modified_at"`
}

// TableName returns the SQL table name for nodeModel.
func (nodeModel) TableName() string { return "nodes" }

func nodeToModel(n *domain.Node) *nodeModel {
	return &nodeModel{
		ID:          n.ID,
		UserID:      n.UserID,
		ParentID:    n.ParentID,
		Name:        n.Name,
		Path:        n.Path,
		IsDir:       n.IsDir,
		Size:        n.Size,
		ContentHash: n.ContentHash,
		ETag:        n.ETag,
		ContentType: n.ContentType,
		State:       string(n.State),
		CreatedAt:   n.CreatedAt,
		ModifiedAt:  n.ModifiedAt,
	}
}

func (m *nodeModel) toDomain() *domain.Node {
	return &domain.Node{
		ID:          m.ID,
		UserID:      m.UserID,
		ParentID:    m.ParentID,
		Name:        m.Name,
		Path:        m.Path,
		IsDir:       m.IsDir,
		Size:        m.Size,
		ContentHash: m.ContentHash,
		ETag:        m.ETag,
		ContentType: m.ContentType,
		State:       domain.NodeState(m.State),
		CreatedAt:   m.CreatedAt,
		ModifiedAt:  m.ModifiedAt,
	}
}

// extentModel maps the extents table.
type extentModel struct {
	ID         uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	NodeID     uuid.UUID `gorm:"column:node_id;type:uuid"`
	Seq        int64     `gorm:"column:seq"`
	FileOffset int64     `gorm:"column:file_offset"`
	Length     int64     `gorm:"column:length"`
	BlobID     uuid.UUID `gorm:"column:blob_id;type:uuid"`
	BlobOffset int64     `gorm:"column:blob_offset"`
}

// TableName returns the SQL table name for extentModel.
func (extentModel) TableName() string { return "extents" }

func extentToModel(e *domain.Extent) *extentModel {
	return &extentModel{
		ID:         e.ID,
		NodeID:     e.NodeID,
		Seq:        e.Seq,
		FileOffset: e.FileOffset,
		Length:     e.Length,
		BlobID:     e.BlobID,
		BlobOffset: e.BlobOffset,
	}
}

func (m *extentModel) toDomain() domain.Extent {
	return domain.Extent{
		ID:         m.ID,
		NodeID:     m.NodeID,
		Seq:        m.Seq,
		FileOffset: m.FileOffset,
		Length:     m.Length,
		BlobID:     m.BlobID,
		BlobOffset: m.BlobOffset,
	}
}

// walChunkModel maps the wal_chunks table.
type walChunkModel struct {
	ID        uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	NodeID    uuid.UUID `gorm:"column:node_id;type:uuid"`
	Seq       int64     `gorm:"column:seq"`
	Data      []byte    `gorm:"column:data;type:bytea"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName returns the SQL table name for walChunkModel.
func (walChunkModel) TableName() string { return "wal_chunks" }

func (m *walChunkModel) toDomain() domain.WALChunk {
	return domain.WALChunk{
		ID:        m.ID,
		NodeID:    m.NodeID,
		Seq:       m.Seq,
		Data:      m.Data,
		CreatedAt: m.CreatedAt,
	}
}

// eventModel maps the events table.
type eventModel struct {
	ID      uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	TS      time.Time `gorm:"column:ts"`
	Kind    string    `gorm:"column:kind"`
	Message string    `gorm:"column:message"`
	Ref     string    `gorm:"column:ref"`
}

// TableName returns the SQL table name for eventModel.
func (eventModel) TableName() string { return "events" }

func (m *eventModel) toDomain() domain.Event {
	return domain.Event{
		ID:      m.ID,
		TS:      m.TS,
		Kind:    m.Kind,
		Message: m.Message,
		Ref:     m.Ref,
	}
}

// statSampleModel maps the stat_samples table.
type statSampleModel struct {
	ID     uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	TS     time.Time `gorm:"column:ts"`
	Metric string    `gorm:"column:metric"`
	Label  string    `gorm:"column:label"`
	Value  float64   `gorm:"column:value"`
}

// TableName returns the SQL table name for statSampleModel.
func (statSampleModel) TableName() string { return "stat_samples" }

func (m *statSampleModel) toDomain() *domain.StatSample {
	return &domain.StatSample{
		ID:     m.ID,
		TS:     m.TS,
		Metric: m.Metric,
		Label:  m.Label,
		Value:  m.Value,
	}
}

// settingsModel maps the single-row settings table. The idle timeout is stored
// as integer milliseconds (wal_idle_timeout_ms).
type settingsModel struct {
	ID                       int       `gorm:"column:id;primaryKey"`
	BlobMaxSize              int64     `gorm:"column:blob_max_size"`
	WALIdleTimeoutMS         int64     `gorm:"column:wal_idle_timeout_ms"`
	MaxFileSize              int64     `gorm:"column:max_file_size"`
	DefaultEvictionThreshold int64     `gorm:"column:default_eviction_threshold"`
	UpdatedAt                time.Time `gorm:"column:updated_at"`
}

// TableName returns the SQL table name for settingsModel.
func (settingsModel) TableName() string { return "settings" }
