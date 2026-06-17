package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// insertTestUser inserts a user row (the FK parent of nodes) and returns its id.
func insertTestUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := sqlc.New(pool).CreateUser(ctx, sqlc.CreateUserParams{
		ID:           id,
		Login:        "user-" + id.String(),
		PasswordHash: "x",
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	return id
}

// insertTestNode inserts a file node owned by userID and returns its id.
func insertTestNode(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, path string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := sqlc.New(pool).CreateNode(ctx, sqlc.CreateNodeParams{
		ID:         id,
		UserID:     userID,
		ParentID:   pgtype.UUID{}, // NULL parent (root-level)
		Name:       path,
		Path:       path,
		IsDir:      false,
		State:      int32(model.NodeStateStored),
		CreatedAt:  pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ModifiedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("insert test node %q: %v", path, err)
	}
	return id
}

// insertTestBlob inserts a stored blob in channelID and returns its id.
func insertTestBlob(ctx context.Context, t *testing.T, pool *pgxpool.Pool, channelID uuid.UUID) uuid.UUID {
	t.Helper()
	b := &model.Blob{ChannelID: channelID, State: model.BlobStateStored}
	if err := NewBlobRepository(pool).Create(ctx, b); err != nil {
		t.Fatalf("insert test blob: %v", err)
	}
	return b.ID
}

func TestExtentRepository_CreateBatchAndListByNode(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewExtentRepository(pool)

	channelID := insertTestChannel(ctx, t, pool)
	userID := insertTestUser(ctx, t, pool)
	nodeID := insertTestNode(ctx, t, pool, userID, "/f")
	blobID := insertTestBlob(ctx, t, pool, channelID)

	// CreateBatch on an empty slice is a no-op.
	if err := repo.CreateBatch(ctx, nil); err != nil {
		t.Fatalf("CreateBatch(nil): %v", err)
	}

	extents := []model.Extent{
		{NodeID: nodeID, Seq: 2, FileOffset: 100, Length: 50, BlobID: blobID, BlobOffset: 0},
		{NodeID: nodeID, Seq: 0, FileOffset: 0, Length: 60, BlobID: blobID, BlobOffset: 200},
		{NodeID: nodeID, Seq: 1, FileOffset: 60, Length: 40, BlobID: blobID, BlobOffset: 260},
	}
	if err := repo.CreateBatch(ctx, extents); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	// CreateBatch must back-fill generated ids on the caller's slice.
	for i := range extents {
		if extents[i].ID == uuid.Nil {
			t.Fatalf("CreateBatch did not assign id at %d", i)
		}
	}

	got, err := repo.ListByNode(ctx, nodeID)
	if err != nil {
		t.Fatalf("ListByNode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListByNode returned %d, want 3", len(got))
	}
	// Ordered by seq ascending.
	for i, want := range []int64{0, 1, 2} {
		if got[i].Seq != want {
			t.Fatalf("ListByNode[%d].Seq = %d, want %d", i, got[i].Seq, want)
		}
	}
	// Round-trip the seq=0 row fully.
	if got[0].FileOffset != 0 || got[0].Length != 60 || got[0].BlobOffset != 200 || got[0].BlobID != blobID {
		t.Fatalf("seq=0 extent mismatch: %+v", got[0])
	}
}

func TestExtentRepository_DeleteByNode(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewExtentRepository(pool)

	channelID := insertTestChannel(ctx, t, pool)
	userID := insertTestUser(ctx, t, pool)
	nodeID := insertTestNode(ctx, t, pool, userID, "/f")
	blobID := insertTestBlob(ctx, t, pool, channelID)

	if err := repo.CreateBatch(ctx, []model.Extent{
		{NodeID: nodeID, Seq: 0, Length: 1, BlobID: blobID},
		{NodeID: nodeID, Seq: 1, Length: 1, BlobID: blobID},
	}); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if err := repo.DeleteByNode(ctx, nodeID); err != nil {
		t.Fatalf("DeleteByNode: %v", err)
	}
	got, err := repo.ListByNode(ctx, nodeID)
	if err != nil {
		t.Fatalf("ListByNode: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListByNode after delete returned %d, want 0", len(got))
	}
	// DeleteByNode on a node with no extents is a no-op (no error).
	if err := repo.DeleteByNode(ctx, nodeID); err != nil {
		t.Fatalf("DeleteByNode (empty): %v", err)
	}
}

func TestExtentRepository_ListBlobIDsByNode(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewExtentRepository(pool)

	channelID := insertTestChannel(ctx, t, pool)
	userID := insertTestUser(ctx, t, pool)
	nodeID := insertTestNode(ctx, t, pool, userID, "/f")
	blobA := insertTestBlob(ctx, t, pool, channelID)
	blobB := insertTestBlob(ctx, t, pool, channelID)

	// Two extents on blobA, one on blobB → distinct should yield 2 ids.
	if err := repo.CreateBatch(ctx, []model.Extent{
		{NodeID: nodeID, Seq: 0, Length: 1, BlobID: blobA},
		{NodeID: nodeID, Seq: 1, Length: 1, BlobID: blobA},
		{NodeID: nodeID, Seq: 2, Length: 1, BlobID: blobB},
	}); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	ids, err := repo.ListBlobIDsByNode(ctx, nodeID)
	if err != nil {
		t.Fatalf("ListBlobIDsByNode: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ListBlobIDsByNode returned %d distinct ids, want 2: %v", len(ids), ids)
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen[blobA] || !seen[blobB] {
		t.Fatalf("ListBlobIDsByNode missing an id: got %v, want {%s,%s}", ids, blobA, blobB)
	}
}

func TestExtentRepository_CopyForNode(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewExtentRepository(pool)

	channelID := insertTestChannel(ctx, t, pool)
	userID := insertTestUser(ctx, t, pool)
	src := insertTestNode(ctx, t, pool, userID, "/src")
	dst := insertTestNode(ctx, t, pool, userID, "/dst")
	blobID := insertTestBlob(ctx, t, pool, channelID)

	srcExtents := []model.Extent{
		{NodeID: src, Seq: 0, FileOffset: 0, Length: 10, BlobID: blobID, BlobOffset: 5},
		{NodeID: src, Seq: 1, FileOffset: 10, Length: 20, BlobID: blobID, BlobOffset: 15},
	}
	if err := repo.CreateBatch(ctx, srcExtents); err != nil {
		t.Fatalf("CreateBatch src: %v", err)
	}

	if err := repo.CopyForNode(ctx, src, dst); err != nil {
		t.Fatalf("CopyForNode: %v", err)
	}

	dstExtents, err := repo.ListByNode(ctx, dst)
	if err != nil {
		t.Fatalf("ListByNode dst: %v", err)
	}
	if len(dstExtents) != len(srcExtents) {
		t.Fatalf("dst has %d extents, want %d", len(dstExtents), len(srcExtents))
	}
	for i, e := range dstExtents {
		if e.NodeID != dst {
			t.Fatalf("dst extent %d has NodeID %s, want %s", i, e.NodeID, dst)
		}
		// New ids must be generated (gen_random_uuid), not copied from src.
		if e.ID == srcExtents[i].ID {
			t.Fatalf("dst extent %d reused src id %s", i, e.ID)
		}
		// Payload columns must match the source row at the same seq.
		if e.Seq != srcExtents[i].Seq || e.FileOffset != srcExtents[i].FileOffset ||
			e.Length != srcExtents[i].Length || e.BlobID != srcExtents[i].BlobID ||
			e.BlobOffset != srcExtents[i].BlobOffset {
			t.Fatalf("dst extent %d payload mismatch: %+v vs %+v", i, e, srcExtents[i])
		}
	}
	// Source extents must be untouched.
	srcAfter, err := repo.ListByNode(ctx, src)
	if err != nil {
		t.Fatalf("ListByNode src after copy: %v", err)
	}
	if len(srcAfter) != len(srcExtents) {
		t.Fatalf("src has %d extents after copy, want %d", len(srcAfter), len(srcExtents))
	}
}

func TestExtentRepository_ListNodesSolelyOnBlob(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewExtentRepository(pool)

	channelID := insertTestChannel(ctx, t, pool)
	userID := insertTestUser(ctx, t, pool)
	target := insertTestBlob(ctx, t, pool, channelID)
	other := insertTestBlob(ctx, t, pool, channelID)

	// soleNode: every extent references only the target blob → included.
	soleNode := insertTestNode(ctx, t, pool, userID, "/sole")
	if err := repo.CreateBatch(ctx, []model.Extent{
		{NodeID: soleNode, Seq: 0, Length: 1, BlobID: target},
		{NodeID: soleNode, Seq: 1, Length: 1, BlobID: target},
	}); err != nil {
		t.Fatalf("CreateBatch sole: %v", err)
	}

	// mixedNode: references target AND another blob → excluded.
	mixedNode := insertTestNode(ctx, t, pool, userID, "/mixed")
	if err := repo.CreateBatch(ctx, []model.Extent{
		{NodeID: mixedNode, Seq: 0, Length: 1, BlobID: target},
		{NodeID: mixedNode, Seq: 1, Length: 1, BlobID: other},
	}); err != nil {
		t.Fatalf("CreateBatch mixed: %v", err)
	}

	// otherNode: references only the other blob → excluded.
	otherNode := insertTestNode(ctx, t, pool, userID, "/other")
	if err := repo.CreateBatch(ctx, []model.Extent{
		{NodeID: otherNode, Seq: 0, Length: 1, BlobID: other},
	}); err != nil {
		t.Fatalf("CreateBatch other: %v", err)
	}

	ids, err := repo.ListNodesSolelyOnBlob(ctx, target)
	if err != nil {
		t.Fatalf("ListNodesSolelyOnBlob: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("ListNodesSolelyOnBlob returned %d ids, want 1: %v", len(ids), ids)
	}
	if ids[0] != soleNode {
		t.Fatalf("ListNodesSolelyOnBlob returned %s, want soleNode %s", ids[0], soleNode)
	}
}
