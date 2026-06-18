package webdavfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
	"github.com/ulbwa/tgwebdav/internal/service"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"":                   "/",
		"/":                  "/",
		"/foo/":              "/foo",
		"foo/bar":            "/foo/bar",
		"/foo/../bar":        "/bar",
		"/../etc/passwd":     "/etc/passwd", // cannot escape root
		"/../../../../x":     "/x",
		"/a//b":              "/a/b",
		"/foo/./bar":         "/foo/bar",
		"/foo/bar/../../qux": "/qux",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathHasPrefix(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/a/b", "/a", true},
		{"/a", "/a", true},
		{"/ab", "/a", false},
		{"/a/b/c", "/a/b", true},
		{"/x", "/a", false},
	}
	for _, c := range cases {
		if got := pathHasPrefix(c.child, c.parent); got != c.want {
			t.Errorf("pathHasPrefix(%q,%q) = %v, want %v", c.child, c.parent, got, c.want)
		}
	}
}

// ---- blob reader fake ------------------------------------------------------

type fakeBlobReader struct{ data map[uuid.UUID][]byte }

func (f fakeBlobReader) ReadBlob(_ context.Context, id uuid.UUID) ([]byte, error) {
	b, ok := f.data[id]
	if !ok {
		return nil, service.ErrBlobUnavailable
	}
	return b, nil
}

func (f fakeBlobReader) Prefetch(_ context.Context, _ []uuid.UUID) {}

func TestExtentReaderAssembly(t *testing.T) {
	blobA, blobB := uuid.New(), uuid.New()
	fs := &FileSystem{blobs: fakeBlobReader{data: map[uuid.UUID][]byte{
		blobA: bytes.Repeat([]byte("A"), 10),
		blobB: bytes.Repeat([]byte("B"), 10),
	}}}
	// File of 15 bytes: 5 from blobA[3:8], 10 from blobB[0:10].
	extents := []model.Extent{
		{FileOffset: 0, Length: 5, BlobID: blobA, BlobOffset: 3},
		{FileOffset: 5, Length: 10, BlobID: blobB, BlobOffset: 0},
	}
	const size = 15
	want := append(bytes.Repeat([]byte("A"), 5), bytes.Repeat([]byte("B"), 10)...)

	for _, start := range []int64{0, 3, 5, 14} {
		r := &extentReader{fs: fs, ctx: context.Background(), extents: extents, pos: start, size: size}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("start=%d ReadAll: %v", start, err)
		}
		if !bytes.Equal(got, want[start:]) {
			t.Errorf("start=%d got %q, want %q", start, got, want[start:])
		}
	}
}

func TestExtentReaderUnavailableBlob(t *testing.T) {
	fs := &FileSystem{blobs: fakeBlobReader{data: map[uuid.UUID][]byte{}}}
	r := &extentReader{
		fs:      fs,
		ctx:     context.Background(),
		extents: []model.Extent{{FileOffset: 0, Length: 4, BlobID: uuid.New(), BlobOffset: 0}},
		pos:     0,
		size:    4,
	}
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("expected error reading from an unavailable blob")
	}
}

// ---- in-memory store fakes -------------------------------------------------

// fakeStore implements nodeStore, extentStore, walStore and blobStore over
// plain maps. It is intentionally not transactional: the fake txManager just
// runs the callback (the FileSystem's correctness under these tests does not
// depend on rollback).
type fakeStore struct {
	mu       sync.Mutex
	nodes    map[uuid.UUID]*model.Node
	extents  map[uuid.UUID][]model.Extent // by nodeID
	wal      map[uuid.UUID][]model.WALChunk
	refcount map[uuid.UUID]int64 // by blobID
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		nodes:    map[uuid.UUID]*model.Node{},
		extents:  map[uuid.UUID][]model.Extent{},
		wal:      map[uuid.UUID][]model.WALChunk{},
		refcount: map[uuid.UUID]int64{},
	}
}

// nodeStore.

func (s *fakeStore) Create(_ context.Context, n *model.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ex := range s.nodes {
		if ex.UserID == n.UserID && ex.Path == n.Path {
			return repository.ErrAlreadyExists
		}
	}
	cp := *n
	s.nodes[n.ID] = &cp
	return nil
}

func (s *fakeStore) Update(_ context.Context, n *model.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[n.ID]; !ok {
		return repository.ErrNotFound
	}
	cp := *n
	s.nodes[n.ID] = &cp
	return nil
}

