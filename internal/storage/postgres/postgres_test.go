package postgres

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// testKey is a fixed 32-byte AES-256 key used for bot-token encryption in tests.
var testKey = []byte("0123456789abcdef0123456789abcdef")

// chatIDCounter yields collision-free Telegram chat ids for test channels.
var chatIDCounter atomic.Int64

// nextChatID returns a unique, -100…-form-like negative chat id for tests.
func nextChatID() int64 {
	return -1_000_000_000_000 - chatIDCounter.Add(1)
}

// setup opens the live test database (gated on TGWEBDAV_TEST_DSN) and returns
// the repositories, tx manager and raw db. It skips the test when the DSN is
// unset so the suite is a no-op in environments without Postgres.
func setup(t *testing.T) (*domain.Repositories, domain.TxManager, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("TGWEBDAV_TEST_DSN")
	if dsn == "" {
		t.Skip("TGWEBDAV_TEST_DSN not set; skipping integration tests")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := Open(dsn, logger)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	repos, tx, err := New(db, testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return repos, tx, db
}

// newUser inserts a unique user and registers a cleanup that deletes it
// (cascading to nodes and tokens). It returns the created user.
func newUser(t *testing.T, ctx context.Context, repos *domain.Repositories) *domain.User {
	t.Helper()
	u := &domain.User{
		Login:        "test_" + uuid.NewString(),
		PasswordHash: "argon2id$dummy",
		QuotaBytes:   1 << 30,
	}
	if err := repos.Users.Create(ctx, u); err != nil {
		t.Fatalf("Users.Create: %v", err)
	}
	t.Cleanup(func() {
		_ = repos.Users.Delete(context.Background(), u.ID)
	})
	return u
}

func TestUserCreateGetLoginCount(t *testing.T) {
	repos, _, _ := setup(t)
	ctx := context.Background()

	before, err := repos.Users.Count(ctx)
	if err != nil {
		t.Fatalf("Count before: %v", err)
	}

	u := newUser(t, ctx, repos)

	got, err := repos.Users.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Login != u.Login || got.QuotaBytes != u.QuotaBytes {
		t.Fatalf("GetByID mismatch: got %+v want %+v", got, u)
	}

	byLogin, err := repos.Users.GetByLogin(ctx, u.Login)
	if err != nil {
		t.Fatalf("GetByLogin: %v", err)
	}
	if byLogin.ID != u.ID {
		t.Fatalf("GetByLogin id mismatch: %v != %v", byLogin.ID, u.ID)
	}

	after, err := repos.Users.Count(ctx)
	if err != nil {
		t.Fatalf("Count after: %v", err)
	}
	if after != before+1 {
		t.Fatalf("Count: got %d want %d", after, before+1)
	}

	// Duplicate login → ErrAlreadyExists.
	dup := &domain.User{Login: u.Login, PasswordHash: "x"}
	err = repos.Users.Create(ctx, dup)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate login: got %v want ErrAlreadyExists", err)
	}

	// Missing user → ErrNotFound.
	_, err = repos.Users.GetByID(ctx, uuid.New())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing user: got %v want ErrNotFound", err)
	}
}

func TestNodeCreateGetPathChildrenSubtree(t *testing.T) {
	repos, _, _ := setup(t)
	ctx := context.Background()
	u := newUser(t, ctx, repos)

	root := &domain.Node{
		UserID: u.ID, Name: "", Path: "/", IsDir: true, State: domain.NodeStored,
	}
	if err := repos.Nodes.Create(ctx, root); err != nil {
		t.Fatalf("create root: %v", err)
	}
	dir := &domain.Node{
		UserID: u.ID, ParentID: &root.ID, Name: "dir", Path: "/dir",
		IsDir: true, State: domain.NodeStored,
	}
	if err := repos.Nodes.Create(ctx, dir); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	file := &domain.Node{
		UserID: u.ID, ParentID: &dir.ID, Name: "f.txt", Path: "/dir/f.txt",
		IsDir: false, Size: 42, State: domain.NodeStored,
	}
	if err := repos.Nodes.Create(ctx, file); err != nil {
		t.Fatalf("create file: %v", err)
	}

	got, err := repos.Nodes.GetByPath(ctx, u.ID, "/dir/f.txt")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got.ID != file.ID || got.Size != 42 {
		t.Fatalf("GetByPath mismatch: %+v", got)
	}

	children, err := repos.Nodes.ListChildren(ctx, u.ID, dir.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 1 || children[0].ID != file.ID {
		t.Fatalf("ListChildren: got %d entries", len(children))
	}

	subtree, err := repos.Nodes.ListSubtree(ctx, u.ID, "/dir")
	if err != nil {
		t.Fatalf("ListSubtree: %v", err)
	}
	if len(subtree) != 2 {
		t.Fatalf("ListSubtree: got %d want 2 (%+v)", len(subtree), subtree)
	}

	sum, err := repos.Nodes.SumSizeByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("SumSizeByUser: %v", err)
	}
	if sum != 42 {
		t.Fatalf("SumSizeByUser: got %d want 42", sum)
	}

	n, err := repos.Nodes.CountChildren(ctx, dir.ID)
	if err != nil {
		t.Fatalf("CountChildren: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountChildren: got %d want 1", n)
	}
}

