// Package webdavfs implements golang.org/x/net/webdav.FileSystem backed by the
// tgwebdav store, as an application service. Each request operates inside the
// acting user's isolated namespace, resolved from the principal carried on the
// context. Reads assemble content from WAL bytes (buffered nodes) or blob
// extents (stored nodes); writes append to the WAL and become readable
// immediately.
//
// All COPY/refcount and quota (507) business logic lives here: the WebDAV HTTP
// handler that hosts x/net/webdav.Handler over this service only translates the
// errors returned here into HTTP status codes.
//
// Following the service-layer convention, this package depends on tiny
// dependency interfaces declared locally (Rule 5): the repository stores, the
// transaction manager, the blob reader, the limiter, the settings service and
// the stat recorder. The concrete repositories, the blob service, the limiter,
// the settings service and the stat recorder satisfy them structurally, so the
// real types never need to be imported here.
package webdavfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/webdav"
	"golang.org/x/text/unicode/norm"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// txManager runs a function inside a single database transaction. The stores
// held by the FileSystem resolve their executor from the context, so the same
// store value operates on the transaction inside fn and on the pool outside it.
// It mirrors database.TxManager.
type txManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// nodeStore persists filesystem nodes. It is the subset of the node repository
// the filesystem uses.
type nodeStore interface {
	Create(ctx context.Context, n *model.Node) error
	Update(ctx context.Context, n *model.Node) error
	Delete(ctx context.Context, id uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*model.Node, error)
	GetByPath(ctx context.Context, userID uuid.UUID, path string) (*model.Node, error)
	ListChildren(ctx context.Context, userID uuid.UUID, parentID uuid.UUID) ([]model.Node, error)
	ListSubtree(ctx context.Context, userID uuid.UUID, prefix string) ([]model.Node, error)
	SumSizeByUser(ctx context.Context, userID uuid.UUID) (int64, error)
}

// extentStore persists extents (file-range → blob-range mappings). It is the
// subset of the extent repository the filesystem uses.
type extentStore interface {
	ListByNode(ctx context.Context, nodeID uuid.UUID) ([]model.Extent, error)
	DeleteByNode(ctx context.Context, nodeID uuid.UUID) error
	CopyForNode(ctx context.Context, srcNodeID, dstNodeID uuid.UUID) error
}

// walStore persists append-only file content awaiting packing. It is the subset
// of the WAL repository the filesystem uses.
type walStore interface {
	AppendChunk(ctx context.Context, c *model.WALChunk) error
	ReadRange(ctx context.Context, nodeID uuid.UUID, offset, length int64) ([]byte, error)
	DeleteByNode(ctx context.Context, nodeID uuid.UUID) error
}

// blobStore mutates blob metadata. The filesystem only adjusts refcounts (COPY
// bumps, DELETE/truncate release).
type blobStore interface {
	AddRefcount(ctx context.Context, id uuid.UUID, delta int64) error
}

// blobReader resolves the full bytes of a stored blob, transparently using the
// disk cache, bot selection and recovery. It is satisfied by the blob service.
type blobReader interface {
	ReadBlob(ctx context.Context, blobID uuid.UUID) ([]byte, error)
	// Prefetch warms the cache by downloading the given blobs concurrently
	// (best-effort), bounded so prefetched data does not exceed cache capacity.
	Prefetch(ctx context.Context, blobIDs []uuid.UUID)
}

// limiter caps read throughput per user. The filesystem only needs the
// bandwidth wrapper; the per-minute request limiter (Allow) is enforced by the
// HTTP layer.
type limiter interface {
	ThrottledReader(r io.Reader, bps int64) io.Reader
}

// settingsGetter reads runtime settings. It is held for parity with the wiring
// of the other services; the filesystem does not currently consult it.
type settingsGetter interface {
	Get(ctx context.Context) (model.Settings, error)
}

// statRecorder accumulates the read/write counters the filesystem emits.
type statRecorder interface {
	AddReadBytes(n int64)
	IncReadOps()
	IncWriteOps()
}

// FileSystem is the webdav.FileSystem implementation. It also exposes Copy (for
// blob-sharing COPY) and CheckQuota (for a correct 507 pre-check), used by the
// WebDAV HTTP server.
type FileSystem struct {
	nodes    nodeStore
	extents  extentStore
	wal      walStore
	blobMeta blobStore
	tx       txManager
	blobs    blobReader
	limiter  limiter
	settings settingsGetter
	stats    statRecorder
	log      *slog.Logger

	rootReady sync.Map // userID → struct{}: root node known to exist
}

