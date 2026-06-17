package model

import (
	"time"

	"github.com/google/uuid"
)

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