// newChannelBlob creates a channel + a stored blob and registers cleanup that
// deletes the blob then the channel (respecting the RESTRICT FK).
func newChannelBlob(t *testing.T, ctx context.Context, repos *domain.Repositories, db *gorm.DB) (*domain.Channel, *domain.Blob) {
	t.Helper()
	ch := &domain.Channel{
		TGChatID: nextChatID(),
		Title:    "test", Available: true, EvictionThreshold: 900000,
	}
	if err := repos.Channels.Create(ctx, ch); err != nil {
		t.Fatalf("Channels.Create: %v", err)
	}
	blob := &domain.Blob{
		ChannelID: ch.ID, MessageID: 1, MessageSeq: 1, Size: 100,
		State: domain.BlobStored, Refcount: 1,
	}
	if err := repos.Blobs.Create(ctx, blob); err != nil {
		t.Fatalf("Blobs.Create: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		// extents have a RESTRICT FK on blob_id; remove any that reference this
		// blob before deleting it (their owning nodes may not be gone yet).
		_ = db.Exec("DELETE FROM extents WHERE blob_id = ?", blob.ID).Error
		_ = repos.Blobs.Delete(bg, blob.ID)
		_ = repos.Channels.Delete(bg, ch.ID)
	})
	return ch, blob
}