// NewFileSystem builds the WebDAV filesystem from its dependencies. nodes,
// extents, wal and blobMeta are repository stores (the same values are reused
// inside transactions, where they resolve the tx executor from the context);
// tx is the transaction manager; blobs is the blob service; lim is the read
// limiter; settings is the settings service; stats is the stat recorder.
func NewFileSystem(
	nodes nodeStore,
	extents extentStore,
	wal walStore,
	blobMeta blobStore,
	tx txManager,
	blobs blobReader,
	lim limiter,
	settings settingsGetter,
	stats statRecorder,
	logger *slog.Logger,
) *FileSystem {
	if logger == nil {
		logger = slog.Default()
	}
	return &FileSystem{
		nodes:    nodes,
		extents:  extents,
		wal:      wal,
		blobMeta: blobMeta,
		tx:       tx,
		blobs:    blobs,
		limiter:  lim,
		settings: settings,
		stats:    stats,
		log:      logger.With("component", "webdavfs"),
	}
}

var _ webdav.FileSystem = (*FileSystem)(nil)

// normalize produces a canonical, NFC-normalized, slash-rooted path without a
// trailing slash (root stays "/"). Cleaning also neutralizes "." and ".."
// segments so a request can never escape the user's namespace.
func normalize(name string) string {
	name = norm.NFC.String(name)
	return path.Clean("/" + strings.TrimLeft(name, "/"))
}

func acting(ctx context.Context) (*model.User, error) {
	p, ok := model.PrincipalFromContext(ctx)
	if !ok || p.Acting == nil {
		return nil, os.ErrPermission
	}
	return p.Acting, nil
}

// toFSErr maps domain errors to the os errors x/net/webdav understands.
func toFSErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, model.ErrNotFound):
		return os.ErrNotExist
	case errors.Is(err, model.ErrAlreadyExists):
		return os.ErrExist
	default:
		return err
	}
}

// ensureRoot returns the user's root directory node, creating it on first use.
func (f *FileSystem) ensureRoot(ctx context.Context, userID uuid.UUID) (*model.Node, error) {
	root, err := f.nodes.GetByPath(ctx, userID, "/")
	if err == nil {
		f.rootReady.Store(userID, struct{}{})
		return root, nil
	}
	if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	now := time.Now()
	root = &model.Node{
		ID:          uuid.New(),
		UserID:      userID,
		ParentID:    nil,
		Name:        "",
		Path:        "/",
		IsDir:       true,
		State:       model.NodeStateStored,
		ContentType: "httpd/unix-directory",
		CreatedAt:   now,
		ModifiedAt:  now,
	}
	if err := f.nodes.Create(ctx, root); err != nil {
		if errors.Is(err, model.ErrAlreadyExists) {
			f.rootReady.Store(userID, struct{}{})
			return f.nodes.GetByPath(ctx, userID, "/")
		}
		return nil, err
	}
	f.rootReady.Store(userID, struct{}{})
	return root, nil
}

// ensureRootExists guarantees the root exists without re-querying once known
// (used on the write/mkdir hot path).
func (f *FileSystem) ensureRootExists(ctx context.Context, userID uuid.UUID) error {
	if _, ok := f.rootReady.Load(userID); ok {
		return nil
	}
	_, err := f.ensureRoot(ctx, userID)
	return err
}

// Stat implements webdav.FileSystem.
func (f *FileSystem) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	user, err := acting(ctx)
	if err != nil {
		return nil, err
	}
	p := normalize(name)
	node, err := f.nodes.GetByPath(ctx, user.ID, p)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) && p == "/" {
			root, rerr := f.ensureRoot(ctx, user.ID)
			if rerr != nil {
				return nil, rerr
			}
			return infoFromNode(root), nil
		}
		return nil, toFSErr(err)
	}
	return infoFromNode(node), nil
}

// Mkdir implements webdav.FileSystem (MKCOL).
func (f *FileSystem) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	user, err := acting(ctx)
	if err != nil {
		return err
	}
	p := normalize(name)
	if p == "/" {
		return os.ErrExist
	}
	if err := f.ensureRootExists(ctx, user.ID); err != nil {
		return err
	}
	if _, err := f.nodes.GetByPath(ctx, user.ID, p); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, model.ErrNotFound) {
		return err
	}
	parent, err := f.nodes.GetByPath(ctx, user.ID, path.Dir(p))
	if err != nil {
		return toFSErr(err) // missing parent → os.ErrNotExist → 409
	}
	if !parent.IsDir {
		return os.ErrNotExist
	}
	now := time.Now()
	node := &model.Node{
		ID:          uuid.New(),
		UserID:      user.ID,
		ParentID:    &parent.ID,
		Name:        path.Base(p),
		Path:        p,
		IsDir:       true,
		State:       model.NodeStateStored,
		ContentType: "httpd/unix-directory",
		CreatedAt:   now,
		ModifiedAt:  now,
	}
	return toFSErr(f.nodes.Create(ctx, node))
}

