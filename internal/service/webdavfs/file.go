package webdavfs

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"sort"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/webdav"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// fileInfo adapts a model.Node to os.FileInfo and supplies ETag/ContentType to
// x/net/webdav so it never has to read content to derive them.
type fileInfo struct {
	name        string
	size        int64
	modTime     time.Time
	isDir       bool
	etag        string
	contentType string
}

func infoFromNode(n *model.Node) fileInfo {
	return fileInfo{
		name:        n.Name,
		size:        n.Size,
		modTime:     n.ModifiedAt,
		isDir:       n.IsDir,
		etag:        n.ETag,
		contentType: n.ContentType,
	}
}

func (fi fileInfo) Name() string { return fi.name }
func (fi fileInfo) Size() int64  { return fi.size }
func (fi fileInfo) Mode() fs.FileMode {
	if fi.isDir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (fi fileInfo) ModTime() time.Time { return fi.modTime }
func (fi fileInfo) IsDir() bool        { return fi.isDir }
func (fi fileInfo) Sys() any           { return nil }

// ETag is consumed by webdav.findETag; returning ErrNotImplemented lets webdav
// fall back to its modtime+size default (used for directories).
func (fi fileInfo) ETag(_ context.Context) (string, error) {
	if fi.etag == "" {
		return "", webdav.ErrNotImplemented
	}
	return fi.etag, nil
}

// ContentType is consumed by webdav.findContentType, avoiding content sniffing.
func (fi fileInfo) ContentType(_ context.Context) (string, error) {
	if fi.contentType == "" {
		return "application/octet-stream", nil
	}
	return fi.contentType, nil
}

// ---- directory file --------------------------------------------------------

// dirFile is returned for read opens of directories; it supports Readdir/Stat.
type dirFile struct {
	fs   *FileSystem
	ctx  context.Context
	node *model.Node
	user uuid.UUID

	children []model.Node
	loaded   bool
	pos      int
}

func (d *dirFile) Close() error { return nil }
func (d *dirFile) Read([]byte) (int, error) {
	return 0, fmt.Errorf("read on directory: %w", os.ErrInvalid)
}
func (d *dirFile) Write([]byte) (int, error)      { return 0, os.ErrPermission }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, os.ErrInvalid }

func (d *dirFile) Stat() (fs.FileInfo, error) { return infoFromNode(d.node), nil }

func (d *dirFile) Readdir(count int) ([]fs.FileInfo, error) {
	if !d.loaded {
		kids, err := d.fs.nodes.ListChildren(d.ctx, d.user, d.node.ID)
		if err != nil {
			return nil, err
		}
		d.children = kids
		d.loaded = true
	}
	if d.pos >= len(d.children) {
		if count <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	end := len(d.children)
	if count > 0 && d.pos+count < end {
		end = d.pos + count
	}
	out := make([]fs.FileInfo, 0, end-d.pos)
	for _, n := range d.children[d.pos:end] {
		n := n
		out = append(out, infoFromNode(&n))
	}
	d.pos = end
	return out, nil
}

// ---- read file -------------------------------------------------------------

// readFile streams a file's content, assembling stored extents (or WAL bytes
// for buffered nodes) on demand and applying the user's bandwidth limit. It is
// an io.ReadSeeker so http.ServeContent can serve Range requests.
type readFile struct {
	fs      *FileSystem
	ctx     context.Context
	node    *model.Node
	extents []model.Extent
	loaded  bool
	bps     int64

	pos    int64
	cursor atomic.Int64 // mirror of pos read by the read-ahead goroutine
	src    io.Reader    // throttled assembler positioned at pos; rebuilt after Seek
}

// ensureExtents loads the node's extents on first read (stored nodes only) and,
// for multi-blob files, kicks off a parallel read-ahead that warms the cache
// with the file's blobs while the client reads sequentially.
func (r *readFile) ensureExtents() error {
	if r.loaded || r.node.State != model.NodeStateStored {
		return nil
	}
	extents, err := r.fs.extents.ListByNode(r.ctx, r.node.ID)
	if err != nil {
		return err
	}
	r.extents = extents
	r.loaded = true

	if ids, starts := planBlobs(extents); len(ids) > 1 {
		go r.readAhead(ids, starts)
	}
	return nil
}

// readAhead keeps the blobs just ahead of the read cursor downloading in
// parallel (blobReader.Prefetch, itself bounded to the cache capacity) so a
// sequential GET never blocks waiting for the next blob. It slides forward as
// the cursor advances and stops when the file is fully read or the request ends.
func (r *readFile) readAhead(ids []uuid.UUID, starts []int64) {
	for {
		if r.ctx.Err() != nil {
			return
		}
		cur := r.cursor.Load()
		if cur >= r.node.Size {
			return
		}
		i := blobIndexFor(starts, cur)
		// Prefetch fetches up to cache-capacity blobs starting at the cursor.
		r.fs.blobs.Prefetch(r.ctx, ids[i:])
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(30 * time.Millisecond):
		}
	}
}

