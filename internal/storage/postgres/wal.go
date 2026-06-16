package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// walRepo implements domain.WALRepository.
type walRepo struct{ base *gorm.DB }

// AppendChunk inserts a WAL chunk using the caller-provided seq. The (node, seq)
// uniqueness is enforced by the table, so a duplicate seq surfaces as
// domain.ErrAlreadyExists.
func (r *walRepo) AppendChunk(ctx context.Context, c *domain.WALChunk) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	m := &walChunkModel{
		ID:        c.ID,
		NodeID:    c.NodeID,
		Seq:       c.Seq,
		Data:      c.Data,
		CreatedAt: c.CreatedAt,
	}
	if err := txFromCtx(ctx, r.base).Create(m).Error; err != nil {
		return fmt.Errorf("append wal chunk: %w", translateError(err))
	}
	return nil
}

// EachChunk streams a node's chunks in seq order, invoking fn per row without
// loading the whole node into memory. Returning an error from fn stops
// iteration and propagates that error.
func (r *walRepo) EachChunk(ctx context.Context, nodeID uuid.UUID, fn func(c domain.WALChunk) error) error {
	rows, err := txFromCtx(ctx, r.base).Model(&walChunkModel{}).
		Where("node_id = ?", nodeID).
		Order("seq").
		Rows()
	if err != nil {
		return fmt.Errorf("stream wal chunks: %w", translateError(err))
	}
	defer rows.Close()

	db := txFromCtx(ctx, r.base)
	for rows.Next() {
		var m walChunkModel
		if err := db.ScanRows(rows, &m); err != nil {
			return fmt.Errorf("scan wal chunk: %w", translateError(err))
		}
		if err := fn(m.toDomain()); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate wal chunks: %w", translateError(err))
	}
	return nil
}

// ReadRange assembles up to length bytes starting at offset by walking the
// node's chunks in seq order. Chunks fully before the window are skipped; once
// the window is filled, iteration stops early.
func (r *walRepo) ReadRange(ctx context.Context, nodeID uuid.UUID, offset, length int64) ([]byte, error) {
	if length <= 0 || offset < 0 {
		return []byte{}, nil
	}
	end := offset + length
	out := make([]byte, 0, length)
	var cursor int64 // byte position of the start of the current chunk

	done := fmt.Errorf("done") // sentinel to short-circuit iteration
	err := r.EachChunk(ctx, nodeID, func(c domain.WALChunk) error {
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

// SizeByNode returns the total number of bytes buffered for a node across all
// of its WAL chunks.
func (r *walRepo) SizeByNode(ctx context.Context, nodeID uuid.UUID) (int64, error) {
	var size *int64
	if err := txFromCtx(ctx, r.base).Model(&walChunkModel{}).
		Where("node_id = ?", nodeID).
		Select("COALESCE(SUM(octet_length(data)), 0)").
		Scan(&size).Error; err != nil {
		return 0, fmt.Errorf("wal size by node: %w", translateError(err))
	}
	if size == nil {
		return 0, nil
	}
	return *size, nil
}

// DeleteByNode removes all WAL chunks of a node (after packing).
func (r *walRepo) DeleteByNode(ctx context.Context, nodeID uuid.UUID) error {
	if err := txFromCtx(ctx, r.base).Where("node_id = ?", nodeID).Delete(&walChunkModel{}).Error; err != nil {
		return fmt.Errorf("delete wal chunks by node: %w", translateError(err))
	}
	return nil
}
