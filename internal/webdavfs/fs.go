// Package webdavfs implements golang.org/x/net/webdav.FileSystem backed by the
// tgwebdav store. Each request operates inside the acting user's isolated
// namespace, resolved from the principal carried on the context. Reads assemble
// content from WAL bytes (buffered nodes) or blob extents (stored nodes); writes
// append to the WAL and become readable immediately.
package webdavfs

import (
	"context"
	"crypto/sha256"
	"errors"
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

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// FileSystem is the webdav.FileSystem implementation. It also exposes Copy (for
// blob-sharing COPY) and CheckQuota (for a correct 507 pre-check), used by the
// WebDAV HTTP server.
type FileSystem struct {
	repos    *domain.Repositories
	tx       domain.TxManager
	blobs    domain.BlobReader
	limiter  domain.Limiter
	settings domain.SettingsService
	stats    domain.StatRecorder
	log      *slog.Logger

	rootReady sync.Map // userID → struct{}: root node known to exist
}

// NewFileSystem builds the WebDAV filesystem.
func NewFileSystem(
	r *domain.Repositories,
	tx domain.TxManager,
	blobs domain.BlobReader,
	limiter domain.Limiter,
	settings domain.SettingsService,
	stats domain.StatRecorder,
	logger *slog.Logger,
) *FileSystem {
	if logger == nil {
		logger = slog.Default()
	}
	return &FileSystem{
		repos:    r,
		tx:       tx,
		blobs:    blobs,
		limiter:  limiter,
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

func acting(ctx context.Context) (*domain.User, error) {
	p, ok := domain.PrincipalFromContext(ctx)
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
	case errors.Is(err, domain.ErrNotFound):
		return os.ErrNotExist
	case errors.Is(err, domain.ErrAlreadyExists):
		return os.ErrExist
	default:
		return err
	}
}

// ensureRoot returns the user's root directory node, creating it on first use.
func (f *FileSystem) ensureRoot(ctx context.Context, userID uuid.UUID) (*domain.Node, error) {
	root, err := f.repos.Nodes.GetByPath(ctx, userID, "/")
	if err == nil {
		f.rootReady.Store(userID, struct{}{})
		return root, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	now := time.Now()
	root = &domain.Node{
		ID:          uuid.New(),
		UserID:      userID,
		ParentID:    nil,
		Name:        "",
		Path:        "/",
		IsDir:       true,
		State:       domain.NodeStored,
		ContentType: "httpd/unix-directory",
		CreatedAt:   now,
		ModifiedAt:  now,
	}
	if err := f.repos.Nodes.Create(ctx, root); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			f.rootReady.Store(userID, struct{}{})
			return f.repos.Nodes.GetByPath(ctx, userID, "/")
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
	node, err := f.repos.Nodes.GetByPath(ctx, user.ID, p)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) && p == "/" {
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
	if _, err := f.repos.Nodes.GetByPath(ctx, user.ID, p); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	parent, err := f.repos.Nodes.GetByPath(ctx, user.ID, path.Dir(p))
	if err != nil {
		return toFSErr(err) // missing parent → os.ErrNotExist → 409
	}
	if !parent.IsDir {
		return os.ErrNotExist
	}
	now := time.Now()
	node := &domain.Node{
		ID:          uuid.New(),
		UserID:      user.ID,
		ParentID:    &parent.ID,
		Name:        path.Base(p),
		Path:        p,
		IsDir:       true,
		State:       domain.NodeStored,
		ContentType: "httpd/unix-directory",
		CreatedAt:   now,
		ModifiedAt:  now,
	}
	return toFSErr(f.repos.Nodes.Create(ctx, node))
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

func (f *FileSystem) openRead(ctx context.Context, user *domain.User, p string) (webdav.File, error) {
	node, err := f.repos.Nodes.GetByPath(ctx, user.ID, p)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) && p == "/" {
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

func (f *FileSystem) openWrite(ctx context.Context, user *domain.User, p string, flag int) (webdav.File, error) {
	if p == "/" {
		return nil, os.ErrInvalid
	}
	parent, err := f.repos.Nodes.GetByPath(ctx, user.ID, path.Dir(p))
	if err != nil {
		return nil, toFSErr(err)
	}
	if !parent.IsDir {
		return nil, os.ErrNotExist
	}

	existing, err := f.repos.Nodes.GetByPath(ctx, user.ID, p)
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
	case errors.Is(err, domain.ErrNotFound):
		now := time.Now()
		node := &domain.Node{
			ID:         uuid.New(),
			UserID:     user.ID,
			ParentID:   &parent.ID,
			Name:       path.Base(p),
			Path:       p,
			IsDir:      false,
			State:      domain.NodeWriting,
			CreatedAt:  now,
			ModifiedAt: now,
		}
		if err := f.repos.Nodes.Create(ctx, node); err != nil {
			return nil, toFSErr(err)
		}
		return f.newWriter(ctx, user, node), nil
	default:
		return nil, err
	}
}

func (f *FileSystem) newWriter(ctx context.Context, user *domain.User, node *domain.Node) *writeFile {
	return &writeFile{fs: f, ctx: ctx, node: node, user: user, hasher: sha256.New()}
}

// truncateNode resets an existing file node to a fresh writing state, releasing
// its old blob references and WAL rows in a single transaction.
func (f *FileSystem) truncateNode(ctx context.Context, node *domain.Node) error {
	return f.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		if err := releaseExtents(ctx, r, node.ID); err != nil {
			return err
		}
		if err := r.Extents.DeleteByNode(ctx, node.ID); err != nil {
			return err
		}
		if err := r.WAL.DeleteByNode(ctx, node.ID); err != nil {
			return err
		}
		fresh, err := r.Nodes.GetByID(ctx, node.ID)
		if err != nil {
			return err
		}
		fresh.State = domain.NodeWriting
		fresh.Size = 0
		fresh.ContentHash = ""
		fresh.ETag = ""
		fresh.ModifiedAt = time.Now()
		return r.Nodes.Update(ctx, fresh)
	})
}

// releaseExtents decrements the refcount of every blob a node's extents
// reference (one decrement per extent).
func releaseExtents(ctx context.Context, r *domain.Repositories, nodeID uuid.UUID) error {
	extents, err := r.Extents.ListByNode(ctx, nodeID)
	if err != nil {
		return err
	}
	for _, e := range extents {
		if err := r.Blobs.AddRefcount(ctx, e.BlobID, -1); err != nil {
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
	node, err := f.repos.Nodes.GetByPath(ctx, user.ID, p)
	if err != nil {
		return toFSErr(err)
	}
	return f.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		subtree, err := r.Nodes.ListSubtree(ctx, user.ID, p)
		if err != nil {
			return err
		}
		if len(subtree) == 0 {
			subtree = []domain.Node{*node}
		}
		for i := range subtree {
			if subtree[i].IsDir {
				continue
			}
			if err := releaseExtents(ctx, r, subtree[i].ID); err != nil {
				return err
			}
		}
		// Deleting the top node cascades children/extents/WAL via FK.
		return r.Nodes.Delete(ctx, node.ID)
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

	return f.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		node, err := r.Nodes.GetByPath(ctx, user.ID, src)
		if err != nil {
			return toFSErr(err)
		}
		parent, err := r.Nodes.GetByPath(ctx, user.ID, path.Dir(dst))
		if err != nil {
			return toFSErr(err)
		}
		if !parent.IsDir {
			return os.ErrNotExist
		}
		// Overwrite destination if present.
		if existing, err := r.Nodes.GetByPath(ctx, user.ID, dst); err == nil {
			if err := f.removeWithin(ctx, r, user.ID, existing); err != nil {
				return err
			}
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}

		if node.IsDir {
			subtree, err := r.Nodes.ListSubtree(ctx, user.ID, src)
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
				if err := r.Nodes.Update(ctx, child); err != nil {
					return err
				}
			}
			return nil
		}
		node.Name = path.Base(dst)
		node.Path = dst
		node.ParentID = &parent.ID
		node.ModifiedAt = time.Now()
		return r.Nodes.Update(ctx, node)
	})
}

// removeWithin deletes a node (and its subtree) within an existing transaction.
func (f *FileSystem) removeWithin(ctx context.Context, r *domain.Repositories, userID uuid.UUID, node *domain.Node) error {
	subtree, err := r.Nodes.ListSubtree(ctx, userID, node.Path)
	if err != nil {
		return err
	}
	if len(subtree) == 0 {
		subtree = []domain.Node{*node}
	}
	for i := range subtree {
		if subtree[i].IsDir {
			continue
		}
		if err := releaseExtents(ctx, r, subtree[i].ID); err != nil {
			return err
		}
	}
	return r.Nodes.Delete(ctx, node.ID)
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
	used, err := f.repos.Nodes.SumSizeByUser(ctx, user.ID)
	if err != nil {
		return err
	}
	if existing, err := f.repos.Nodes.GetByPath(ctx, user.ID, normalize(p)); err == nil && !existing.IsDir {
		used -= existing.Size
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if used+additional > user.QuotaBytes {
		return domain.ErrQuotaExceeded
	}
	return nil
}

// QuotaUsage returns (used, total) bytes for the acting user (total 0 = unlimited).
func (f *FileSystem) QuotaUsage(ctx context.Context) (used, total int64, err error) {
	user, err := acting(ctx)
	if err != nil {
		return 0, 0, err
	}
	used, err = f.repos.Nodes.SumSizeByUser(ctx, user.ID)
	return used, user.QuotaBytes, err
}
