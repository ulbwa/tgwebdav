package model

import (
	"time"

	"github.com/google/uuid"
)

// WALChunk is an append-only fragment of a file's content held in Postgres
// until the packer flushes it into a blob.
type WALChunk struct {
	ID        uuid.UUID
	NodeID    uuid.UUID
	Seq       int64
	Data      []byte
	CreatedAt time.Time
}
