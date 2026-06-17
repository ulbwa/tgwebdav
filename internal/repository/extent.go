package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// extentRepository persists extents (file-range → blob-range mappings) on
// Postgres via sqlc. It resolves its executor per call from the context so the
// same value is correct inside or outside a transaction.
type extentRepository struct {
	pool *pgxpool.Pool
}

// NewExtentRepository returns an extent repository backed by pool. Its method
// set mirrors the ExtentRepository contract (domain.Extent → model.Extent).
func NewExtentRepository(pool *pgxpool.Pool) *extentRepository {
	return &extentRepository{pool: pool}
}

// extentRowToModel maps a sqlc extent row onto the model.
func extentRowToModel(row sqlc.Extent) model.Extent {
	return model.Extent{
		ID:         row.ID,
		NodeID:     row.NodeID,
		Seq:        row.Seq,
		FileOffset: row.FileOffset,
		Length:     row.Length,
		BlobID:     row.BlobID,
		BlobOffset: row.BlobOffset,
	}
}

// CreateBatch inserts a slice of extents in one COPY (via the sqlc :copyfrom
// query). Missing ids are generated in Go so each row carries an explicit id,
// mirroring the old GORM behavior.
func (r *extentRepository) CreateBatch(ctx context.Context, extents []model.Extent) error {
	if len(extents) == 0 {
		return nil
	}
	params := make([]sqlc.CreateExtentsParams, len(extents))
	for i := range extents {
		if extents[i].ID == uuid.Nil {
			extents[i].ID = uuid.New()
		}
		params[i] = sqlc.CreateExtentsParams{
			ID:         extents[i].ID,
			NodeID:     extents[i].NodeID,
			Seq:        extents[i].Seq,
			FileOffset: extents[i].FileOffset,
			Length:     extents[i].Length,
			BlobID:     extents[i].BlobID,
			BlobOffset: extents[i].BlobOffset,
		}
	}
	db := database.FromContext(ctx, r.pool)
	if _, err := sqlc.New(db).CreateExtents(ctx, params); err != nil {
		return fmt.Errorf("create extents: %w", translateError(err))
	}
	return nil
}

// ListByNode returns a node's extents ordered by seq.
func (r *extentRepository) ListByNode(ctx context.Context, nodeID uuid.UUID) ([]model.Extent, error) {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ListExtentsByNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list extents by node: %w", translateError(err))
	}
	out := make([]model.Extent, len(rows))
	for i := range rows {
		out[i] = extentRowToModel(rows[i])
	}
	return out, nil
}

// DeleteByNode removes all extents of a node.
func (r *extentRepository) DeleteByNode(ctx context.Context, nodeID uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	if err := sqlc.New(db).DeleteExtentsByNode(ctx, nodeID); err != nil {
		return fmt.Errorf("delete extents by node: %w", translateError(err))
	}
	return nil
}

// ListBlobIDsByNode returns the distinct blob ids a node's extents reference.
func (r *extentRepository) ListBlobIDsByNode(ctx context.Context, nodeID uuid.UUID) ([]uuid.UUID, error) {
	db := database.FromContext(ctx, r.pool)
	ids, err := sqlc.New(db).ListBlobIDsByNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list blob ids by node: %w", translateError(err))
	}
	return ids, nil
}

// CopyForNode duplicates srcNode's extents onto dstNode with fresh ids via a
// single INSERT ... SELECT gen_random_uuid().
func (r *extentRepository) CopyForNode(ctx context.Context, srcNodeID, dstNodeID uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	// The generated query takes the destination node id first ($1) and the
	// source node id second ($2): NodeID is the INSERT target, NodeID_2 is the
	// SELECT filter.
	err := sqlc.New(db).CopyExtentsForNode(ctx, sqlc.CopyExtentsForNodeParams{
		NodeID:   dstNodeID,
		NodeID_2: srcNodeID,
	})
	if err != nil {
		return fmt.Errorf("copy extents for node: %w", translateError(err))
	}
	return nil
}

// ListNodesSolelyOnBlob returns the ids of nodes whose every extent references
// only blobID. The generated query groups all extents by node and keeps a node
// only when bool_and(blob_id = blobID) holds, which implies the node has at
// least one extent and none of them point at any other blob — matching the old
// GORM "at least one extent on blobID and no extent on a different blob".
func (r *extentRepository) ListNodesSolelyOnBlob(ctx context.Context, blobID uuid.UUID) ([]uuid.UUID, error) {
	db := database.FromContext(ctx, r.pool)
	ids, err := sqlc.New(db).ListNodesSolelyOnBlob(ctx, blobID)
	if err != nil {
		return nil, fmt.Errorf("list nodes solely on blob: %w", translateError(err))
	}
	return ids, nil
}
