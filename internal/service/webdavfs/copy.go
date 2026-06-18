package webdavfs

import (
	"context"
	"errors"
	"os"
	"path"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
)

// Copy duplicates src to dst. Stored files share their immutable blobs (extents
// are duplicated and blob refcounts bumped — no re-upload); buffered files copy
// their WAL bytes. Directories are copied shallowly unless recursive is true.
// It returns os.ErrExist if dst exists and overwrite is false.
func (f *FileSystem) Copy(ctx context.Context, src, dst string, recursive, overwrite bool) error {
	user, err := acting(ctx)
	if err != nil {
		return err
	}
	s := normalize(src)
	d := normalize(dst)
	if s == "/" || d == "/" {
		return os.ErrPermission
	}
	if s == d {
		return nil
	}
	if pathHasPrefix(d, s) {
		return os.ErrInvalid // cannot copy into itself
	}
	if _, err := f.ensureRoot(ctx, user.ID); err != nil {
		return err
	}

	return f.tx.WithTx(ctx, func(ctx context.Context) error {
		srcNode, err := f.nodes.GetByPath(ctx, user.ID, s)
		if err != nil {
			return toFSErr(err)
		}
		parent, err := f.nodes.GetByPath(ctx, user.ID, path.Dir(d))
		if err != nil {
			return toFSErr(err)
		}
		if !parent.IsDir {
			return os.ErrNotExist
		}
		// Quota: a copy adds the source's logical size to the user's usage. If the
		// destination already exists (overwrite) its freed size is discounted.
		if user.QuotaBytes > 0 {
			used, err := f.nodes.SumSizeByUser(ctx, user.ID)
			if err != nil {
				return err
			}
			add, err := f.subtreeSize(ctx, user.ID, s)
			if err != nil {
				return err
			}
			if existing, err := f.nodes.GetByPath(ctx, user.ID, d); err == nil {
				freed, err := f.subtreeSize(ctx, user.ID, existing.Path)
				if err != nil {
					return err
				}
				add -= freed
			} else if !errors.Is(err, repository.ErrNotFound) {
				return err
			}
			if used+add > user.QuotaBytes {
				return ErrQuotaExceeded
			}
		}
		if existing, err := f.nodes.GetByPath(ctx, user.ID, d); err == nil {
			if !overwrite {
				return os.ErrExist
			}
			if err := f.removeWithin(ctx, user.ID, existing); err != nil {
				return err
			}
		} else if !errors.Is(err, repository.ErrNotFound) {
			return err
		}
		return f.copyNode(ctx, user.ID, srcNode, d, &parent.ID, recursive)
	})
}

// subtreeSize sums the logical size of all file nodes at or below path p.
func (f *FileSystem) subtreeSize(ctx context.Context, userID uuid.UUID, p string) (int64, error) {
	nodes, err := f.nodes.ListSubtree(ctx, userID, p)
	if err != nil {
		return 0, err
	}
	var sum int64
	for i := range nodes {
		if !nodes[i].IsDir {
			sum += nodes[i].Size
		}
	}
	return sum, nil
}

func (f *FileSystem) copyNode(ctx context.Context, userID uuid.UUID, srcNode *model.Node, dstPath string, dstParentID *uuid.UUID, recursive bool) error {
	now := time.Now()
	newID := uuid.New()
	dstNode := &model.Node{
		ID:          newID,
		UserID:      userID,
		ParentID:    dstParentID,
		Name:        path.Base(dstPath),
		Path:        dstPath,
		IsDir:       srcNode.IsDir,
		Size:        srcNode.Size,
		ContentHash: srcNode.ContentHash,
		ETag:        srcNode.ETag,
		ContentType: srcNode.ContentType,
		State:       srcNode.State,
		CreatedAt:   now,
		ModifiedAt:  now,
	}

	if srcNode.IsDir {
		dstNode.State = model.NodeStateStored
		if err := f.nodes.Create(ctx, dstNode); err != nil {
			return toFSErr(err)
		}
		if !recursive {
			return nil
		}
		children, err := f.nodes.ListChildren(ctx, userID, srcNode.ID)
		if err != nil {
			return err
		}
		for i := range children {
			child := &children[i]
			childDst := dstPath + "/" + child.Name
			if err := f.copyNode(ctx, userID, child, childDst, &newID, recursive); err != nil {
				return err
			}
		}
		return nil
	}

	// File.
	if srcNode.State == model.NodeStateStored {
		dstNode.State = model.NodeStateStored
		if err := f.nodes.Create(ctx, dstNode); err != nil {
			return toFSErr(err)
		}
		srcExtents, err := f.extents.ListByNode(ctx, srcNode.ID)
		if err != nil {
			return err
		}
		if err := f.extents.CopyForNode(ctx, srcNode.ID, newID); err != nil {
			return err
		}
		for _, e := range srcExtents {
			if err := f.blobMeta.AddRefcount(ctx, e.BlobID, 1); err != nil {
				return err
			}
		}
		return nil
	}

	// Buffered/writing: copy WAL bytes so the new node packs independently.
	dstNode.State = model.NodeStateBuffered
	if err := f.nodes.Create(ctx, dstNode); err != nil {
		return toFSErr(err)
	}
	const block = 1 << 20
	var off, seq int64
	for off < srcNode.Size {
		n := int64(block)
		if off+n > srcNode.Size {
			n = srcNode.Size - off
		}
		data, err := f.wal.ReadRange(ctx, srcNode.ID, off, n)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			break
		}
		if err := f.wal.AppendChunk(ctx, &model.WALChunk{ID: uuid.New(), NodeID: newID, Seq: seq, Data: data}); err != nil {
			return err
		}
		off += int64(len(data))
		seq++
	}
	return nil
}

// pathHasPrefix reports whether child is at or under parent.
func pathHasPrefix(child, parent string) bool {
	return child == parent || len(child) > len(parent) && child[:len(parent)] == parent && child[len(parent)] == '/'
}