// planBlobs returns the distinct blob ids of a file in read order together with
// each blob's starting byte offset within the file.
func planBlobs(extents []model.Extent) (ids []uuid.UUID, starts []int64) {
	type be struct {
		id    uuid.UUID
		start int64
	}
	idx := make(map[uuid.UUID]int, len(extents))
	var list []be
	for _, e := range extents {
		if i, ok := idx[e.BlobID]; ok {
			if e.FileOffset < list[i].start {
				list[i].start = e.FileOffset
			}
			continue
		}
		idx[e.BlobID] = len(list)
		list = append(list, be{id: e.BlobID, start: e.FileOffset})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].start < list[j].start })
	for _, b := range list {
		ids = append(ids, b.id)
		starts = append(starts, b.start)
	}
	return ids, starts
}

// blobIndexFor returns the index of the blob covering byte offset cur (the last
// blob whose start is <= cur).
func blobIndexFor(starts []int64, cur int64) int {
	i := 0
	for i+1 < len(starts) && starts[i+1] <= cur {
		i++
	}
	return i
}

func (r *readFile) Close() error              { return nil }
func (r *readFile) Write([]byte) (int, error) { return 0, os.ErrPermission }
func (r *readFile) Readdir(int) ([]fs.FileInfo, error) {
	return nil, fmt.Errorf("readdir on file: %w", os.ErrInvalid)
}
func (r *readFile) Stat() (fs.FileInfo, error) { return infoFromNode(r.node), nil }

func (r *readFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.node.Size + offset
	default:
		return 0, os.ErrInvalid
	}
	if abs < 0 {
		return 0, os.ErrInvalid
	}
	r.pos = abs
	r.cursor.Store(abs)
	r.src = nil // force rebuild at new position
	return abs, nil
}

func (r *readFile) Read(p []byte) (int, error) {
	if r.pos >= r.node.Size {
		return 0, io.EOF
	}
	if err := r.ensureExtents(); err != nil {
		return 0, err
	}
	if r.src == nil {
		r.fs.stats.IncReadOps()
		raw := r.fs.newContentReader(r.ctx, r.node, r.extents, r.pos)
		r.src = r.fs.limiter.ThrottledReader(raw, r.bps)
	}
	n, err := r.src.Read(p)
	r.pos += int64(n)
	r.cursor.Store(r.pos)
	if n > 0 {
		r.fs.stats.AddReadBytes(int64(n))
	}
	return n, err
}

// newContentReader returns an io.Reader streaming bytes [start, size).
func (f *FileSystem) newContentReader(ctx context.Context, node *model.Node, extents []model.Extent, start int64) io.Reader {
	if node.State == model.NodeStateBuffered || node.State == model.NodeStateWriting {
		return &walReader{fs: f, ctx: ctx, nodeID: node.ID, pos: start, size: node.Size}
	}
	return &extentReader{fs: f, ctx: ctx, extents: extents, pos: start, size: node.Size}
}

// walReader streams a buffered node's bytes from the WAL in blocks.
type walReader struct {
	fs     *FileSystem
	ctx    context.Context
	nodeID uuid.UUID
	pos    int64
	size   int64
	buf    []byte
}

const walReadBlock = 256 * 1024

func (w *walReader) Read(p []byte) (int, error) {
	if len(w.buf) == 0 {
		if w.pos >= w.size {
			return 0, io.EOF
		}
		n := int64(walReadBlock)
		if w.pos+n > w.size {
			n = w.size - w.pos
		}
		data, err := w.fs.wal.ReadRange(w.ctx, w.nodeID, w.pos, n)
		if err != nil {
			return 0, err
		}
		if len(data) == 0 {
			return 0, io.EOF
		}
		w.buf = data
		w.pos += int64(len(data))
	}
	k := copy(p, w.buf)
	w.buf = w.buf[k:]
	return k, nil
}

// extentReader streams a stored node's bytes by fetching the blobs its extents
// reference, caching the most recently fetched blob in memory.
type extentReader struct {
	fs      *FileSystem
	ctx     context.Context
	extents []model.Extent
	pos     int64
	size    int64

	curBlob  uuid.UUID
	curBytes []byte
	haveBlob bool
}

