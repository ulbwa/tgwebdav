package model

import "github.com/google/uuid"

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
