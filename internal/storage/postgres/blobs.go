package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// blobRepo implements domain.BlobRepository.
type blobRepo struct{ base *gorm.DB }

// Create inserts a new blob.
func (r *blobRepo) Create(ctx context.Context, b *domain.Blob) error {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	if err := txFromCtx(ctx, r.base).Create(blobToModel(b)).Error; err != nil {
		return fmt.Errorf("create blob: %w", translateError(err))
	}
	return nil
}

// GetByID loads a blob by primary key.
func (r *blobRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Blob, error) {
	var m blobModel
	if err := txFromCtx(ctx, r.base).Where("id = ?", id).First(&m).Error; err != nil {
		return nil, fmt.Errorf("get blob by id: %w", translateError(err))
	}
	return m.toDomain(), nil
}

// Update saves the mutable columns of a blob.
func (r *blobRepo) Update(ctx context.Context, b *domain.Blob) error {
	res := txFromCtx(ctx, r.base).Model(&blobModel{}).
		Where("id = ?", b.ID).
		Updates(map[string]any{
			"channel_id":  b.ChannelID,
			"message_id":  b.MessageID,
			"message_seq": b.MessageSeq,
			"size":        b.Size,
			"state":       string(b.State),
			"refcount":    b.Refcount,
			"sealed_at":   b.SealedAt,
		})
	if res.Error != nil {
		return fmt.Errorf("update blob: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update blob: %w", domain.ErrNotFound)
	}
	return nil
}

// SetState flips a blob's lifecycle state.
func (r *blobRepo) SetState(ctx context.Context, id uuid.UUID, state domain.BlobState) error {
	res := txFromCtx(ctx, r.base).Model(&blobModel{}).
		Where("id = ?", id).
		Update("state", string(state))
	if res.Error != nil {
		return fmt.Errorf("set blob state: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set blob state: %w", domain.ErrNotFound)
	}
	return nil
}

// AddRefcount atomically adds delta (possibly negative) to a blob's refcount.
func (r *blobRepo) AddRefcount(ctx context.Context, id uuid.UUID, delta int64) error {
	res := txFromCtx(ctx, r.base).Model(&blobModel{}).
		Where("id = ?", id).
		UpdateColumn("refcount", gorm.Expr("refcount + ?", delta))
	if res.Error != nil {
		return fmt.Errorf("add blob refcount: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("add blob refcount: %w", domain.ErrNotFound)
	}
	return nil
}

// ListByChannel returns every blob in a channel.
func (r *blobRepo) ListByChannel(ctx context.Context, channelID uuid.UUID) ([]domain.Blob, error) {
	var ms []blobModel
	if err := txFromCtx(ctx, r.base).Where("channel_id = ?", channelID).Order("message_seq").Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list blobs by channel: %w", translateError(err))
	}
	return blobsToDomain(ms), nil
}

// ListByState returns every blob currently in the given state.
func (r *blobRepo) ListByState(ctx context.Context, state domain.BlobState) ([]domain.Blob, error) {
	var ms []blobModel
	if err := txFromCtx(ctx, r.base).Where("state = ?", string(state)).Order("created_at").Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list blobs by state: %w", translateError(err))
	}
	return blobsToDomain(ms), nil
}

// ListCollectable returns stored blobs whose refcount has dropped to <= 0.
func (r *blobRepo) ListCollectable(ctx context.Context, limit int) ([]domain.Blob, error) {
	var ms []blobModel
	// A 10-minute grace period protects freshly-uploaded blobs whose extents are
	// still being written by the packer in a separate finalize transaction (a new
	// blob is created with refcount 0; the refcount is incremented when each of
	// its nodes is finalized). Without it the GC could delete an in-flight blob.
	q := txFromCtx(ctx, r.base).
		Where("state = ? AND refcount <= 0 AND created_at < now() - interval '10 minutes'", string(domain.BlobStored)).
		Order("created_at")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list collectable blobs: %w", translateError(err))
	}
	return blobsToDomain(ms), nil
}

// MarkChannelUnavailable flips every blob of a channel to the unavailable
// state, leaving perm_unavailable blobs untouched (those never recover).
func (r *blobRepo) MarkChannelUnavailable(ctx context.Context, channelID uuid.UUID) error {
	err := txFromCtx(ctx, r.base).Model(&blobModel{}).
		Where("channel_id = ? AND state <> ?", channelID, string(domain.BlobPermUnavailable)).
		Update("state", string(domain.BlobUnavailable)).Error
	if err != nil {
		return fmt.Errorf("mark channel blobs unavailable: %w", translateError(err))
	}
	return nil
}

// MarkChannelAvailable restores a channel's previously-unavailable blobs to the
// stored state. perm_unavailable blobs are never restored.
func (r *blobRepo) MarkChannelAvailable(ctx context.Context, channelID uuid.UUID) error {
	err := txFromCtx(ctx, r.base).Model(&blobModel{}).
		Where("channel_id = ? AND state = ?", channelID, string(domain.BlobUnavailable)).
		Update("state", string(domain.BlobStored)).Error
	if err != nil {
		return fmt.Errorf("mark channel blobs available: %w", translateError(err))
	}
	return nil
}

// EvictOlderThan marks every non-perm blob with message_seq < minSeq as
// unavailable and returns the number of rows affected.
func (r *blobRepo) EvictOlderThan(ctx context.Context, channelID uuid.UUID, minSeq int64) (int64, error) {
	res := txFromCtx(ctx, r.base).Model(&blobModel{}).
		Where("channel_id = ? AND message_seq < ? AND state <> ?",
			channelID, minSeq, string(domain.BlobPermUnavailable)).
		Update("state", string(domain.BlobUnavailable))
	if res.Error != nil {
		return 0, fmt.Errorf("evict blobs: %w", translateError(res.Error))
	}
	return res.RowsAffected, nil
}

// Delete removes a blob by id.
func (r *blobRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res := txFromCtx(ctx, r.base).Where("id = ?", id).Delete(&blobModel{})
	if res.Error != nil {
		return fmt.Errorf("delete blob: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete blob: %w", domain.ErrNotFound)
	}
	return nil
}

// Count returns the number of blobs.
func (r *blobRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := txFromCtx(ctx, r.base).Model(&blobModel{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count blobs: %w", translateError(err))
	}
	return n, nil
}

func blobsToDomain(ms []blobModel) []domain.Blob {
	out := make([]domain.Blob, len(ms))
	for i := range ms {
		out[i] = *ms[i].toDomain()
	}
	return out
}
