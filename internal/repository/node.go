package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// NodeRepository persists filesystem nodes via sqlc. It resolves its executor
// per call through database.FromContext, so the same value works inside a
// transaction (TxManager.WithTx) and against the pool otherwise.
type NodeRepository struct {
	pool *pgxpool.Pool
}

// NewNodeRepository returns a NodeRepository bound to pool as its fallback
// executor.
func NewNodeRepository(pool *pgxpool.Pool) *NodeRepository {
	return &NodeRepository{pool: pool}
}

// Create inserts a new filesystem node, assigning an id and timestamps when the
// caller left them zero.
func (r *NodeRepository) Create(ctx context.Context, n *model.Node) error {
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
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).CreateNode(ctx, sqlc.CreateNodeParams{
		ID:          n.ID,
		UserID:      n.UserID,
		ParentID:    ptrToUUID(n.ParentID),
		Name:        n.Name,
		Path:        n.Path,
		IsDir:       n.IsDir,
		Size:        n.Size,
		ContentHash: n.ContentHash,
		Etag:        n.ETag,
		ContentType: n.ContentType,
		State:       int32(n.State),
		CreatedAt:   pgtype.Timestamptz{Time: n.CreatedAt, Valid: true},
		ModifiedAt:  pgtype.Timestamptz{Time: n.ModifiedAt, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("create node: %w", translateError(err))
	}
	return nil
}

// Update saves the mutable columns of a node. The packer-lease columns are
// managed separately (ClaimBufferedForPacking / ReleaseLease / MarkStoredIfOwner)
// and are intentionally not touched here.
func (r *NodeRepository) Update(ctx context.Context, n *model.Node) error {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).UpdateNode(ctx, sqlc.UpdateNodeParams{
		ID:          n.ID,
		ParentID:    ptrToUUID(n.ParentID),
		Name:        n.Name,
		Path:        n.Path,
		IsDir:       n.IsDir,
		Size:        n.Size,
		ContentHash: n.ContentHash,
		Etag:        n.ETag,
		ContentType: n.ContentType,
		State:       int32(n.State),
		ModifiedAt:  pgtype.Timestamptz{Time: n.ModifiedAt, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("update node: %w", translateError(err))
	}
	if rows == 0 {
		return fmt.Errorf("update node: %w", ErrNotFound)
	}
	return nil
}

// Delete removes a node by id (cascades to children, extents and wal_chunks).
func (r *NodeRepository) Delete(ctx context.Context, id uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).DeleteNode(ctx, id)
	if err != nil {
		return fmt.Errorf("delete node: %w", translateError(err))
	}
	if rows == 0 {
		return fmt.Errorf("delete node: %w", ErrNotFound)
	}
	return nil
}

// GetByID loads a node by primary key.
func (r *NodeRepository) GetByID(ctx context.Context, id uuid.UUID) (*model.Node, error) {
	db := database.FromContext(ctx, r.pool)
	row, err := sqlc.New(db).GetNodeByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get node by id: %w", translateError(err))
	}
	return nodeToModel(row), nil
}

// GetByPath loads a node by its normalized path within a user's namespace.
func (r *NodeRepository) GetByPath(ctx context.Context, userID uuid.UUID, path string) (*model.Node, error) {
	db := database.FromContext(ctx, r.pool)
	row, err := sqlc.New(db).GetNodeByPath(ctx, sqlc.GetNodeByPathParams{
		UserID: userID,
		Path:   path,
	})
	if err != nil {
		return nil, fmt.Errorf("get node by path: %w", translateError(err))
	}
	return nodeToModel(row), nil
}