func TestBlobRefcountAndCollectable(t *testing.T) {
	repos, _, db := setup(t)
	ctx := context.Background()
	_, blob := newChannelBlob(t, ctx, repos, db)

	if err := repos.Blobs.AddRefcount(ctx, blob.ID, -1); err != nil {
		t.Fatalf("AddRefcount: %v", err)
	}
	got, err := repos.Blobs.GetByID(ctx, blob.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Refcount != 0 {
		t.Fatalf("Refcount: got %d want 0", got.Refcount)
	}

	// ListCollectable applies a grace period on created_at to protect blobs
	// whose extents are still being written; backdate this one so it qualifies.
	if err := db.Exec("UPDATE blobs SET created_at = now() - interval '1 hour' WHERE id = ?", blob.ID).Error; err != nil {
		t.Fatalf("backdate blob: %v", err)
	}

	collectable, err := repos.Blobs.ListCollectable(ctx, 100)
	if err != nil {
		t.Fatalf("ListCollectable: %v", err)
	}
	found := false
	for _, b := range collectable {
		if b.ID == blob.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListCollectable did not include our zero-refcount stored blob")
	}
}

func TestExtentsCreateBatchAndSolelyOnBlob(t *testing.T) {
	repos, _, db := setup(t)
	ctx := context.Background()
	u := newUser(t, ctx, repos)
	_, blobA := newChannelBlob(t, ctx, repos, db)
	_, blobB := newChannelBlob(t, ctx, repos, db)

	// nodeA references only blobA; nodeB references both blobA and blobB.
	nodeA := &domain.Node{UserID: u.ID, Name: "a", Path: "/a", State: domain.NodeStored}
	nodeB := &domain.Node{UserID: u.ID, Name: "b", Path: "/b", State: domain.NodeStored}
	if err := repos.Nodes.Create(ctx, nodeA); err != nil {
		t.Fatalf("create nodeA: %v", err)
	}
	if err := repos.Nodes.Create(ctx, nodeB); err != nil {
		t.Fatalf("create nodeB: %v", err)
	}

	exts := []domain.Extent{
		{NodeID: nodeA.ID, Seq: 0, FileOffset: 0, Length: 50, BlobID: blobA.ID, BlobOffset: 0},
		{NodeID: nodeB.ID, Seq: 0, FileOffset: 0, Length: 50, BlobID: blobA.ID, BlobOffset: 50},
		{NodeID: nodeB.ID, Seq: 1, FileOffset: 50, Length: 50, BlobID: blobB.ID, BlobOffset: 0},
	}
	if err := repos.Extents.CreateBatch(ctx, exts); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	listA, err := repos.Extents.ListByNode(ctx, nodeA.ID)
	if err != nil {
		t.Fatalf("ListByNode: %v", err)
	}
	if len(listA) != 1 {
		t.Fatalf("ListByNode A: got %d want 1", len(listA))
	}

	blobIDs, err := repos.Extents.ListBlobIDsByNode(ctx, nodeB.ID)
	if err != nil {
		t.Fatalf("ListBlobIDsByNode: %v", err)
	}
	if len(blobIDs) != 2 {
		t.Fatalf("ListBlobIDsByNode B: got %d want 2", len(blobIDs))
	}

	solely, err := repos.Extents.ListNodesSolelyOnBlob(ctx, blobA.ID)
	if err != nil {
		t.Fatalf("ListNodesSolelyOnBlob: %v", err)
	}
	// Only nodeA is solely on blobA; nodeB also references blobB.
	if len(solely) != 1 || solely[0] != nodeA.ID {
		t.Fatalf("ListNodesSolelyOnBlob: got %v want [%v]", solely, nodeA.ID)
	}
}

func TestWALAppendReadSize(t *testing.T) {
	repos, _, _ := setup(t)
	ctx := context.Background()
	u := newUser(t, ctx, repos)
	node := &domain.Node{UserID: u.ID, Name: "w", Path: "/w", State: domain.NodeBuffered}
	if err := repos.Nodes.Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	chunks := [][]byte{[]byte("hello "), []byte("world"), []byte("!!!")}
	for i, data := range chunks {
		if err := repos.WAL.AppendChunk(ctx, &domain.WALChunk{
			NodeID: node.ID, Seq: int64(i), Data: data,
		}); err != nil {
			t.Fatalf("AppendChunk %d: %v", i, err)
		}
	}

	size, err := repos.WAL.SizeByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("SizeByNode: %v", err)
	}
	if size != 14 { // 6 + 5 + 3
		t.Fatalf("SizeByNode: got %d want 14", size)
	}

	// EachChunk should stream in seq order.
	var assembled []byte
	var lastSeq int64 = -1
	err = repos.WAL.EachChunk(ctx, node.ID, func(c domain.WALChunk) error {
		if c.Seq <= lastSeq {
			t.Fatalf("EachChunk out of order: seq %d after %d", c.Seq, lastSeq)
		}
		lastSeq = c.Seq
		assembled = append(assembled, c.Data...)
		return nil
	})
	if err != nil {
		t.Fatalf("EachChunk: %v", err)
	}
	if string(assembled) != "hello world!!!" {
		t.Fatalf("EachChunk assembled %q", assembled)
	}

	// ReadRange spanning chunk boundaries: bytes [3,10) of "hello world!!!".
	got, err := repos.WAL.ReadRange(ctx, node.ID, 3, 7)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if string(got) != "lo worl" {
		t.Fatalf("ReadRange: got %q want %q", got, "lo worl")
	}

	// ReadRange beyond EOF clamps.
	got, err = repos.WAL.ReadRange(ctx, node.ID, 11, 100)
	if err != nil {
		t.Fatalf("ReadRange tail: %v", err)
	}
	if string(got) != "!!!" {
		t.Fatalf("ReadRange tail: got %q want %q", got, "!!!")
	}

	if err := repos.WAL.DeleteByNode(ctx, node.ID); err != nil {
		t.Fatalf("DeleteByNode: %v", err)
	}
	size, err = repos.WAL.SizeByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("SizeByNode after delete: %v", err)
	}
	if size != 0 {
		t.Fatalf("SizeByNode after delete: got %d want 0", size)
	}
}

func TestWithTxRollback(t *testing.T) {
	repos, tx, _ := setup(t)
	ctx := context.Background()

	login := "rollback_" + uuid.NewString()
	sentinel := errors.New("force rollback")

	err := tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		u := &domain.User{Login: login, PasswordHash: "x"}
		if err := r.Users.Create(ctx, u); err != nil {
			return err
		}
		// Visible inside the same transaction.
		if _, err := r.Users.GetByLogin(ctx, login); err != nil {
			t.Fatalf("GetByLogin inside tx: %v", err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx: got %v want sentinel", err)
	}

	// After rollback the user must not exist.
	_, err = repos.Users.GetByLogin(ctx, login)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("post-rollback GetByLogin: got %v want ErrNotFound", err)
	}
}

func TestWithTxCommit(t *testing.T) {
	repos, tx, _ := setup(t)
	ctx := context.Background()

	login := "commit_" + uuid.NewString()
	var id uuid.UUID
	err := tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		u := &domain.User{Login: login, PasswordHash: "x"}
		if err := r.Users.Create(ctx, u); err != nil {
			return err
		}
		id = u.ID
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}
	t.Cleanup(func() { _ = repos.Users.Delete(context.Background(), id) })

	if _, err := repos.Users.GetByLogin(ctx, login); err != nil {
		t.Fatalf("post-commit GetByLogin: %v", err)
	}
}

