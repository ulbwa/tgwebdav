package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// Principal is the result of authenticating a WebDAV request. Acting is the
// user whose namespace is served; Auth is the identity that authenticated.
// They differ only during admin impersonation (Basic "admin/target").
type Principal struct {
	Acting *User
	Auth   *User
}

// IsAdmin reports whether the authenticated identity is an administrator.
func (p Principal) IsAdmin() bool { return p.Auth != nil && p.Auth.IsAdmin }

// Impersonating reports whether an admin is acting in another user's namespace.
func (p Principal) Impersonating() bool {
	return p.Acting != nil && p.Auth != nil && p.Acting.ID != p.Auth.ID
}

// AuthService authenticates principals for the WebDAV and Management servers.
type AuthService interface {
	// AuthenticateBasic parses "user" or "admin/target" usernames and verifies
	// the password, returning the resolved principal.
	AuthenticateBasic(ctx context.Context, username, password string) (*Principal, error)
	// AuthenticateBearer verifies a Management API bearer token.
	AuthenticateBearer(ctx context.Context, token string) (*User, error)
	// HashPassword produces an argon2id hash for storage.
	HashPassword(password string) (string, error)
}

// ---- Telegram port ---------------------------------------------------------

// RateLimitError reports a Telegram 429 with the server-provided retry delay.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("telegram rate limited, retry after %s", e.RetryAfter)
}

var (
	// ErrTelegramNotFound means the message/file is gone (definitive); triggers
	// blob perm-unavailable + cascade delete.
	ErrTelegramNotFound = errors.New("telegram: message or file not found")
	// ErrTelegramForbidden means the bot lacks access (kicked/not admin).
	ErrTelegramForbidden = errors.New("telegram: forbidden")
)

// TGSendResult is the outcome of posting/forwarding a document.
type TGSendResult struct {
	MessageID    int64
	FileID       string
	FileUniqueID string
}

// TelegramAPI is the narrow Bot API surface tgwebdav needs. Every call is
// scoped to a specific bot (for per-bot rate limiting) and returns typed
// errors (*RateLimitError, ErrTelegramNotFound, ErrTelegramForbidden).
type TelegramAPI interface {
	// GetMe returns the bot's username (validates the token).
	GetMe(ctx context.Context, bot *Bot) (username string, err error)
	// GetChat reports whether the bot can access chatID and its title.
	GetChat(ctx context.Context, bot *Bot, chatID int64) (title string, member bool, err error)
	// SendDocument uploads raw bytes as a document and returns the message/file ids.
	SendDocument(ctx context.Context, bot *Bot, chatID int64, filename string, data []byte) (TGSendResult, error)
	// SendByFileID re-posts an existing file_id (no re-upload).
	SendByFileID(ctx context.Context, bot *Bot, chatID int64, fileID string) (TGSendResult, error)
	// ForwardMessage forwards a message within/between chats to recover a fresh file_id.
	ForwardMessage(ctx context.Context, bot *Bot, toChatID, fromChatID, messageID int64) (TGSendResult, error)
	// DeleteMessage removes a message (best-effort).
	DeleteMessage(ctx context.Context, bot *Bot, chatID, messageID int64) error
	// DownloadFile resolves a file_id via getFile and downloads the bytes.
	DownloadFile(ctx context.Context, bot *Bot, fileID string) ([]byte, error)
}

// ---- Cache port ------------------------------------------------------------

// BlobCache is a disk-backed LRU over whole blobs keyed by blob id.
type BlobCache interface {
	Get(id uuid.UUID) ([]byte, bool)
	Put(id uuid.UUID, data []byte) error
	Remove(id uuid.UUID)
	// Stats reports current cache size in bytes and number of entries.
	Stats() (bytes int64, entries int)
}

// ---- Blob read port --------------------------------------------------------

// BlobReader resolves the full bytes of a stored blob, transparently using the
// disk cache, bot selection, cross-bot recovery, and cascade handling.
type BlobReader interface {
	ReadBlob(ctx context.Context, blobID uuid.UUID) ([]byte, error)
}

// ---- Limits port -----------------------------------------------------------

// Limiter enforces per-user request rate and read bandwidth.
type Limiter interface {
	// Allow consumes one token from the user's per-minute bucket (sized by
	// ratePerMin; 0 = unlimited). Returns false when the limit is exceeded.
	Allow(userID uuid.UUID, ratePerMin int) bool
	// ThrottledReader wraps r to cap throughput at bps bytes/sec (0 = unlimited).
	ThrottledReader(r io.Reader, bps int64) io.Reader
}

// ---- Stats port ------------------------------------------------------------

// StatRecorder accumulates in-memory counters flushed periodically to the
// StatRepository. All methods are safe for concurrent use.
type StatRecorder interface {
	AddReadBytes(n int64)
	AddWriteBytes(n int64)
	IncReadOps()
	IncWriteOps()
	IncCacheHit()
	IncCacheMiss()
	IncTelegramReq()
}

// ---- Bot / channel / settings management services --------------------------

// BotService encapsulates bot lifecycle and channel-availability rebalancing.
type BotService interface {
	// Add validates the token (getMe), records the bot, and checks membership
	// of every known channel.
	Add(ctx context.Context, token string) (*Bot, error)
	// Remove deletes a bot and re-evaluates each channel's availability.
	Remove(ctx context.Context, id uuid.UUID) error
	SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) error
	List(ctx context.Context) ([]Bot, error)
	Get(ctx context.Context, id uuid.UUID) (*Bot, error)
	// RefreshMembership re-checks every (bot, channel) pair via getChat.
	RefreshMembership(ctx context.Context) error
	// PickForUpload returns an available bot that is a member of channelID,
	// preferring the least recently used.
	PickForUpload(ctx context.Context, channelID uuid.UUID) (*Bot, error)
}

// ChannelService encapsulates channel lifecycle and availability.
type ChannelService interface {
	// Add registers a channel by its bare id (the -100 prefix is applied
	// internally) and checks membership of every known bot.
	Add(ctx context.Context, bareID int64) (*Channel, error)
	Remove(ctx context.Context, id uuid.UUID) error
	SetEvictionThreshold(ctx context.Context, id uuid.UUID, threshold int64) error
	List(ctx context.Context) ([]Channel, error)
	Get(ctx context.Context, id uuid.UUID) (*Channel, error)
	// ReevaluateAvailability recomputes channel.available from bot membership
	// and propagates the result to the channel's blobs.
	ReevaluateAvailability(ctx context.Context) error
	// PickForUpload returns an available channel with at least one member bot.
	PickForUpload(ctx context.Context) (*Channel, error)
}

// SettingsService reads and updates runtime settings.
type SettingsService interface {
	Get(ctx context.Context) (Settings, error)
	Update(ctx context.Context, s Settings) error
}