// OpenFile implements webdav.FileSystem (GET/HEAD reads and PUT writes).
func (f *FileSystem) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	user, err := acting(ctx)
	if err != nil {
		return nil, err
	}
	p := normalize(name)

	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0
	if !writable {
		return f.openRead(ctx, user, p)
	}
	if err := f.ensureRootExists(ctx, user.ID); err != nil {
		return nil, err
	}
	return f.openWrite(ctx, user, p, flag)
}

func (f *FileSystem) openRead(ctx context.Context, user *model.User, p string) (webdav.File, error) {
	node, err := f.nodes.GetByPath(ctx, user.ID, p)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) && p == "/" {
			root, rerr := f.ensureRoot(ctx, user.ID)
			if rerr != nil {
				return nil, rerr
			}
			return &dirFile{fs: f, ctx: ctx, node: root, user: user.ID}, nil
		}
		return nil, toFSErr(err)
	}
	if node.IsDir {
		return &dirFile{fs: f, ctx: ctx, node: node, user: user.ID}, nil
	}
	// Extents are loaded lazily on the first Read: PROPFIND opens every child
	// just to Stat it and never reads, so eager loading would issue one query
	// per directory entry.
	return &readFile{fs: f, ctx: ctx, node: node, bps: user.BandwidthBPS}, nil
}

func (f *FileSystem) openWrite(ctx context.Context, user *model.User, p string, flag int) (webdav.File, error) {
	if p == "/" {
		return nil, os.ErrInvalid
	}
	parent, err := f.nodes.GetByPath(ctx, user.ID, path.Dir(p))
	if err != nil {
		return nil, toFSErr(err)
	}
	if !parent.IsDir {
		return nil, os.ErrNotExist
	}

	existing, err := f.nodes.GetByPath(ctx, user.ID, p)
	switch {
	case err == nil:
		if existing.IsDir {
			return nil, os.ErrInvalid
		}
		if flag&os.O_EXCL != 0 {
			return nil, os.ErrExist
		}
		// Truncate: drop old extents (releasing blob refs) and old WAL, reset.
		if err := f.truncateNode(ctx, existing); err != nil {
			return nil, err
		}
		return f.newWriter(ctx, user, existing), nil
	case errors.Is(err, model.ErrNotFound):
		now := time.Now()
		node := &model.Node{
			ID:         uuid.New(),
			UserID:     user.ID,
			ParentID:   &parent.ID,
			Name:       path.Base(p),
			Path:       p,
			IsDir:      false,
			State:      model.NodeStateWriting,
			CreatedAt:  now,
			ModifiedAt: now,
		}
		if err := f.nodes.Create(ctx, node); err != nil {
			return nil, toFSErr(err)
		}
		return f.newWriter(ctx, user, node), nil
	default:
		return nil, err
	}
}

func (f *FileSystem) newWriter(ctx context.Context, user *model.User, node *model.Node) *writeFile {
	return &writeFile{fs: f, ctx: ctx, node: node, user: user, hasher: sha256.New()}
}

// truncateNode resets an existing file node to a fresh writing state, releasing
// its old blob references and WAL rows in a single transaction.
func (f *FileSystem) truncateNode(ctx context.Context, node *model.Node) error {
	return f.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := f.releaseExtents(ctx, node.ID); err != nil {
			return err
		}
		if err := f.extents.DeleteByNode(ctx, node.ID); err != nil {
			return err
		}
		if err := f.wal.DeleteByNode(ctx, node.ID); err != nil {
			return err
		}
		fresh, err := f.nodes.GetByID(ctx, node.ID)
		if err != nil {
			return err
		}
		fresh.State = model.NodeStateWriting
		fresh.Size = 0
		fresh.ContentHash = ""
		fresh.ETag = ""
		fresh.ModifiedAt = time.Now()
		return f.nodes.Update(ctx, fresh)
	})
}

// releaseExtents decrements the refcount of every blob a node's extents
// reference (one decrement per extent).
func (f *FileSystem) releaseExtents(ctx context.Context, nodeID uuid.UUID) error {
	extents, err := f.extents.ListByNode(ctx, nodeID)
	if err != nil {
		return err
	}
	for _, e := range extents {
		if err := f.blobMeta.AddRefcount(ctx, e.BlobID, -1); err != nil {
			return err
		}
	}
	return nil
}