func (s *fakeStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.nodes[id]
	if !ok {
		return repository.ErrNotFound
	}
	// Cascade: drop the node and every descendant by path, plus their extents/WAL.
	prefix := node.Path
	for nid, n := range s.nodes {
		if n.UserID == node.UserID && (n.Path == prefix || strings.HasPrefix(n.Path, prefix+"/")) {
			delete(s.nodes, nid)
			delete(s.extents, nid)
			delete(s.wal, nid)
		}
	}
	return nil
}

func (s *fakeStore) GetByID(_ context.Context, id uuid.UUID) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *n
	return &cp, nil
}

func (s *fakeStore) GetByPath(_ context.Context, userID uuid.UUID, p string) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.nodes {
		if n.UserID == userID && n.Path == p {
			cp := *n
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (s *fakeStore) ListChildren(_ context.Context, userID, parentID uuid.UUID) ([]model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Node
	for _, n := range s.nodes {
		if n.UserID == userID && n.ParentID != nil && *n.ParentID == parentID {
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *fakeStore) ListSubtree(_ context.Context, userID uuid.UUID, prefix string) ([]model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Node
	for _, n := range s.nodes {
		if n.UserID == userID && (n.Path == prefix || strings.HasPrefix(n.Path, prefix+"/")) {
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *fakeStore) SumSizeByUser(_ context.Context, userID uuid.UUID) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, n := range s.nodes {
		if n.UserID == userID && !n.IsDir {
			sum += n.Size
		}
	}
	return sum, nil
}

// extentStore.

func (s *fakeStore) ListByNode(_ context.Context, nodeID uuid.UUID) ([]model.Extent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Extent(nil), s.extents[nodeID]...), nil
}

func (s *fakeStore) DeleteByNode(_ context.Context, nodeID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.extents, nodeID)
	delete(s.wal, nodeID)
	return nil
}

func (s *fakeStore) CopyForNode(_ context.Context, srcNodeID, dstNodeID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.extents[srcNodeID]
	dst := make([]model.Extent, len(src))
	for i, e := range src {
		e.ID = uuid.New()
		e.NodeID = dstNodeID
		dst[i] = e
	}
	s.extents[dstNodeID] = dst
	return nil
}

// walStore.

func (s *fakeStore) AppendChunk(_ context.Context, c *model.WALChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wal[c.NodeID] = append(s.wal[c.NodeID], *c)
	return nil
}

func (s *fakeStore) ReadRange(_ context.Context, nodeID uuid.UUID, offset, length int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chunks := append([]model.WALChunk(nil), s.wal[nodeID]...)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Seq < chunks[j].Seq })
	var all []byte
	for _, c := range chunks {
		all = append(all, c.Data...)
	}
	if offset >= int64(len(all)) {
		return []byte{}, nil
	}
	end := offset + length
	if end > int64(len(all)) {
		end = int64(len(all))
	}
	return append([]byte(nil), all[offset:end]...), nil
}

// blobStore.

func (s *fakeStore) AddRefcount(_ context.Context, id uuid.UUID, delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refcount[id] += delta
	return nil
}

func (s *fakeStore) getRefcount(id uuid.UUID) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refcount[id]
}

func (s *fakeStore) setExtents(nodeID uuid.UUID, ext []model.Extent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extents[nodeID] = ext
}

// ---- other dependency fakes ------------------------------------------------

// fakeTx runs the callback directly (no real transaction).
type fakeTx struct{}

