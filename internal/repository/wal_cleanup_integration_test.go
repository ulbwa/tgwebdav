package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
)

// insertBufferedNode inserts a buffered (state = NodeStateBuffered) file node so
// it is claimable for packing, returning its id.
func insertBufferedNode(t *testing.T, ctx context.Context, repo *NodeRepository, userID uuid.UUID, size int64) uuid.UUID {
	t.Helper()
	n := &model.Node{
		UserID: userID,
		Name:   "buffered",
		Path:   "/buffered-" + uuid.NewString(),
		IsDir:  false,
		Size:   size,
		State:  model.NodeStateBuffered,
	}
	if err := repo.Create(ctx, n); err != nil {
		t.Fatalf("create buffered node: %v", err)
	}
	return n.ID
}

// TestWALCleanup_FinalizeDeletesWALChunks proves the core "we don't permanently
// duplicate data in Postgres" invariant from the packer's finalize: once a node
// transitions to stored, ALL of its wal_chunks are gone. It reproduces finalize's
// atomic step — MarkStoredIfOwner followed by WAL DeleteByNode inside one
// transaction — and asserts both the byte size and the raw row count drop to 0.
func TestWALCleanup_FinalizeDeletesWALChunks(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	nodeRepo := NewNodeRepository(pool)
	walRepo := NewWALRepository(pool)
	tx := database.NewTxManager(pool)

	userID := insertUser(t, pool)
	nodeID := insertBufferedNode(t, ctx, nodeRepo, userID, 3*model.WALChunkSize)

	// Buffer several WAL chunks for the node (as a PUT would).
	for seq := int64(0); seq < 3; seq++ {
		if err := walRepo.AppendChunk(ctx, &model.WALChunk{
			NodeID: nodeID,
			Seq:    seq,
			Data:   []byte("chunk-payload"),
		}); err != nil {
			t.Fatalf("append wal chunk seq %d: %v", seq, err)
		}
	}
	if size, err := walRepo.SizeByNode(ctx, nodeID); err != nil || size == 0 {
		t.Fatalf("precondition: SizeByNode = (%d, %v), want (>0, nil)", size, err)
	}

	// A packer worker claims the node (taking the lease), then finalizes it:
	// MarkStoredIfOwner + WAL DeleteByNode atomically, exactly as Packer.finalize.
	const owner = "test-packer"
	claimed, err := nodeRepo.ClaimBufferedForPacking(ctx, owner, time.Minute, 10)
	if err != nil {
		t.Fatalf("claim buffered for packing: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != nodeID {
		t.Fatalf("claim returned %d nodes, want exactly our node %s", len(claimed), nodeID)
	}

	err = tx.WithTx(ctx, func(ctx context.Context) error {
		owned, err := nodeRepo.MarkStoredIfOwner(ctx, nodeID, owner)
		if err != nil {
			return err
		}
		if !owned {
			t.Fatal("MarkStoredIfOwner = false, want true (we hold the lease)")
		}
		return walRepo.DeleteByNode(ctx, nodeID)
	})
	if err != nil {
		t.Fatalf("finalize transaction: %v", err)
	}

	// Invariant 1: no buffered bytes remain for the stored node.
	if size, err := walRepo.SizeByNode(ctx, nodeID); err != nil || size != 0 {
		t.Fatalf("after finalize: SizeByNode = (%d, %v), want (0, nil)", size, err)
	}
	// Invariant 1 (raw row count): not a single wal_chunks row remains.
	if got := countWALRows(t, pool, nodeID); got != 0 {
		t.Fatalf("after finalize: wal_chunks rows = %d, want 0", got)
	}

	// The node is genuinely stored (state advanced), confirming this was a real
	// finalize and not just a WAL wipe.
	stored, err := nodeRepo.GetByID(ctx, nodeID)
	if err != nil {
		t.Fatalf("get node after finalize: %v", err)
	}
	if stored.State != model.NodeStateStored {
		t.Fatalf("node state = %v, want NodeStateStored", stored.State)
	}
}

// TestWALCleanup_DeleteNodeCascadesWALChunks proves the second leg of the
// invariant: removing a buffered/overwritten node (DELETE / overwrite) drops its
// wal_chunks via the FK ON DELETE CASCADE, so no orphaned buffered bytes linger.
func TestWALCleanup_DeleteNodeCascadesWALChunks(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	nodeRepo := NewNodeRepository(pool)
	walRepo := NewWALRepository(pool)

	userID := insertUser(t, pool)
	nodeID := insertBufferedNode(t, ctx, nodeRepo, userID, model.WALChunkSize)
	survivorID := insertBufferedNode(t, ctx, nodeRepo, userID, model.WALChunkSize)

	for seq := int64(0); seq < 4; seq++ {
		if err := walRepo.AppendChunk(ctx, &model.WALChunk{
			NodeID: nodeID,
			Seq:    seq,
			Data:   []byte("payload"),
		}); err != nil {
			t.Fatalf("append chunk seq %d: %v", seq, err)
		}
	}
	// A chunk on another node must survive the cascade.
	if err := walRepo.AppendChunk(ctx, &model.WALChunk{
		NodeID: survivorID,
		Seq:    0,
		Data:   []byte("keep"),
	}); err != nil {
		t.Fatalf("append survivor chunk: %v", err)
	}

	if got := countWALRows(t, pool, nodeID); got != 4 {
		t.Fatalf("precondition: wal_chunks rows = %d, want 4", got)
	}

	// Deleting the node must cascade-delete its wal_chunks (FK ON DELETE CASCADE).
	if err := nodeRepo.Delete(ctx, nodeID); err != nil {
		t.Fatalf("delete node: %v", err)
	}

	if got := countWALRows(t, pool, nodeID); got != 0 {
		t.Fatalf("after node delete: wal_chunks rows = %d, want 0 (cascade)", got)
	}
	if size, err := walRepo.SizeByNode(ctx, nodeID); err != nil || size != 0 {
		t.Fatalf("after node delete: SizeByNode = (%d, %v), want (0, nil)", size, err)
	}
	// The other node's chunk is untouched.
	if got := countWALRows(t, pool, survivorID); got != 1 {
		t.Fatalf("survivor node wal_chunks rows = %d, want 1", got)
	}
}

// countWALRows returns the raw number of wal_chunks rows for nodeID, querying the
// table directly so the test is independent of the repository's own counting.
func countWALRows(t *testing.T, pool *pgxpool.Pool, nodeID uuid.UUID) int64 {
	t.Helper()
	var n int64
	row := pool.QueryRow(context.Background(), "SELECT count(*) FROM wal_chunks WHERE node_id = $1", nodeID)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count wal_chunks rows: %v", err)
	}
	return n
}