func TestBotTokenRoundTrip(t *testing.T) {
	repos, _, _ := setup(t)
	ctx := context.Background()

	const token = "123456:ABCDEF_secret_token"
	bot := &domain.Bot{
		Username: "bot_" + uuid.NewString(),
		Token:    token,
		Enabled:  true,
	}
	if err := repos.Bots.Create(ctx, bot); err != nil {
		t.Fatalf("Bots.Create: %v", err)
	}
	t.Cleanup(func() { _ = repos.Bots.Delete(context.Background(), bot.ID) })

	got, err := repos.Bots.GetByID(ctx, bot.ID)
	if err != nil {
		t.Fatalf("Bots.GetByID: %v", err)
	}
	if got.Token != token {
		t.Fatalf("decrypted token mismatch: got %q want %q", got.Token, token)
	}
	if !got.Enabled {
		t.Fatalf("bot should be enabled")
	}
}

func TestChannelIncrementCounter(t *testing.T) {
	repos, _, _ := setup(t)
	ctx := context.Background()
	ch := &domain.Channel{
		TGChatID:  nextChatID(),
		Available: true,
	}
	if err := repos.Channels.Create(ctx, ch); err != nil {
		t.Fatalf("Channels.Create: %v", err)
	}
	t.Cleanup(func() { _ = repos.Channels.Delete(context.Background(), ch.ID) })

	n, err := repos.Channels.IncrementCounter(ctx, ch.ID, 5)
	if err != nil {
		t.Fatalf("IncrementCounter: %v", err)
	}
	if n != 5 {
		t.Fatalf("IncrementCounter: got %d want 5", n)
	}
	n, err = repos.Channels.IncrementCounter(ctx, ch.ID, 3)
	if err != nil {
		t.Fatalf("IncrementCounter 2: %v", err)
	}
	if n != 8 {
		t.Fatalf("IncrementCounter 2: got %d want 8", n)
	}
}

func TestSettingsDefaults(t *testing.T) {
	repos, _, _ := setup(t)
	ctx := context.Background()

	// The migration seeds a row; Get must return a fully-populated Settings.
	s, err := repos.Settings.Get(ctx)
	if err != nil {
		t.Fatalf("Settings.Get: %v", err)
	}
	if s.BlobMaxSize == 0 || s.WALIdleTimeout == 0 || s.DefaultEvictionThreshold == 0 {
		t.Fatalf("Settings.Get returned zero fields: %+v", s)
	}

	// Round-trip an update and restore afterwards.
	orig := s
	t.Cleanup(func() { _ = repos.Settings.Update(context.Background(), orig) })

	updated := orig
	updated.WALIdleTimeout = 7 * time.Second
	updated.MaxFileSize = 12345
	if err := repos.Settings.Update(ctx, updated); err != nil {
		t.Fatalf("Settings.Update: %v", err)
	}
	got, err := repos.Settings.Get(ctx)
	if err != nil {
		t.Fatalf("Settings.Get after update: %v", err)
	}
	if got.WALIdleTimeout != 7*time.Second {
		t.Fatalf("WALIdleTimeout: got %v want 7s", got.WALIdleTimeout)
	}
	if got.MaxFileSize != 12345 {
		t.Fatalf("MaxFileSize: got %d want 12345", got.MaxFileSize)
	}
}

func TestClaimBufferedForPacking(t *testing.T) {
	repos, _, _ := setup(t)
	ctx := context.Background()
	u := newUser(t, ctx, repos)

	node := &domain.Node{
		UserID: u.ID, Name: "buf", Path: "/buf",
		IsDir: false, State: domain.NodeBuffered,
		ModifiedAt: time.Now(),
	}
	if err := repos.Nodes.Create(ctx, node); err != nil {
		t.Fatalf("create buffered node: %v", err)
	}

	claimed, err := repos.Nodes.ClaimBufferedForPacking(ctx, "worker-1", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimBufferedForPacking: %v", err)
	}
	found := false
	for _, n := range claimed {
		if n.ID == node.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ClaimBufferedForPacking did not return our buffered node")
	}

	// A second immediate claim must NOT re-return the leased node.
	claimed2, err := repos.Nodes.ClaimBufferedForPacking(ctx, "worker-2", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimBufferedForPacking 2: %v", err)
	}
	for _, n := range claimed2 {
		if n.ID == node.ID {
			t.Fatalf("leased node was claimed twice")
		}
	}

	// Releasing the lease makes it claimable again.
	if err := repos.Nodes.ReleaseLease(ctx, node.ID); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	claimed3, err := repos.Nodes.ClaimBufferedForPacking(ctx, "worker-3", time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimBufferedForPacking 3: %v", err)
	}
	found = false
	for _, n := range claimed3 {
		if n.ID == node.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("released node was not re-claimable")
	}
}