func (e *extentReader) Read(p []byte) (int, error) {
	if e.pos >= e.size {
		return 0, io.EOF
	}
	ext, ok := e.extentAt(e.pos)
	if !ok {
		// Hole (should not happen for well-formed files) — treat as EOF.
		return 0, io.EOF
	}
	if !e.haveBlob || e.curBlob != ext.BlobID {
		data, err := e.fs.blobs.ReadBlob(e.ctx, ext.BlobID)
		if err != nil {
			return 0, err
		}
		e.curBlob, e.curBytes, e.haveBlob = ext.BlobID, data, true
	}
	within := e.pos - ext.FileOffset
	blobPos := ext.BlobOffset + within
	avail := ext.Length - within
	if blobPos+avail > int64(len(e.curBytes)) {
		avail = int64(len(e.curBytes)) - blobPos
	}
	if avail <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	end := blobPos + avail
	n := copy(p, e.curBytes[blobPos:end])
	e.pos += int64(n)
	return n, nil
}

func (e *extentReader) extentAt(pos int64) (model.Extent, bool) {
	for _, ext := range e.extents {
		if pos >= ext.FileOffset && pos < ext.FileOffset+ext.Length {
			return ext, true
		}
	}
	return model.Extent{}, false
}

// ---- write file ------------------------------------------------------------

// writeFile buffers PUT content into the WAL in ~1 MiB chunks and finalizes the
// node (size, hash, etag, content-type, quota check) on Close.
type writeFile struct {
	fs   *FileSystem
	ctx  context.Context
	node *model.Node
	user *model.User

	hasher hash.Hash
	buf    []byte
	seq    int64
	size   int64
	closed bool
}

const walChunkSize = 1 << 20 // 1 MiB

func (w *writeFile) Read([]byte) (int, error) { return 0, os.ErrPermission }
func (w *writeFile) Seek(int64, int) (int64, error) {
	return 0, os.ErrInvalid
}
func (w *writeFile) Readdir(int) ([]fs.FileInfo, error) {
	return nil, fmt.Errorf("readdir on file: %w", os.ErrInvalid)
}

func (w *writeFile) Write(p []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	w.hasher.Write(p)
	w.size += int64(len(p))
	w.buf = append(w.buf, p...)
	for len(w.buf) >= walChunkSize {
		if err := w.flushChunk(w.buf[:walChunkSize]); err != nil {
			return 0, err
		}
		w.buf = w.buf[walChunkSize:]
	}
	return len(p), nil
}

func (w *writeFile) flushChunk(data []byte) error {
	chunk := &model.WALChunk{
		ID:     uuid.New(),
		NodeID: w.node.ID,
		Seq:    w.seq,
		Data:   append([]byte(nil), data...),
	}
	if err := w.fs.wal.AppendChunk(w.ctx, chunk); err != nil {
		return err
	}
	w.seq++
	return nil
}

func (w *writeFile) Stat() (fs.FileInfo, error) {
	// Called by the PUT handler before Close; reflect bytes written so far.
	return fileInfo{
		name:        w.node.Name,
		size:        w.size,
		modTime:     time.Now(),
		isDir:       false,
		etag:        etagOf(w.hasher),
		contentType: contentTypeFor(w.node.Name),
	}, nil
}

func (w *writeFile) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	if len(w.buf) > 0 {
		if err := w.flushChunk(w.buf); err != nil {
			return err
		}
		w.buf = nil
	}

	sum := hex.EncodeToString(w.hasher.Sum(nil))
	now := time.Now()

	err := w.fs.tx.WithTx(w.ctx, func(ctx context.Context) error {
		// Quota: logical size of all other file nodes + this file's new size.
		if w.user.QuotaBytes > 0 {
			used, err := w.fs.nodes.SumSizeByUser(ctx, w.user.ID)
			if err != nil {
				return err
			}
			if used+w.size > w.user.QuotaBytes {
				return model.ErrQuotaExceeded
			}
		}
		n, err := w.fs.nodes.GetByID(ctx, w.node.ID)
		if err != nil {
			return err
		}
		n.Size = w.size
		n.ContentHash = sum
		n.ETag = fmt.Sprintf("%q", sum)
		n.ContentType = contentTypeFor(n.Name)
		n.State = model.NodeStateBuffered
		n.ModifiedAt = now
		return w.fs.nodes.Update(ctx, n)
	})
	if err != nil {
		// Roll the half-written node back so a failed PUT leaves no ghost.
		_ = w.fs.tx.WithTx(w.ctx, func(ctx context.Context) error {
			_ = w.fs.wal.DeleteByNode(ctx, w.node.ID)
			return w.fs.nodes.Delete(ctx, w.node.ID)
		})
		return err
	}
	w.fs.stats.IncWriteOps()
	return nil
}

func etagOf(h hash.Hash) string {
	return fmt.Sprintf("%q", hex.EncodeToString(h.Sum(nil)))
}

func contentTypeFor(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
