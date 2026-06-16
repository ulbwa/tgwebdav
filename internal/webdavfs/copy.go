package webdavfs

import (
	"context"
	"errors"
	"os"
	"path"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
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

	return f.tx.WithTx(ctx, func(ctx context.Context, r *domain.Repositories) error {
		srcNode, err := r.Nodes.GetByPath(ctx, user.ID, s)
		if err != nil {
			return toFSErr(err)
		}
		parent, err := r.Nodes.GetByPath(ctx, user.ID, path.Dir(d))
		if err != nil {
			return toFSErr(err)
		}
		if !parent.IsDir {
			return os.ErrNotExist
		}
		if existing, err := r.Nodes.GetByPath(ctx, user.ID, d); err == nil {
			if !overwrite {
				return os.ErrExist
			}
			if err := f.removeWithin(ctx, r, user.ID, existing); err != nil {
				return err
			}
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		return f.copyNode(ctx, r, user.ID, srcNode, d, &parent.ID, recursive)
	})
}

func (f *FileSystem) copyNode(ctx context.Context, r *domain.Repositories, userID uuid.UUID, srcNode *domain.Node, dstPath string, dstParentID *uuid.UUID, recursive bool) error {
	now := time.Now()
	newID := uuid.New()
	dstNode := &domain.Node{
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
		dstNode.State = domain.NodeStored
		if err := r.Nodes.Create(ctx, dstNode); err != nil {
			return toFSErr(err)
		}
		if !recursive {
			return nil
		}
		children, err := r.Nodes.ListChildren(ctx, userID, srcNode.ID)
		if err != nil {
			return err
		}
		for i := range children {
			child := &children[i]
			childDst := dstPath + "/" + child.Name
			if err := f.copyNode(ctx, r, userID, child, childDst, &newID, recursive); err != nil {
				return err
			}
		}
		return nil
	}

	// File.
	if srcNode.State == domain.NodeStored {
		dstNode.State = domain.NodeStored
		if err := r.Nodes.Create(ctx, dstNode); err != nil {
			return toFSErr(err)
		}
		srcExtents, err := r.Extents.ListByNode(ctx, srcNode.ID)
		if err != nil {
			return err
		}
		if err := r.Extents.CopyForNode(ctx, srcNode.ID, newID); err != nil {
			return err
		}
		for _, e := range srcExtents {
			if err := r.Blobs.AddRefcount(ctx, e.BlobID, 1); err != nil {
				return err
			}
		}
		return nil
	}

	// Buffered/writing: copy WAL bytes so the new node packs independently.
	dstNode.State = domain.NodeBuffered
	if err := r.Nodes.Create(ctx, dstNode); err != nil {
		return toFSErr(err)
	}
	const block = 1 << 20
	var off, seq int64
	for off < srcNode.Size {
		n := int64(block)
		if off+n > srcNode.Size {
			n = srcNode.Size - off
		}
		data, err := r.WAL.ReadRange(ctx, srcNode.ID, off, n)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			break
		}
		if err := r.WAL.AppendChunk(ctx, &domain.WALChunk{ID: uuid.New(), NodeID: newID, Seq: seq, Data: data}); err != nil {
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