func (fakeTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// noLimiter returns the reader unchanged.
type noLimiter struct{}

func (noLimiter) ThrottledReader(r io.Reader, _ int64) io.Reader { return r }

// fakeStats discards all counters.
type fakeStats struct{}

func (fakeStats) AddReadBytes(int64) {}
func (fakeStats) IncReadOps()        {}
func (fakeStats) IncWriteOps()       {}

// fakeSettings always returns defaults.
type fakeSettings struct{}

func (fakeSettings) Get(context.Context) (model.Settings, error) { return model.DefaultSettings(), nil }

// ---- harness ---------------------------------------------------------------

func newTestFS(store *fakeStore, blobs blobReader) *FileSystem {
	return NewFileSystem(store, store, store, store, fakeTx{}, blobs, noLimiter{}, fakeSettings{}, fakeStats{}, nil)
}

func ctxFor(u *model.User) context.Context {
	return model.ContextWithPrincipal(context.Background(), &model.Principal{Acting: u, Auth: u})
}

// putFile stores a node with extents pointing at the given blob, registering an
// initial refcount of 1, as if the packer had finalized it.
func putStored(store *fakeStore, user *model.User, p string, size int64, blobID uuid.UUID) *model.Node {
	parentPath := "/"
	var parentID *uuid.UUID
	if pn, err := store.GetByPath(context.Background(), user.ID, parentPath); err == nil {
		id := pn.ID
		parentID = &id
	}
	node := &model.Node{
		ID:       uuid.New(),
		UserID:   user.ID,
		ParentID: parentID,
		Name:     strings.TrimPrefix(p, "/"),
		Path:     p,
		IsDir:    false,
		Size:     size,
		State:    model.NodeStateStored,
	}
	_ = store.Create(context.Background(), node)
	store.setExtents(node.ID, []model.Extent{{
		ID: uuid.New(), NodeID: node.ID, FileOffset: 0, Length: size, BlobID: blobID, BlobOffset: 0,
	}})
	_ = store.AddRefcount(context.Background(), blobID, 1)
	return node
}

// ---- behavior tests --------------------------------------------------------

func TestMoveRenamesMetadataOnly(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	blobID := uuid.New()
	blobs := fakeBlobReader{data: map[uuid.UUID][]byte{blobID: []byte("hello world")}}
	fs := newTestFS(store, blobs)
	ctx := ctxFor(user)

	if _, err := fs.ensureRoot(ctx, user.ID); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}
	node := putStored(store, user, "/a.txt", 11, blobID)

	if err := fs.Rename(ctx, "/a.txt", "/b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	// Old path gone, new path present, same node id, blob untouched (no re-upload).
	if _, err := store.GetByPath(ctx, user.ID, "/a.txt"); err == nil {
		t.Error("old path still resolves after MOVE")
	}
	moved, err := store.GetByPath(ctx, user.ID, "/b.txt")
	if err != nil {
		t.Fatalf("new path missing after MOVE: %v", err)
	}
	if moved.ID != node.ID {
		t.Errorf("MOVE changed node id: %v -> %v", node.ID, moved.ID)
	}
	if got := store.getRefcount(blobID); got != 1 {
		t.Errorf("MOVE changed blob refcount: got %d, want 1", got)
	}
}

func TestCopySharesBlobAndBumpsRefcount(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	blobID := uuid.New()
	blobs := fakeBlobReader{data: map[uuid.UUID][]byte{blobID: []byte("0123456789")}}
	fs := newTestFS(store, blobs)
	ctx := ctxFor(user)

	if _, err := fs.ensureRoot(ctx, user.ID); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}
	src := putStored(store, user, "/src.bin", 10, blobID)

	if err := fs.Copy(ctx, "/src.bin", "/dst.bin", true, false); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	dst, err := store.GetByPath(ctx, user.ID, "/dst.bin")
	if err != nil {
		t.Fatalf("dst missing: %v", err)
	}
	if dst.ID == src.ID {
		t.Error("Copy reused the source node id")
	}
	// Both nodes' extents point at the same immutable blob.
	dstExt, _ := store.ListByNode(ctx, dst.ID)
	if len(dstExt) != 1 || dstExt[0].BlobID != blobID {
		t.Fatalf("dst extents do not share the source blob: %+v", dstExt)
	}
	// Refcount bumped from 1 to 2 (one per copied extent), no re-upload.
	if got := store.getRefcount(blobID); got != 2 {
		t.Errorf("refcount after COPY = %d, want 2", got)
	}
}

func TestCopyDeleteCascadeReleasesRefcount(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	blobID := uuid.New()
	blobs := fakeBlobReader{data: map[uuid.UUID][]byte{blobID: []byte("0123456789")}}
	fs := newTestFS(store, blobs)
	ctx := ctxFor(user)

	if _, err := fs.ensureRoot(ctx, user.ID); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}
	putStored(store, user, "/src.bin", 10, blobID)
	if err := fs.Copy(ctx, "/src.bin", "/dst.bin", true, false); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := store.getRefcount(blobID); got != 2 {
		t.Fatalf("precondition refcount = %d, want 2", got)
	}
	if err := fs.RemoveAll(ctx, "/dst.bin"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if got := store.getRefcount(blobID); got != 1 {
		t.Errorf("refcount after DELETE = %d, want 1", got)
	}
}

