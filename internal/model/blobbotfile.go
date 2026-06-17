package model

import (
	"time"

	"github.com/google/uuid"
)

// BlobBotFile caches a per-bot file_id for a blob (file_ids are bot-specific).
type BlobBotFile struct {
	BlobID       uuid.UUID
	BotID        uuid.UUID
	FileID       string
	FileUniqueID string
	FetchedAt    time.Time
}
