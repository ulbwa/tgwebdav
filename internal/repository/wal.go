package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// WALRepository persists append-only file content (WAL chunks) awaiting packing.
// It resolves its executor per call through database.FromContext.
type WALRepository struct {
	pool *pgxpool.Pool
}

// NewWALRepository returns a WALRepository bound to pool as its fallback
// executor.
func NewWALRepository(pool *pgxpool.Pool) *WALRepository {
	return &WALRepository{pool: pool}
}

// AppendChunk inserts a WAL chunk using the caller-provided seq. The (node, seq)
// uniqueness is enforced by the table, so a duplicate seq surfaces as
// ErrAlreadyExists.
func (r *WALRepository) AppendChunk(ctx context.Context, c *model.WALChunk) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).AppendWALChunk(ctx, sqlc.AppendWALChunkParams{
		ID:        c.ID,
		NodeID:    c.NodeID,
		Seq:       c.Seq,
		Data:      c.Data,
		CreatedAt: pgtype.Timestamptz{Time: c.CreatedAt, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("append wal chunk: %w", translateError(err))
	}
	return nil
}

// EachChunk streams a node's chunks in seq order, invoking fn per chunk.
// Returning an error from fn stops iteration and propagates that error
// unchanged. The chunks are loaded ordered by seq by the underlying query.
func (r *WALRepository) EachChunk(ctx context.Context, nodeID uuid.UUID, fn func(c model.WALChunk) error) error {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ListWALChunksByNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("each wal chunk: %w", translateError(err))
	}
	for _, row := range rows {
		if err := fn(walChunkToModel(row)); err != nil {
			return err
		}
	}
	return nil
}

// ReadRange assembles up to length bytes starting at offset. WAL chunks are
// fixed model.WALChunkSize bytes (only the final chunk may be smaller), so a
// chunk's seq maps deterministically to its byte offset. The window therefore
// touches only chunks firstSeq..lastSeq, and ReadRange fetches exactly those —
// bounding memory to the requested window instead of loading the whole file.
func (r *WALRepository) ReadRange(ctx context.Context, nodeID uuid.UUID, offset, length int64) ([]byte, error) {
	if length <= 0 || offset < 0 {
		return []byte{}, nil
	}

	end := offset + length
	firstSeq := offset / model.WALChunkSize
	lastSeq := (end - 1) / model.WALChunkSize
	base := firstSeq * model.WALChunkSize // byte offset of firstSeq's first byte

	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ListWALChunksByNodeSeqRange(ctx, sqlc.ListWALChunksByNodeSeqRangeParams{
		NodeID: nodeID,
		Seq:    firstSeq,
		Seq_2:  lastSeq,
	})
	if err != nil {
		return nil, fmt.Errorf("read range: %w", translateError(err))
	}

	// Concatenate the window's chunks in seq order (the query orders by seq).
	assembled := make([]byte, 0, length)
	for _, row := range rows {
		assembled = append(assembled, row.Data...)
	}

	// Slice the window-relative range, clamping the tail to what is available.
	from := offset - base
	to := end - base
	if to > int64(len(assembled)) {
		to = int64(len(assembled))
	}
	if from >= to {
		return []byte{}, nil
	}
	out := make([]byte, to-from)
	copy(out, assembled[from:to])
	return out, nil
}

// SizeByNode returns the total number of bytes buffered for a node across all of
// its WAL chunks.
func (r *WALRepository) SizeByNode(ctx context.Context, nodeID uuid.UUID) (int64, error) {
	db := database.FromContext(ctx, r.pool)
	size, err := sqlc.New(db).WALSizeByNode(ctx, nodeID)
	if err != nil {
		return 0, fmt.Errorf("wal size by node: %w", translateError(err))
	}
	return size, nil
}

// DeleteByNode removes all WAL chunks of a node (after packing).
func (r *WALRepository) DeleteByNode(ctx context.Context, nodeID uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	if err := sqlc.New(db).DeleteWALChunksByNode(ctx, nodeID); err != nil {
		return fmt.Errorf("delete wal chunks by node: %w", translateError(err))
	}
	return nil
}

// walChunkToModel maps a sqlc WalChunk row onto the domain model.
func walChunkToModel(c sqlc.WalChunk) model.WALChunk {
	return model.WALChunk{
		ID:        c.ID,
		NodeID:    c.NodeID,
		Seq:       c.Seq,
		Data:      c.Data,
		CreatedAt: c.CreatedAt.Time,
	}
}
