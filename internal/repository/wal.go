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
// model.ErrAlreadyExists.
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

// ReadRange assembles up to length bytes starting at offset by walking the
// node's chunks in seq order. Chunks fully before the window are skipped; once
// the window is filled, iteration stops early. This mirrors the slicing/assembly
// logic of the GORM original.
func (r *WALRepository) ReadRange(ctx context.Context, nodeID uuid.UUID, offset, length int64) ([]byte, error) {
	if length <= 0 || offset < 0 {
		return []byte{}, nil
	}
	end := offset + length
	out := make([]byte, 0, length)
	var cursor int64 // byte position of the start of the current chunk

	done := fmt.Errorf("done") // sentinel to short-circuit iteration
	err := r.EachChunk(ctx, nodeID, func(c model.WALChunk) error {
		chunkStart := cursor
		chunkEnd := cursor + int64(len(c.Data))
		cursor = chunkEnd

		// Skip chunks entirely before the requested window.
		if chunkEnd <= offset {
			return nil
		}
		// Stop once we are past the requested window.
		if chunkStart >= end {
			return done
		}
		// Compute the overlap [from, to) within this chunk's local coordinates.
		from := int64(0)
		if offset > chunkStart {
			from = offset - chunkStart
		}
		to := int64(len(c.Data))
		if end < chunkEnd {
			to = end - chunkStart
		}
		out = append(out, c.Data[from:to]...)
		if cursor >= end {
			return done
		}
		return nil
	})
	if err != nil && err != done {
		return nil, err
	}
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