func TestCopyQuotaExceeded(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u", QuotaBytes: 15} // room for one 10-byte file, not two
	blobID := uuid.New()
	blobs := fakeBlobReader{data: map[uuid.UUID][]byte{blobID: []byte("0123456789")}}
	fs := newTestFS(store, blobs)
	ctx := ctxFor(user)

	if _, err := fs.ensureRoot(ctx, user.ID); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}
	putStored(store, user, "/src.bin", 10, blobID)

	err := fs.Copy(ctx, "/src.bin", "/dst.bin", true, false)
	if err != ErrQuotaExceeded {
		t.Fatalf("Copy quota: got %v, want ErrQuotaExceeded", err)
	}
	// The destination must not have been created.
	if _, err := store.GetByPath(ctx, user.ID, "/dst.bin"); err == nil {
		t.Error("dst created despite quota failure")
	}
	// Refcount unchanged.
	if got := store.getRefcount(blobID); got != 1 {
		t.Errorf("refcount after failed COPY = %d, want 1", got)
	}
}

func TestCheckQuotaSignals507(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u", QuotaBytes: 20}
	fs := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	if _, err := fs.ensureRoot(ctx, user.ID); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}
	putStored(store, user, "/big.bin", 18, uuid.New())

	if err := fs.CheckQuota(ctx, "/new.bin", 5); err != ErrQuotaExceeded {
		t.Errorf("CheckQuota over limit: got %v, want ErrQuotaExceeded", err)
	}
	if err := fs.CheckQuota(ctx, "/new.bin", 1); err != nil {
		t.Errorf("CheckQuota within limit: got %v, want nil", err)
	}
	// Overwriting the existing file discounts its current size.
	if err := fs.CheckQuota(ctx, "/big.bin", 19); err != nil {
		t.Errorf("CheckQuota in-place overwrite: got %v, want nil", err)
	}
}

func TestWriteQuotaExceededRollsBack(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u", QuotaBytes: 5}
	fs := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	wf, err := fs.OpenFile(ctx, "/over.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := wf.Write([]byte("0123456789")); err != nil { // 10 > 5
		t.Fatalf("Write: %v", err)
	}
	if err := wf.Close(); err != ErrQuotaExceeded {
		t.Fatalf("Close: got %v, want ErrQuotaExceeded", err)
	}
	// The half-written node must have been rolled back.
	if _, err := store.GetByPath(ctx, user.ID, "/over.txt"); err == nil {
		t.Error("ghost node left behind after a quota-failed PUT")
	}
}

func TestWriteThenReadRoundTrip(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fs := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	want := []byte("the quick brown fox jumps over the lazy dog")
	wf, err := fs.OpenFile(ctx, "/doc.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile (write): %v", err)
	}
	if _, err := wf.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rf, err := fs.OpenFile(ctx, "/doc.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile (read): %v", err)
	}
	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, want)
	}
}

func TestRangeReadViaSeek(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	blobID := uuid.New()
	content := []byte("0123456789ABCDEF")
	blobs := fakeBlobReader{data: map[uuid.UUID][]byte{blobID: content}}
	fs := newTestFS(store, blobs)
	ctx := ctxFor(user)

	if _, err := fs.ensureRoot(ctx, user.ID); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}
	putStored(store, user, "/data.bin", int64(len(content)), blobID)

	rf, err := fs.OpenFile(ctx, "/data.bin", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	seeker, ok := rf.(io.ReadSeeker)
	if !ok {
		t.Fatal("read file is not an io.ReadSeeker")
	}
	// Range bytes=4-9 (http.ServeContent drives Seek+Read).
	if _, err := seeker.Seek(4, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(seeker, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(buf, content[4:10]) {
		t.Errorf("range read = %q, want %q", buf, content[4:10])
	}
	// Seek to end yields EOF.
	if _, err := seeker.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("Seek end: %v", err)
	}
	if n, err := seeker.Read(make([]byte, 4)); n != 0 || err != io.EOF {
		t.Errorf("read at EOF: n=%d err=%v, want 0/EOF", n, err)
	}
}

// Sanity check: the read file satisfies http.File semantics enough for
// http.ServeContent (which only needs Read+Seek+Stat).
var _ io.ReadSeeker = (*readFile)(nil)

func TestMkdirAndStat(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fs := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	if err := fs.Mkdir(ctx, "/dir", 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	fi, err := fs.Stat(ctx, "/dir")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !fi.IsDir() {
		t.Error("Mkdir target is not reported as a directory")
	}
	// Mkdir on an existing path fails.
	if err := fs.Mkdir(ctx, "/dir", 0o755); err != os.ErrExist {
		t.Errorf("Mkdir existing: got %v, want os.ErrExist", err)
	}
	// Mkdir under a missing parent fails (maps to ErrNotExist → 409).
	if err := fs.Mkdir(ctx, "/missing/child", 0o755); err == nil {
		t.Error("Mkdir under missing parent unexpectedly succeeded")
	}
}
