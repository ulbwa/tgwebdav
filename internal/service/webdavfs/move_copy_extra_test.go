package webdavfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// writeBuffered writes p to path through the FileSystem, leaving the node in the
// buffered (WAL-backed) state (the packer is not run).
func writeBuffered(t *testing.T, fsys *FileSystem, ctx context.Context, path string, p []byte) {
	t.Helper()
	wf, err := fsys.OpenFile(ctx, path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile %s: %v", path, err)
	}
	if _, err := wf.Write(p); err != nil {
		t.Fatalf("Write %s: %v", path, err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("Close %s: %v", path, err)
	}
}

// ---- Rename edge cases -----------------------------------------------------

func TestRenameRootRejected(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	if err := fsys.Rename(ctx, "/", "/x"); err != os.ErrPermission {
		t.Errorf("Rename from / err = %v, want ErrPermission", err)
	}
	if err := fsys.Rename(ctx, "/x", "/"); err != os.ErrPermission {
		t.Errorf("Rename to / err = %v, want ErrPermission", err)
	}
}

func TestRenameSamePathNoop(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	writeBuffered(t, fsys, ctx, "/same.txt", []byte("x"))
	if err := fsys.Rename(ctx, "/same.txt", "/same.txt"); err != nil {
		t.Fatalf("Rename same path should be a no-op, got %v", err)
	}
}

func TestRenameIntoItselfRejected(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	if err := fsys.Mkdir(ctx, "/dir", 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := fsys.Rename(ctx, "/dir", "/dir/sub"); err != os.ErrInvalid {
		t.Errorf("Rename into itself err = %v, want ErrInvalid", err)
	}
}

func TestRenameOverwritesExistingDestination(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	writeBuffered(t, fsys, ctx, "/src.txt", []byte("source"))
	writeBuffered(t, fsys, ctx, "/dst.txt", []byte("dest-old"))

	if err := fsys.Rename(ctx, "/src.txt", "/dst.txt"); err != nil {
		t.Fatalf("Rename overwrite: %v", err)
	}
	if _, err := store.GetByPath(ctx, user.ID, "/src.txt"); err == nil {
		t.Error("source still present after MOVE")
	}
	rf, err := fsys.OpenFile(ctx, "/dst.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile dst: %v", err)
	}
	got, _ := io.ReadAll(rf)
	if string(got) != "source" {
		t.Errorf("dst content after overwrite = %q, want source", got)
	}
}

func TestRenameDirectorySubtree(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	if err := fsys.Mkdir(ctx, "/a", 0o755); err != nil {
		t.Fatalf("Mkdir /a: %v", err)
	}
	if err := fsys.Mkdir(ctx, "/a/b", 0o755); err != nil {
		t.Fatalf("Mkdir /a/b: %v", err)
	}
	writeBuffered(t, fsys, ctx, "/a/b/leaf.txt", []byte("deep"))

	if err := fsys.Rename(ctx, "/a", "/z"); err != nil {
		t.Fatalf("Rename dir subtree: %v", err)
	}
	if _, err := store.GetByPath(ctx, user.ID, "/a/b/leaf.txt"); err == nil {
		t.Error("old descendant path still resolves after dir MOVE")
	}
	rf, err := fsys.OpenFile(ctx, "/z/b/leaf.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("descendant missing after dir MOVE: %v", err)
	}
	got, _ := io.ReadAll(rf)
	if string(got) != "deep" {
		t.Errorf("descendant content = %q, want deep", got)
	}
}

func TestRenameMissingSource(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	if err := fsys.Rename(ctx, "/nope.txt", "/dst.txt"); err != os.ErrNotExist {
		t.Errorf("Rename missing source err = %v, want ErrNotExist", err)
	}
}

func TestRenameMissingDestinationParent(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	writeBuffered(t, fsys, ctx, "/src.txt", []byte("x"))
	if err := fsys.Rename(ctx, "/src.txt", "/missing/dst.txt"); err != os.ErrNotExist {
		t.Errorf("Rename to missing parent err = %v, want ErrNotExist", err)
	}
}

// ---- Copy edge cases -------------------------------------------------------

func TestCopyRootRejectedAndSamePathNoop(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	if err := fsys.Copy(ctx, "/", "/x", false, false); err != os.ErrPermission {
		t.Errorf("Copy from / err = %v, want ErrPermission", err)
	}
	if err := fsys.Copy(ctx, "/x", "/", false, false); err != os.ErrPermission {
		t.Errorf("Copy to / err = %v, want ErrPermission", err)
	}
	if err := fsys.Copy(ctx, "/a.txt", "/a.txt", false, false); err != nil {
		t.Errorf("Copy same path should be a no-op, got %v", err)
	}
}

func TestCopyIntoItselfRejected(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	if err := fsys.Mkdir(ctx, "/d", 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := fsys.Copy(ctx, "/d", "/d/sub", true, false); err != os.ErrInvalid {
		t.Errorf("Copy into itself err = %v, want ErrInvalid", err)
	}
}

func TestCopyBufferedFileDuplicatesWAL(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	content := bytes.Repeat([]byte("buffered-bytes-"), 1000)
	writeBuffered(t, fsys, ctx, "/buf.txt", content)

	if err := fsys.Copy(ctx, "/buf.txt", "/buf-copy.txt", false, false); err != nil {
		t.Fatalf("Copy buffered: %v", err)
	}
	rf, err := fsys.OpenFile(ctx, "/buf-copy.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile copy: %v", err)
	}
	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatalf("ReadAll copy: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("buffered copy mismatch: got %d bytes, want %d", len(got), len(content))
	}
	dst, _ := store.GetByPath(ctx, user.ID, "/buf-copy.txt")
	if dst.State != model.NodeStateBuffered {
		t.Errorf("buffered copy state = %v, want buffered", dst.State)
	}
}

func TestCopyShallowDirectory(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	if err := fsys.Mkdir(ctx, "/dir", 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	writeBuffered(t, fsys, ctx, "/dir/child.txt", []byte("c"))

	if err := fsys.Copy(ctx, "/dir", "/dir-shallow", false, false); err != nil {
		t.Fatalf("Copy shallow: %v", err)
	}
	if _, err := store.GetByPath(ctx, user.ID, "/dir-shallow"); err != nil {
		t.Fatalf("shallow dir copy missing: %v", err)
	}
	if _, err := store.GetByPath(ctx, user.ID, "/dir-shallow/child.txt"); err == nil {
		t.Error("shallow copy unexpectedly copied a child")
	}
}

func TestCopyOverwriteExistingDestination(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	writeBuffered(t, fsys, ctx, "/src.txt", []byte("src-content"))
	writeBuffered(t, fsys, ctx, "/dst.txt", []byte("old"))

	if err := fsys.Copy(ctx, "/src.txt", "/dst.txt", false, false); err != os.ErrExist {
		t.Fatalf("Copy no-overwrite over existing err = %v, want ErrExist", err)
	}
	if err := fsys.Copy(ctx, "/src.txt", "/dst.txt", false, true); err != nil {
		t.Fatalf("Copy overwrite: %v", err)
	}
	rf, _ := fsys.OpenFile(ctx, "/dst.txt", os.O_RDONLY, 0)
	got, _ := io.ReadAll(rf)
	if string(got) != "src-content" {
		t.Errorf("dst after overwrite = %q, want src-content", got)
	}
}

func TestCopyMissingSource(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	if err := fsys.Copy(ctx, "/nope.txt", "/dst.txt", false, false); err != os.ErrNotExist {
		t.Errorf("Copy missing source err = %v, want ErrNotExist", err)
	}
}

// ---- RemoveAll edge cases --------------------------------------------------

func TestRemoveAllRootRejected(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	if err := fsys.RemoveAll(ctx, "/"); err != os.ErrPermission {
		t.Errorf("RemoveAll(/) err = %v, want ErrPermission", err)
	}
}

func TestRemoveAllMissingIsNotExist(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	if err := fsys.RemoveAll(ctx, "/nope"); err != os.ErrNotExist {
		t.Errorf("RemoveAll(missing) err = %v, want ErrNotExist", err)
	}
}

func TestRemoveAllDirectoryCascadeReleasesRefcounts(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	blobID := uuid.New()
	blobs := fakeBlobReader{data: map[uuid.UUID][]byte{blobID: []byte("0123456789")}}
	fsys := newTestFS(store, blobs)
	ctx := ctxFor(user)

	if _, err := fsys.ensureRoot(ctx, user.ID); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}
	if err := fsys.Mkdir(ctx, "/dir", 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	putStoredUnder(store, user, "/dir", "/dir/f.bin", 10, blobID)
	if got := store.getRefcount(blobID); got != 1 {
		t.Fatalf("precondition refcount = %d, want 1", got)
	}

	if err := fsys.RemoveAll(ctx, "/dir"); err != nil {
		t.Fatalf("RemoveAll dir: %v", err)
	}
	if got := store.getRefcount(blobID); got != 0 {
		t.Errorf("refcount after dir DELETE = %d, want 0", got)
	}
	if _, err := store.GetByPath(ctx, user.ID, "/dir/f.bin"); err == nil {
		t.Error("child still present after dir DELETE")
	}
}

// putStoredUnder is like putStored but parents the node under an existing dir.
func putStoredUnder(store *fakeStore, user *model.User, parentPath, p string, size int64, blobID uuid.UUID) {
	var parentID *uuid.UUID
	if pn, err := store.GetByPath(context.Background(), user.ID, parentPath); err == nil {
		id := pn.ID
		parentID = &id
	}
	node := &model.Node{
		ID:       uuid.New(),
		UserID:   user.ID,
		ParentID: parentID,
		Name:     p[len(parentPath)+1:],
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
}

// ---- NFC normalization end-to-end & deep paths -----------------------------

func TestNFCNormalizationRoundTrip(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	// "café" in two normalization forms, built from explicit code points so the
	// byte sequences genuinely differ:
	//   NFD: 'e' + U+0301 (combining acute accent)
	//   NFC: U+00E9 (precomposed é)
	// normalize() canonicalizes both to NFC, so a file written under the NFD form
	// must resolve under the NFC form.
	nfd := "/cafe\u0301.txt" // e + U+0301 combining acute (NFD)
	nfc := "/caf\u00e9.txt"  // precomposed U+00E9 (NFC)
	if nfd == nfc {
		t.Fatal("test setup error: NFD and NFC forms are byte-identical")
	}
	writeBuffered(t, fsys, ctx, nfd, []byte("accented"))

	rf, err := fsys.OpenFile(ctx, nfc, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile NFC form of an NFD-written file: %v", err)
	}
	got, _ := io.ReadAll(rf)
	if string(got) != "accented" {
		t.Errorf("NFC/NFD content = %q, want accented", got)
	}
}

func TestDeepPathRoundTrip(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	for _, d := range []string{"/a", "/a/b", "/a/b/c", "/a/b/c/d"} {
		if err := fsys.Mkdir(ctx, d, 0o755); err != nil {
			t.Fatalf("Mkdir %s: %v", d, err)
		}
	}
	writeBuffered(t, fsys, ctx, "/a/b/c/d/deep.txt", []byte("way down"))
	rf, err := fsys.OpenFile(ctx, "/a/b/c/d/deep.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile deep: %v", err)
	}
	got, _ := io.ReadAll(rf)
	if string(got) != "way down" {
		t.Errorf("deep file content = %q, want 'way down'", got)
	}
}
