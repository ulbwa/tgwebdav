package webdavfs

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/webdav"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// ---- acting / principal --------------------------------------------------

// TestNoPrincipalIsPermissionError verifies every entry point rejects a request
// without a principal on the context (os.ErrPermission).
func TestNoPrincipalIsPermissionError(t *testing.T) {
	store := newFakeStore()
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := context.Background() // no principal

	if _, err := fsys.Stat(ctx, "/x"); err != os.ErrPermission {
		t.Errorf("Stat no-principal err = %v, want ErrPermission", err)
	}
	if err := fsys.Mkdir(ctx, "/x", 0o755); err != os.ErrPermission {
		t.Errorf("Mkdir no-principal err = %v, want ErrPermission", err)
	}
	if _, err := fsys.OpenFile(ctx, "/x", os.O_RDONLY, 0); err != os.ErrPermission {
		t.Errorf("OpenFile no-principal err = %v, want ErrPermission", err)
	}
	if err := fsys.RemoveAll(ctx, "/x"); err != os.ErrPermission {
		t.Errorf("RemoveAll no-principal err = %v, want ErrPermission", err)
	}
	if err := fsys.Rename(ctx, "/a", "/b"); err != os.ErrPermission {
		t.Errorf("Rename no-principal err = %v, want ErrPermission", err)
	}
	if err := fsys.Copy(ctx, "/a", "/b", false, false); err != os.ErrPermission {
		t.Errorf("Copy no-principal err = %v, want ErrPermission", err)
	}
	if err := fsys.CheckQuota(ctx, "/x", 1); err != os.ErrPermission {
		t.Errorf("CheckQuota no-principal err = %v, want ErrPermission", err)
	}
}

// ---- Stat ------------------------------------------------------------------

func TestStatRootAutocreatesAndNotFound(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	// Stat("/") on a brand-new namespace auto-creates and reports a directory.
	fi, err := fsys.Stat(ctx, "/")
	if err != nil {
		t.Fatalf("Stat(/): %v", err)
	}
	if !fi.IsDir() {
		t.Fatal("root should be a directory")
	}

	// A missing path is os.ErrNotExist.
	if _, err := fsys.Stat(ctx, "/missing"); err != os.ErrNotExist {
		t.Fatalf("Stat(missing) = %v, want ErrNotExist", err)
	}
}

// ---- fileInfo adapter ------------------------------------------------------

func TestFileInfoAdapter(t *testing.T) {
	node := &model.Node{Name: "doc.txt", Size: 42, IsDir: false, ETag: `"abc"`, ContentType: "text/plain"}
	fi := infoFromNode(node)

	if fi.Name() != "doc.txt" {
		t.Errorf("Name = %q", fi.Name())
	}
	if fi.Size() != 42 {
		t.Errorf("Size = %d", fi.Size())
	}
	if fi.Mode() != 0o644 {
		t.Errorf("file Mode = %v, want 0o644", fi.Mode())
	}
	if fi.IsDir() {
		t.Error("IsDir should be false for a file")
	}
	if fi.Sys() != nil {
		t.Error("Sys should be nil")
	}
	if !fi.ModTime().Equal(node.ModifiedAt) {
		t.Error("ModTime mismatch")
	}

	etag, err := fi.ETag(context.Background())
	if err != nil || etag != `"abc"` {
		t.Errorf("ETag = %q, %v", etag, err)
	}
	ct, err := fi.ContentType(context.Background())
	if err != nil || ct != "text/plain" {
		t.Errorf("ContentType = %q, %v", ct, err)
	}

	// Directory mode + the empty-etag / empty-content-type fallbacks.
	dir := infoFromNode(&model.Node{Name: "d", IsDir: true})
	if dir.Mode() != (fs.ModeDir | 0o755) {
		t.Errorf("dir Mode = %v, want ModeDir|0o755", dir.Mode())
	}
	if _, err := dir.ETag(context.Background()); err != webdav.ErrNotImplemented {
		t.Errorf("empty ETag err = %v, want ErrNotImplemented", err)
	}
	if ct, _ := dir.ContentType(context.Background()); ct != "application/octet-stream" {
		t.Errorf("empty ContentType = %q, want application/octet-stream", ct)
	}
}

// ---- write file: error/Stat methods ----------------------------------------

