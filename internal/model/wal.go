package model

import (
	"time"

	"github.com/google/uuid"
)

// WALChunkSize is the fixed size, in bytes, of every WAL chunk a file's content
// is split into when buffered. It is a domain invariant: the writer always emits
// full WALChunkSize-byte chunks and only the final chunk of a file may be
// smaller. Because the size is fixed, a chunk's seq maps deterministically to its
// byte offset — chunk seq covers bytes [seq*WALChunkSize, (seq+1)*WALChunkSize) —
// which lets a windowed read fetch only the chunks it needs.
const WALChunkSize int64 = 1 << 20 // 1 MiB

// WALChunk is an append-only fragment of a file's content held in Postgres
// until the packer flushes it into a blob.
type WALChunk struct {
	ID        uuid.UUID
	NodeID    uuid.UUID
	Seq       int64
	Data      []byte
	CreatedAt time.Time
}
