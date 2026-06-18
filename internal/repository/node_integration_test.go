package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// insertUser inserts a user row directly so node tests have a valid FK parent.
func insertUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := sqlc.New(pool).CreateUser(context.Background(), sqlc.CreateUserParams{
		ID:           id,
		Login:        "user-" + id.String(),
		PasswordHash: "x",
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// newFileNode builds a stored file node with the given path/size under userID.
func newFileNode(userID uuid.UUID, path string, size int64) *model.Node {
	return &model.Node{
		UserID:      userID,
		Name:        path,
		Path:        path,
		IsDir:       false,
		Size:        size,
		ContentType: "application/octet-stream",
		State:       model.NodeStateStored,
	}
}

func TestNodeRepository_CRUD(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	n := newFileNode(userID, "/file.txt", 10)
	n.ContentHash = "abc"
	n.ETag = "etag1"
	if err := repo.Create(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n.ID == uuid.Nil {
		t.Fatal("create: id not assigned")
	}
	if n.CreatedAt.IsZero() || n.ModifiedAt.IsZero() {
		t.Fatal("create: timestamps not set")
	}

	got, err := repo.GetByID(ctx, n.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Path != "/file.txt" || got.Size != 10 || got.ContentHash != "abc" || got.ETag != "etag1" {
		t.Fatalf("get by id mismatch: %+v", got)
	}
	if got.State != model.NodeStateStored {
		t.Fatalf("get by id state = %v, want stored", got.State)
	}
	if got.ParentID != nil {
		t.Fatalf("get by id parent = %v, want nil", got.ParentID)
	}

	// Update mutable columns.
	got.Size = 20
	got.ETag = "etag2"
	got.State = model.NodeStateBuffered
	got.ModifiedAt = time.Now().Add(time.Second)
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reloaded, err := repo.GetByID(ctx, n.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if reloaded.Size != 20 || reloaded.ETag != "etag2" || reloaded.State != model.NodeStateBuffered {
		t.Fatalf("update not applied: %+v", reloaded)
	}

	// Delete.
	if err := repo.Delete(ctx, n.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
}

func TestNodeRepository_NotFound(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	if _, err := repo.GetByID(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID missing = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByPath(ctx, userID, "/nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByPath missing = %v, want ErrNotFound", err)
	}
	if err := repo.Update(ctx, newFileNode(userID, "/ghost", 0)); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update missing = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing = %v, want ErrNotFound", err)
	}
	if err := repo.ReleaseLease(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReleaseLease missing = %v, want ErrNotFound", err)
	}
}

func TestNodeRepository_GetByPath(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userA := insertUser(t, pool)
	userB := insertUser(t, pool)

	a := newFileNode(userA, "/shared.txt", 1)
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create A: %v", err)
	}
	b := newFileNode(userB, "/shared.txt", 2)
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("create B: %v", err)
	}

	got, err := repo.GetByPath(ctx, userA, "/shared.txt")
	if err != nil {
		t.Fatalf("get by path: %v", err)
	}
	if got.ID != a.ID {
		t.Fatalf("get by path returned wrong user's node: %v want %v", got.ID, a.ID)
	}
}

func TestNodeRepository_ListChildrenAndCount(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	dir := &model.Node{UserID: userID, Name: "dir", Path: "/dir", IsDir: true, State: model.NodeStateStored}
	if err := repo.Create(ctx, dir); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	// Children intentionally created out of name order to prove ORDER BY name.
	for _, name := range []string{"c.txt", "a.txt", "b.txt"} {
		child := newFileNode(userID, "/dir/"+name, 1)
		child.Name = name
		child.ParentID = &dir.ID
		if err := repo.Create(ctx, child); err != nil {
			t.Fatalf("create child %s: %v", name, err)
		}
	}
	// A node under a different parent must not appear.
	other := newFileNode(userID, "/other.txt", 1)
	if err := repo.Create(ctx, other); err != nil {
		t.Fatalf("create other: %v", err)
	}

	children, err := repo.ListChildren(ctx, userID, dir.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("list children len = %d, want 3", len(children))
	}
	wantOrder := []string{"a.txt", "b.txt", "c.txt"}
	for i, c := range children {
		if c.Name != wantOrder[i] {
			t.Fatalf("children[%d].Name = %q, want %q (order)", i, c.Name, wantOrder[i])
		}
		if c.ParentID == nil || *c.ParentID != dir.ID {
			t.Fatalf("children[%d].ParentID = %v, want %v", i, c.ParentID, dir.ID)
		}
	}

	count, err := repo.CountChildren(ctx, dir.ID)
	if err != nil {
		t.Fatalf("count children: %v", err)
	}
	if count != 3 {
		t.Fatalf("count children = %d, want 3", count)
	}
}

func TestNodeRepository_ListSubtree(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	paths := []string{"/a", "/a/b", "/a/b/c.txt", "/a/d.txt", "/ab", "/z"}
	for _, p := range paths {
		n := newFileNode(userID, p, 1)
		n.IsDir = p == "/a" || p == "/a/b" || p == "/ab"
		if err := repo.Create(ctx, n); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}

	sub, err := repo.ListSubtree(ctx, userID, "/a")
	if err != nil {
		t.Fatalf("list subtree: %v", err)
	}
	// Should match "/a" and its descendants, but NOT "/ab" (no false prefix
	// match) and NOT "/z". Ordered by path.
	want := []string{"/a", "/a/b", "/a/b/c.txt", "/a/d.txt"}
	if len(sub) != len(want) {
		var got []string
		for _, n := range sub {
			got = append(got, n.Path)
		}
		t.Fatalf("subtree paths = %v, want %v", got, want)
	}
	for i, n := range sub {
		if n.Path != want[i] {
			t.Fatalf("subtree[%d].Path = %q, want %q", i, n.Path, want[i])
		}
	}
}

func TestNodeRepository_ListSubtree_EscapesWildcards(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	// "/a%" used as a prefix must match literally, not as a LIKE wildcard.
	for _, p := range []string{"/a%", "/a%/child.txt", "/axyz"} {
		n := newFileNode(userID, p, 1)
		n.IsDir = p == "/a%"
		if err := repo.Create(ctx, n); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}

	sub, err := repo.ListSubtree(ctx, userID, "/a%")
	if err != nil {
		t.Fatalf("list subtree: %v", err)
	}
	want := []string{"/a%", "/a%/child.txt"}
	if len(sub) != len(want) {
		var got []string
		for _, n := range sub {
			got = append(got, n.Path)
		}
		t.Fatalf("escaped subtree paths = %v, want %v (/axyz must not match)", got, want)
	}
}

func TestNodeRepository_SumSizeByUser(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)
	otherUser := insertUser(t, pool)

	// File nodes count; directories do not.
	for _, spec := range []struct {
		path  string
		size  int64
		isDir bool
	}{
		{"/f1", 100, false},
		{"/f2", 250, false},
		{"/d1", 9999, true}, // directory: excluded
	} {
		n := newFileNode(userID, spec.path, spec.size)
		n.IsDir = spec.isDir
		if err := repo.Create(ctx, n); err != nil {
			t.Fatalf("create %s: %v", spec.path, err)
		}
	}
	// Another user's file must not be summed.
	if err := repo.Create(ctx, newFileNode(otherUser, "/x", 500)); err != nil {
		t.Fatalf("create other user file: %v", err)
	}

	sum, err := repo.SumSizeByUser(ctx, userID)
	if err != nil {
		t.Fatalf("sum size: %v", err)
	}
	if sum != 350 {
		t.Fatalf("sum size = %d, want 350", sum)
	}

	// User with no files sums to zero (COALESCE).
	empty := insertUser(t, pool)
	if got, err := repo.SumSizeByUser(ctx, empty); err != nil || got != 0 {
		t.Fatalf("sum size empty user = (%d, %v), want (0, nil)", got, err)
	}
}

// createBufferedNode inserts a buffered file node ready to be packed.
func createBufferedNode(t *testing.T, repo *NodeRepository, userID uuid.UUID, path string) *model.Node {
	t.Helper()
	n := newFileNode(userID, path, 1)
	n.State = model.NodeStateBuffered
	if err := repo.Create(context.Background(), n); err != nil {
		t.Fatalf("create buffered node %s: %v", path, err)
	}
	return n
}

func TestNodeRepository_ClaimBufferedForPacking_DisjointConcurrent(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	const total = 6
	for i := 0; i < total; i++ {
		createBufferedNode(t, repo, userID, "/buf"+uuid.NewString())
	}

	// Two concurrent claims with different owners must get disjoint sets, proving
	// FOR UPDATE SKIP LOCKED. Each claims up to 3.
	type result struct {
		nodes []model.Node
		err   error
	}
	resCh := make(chan result, 2)
	for _, owner := range []string{"owner-A", "owner-B"} {
		owner := owner
		go func() {
			nodes, err := repo.ClaimBufferedForPacking(ctx, owner, time.Minute, 3)
			resCh <- result{nodes, err}
		}()
	}
	r1 := <-resCh
	r2 := <-resCh
	if r1.err != nil || r2.err != nil {
		t.Fatalf("claim errors: %v / %v", r1.err, r2.err)
	}

	seen := map[uuid.UUID]bool{}
	for _, n := range append(append([]model.Node{}, r1.nodes...), r2.nodes...) {
		if seen[n.ID] {
			t.Fatalf("node %v claimed by both owners (not disjoint)", n.ID)
		}
		seen[n.ID] = true
		if n.State != model.NodeStateBuffered {
			t.Fatalf("claimed node state = %v, want buffered", n.State)
		}
	}
	// All 6 buffered nodes should have been claimed across the two owners.
	if len(seen) != total {
		t.Fatalf("claimed %d distinct nodes, want %d", len(seen), total)
	}
}

func TestNodeRepository_ClaimBufferedForPacking_Limit(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	for i := 0; i < 5; i++ {
		createBufferedNode(t, repo, userID, "/lim"+uuid.NewString())
	}
	claimed, err := repo.ClaimBufferedForPacking(ctx, "owner", time.Minute, 2)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d nodes, want limit 2", len(claimed))
	}
	// limit <= 0 returns nil without touching the DB.
	if got, err := repo.ClaimBufferedForPacking(ctx, "owner", time.Minute, 0); err != nil || got != nil {
		t.Fatalf("claim limit 0 = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestNodeRepository_ClaimBufferedForPacking_ExpiredLeaseReclaim(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	n := createBufferedNode(t, repo, userID, "/expire")

	// Claim with a lease that is already expired (negative duration).
	first, err := repo.ClaimBufferedForPacking(ctx, "stale-owner", -time.Minute, 10)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 || first[0].ID != n.ID {
		t.Fatalf("first claim = %+v, want the one buffered node", first)
	}

	// A second claim by a fresh owner must re-acquire it because the prior lease
	// is expired.
	second, err := repo.ClaimBufferedForPacking(ctx, "fresh-owner", time.Minute, 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 1 || second[0].ID != n.ID {
		t.Fatalf("expired-lease reclaim = %+v, want the node re-claimed", second)
	}

	// Now the lease is valid: a third claim gets nothing.
	third, err := repo.ClaimBufferedForPacking(ctx, "late-owner", time.Minute, 10)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if len(third) != 0 {
		t.Fatalf("third claim = %d nodes, want 0 (lease still valid)", len(third))
	}
}

func TestNodeRepository_MarkStoredIfOwner(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	n := createBufferedNode(t, repo, userID, "/store")
	claimed, err := repo.ClaimBufferedForPacking(ctx, "owner", time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: (%v, %v)", claimed, err)
	}

	// Wrong owner cannot finalize.
	ok, err := repo.MarkStoredIfOwner(ctx, n.ID, "not-owner")
	if err != nil {
		t.Fatalf("mark wrong owner err: %v", err)
	}
	if ok {
		t.Fatal("mark stored succeeded for wrong owner, want false")
	}

	// Correct owner finalizes exactly once.
	ok, err = repo.MarkStoredIfOwner(ctx, n.ID, "owner")
	if err != nil {
		t.Fatalf("mark owner err: %v", err)
	}
	if !ok {
		t.Fatal("mark stored = false for owner, want true")
	}
	reloaded, err := repo.GetByID(ctx, n.ID)
	if err != nil {
		t.Fatalf("get after mark: %v", err)
	}
	if reloaded.State != model.NodeStateStored {
		t.Fatalf("state after mark = %v, want stored", reloaded.State)
	}

	// Second call is a no-op (already stored, lease cleared): false, no error.
	ok, err = repo.MarkStoredIfOwner(ctx, n.ID, "owner")
	if err != nil {
		t.Fatalf("second mark err: %v", err)
	}
	if ok {
		t.Fatal("second mark stored = true, want false (idempotent guard)")
	}

	// Missing id is also (false, nil), never ErrNotFound.
	ok, err = repo.MarkStoredIfOwner(ctx, uuid.New(), "owner")
	if err != nil || ok {
		t.Fatalf("mark missing = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestNodeRepository_ReleaseLease(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewNodeRepository(pool)
	ctx := context.Background()
	userID := insertUser(t, pool)

	n := createBufferedNode(t, repo, userID, "/release")
	if _, err := repo.ClaimBufferedForPacking(ctx, "owner", time.Minute, 1); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// After releasing, the node is immediately re-claimable by another owner.
	if err := repo.ReleaseLease(ctx, n.ID); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	reclaimed, err := repo.ClaimBufferedForPacking(ctx, "other", time.Minute, 1)
	if err != nil {
		t.Fatalf("reclaim after release: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID != n.ID {
		t.Fatalf("reclaim after release = %+v, want the node", reclaimed)
	}
}
