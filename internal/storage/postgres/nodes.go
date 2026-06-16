package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// nodeRepo implements domain.NodeRepository.
type nodeRepo struct{ base *gorm.DB }

// Create inserts a new filesystem node.
func (r *nodeRepo) Create(ctx context.Context, n *domain.Node) error {
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	now := time.Now()
	if n.CreatedAt.IsZero() {
		n.CreatedAt = now
	}
	if n.ModifiedAt.IsZero() {
		n.ModifiedAt = now
	}
	if err := txFromCtx(ctx, r.base).Create(nodeToModel(n)).Error; err != nil {
		return fmt.Errorf("create node: %w", translateError(err))
	}
	return nil
}

// Update saves the mutable columns of a node. The packer-lease columns are
// managed separately (ClaimBufferedForPacking / ReleaseLease) and are not
// touched here.
func (r *nodeRepo) Update(ctx context.Context, n *domain.Node) error {
	res := txFromCtx(ctx, r.base).Model(&nodeModel{}).
		Where("id = ?", n.ID).
		Updates(map[string]any{
			"parent_id":    n.ParentID,
			"name":         n.Name,
			"path":         n.Path,
			"is_dir":       n.IsDir,
			"size":         n.Size,
			"content_hash": n.ContentHash,
			"etag":         n.ETag,
			"content_type": n.ContentType,
			"state":        string(n.State),
			"modified_at":  n.ModifiedAt,
		})
	if res.Error != nil {
		return fmt.Errorf("update node: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update node: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes a node by id (cascades to children, extents and wal_chunks).
func (r *nodeRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res := txFromCtx(ctx, r.base).Where("id = ?", id).Delete(&nodeModel{})
	if res.Error != nil {
		return fmt.Errorf("delete node: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete node: %w", domain.ErrNotFound)
	}
	return nil
}

// GetByID loads a node by primary key.
func (r *nodeRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Node, error) {
	var m nodeModel
	if err := txFromCtx(ctx, r.base).Where("id = ?", id).First(&m).Error; err != nil {
		return nil, fmt.Errorf("get node by id: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// GetByPath loads a node by its normalized path within a user's namespace.
func (r *nodeRepo) GetByPath(ctx context.Context, userID uuid.UUID, path string) (*domain.Node, error) {
	var m nodeModel
	if err := txFromCtx(ctx, r.base).
		Where("user_id = ? AND path = ?", userID, path).
		First(&m).Error; err != nil {
		return nil, fmt.Errorf("get node by path: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// ListChildren returns the direct children of a directory node.
func (r *nodeRepo) ListChildren(ctx context.Context, userID uuid.UUID, parentID uuid.UUID) ([]domain.Node, error) {
	var ms []nodeModel
	if err := txFromCtx(ctx, r.base).
		Where("user_id = ? AND parent_id = ?", userID, parentID).
		Order("name").
		Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list children: %w", translateError(err))
	}
	return nodesToDomain(ms), nil
}

// ListSubtree returns the node at prefix plus all of its descendants, ordered by
// path. LIKE wildcards in the prefix are escaped so paths containing %, _ or the
// escape character match literally.
func (r *nodeRepo) ListSubtree(ctx context.Context, userID uuid.UUID, prefix string) ([]domain.Node, error) {
	var ms []nodeModel
	like := escapeLike(prefix) + "/%"
	if err := txFromCtx(ctx, r.base).
		Where(`user_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`, userID, prefix, like).
		Order("path").
		Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list subtree: %w", translateError(err))
	}
	return nodesToDomain(ms), nil
}

// CountChildren returns the number of direct children of a node.
func (r *nodeRepo) CountChildren(ctx context.Context, parentID uuid.UUID) (int64, error) {
	var n int64
	if err := txFromCtx(ctx, r.base).Model(&nodeModel{}).
		Where("parent_id = ?", parentID).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count children: %w", translateError(err))
	}
	return n, nil
}

// SumSizeByUser returns the total logical size of a user's file nodes.
func (r *nodeRepo) SumSizeByUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	var sum *int64
	if err := txFromCtx(ctx, r.base).Model(&nodeModel{}).
		Where("user_id = ? AND is_dir = false", userID).
		Select("COALESCE(SUM(size), 0)").
		Scan(&sum).Error; err != nil {
		return 0, fmt.Errorf("sum size by user: %w", translateError(err))
	}
	if sum == nil {
		return 0, nil
	}
	return *sum, nil
}

// ClaimBufferedForPacking atomically leases up to limit buffered nodes whose
// lease is free or expired, using FOR UPDATE SKIP LOCKED so concurrent packer
// workers never claim the same row. Leased rows have their owner and expiry set
// and are returned ordered by modified_at.
func (r *nodeRepo) ClaimBufferedForPacking(ctx context.Context, leaseOwner string, leaseFor time.Duration, limit int) ([]domain.Node, error) {
	if limit <= 0 {
		return nil, nil
	}
	leaseUntil := time.Now().Add(leaseFor)
	const q = `
UPDATE nodes
SET packer_lease_owner = ?, packer_lease_until = ?
WHERE id IN (
    SELECT id FROM nodes
    WHERE state = 'buffered'
      AND (packer_lease_until IS NULL OR packer_lease_until < now())
    ORDER BY modified_at
    LIMIT ?
    FOR UPDATE SKIP LOCKED
)
RETURNING *`
	var ms []nodeModel
	if err := txFromCtx(ctx, r.base).Raw(q, leaseOwner, leaseUntil, limit).Scan(&ms).Error; err != nil {
		return nil, fmt.Errorf("claim buffered nodes: %w", translateError(err))
	}
	return nodesToDomain(ms), nil
}

// ReleaseLease clears the packer lease on a node (e.g. after a failed pack).
func (r *nodeRepo) ReleaseLease(ctx context.Context, id uuid.UUID) error {
	res := txFromCtx(ctx, r.base).Model(&nodeModel{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"packer_lease_owner": "",
			"packer_lease_until": nil,
		})
	if res.Error != nil {
		return fmt.Errorf("release node lease: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("release node lease: %w", domain.ErrNotFound)
	}
	return nil
}

// MarkStoredIfOwner atomically marks a node stored and clears its lease only if
// it is still buffered and still owned by owner. Returns true iff a row changed.
func (r *nodeRepo) MarkStoredIfOwner(ctx context.Context, id uuid.UUID, owner string) (bool, error) {
	res := txFromCtx(ctx, r.base).Model(&nodeModel{}).
		Where("id = ? AND state = ? AND packer_lease_owner = ?", id, string(domain.NodeBuffered), owner).
		Updates(map[string]any{
			"state":              string(domain.NodeStored),
			"packer_lease_owner": "",
			"packer_lease_until": nil,
		})
	if res.Error != nil {
		return false, fmt.Errorf("mark stored if owner: %w", translateError(res.Error))
	}
	return res.RowsAffected > 0, nil
}

// escapeLike escapes LIKE metacharacters (\, %, _) using backslash as the
// escape character (paired with ESCAPE '\' in the query).
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func nodesToDomain(ms []nodeModel) []domain.Node {
	out := make([]domain.Node, len(ms))
	for i := range ms {
		out[i] = *ms[i].toDomain()
	}
	return out
}
