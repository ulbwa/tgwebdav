package model

import (
	"time"

	"github.com/google/uuid"
)

// BlobState describes the lifecycle of a blob (a Telegram channel message).
// ENUM(open, sealed, uploading, stored, unavailable, perm_unavailable)
//
//go:generate go-enum --values --sql
type BlobState int32

// Readable reports whether a blob in this state can be downloaded.
func (s BlobState) Readable() bool { return s == BlobStateStored }

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