// RemoveAll implements webdav.FileSystem (DELETE).
func (f *FileSystem) RemoveAll(ctx context.Context, name string) error {
	user, err := acting(ctx)
	if err != nil {
		return err
	}
	p := normalize(name)
	if p == "/" {
		return os.ErrPermission
	}
	node, err := f.nodes.GetByPath(ctx, user.ID, p)
	if err != nil {
		return toFSErr(err)
	}
	return f.tx.WithTx(ctx, func(ctx context.Context) error {
		subtree, err := f.nodes.ListSubtree(ctx, user.ID, p)
		if err != nil {
			return err
		}
		if len(subtree) == 0 {
			subtree = []model.Node{*node}
		}
		for i := range subtree {
			if subtree[i].IsDir {
				continue
			}
			if err := f.releaseExtents(ctx, subtree[i].ID); err != nil {
				return err
			}
		}
		// Deleting the top node cascades children/extents/WAL via FK.
		return f.nodes.Delete(ctx, node.ID)
	})
}

// Rename implements webdav.FileSystem (MOVE) as a metadata-only path rewrite.
func (f *FileSystem) Rename(ctx context.Context, oldName, newName string) error {
	user, err := acting(ctx)
	if err != nil {
		return err
	}
	src := normalize(oldName)
	dst := normalize(newName)
	if src == "/" || dst == "/" {
		return os.ErrPermission
	}
	if src == dst {
		return nil
	}
	if strings.HasPrefix(dst+"/", src+"/") {
		return os.ErrInvalid // cannot move into itself
	}

	return f.tx.WithTx(ctx, func(ctx context.Context) error {
		node, err := f.nodes.GetByPath(ctx, user.ID, src)
		if err != nil {
			return toFSErr(err)
		}
		parent, err := f.nodes.GetByPath(ctx, user.ID, path.Dir(dst))
		if err != nil {
			return toFSErr(err)
		}
		if !parent.IsDir {
			return os.ErrNotExist
		}
		// Overwrite destination if present.
		if existing, err := f.nodes.GetByPath(ctx, user.ID, dst); err == nil {
			if err := f.removeWithin(ctx, user.ID, existing); err != nil {
				return err
			}
		} else if !errors.Is(err, model.ErrNotFound) {
			return err
		}

		if node.IsDir {
			subtree, err := f.nodes.ListSubtree(ctx, user.ID, src)
			if err != nil {
				return err
			}
			for i := range subtree {
				child := &subtree[i]
				newPath := dst + strings.TrimPrefix(child.Path, src)
				child.Path = newPath
				if child.ID == node.ID {
					child.Name = path.Base(dst)
					child.ParentID = &parent.ID
				}
				child.ModifiedAt = time.Now()
				if err := f.nodes.Update(ctx, child); err != nil {
					return err
				}
			}
			return nil
		}
		node.Name = path.Base(dst)
		node.Path = dst
		node.ParentID = &parent.ID
		node.ModifiedAt = time.Now()
		return f.nodes.Update(ctx, node)
	})
}

// removeWithin deletes a node (and its subtree) within an existing transaction.
func (f *FileSystem) removeWithin(ctx context.Context, userID uuid.UUID, node *model.Node) error {
	subtree, err := f.nodes.ListSubtree(ctx, userID, node.Path)
	if err != nil {
		return err
	}
	if len(subtree) == 0 {
		subtree = []model.Node{*node}
	}
	for i := range subtree {
		if subtree[i].IsDir {
			continue
		}
		if err := f.releaseExtents(ctx, subtree[i].ID); err != nil {
			return err
		}
	}
	return f.nodes.Delete(ctx, node.ID)
}

// CheckQuota reports whether writing `additional` bytes to path p would exceed
// the acting user's quota; used by the server to answer PUT with 507 before
// streaming. If a file already exists at p it is about to be overwritten, so its
// current size (already included in the user's usage) is discounted to avoid a
// spurious 507 on an in-place overwrite.
func (f *FileSystem) CheckQuota(ctx context.Context, p string, additional int64) error {
	user, err := acting(ctx)
	if err != nil {
		return err
	}
	if user.QuotaBytes <= 0 || additional <= 0 {
		return nil
	}
	used, err := f.nodes.SumSizeByUser(ctx, user.ID)
	if err != nil {
		return err
	}
	if existing, err := f.nodes.GetByPath(ctx, user.ID, normalize(p)); err == nil && !existing.IsDir {
		used -= existing.Size
	} else if err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}
	if used+additional > user.QuotaBytes {
		return model.ErrQuotaExceeded
	}
	return nil
}

// QuotaUsage returns (used, total) bytes for the acting user (total 0 = unlimited).
func (f *FileSystem) QuotaUsage(ctx context.Context) (used, total int64, err error) {
	user, err := acting(ctx)
	if err != nil {
		return 0, 0, err
	}
	used, err = f.nodes.SumSizeByUser(ctx, user.ID)
	return used, user.QuotaBytes, err
}