// ListChildren returns the direct children of a directory node, ordered by name.
func (r *NodeRepository) ListChildren(ctx context.Context, userID uuid.UUID, parentID uuid.UUID) ([]model.Node, error) {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ListChildren(ctx, sqlc.ListChildrenParams{
		UserID:   userID,
		ParentID: pgtype.UUID{Bytes: parentID, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("list children: %w", translateError(err))
	}
	return nodesToModel(rows), nil
}

// ListSubtree returns the node at prefix plus all of its descendants (path =
// prefix or path LIKE prefix/%), ordered by path. LIKE wildcards in the prefix
// are escaped so paths containing %, _ or the escape character match literally.
func (r *NodeRepository) ListSubtree(ctx context.Context, userID uuid.UUID, prefix string) ([]model.Node, error) {
	db := database.FromContext(ctx, r.pool)
	like := escapeLike(prefix) + "/%"
	rows, err := sqlc.New(db).ListSubtree(ctx, sqlc.ListSubtreeParams{
		UserID:     userID,
		Path:       prefix,
		LikeEscape: []byte(like),
	})
	if err != nil {
		return nil, fmt.Errorf("list subtree: %w", translateError(err))
	}
	return nodesToModel(rows), nil
}

// CountChildren returns the number of direct children of a node.
func (r *NodeRepository) CountChildren(ctx context.Context, parentID uuid.UUID) (int64, error) {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).CountChildren(ctx, pgtype.UUID{Bytes: parentID, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("count children: %w", translateError(err))
	}
	return n, nil
}

// SumSizeByUser returns the total logical size of a user's file nodes
// (directories are excluded).
func (r *NodeRepository) SumSizeByUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	db := database.FromContext(ctx, r.pool)
	sum, err := sqlc.New(db).SumSizeByUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("sum size by user: %w", translateError(err))
	}
	return sum, nil
}

// ClaimBufferedForPacking atomically leases up to limit buffered nodes whose
// lease is free or expired and returns them. The underlying query uses FOR
// UPDATE SKIP LOCKED, so concurrent packer workers never claim the same row.
// Leases expire leaseFor after now.
func (r *NodeRepository) ClaimBufferedForPacking(ctx context.Context, leaseOwner string, leaseFor time.Duration, limit int) ([]model.Node, error) {
	if limit <= 0 {
		return nil, nil
	}
	leaseExpires := time.Now().Add(leaseFor)
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ClaimBufferedForPacking(ctx, sqlc.ClaimBufferedForPackingParams{
		PackerLeaseOwner: leaseOwner,
		PackerLeaseUntil: pgtype.Timestamptz{Time: leaseExpires, Valid: true},
		Limit:            int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("claim buffered for packing: %w", translateError(err))
	}
	return nodesToModel(rows), nil
}

// ReleaseLease clears the packer lease on a node (e.g. after a failed pack).
func (r *NodeRepository) ReleaseLease(ctx context.Context, id uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ReleaseLease(ctx, id)
	if err != nil {
		return fmt.Errorf("release lease: %w", translateError(err))
	}
	if rows == 0 {
		return fmt.Errorf("release lease: %w", ErrNotFound)
	}
	return nil
}

// MarkStoredIfOwner atomically transitions a node from buffered to stored and
// clears its lease, but only if it is still buffered and still leased by owner.
// It reports whether the transition happened: false means another worker already
// finalized it or stole the lease. A no-rows result is the expected negative
// outcome and is mapped to (false, nil) rather than ErrNotFound.
func (r *NodeRepository) MarkStoredIfOwner(ctx context.Context, id uuid.UUID, owner string) (bool, error) {
	db := database.FromContext(ctx, r.pool)
	_, err := sqlc.New(db).MarkStoredIfOwner(ctx, sqlc.MarkStoredIfOwnerParams{
		ID:               id,
		PackerLeaseOwner: owner,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("mark stored if owner: %w", translateError(err))
	}
	return true, nil
}

// escapeLike escapes LIKE metacharacters (\, %, _) using backslash as the escape
// character, paired with ESCAPE '\' in the ListSubtree query.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// nodeToModel maps a sqlc Node row onto the domain model, converting the int32
// state column to NodeState and the nullable parent_id/timestamp columns via the
// shared convert helpers.
func nodeToModel(n sqlc.Node) *model.Node {
	return &model.Node{
		ID:          n.ID,
		UserID:      n.UserID,
		ParentID:    uuidToPtr(n.ParentID),
		Name:        n.Name,
		Path:        n.Path,
		IsDir:       n.IsDir,
		Size:        n.Size,
		ContentHash: n.ContentHash,
		ETag:        n.Etag,
		ContentType: n.ContentType,
		State:       model.NodeState(n.State),
		CreatedAt:   n.CreatedAt.Time,
		ModifiedAt:  n.ModifiedAt.Time,
	}
}

func nodesToModel(rows []sqlc.Node) []model.Node {
	out := make([]model.Node, len(rows))
	for i := range rows {
		out[i] = *nodeToModel(rows[i])
	}
	return out
}