func TestWriteFileSemantics(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	wf, err := fsys.OpenFile(ctx, "/w.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := wf.Write([]byte("abcde")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Stat on the in-flight writer reflects bytes written so far and a content type.
	st, err := wf.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() != 5 {
		t.Errorf("writer Stat size = %d, want 5", st.Size())
	}

	// Read / Seek / Readdir on a write handle are rejected.
	if _, err := wf.Read(make([]byte, 1)); err != os.ErrPermission {
		t.Errorf("write.Read err = %v, want ErrPermission", err)
	}
	if _, err := wf.Seek(0, io.SeekStart); err != os.ErrInvalid {
		t.Errorf("write.Seek err = %v, want ErrInvalid", err)
	}
	if _, err := wf.Readdir(-1); err == nil {
		t.Error("write.Readdir should error")
	}

	if err := wf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double Close is a no-op.
	if err := wf.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
	// Writing after Close is rejected.
	if _, err := wf.Write([]byte("x")); err != os.ErrClosed {
		t.Errorf("write after Close err = %v, want ErrClosed", err)
	}
}

// ---- zero-byte and content-type --------------------------------------------

func TestZeroByteFileRoundTrip(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	wf, err := fsys.OpenFile(ctx, "/empty.dat", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := wf.Close(); err != nil { // no Write at all
		t.Fatalf("Close empty: %v", err)
	}
	fi, err := fsys.Stat(ctx, "/empty.dat")
	if err != nil {
		t.Fatalf("Stat empty: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("empty file size = %d, want 0", fi.Size())
	}
	// Reading a zero-byte buffered file yields no bytes.
	rf, err := fsys.OpenFile(ctx, "/empty.dat", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile read empty: %v", err)
	}
	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatalf("ReadAll empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty read = %q, want nothing", got)
	}
}

func TestContentTypeForExtension(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	// A .txt file gets a text content type; an unknown extension falls back.
	for _, p := range []string{"/note.txt", "/blob.weirdext"} {
		wf, err := fsys.OpenFile(ctx, p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatalf("OpenFile %s: %v", p, err)
		}
		if _, err := wf.Write([]byte("data")); err != nil {
			t.Fatalf("Write %s: %v", p, err)
		}
		if err := wf.Close(); err != nil {
			t.Fatalf("Close %s: %v", p, err)
		}
	}
	fi, _ := fsys.Stat(ctx, "/note.txt")
	if ct := fi.(fileInfo).contentType; ct == "" || ct == "application/octet-stream" {
		t.Errorf(".txt content type = %q, want a text type", ct)
	}
	fi, _ = fsys.Stat(ctx, "/blob.weirdext")
	if ct := fi.(fileInfo).contentType; ct != "application/octet-stream" {
		t.Errorf("unknown ext content type = %q, want application/octet-stream", ct)
	}
}

// ---- buffered (WAL) read across the block boundary -------------------------

func TestBufferedReadAcrossWALBlocks(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	// Write more than one walReadBlock (256 KiB) so the walReader loops.
	payload := bytes.Repeat([]byte("0123456789"), walReadBlock/10+100) // > 256 KiB
	wf, err := fsys.OpenFile(ctx, "/big.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := wf.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rf, err := fsys.OpenFile(ctx, "/big.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile read: %v", err)
	}
	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("buffered read mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	// Seek-based range read of a buffered node.
	seeker := rf.(io.ReadSeeker)
	if _, err := seeker.Seek(100, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 20)
	if _, err := io.ReadFull(seeker, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(buf, payload[100:120]) {
		t.Errorf("buffered range read = %q, want %q", buf, payload[100:120])
	}
}

// ---- dirFile.Readdir paging ------------------------------------------------

func TestDirReaddirPaging(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	if err := fsys.Mkdir(ctx, "/d", 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, n := range []string{"a", "b", "c"} {
		wf, err := fsys.OpenFile(ctx, "/d/"+n, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatalf("OpenFile %s: %v", n, err)
		}
		_, _ = wf.Write([]byte("x"))
		if err := wf.Close(); err != nil {
			t.Fatalf("Close %s: %v", n, err)
		}
	}

	df, err := fsys.OpenFile(ctx, "/d", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile dir: %v", err)
	}
	dir := df.(*dirFile)

	// Paged: first 2, then the remaining 1, then EOF.
	first, err := dir.Readdir(2)
	if err != nil || len(first) != 2 {
		t.Fatalf("Readdir(2) = %d entries, err %v, want 2", len(first), err)
	}
	second, err := dir.Readdir(2)
	if err != nil || len(second) != 1 {
		t.Fatalf("Readdir(2) again = %d entries, err %v, want 1", len(second), err)
	}
	if _, err := dir.Readdir(2); err != io.EOF {
		t.Fatalf("Readdir past end err = %v, want io.EOF", err)
	}

	// count<=0 returns everything in one shot on a fresh handle.
	df2, _ := fsys.OpenFile(ctx, "/d", os.O_RDONLY, 0)
	all, err := df2.(*dirFile).Readdir(-1)
	if err != nil || len(all) != 3 {
		t.Fatalf("Readdir(-1) = %d entries, err %v, want 3", len(all), err)
	}

	// dirFile rejects file operations.
	if _, err := df2.Read(make([]byte, 1)); err == nil {
		t.Error("dir.Read should error")
	}
	if _, err := df2.Write([]byte("x")); err != os.ErrPermission {
		t.Errorf("dir.Write err = %v, want ErrPermission", err)
	}
	if _, err := df2.Seek(0, io.SeekStart); err != os.ErrInvalid {
		t.Errorf("dir.Seek err = %v, want ErrInvalid", err)
	}
	if st, err := df2.Stat(); err != nil || !st.IsDir() {
		t.Errorf("dir.Stat err=%v isDir=%v, want a dir", err, st.IsDir())
	}
}

// ---- truncate-on-overwrite -------------------------------------------------

func TestOpenFileTruncateExisting(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	blobID := uuid.New()
	blobs := fakeBlobReader{data: map[uuid.UUID][]byte{blobID: []byte("OLDOLDOLD")}}
	fsys := newTestFS(store, blobs)
	ctx := ctxFor(user)

	if _, err := fsys.ensureRoot(ctx, user.ID); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}
	// Seed a stored file referencing blobID with refcount 1.
	putStored(store, user, "/f.txt", 9, blobID)
	if got := store.getRefcount(blobID); got != 1 {
		t.Fatalf("precondition refcount = %d, want 1", got)
	}

	// Re-open with O_TRUNC: the old extents are released (refcount → 0) and the
	// node resets to a fresh write.
	wf, err := fsys.OpenFile(ctx, "/f.txt", os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile truncate: %v", err)
	}
	if _, err := wf.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := store.getRefcount(blobID); got != 0 {
		t.Errorf("refcount after truncate = %d, want 0 (old extent released)", got)
	}
	fi, _ := fsys.Stat(ctx, "/f.txt")
	if fi.Size() != 3 {
		t.Errorf("size after truncate+rewrite = %d, want 3", fi.Size())
	}
}

// TestOpenFileExclusiveExisting verifies O_EXCL on an existing file errors.
func TestOpenFileExclusiveExisting(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)

	wf, err := fsys.OpenFile(ctx, "/x.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, _ = wf.Write([]byte("hi"))
	if err := wf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := fsys.OpenFile(ctx, "/x.txt", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err != os.ErrExist {
		t.Fatalf("O_EXCL on existing err = %v, want ErrExist", err)
	}
}

// TestOpenWriteRootInvalid verifies writing to "/" is rejected.
func TestOpenWriteRootInvalid(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	fsys := newTestFS(store, fakeBlobReader{data: map[uuid.UUID][]byte{}})
	ctx := ctxFor(user)
	if _, err := fsys.OpenFile(ctx, "/", os.O_WRONLY, 0o644); err != os.ErrInvalid {
		t.Fatalf("OpenFile(/) write err = %v, want ErrInvalid", err)
	}
}

// TestMultiBlobReadTriggersReadAhead exercises the multi-blob read path: a
// stored file spanning two blobs assembles correctly across the boundary and
// kicks off the read-ahead prefetcher (planBlobs len>1).
func TestMultiBlobReadTriggersReadAhead(t *testing.T) {
	store := newFakeStore()
	user := &model.User{ID: uuid.New(), Login: "u"}
	blobA, blobB := uuid.New(), uuid.New()
	blobs := fakeBlobReader{data: map[uuid.UUID][]byte{
		blobA: bytes.Repeat([]byte("A"), 10),
		blobB: bytes.Repeat([]byte("B"), 10),
	}}
	fsys := newTestFS(store, blobs)
	ctx := ctxFor(user)

	if _, err := fsys.ensureRoot(ctx, user.ID); err != nil {
		t.Fatalf("ensureRoot: %v", err)
	}
	// A 20-byte file: bytes [0,10) on blobA, [10,20) on blobB.
	node := &model.Node{
		ID: uuid.New(), UserID: user.ID, Name: "two.bin", Path: "/two.bin",
		Size: 20, State: model.NodeStateStored,
	}
	if pn, err := store.GetByPath(ctx, user.ID, "/"); err == nil {
		node.ParentID = &pn.ID
	}
	_ = store.Create(ctx, node)
	store.setExtents(node.ID, []model.Extent{
		{ID: uuid.New(), NodeID: node.ID, FileOffset: 0, Length: 10, BlobID: blobA, BlobOffset: 0},
		{ID: uuid.New(), NodeID: node.ID, FileOffset: 10, Length: 10, BlobID: blobB, BlobOffset: 0},
	})

	rf, err := fsys.OpenFile(ctx, "/two.bin", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := append(bytes.Repeat([]byte("A"), 10), bytes.Repeat([]byte("B"), 10)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("multi-blob read = %q, want %q", got, want)
	}

	// readFile no-op / error methods.
	if err := rf.Close(); err != nil {
		t.Errorf("readFile.Close err = %v, want nil", err)
	}
	if _, err := rf.Write([]byte("x")); err != os.ErrPermission {
		t.Errorf("readFile.Write err = %v, want ErrPermission", err)
	}
	if _, err := rf.Readdir(-1); err == nil {
		t.Error("readFile.Readdir should error")
	}
	if st, err := rf.Stat(); err != nil || st.IsDir() {
		t.Errorf("readFile.Stat err=%v isDir=%v, want a file", err, st.IsDir())
	}
}

// TestBlobIndexFor exercises the read-ahead blob-index helper directly across
// its branches.
func TestBlobIndexFor(t *testing.T) {
	starts := []int64{0, 100, 250}
	cases := []struct {
		cur  int64
		want int
	}{
		{0, 0}, {50, 0}, {100, 1}, {200, 1}, {250, 2}, {9999, 2},
	}
	for _, c := range cases {
		if got := blobIndexFor(starts, c.cur); got != c.want {
			t.Errorf("blobIndexFor(%v, %d) = %d, want %d", starts, c.cur, got, c.want)
		}
	}
}
