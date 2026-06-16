package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// extentRepo implements domain.ExtentRepository.
type extentRepo struct{ base *gorm.DB }

// CreateBatch inserts a slice of extents in one statement. Missing ids are
// generated in Go.
func (r *extentRepo) CreateBatch(ctx context.Context, extents []domain.Extent) error {
	if len(extents) == 0 {
		return nil
	}
	ms := make([]extentModel, len(extents))
	for i := range extents {
		if extents[i].ID == uuid.Nil {
			extents[i].ID = uuid.New()
		}
		ms[i] = *extentToModel(&extents[i])
	}
	if err := txFromCtx(ctx, r.base).Create(&ms).Error; err != nil {
		return fmt.Errorf("create extents: %w", translateError(err))
	}
	return nil
}

// ListByNode returns a node's extents ordered by seq.
func (r *extentRepo) ListByNode(ctx context.Context, nodeID uuid.UUID) ([]domain.Extent, error) {
	var ms []extentModel
	if err := txFromCtx(ctx, r.base).Where("node_id = ?", nodeID).Order("seq").Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list extents by node: %w", translateError(err))
	}
	out := make([]domain.Extent, len(ms))
	for i := range ms {
		out[i] = ms[i].toDomain()
	}
	return out, nil
}

// DeleteByNode removes all extents of a node.
func (r *extentRepo) DeleteByNode(ctx context.Context, nodeID uuid.UUID) error {
	if err := txFromCtx(ctx, r.base).Where("node_id = ?", nodeID).Delete(&extentModel{}).Error; err != nil {
		return fmt.Errorf("delete extents by node: %w", translateError(err))
	}
	return nil
}

// ListBlobIDsByNode returns the distinct blob ids a node's extents reference.
func (r *extentRepo) ListBlobIDsByNode(ctx context.Context, nodeID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	if err := txFromCtx(ctx, r.base).Model(&extentModel{}).
		Where("node_id = ?", nodeID).
		Distinct("blob_id").
		Pluck("blob_id", &ids).Error; err != nil {
		return nil, fmt.Errorf("list blob ids by node: %w", translateError(err))
	}
	return ids, nil
}

// CopyForNode duplicates srcNode's extents onto dstNode with fresh ids. The
// insert and source select run in a single INSERT ... SELECT.
func (r *extentRepo) CopyForNode(ctx context.Context, srcNodeID, dstNodeID uuid.UUID) error {
	const q = `
INSERT INTO extents (id, node_id, seq, file_offset, length, blob_id, blob_offset)
SELECT gen_random_uuid(), ?, seq, file_offset, length, blob_id, blob_offset
FROM extents
WHERE node_id = ?`
	if err := txFromCtx(ctx, r.base).Exec(q, dstNodeID, srcNodeID).Error; err != nil {
		return fmt.Errorf("copy extents for node: %w", translateError(err))
	}
	return nil
}

// ListNodesSolelyOnBlob returns the ids of nodes that have at least one extent
// on blobID and no extent referencing any other blob. Used to cascade-delete
// files whose sole backing blob became permanently unavailable.
func (r *extentRepo) ListNodesSolelyOnBlob(ctx context.Context, blobID uuid.UUID) ([]uuid.UUID, error) {
	const q = `
SELECT DISTINCT e.node_id
FROM extents e
WHERE e.blob_id = ?
  AND NOT EXISTS (
      SELECT 1 FROM extents o
      WHERE o.node_id = e.node_id AND o.blob_id <> ?
  )`
	var ids []uuid.UUID
	if err := txFromCtx(ctx, r.base).Raw(q, blobID, blobID).Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("list nodes solely on blob: %w", translateError(err))
	}
	return ids, nil
}
