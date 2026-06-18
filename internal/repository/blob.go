package repository

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// blobRepository persists blobs (Telegram channel messages) on Postgres via
// sqlc. It resolves its executor per call from the context so the same value is
// correct inside or outside a transaction.
type blobRepository struct {
	pool *pgxpool.Pool
}

// NewBlobRepository returns a blob repository backed by pool. Its method set
// mirrors the BlobRepository contract (domain.Blob → model.Blob).
func NewBlobRepository(pool *pgxpool.Pool) *blobRepository {
	return &blobRepository{pool: pool}
}

// blobRowToModel maps a sqlc blob row onto the model, casting the integer state
// column to the BlobState enum and the nullable sealed_at timestamp to a pointer.
func blobRowToModel(row sqlc.Blob) model.Blob {
	return model.Blob{
		ID:         row.ID,
		ChannelID:  row.ChannelID,
		MessageID:  row.MessageID,
		MessageSeq: row.MessageSeq,
		Size:       row.Size,
		State:      model.BlobState(row.State),
		Refcount:   row.Refcount,
		CreatedAt:  row.CreatedAt.Time,
		SealedAt:   timeToPtr(row.SealedAt),
	}
}

// blobRowsToModel maps a slice of sqlc rows onto the model.
func blobRowsToModel(rows []sqlc.Blob) []model.Blob {
	out := make([]model.Blob, len(rows))
	for i := range rows {
		out[i] = blobRowToModel(rows[i])
	}
	return out
}

// Create inserts a new blob, generating an id and created_at when unset (mirrors
// the old GORM behavior).
func (r *blobRepository) Create(ctx context.Context, b *model.Blob) error {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).CreateBlob(ctx, sqlc.CreateBlobParams{
		ID:         b.ID,
		ChannelID:  b.ChannelID,
		MessageID:  b.MessageID,
		MessageSeq: b.MessageSeq,
		Size:       b.Size,
		State:      int32(b.State),
		Refcount:   b.Refcount,
		CreatedAt:  ptrToTime(&b.CreatedAt),
		SealedAt:   ptrToTime(b.SealedAt),
	})
	if err != nil {
		return fmt.Errorf("create blob: %w", translateError(err))
	}
	return nil
}

// GetByID loads a blob by primary key.
func (r *blobRepository) GetByID(ctx context.Context, id uuid.UUID) (*model.Blob, error) {
	db := database.FromContext(ctx, r.pool)
	row, err := sqlc.New(db).GetBlobByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get blob by id: %w", translateError(err))
	}
	b := blobRowToModel(row)
	return &b, nil
}

// Update saves the mutable columns of a blob.
func (r *blobRepository) Update(ctx context.Context, b *model.Blob) error {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).UpdateBlob(ctx, sqlc.UpdateBlobParams{
		ID:         b.ID,
		ChannelID:  b.ChannelID,
		MessageID:  b.MessageID,
		MessageSeq: b.MessageSeq,
		Size:       b.Size,
		State:      int32(b.State),
		Refcount:   b.Refcount,
		SealedAt:   ptrToTime(b.SealedAt),
	})
	if err != nil {
		return fmt.Errorf("update blob: %w", translateError(err))
	}
	if n == 0 {
		return fmt.Errorf("update blob: %w", ErrNotFound)
	}
	return nil
}

// SetState flips a blob's lifecycle state.
func (r *blobRepository) SetState(ctx context.Context, id uuid.UUID, state model.BlobState) error {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).SetBlobState(ctx, sqlc.SetBlobStateParams{
		ID:    id,
		State: int32(state),
	})
	if err != nil {
		return fmt.Errorf("set blob state: %w", translateError(err))
	}
	if n == 0 {
		return fmt.Errorf("set blob state: %w", ErrNotFound)
	}
	return nil
}

// AddRefcount atomically adds delta (possibly negative) to a blob's refcount.
// The underlying UPDATE ... RETURNING applies the addition in a single statement
// so concurrent callers each see a consistent increment. The returned refcount is
// discarded to match the BlobRepository signature. When no row matches the id the
// :one query yields pgx.ErrNoRows, which translateError maps to ErrNotFound.
func (r *blobRepository) AddRefcount(ctx context.Context, id uuid.UUID, delta int64) error {
	db := database.FromContext(ctx, r.pool)
	_, err := sqlc.New(db).AddBlobRefcount(ctx, sqlc.AddBlobRefcountParams{
		ID:       id,
		Refcount: delta,
	})
	if err != nil {
		return fmt.Errorf("add blob refcount: %w", translateError(err))
	}
	return nil
}

// ListByChannel returns every blob in a channel ordered by message_seq.
func (r *blobRepository) ListByChannel(ctx context.Context, channelID uuid.UUID) ([]model.Blob, error) {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ListBlobsByChannel(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("list blobs by channel: %w", translateError(err))
	}
	return blobRowsToModel(rows), nil
}

// ListByState returns every blob currently in the given state ordered by
// created_at.
func (r *blobRepository) ListByState(ctx context.Context, state model.BlobState) ([]model.Blob, error) {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ListBlobsByState(ctx, int32(state))
	if err != nil {
		return nil, fmt.Errorf("list blobs by state: %w", translateError(err))
	}
	return blobRowsToModel(rows), nil
}

// ListCollectable returns stored blobs whose refcount has dropped to <= 0 and
// that survived the 10-minute grace period enforced by the query. The generated
// query always applies a LIMIT, but the old GORM behavior treated limit <= 0 as
// unlimited; preserve that by passing a very large limit in that case.
func (r *blobRepository) ListCollectable(ctx context.Context, limit int) ([]model.Blob, error) {
	lim := int32(limit)
	if limit <= 0 {
		lim = math.MaxInt32
	}
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).ListCollectableBlobs(ctx, sqlc.ListCollectableBlobsParams{
		State: int32(model.BlobStateStored),
		Limit: lim,
	})
	if err != nil {
		return nil, fmt.Errorf("list collectable blobs: %w", translateError(err))
	}
	return blobRowsToModel(rows), nil
}

// MarkChannelUnavailable flips every blob of a channel to the unavailable state,
// leaving perm_unavailable blobs untouched (those never recover).
func (r *blobRepository) MarkChannelUnavailable(ctx context.Context, channelID uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).MarkChannelBlobsUnavailable(ctx, sqlc.MarkChannelBlobsUnavailableParams{
		ChannelID: channelID,
		State:     int32(model.BlobStateUnavailable),
		State_2:   int32(model.BlobStatePermUnavailable),
	})
	if err != nil {
		return fmt.Errorf("mark channel blobs unavailable: %w", translateError(err))
	}
	return nil
}

// MarkChannelAvailable restores a channel's previously-unavailable blobs to the
// stored state. perm_unavailable blobs are never restored.
func (r *blobRepository) MarkChannelAvailable(ctx context.Context, channelID uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).MarkChannelBlobsAvailable(ctx, sqlc.MarkChannelBlobsAvailableParams{
		ChannelID: channelID,
		State:     int32(model.BlobStateStored),
		State_2:   int32(model.BlobStateUnavailable),
	})
	if err != nil {
		return fmt.Errorf("mark channel blobs available: %w", translateError(err))
	}
	return nil
}

// EvictOlderThan marks every non-perm blob with message_seq < minSeq as
// unavailable and returns the number of rows affected.
func (r *blobRepository) EvictOlderThan(ctx context.Context, channelID uuid.UUID, minSeq int64) (int64, error) {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).EvictBlobsOlderThan(ctx, sqlc.EvictBlobsOlderThanParams{
		ChannelID:  channelID,
		MessageSeq: minSeq,
		State:      int32(model.BlobStatePermUnavailable),
		State_2:    int32(model.BlobStateUnavailable),
	})
	if err != nil {
		return 0, fmt.Errorf("evict blobs: %w", translateError(err))
	}
	return n, nil
}

// Delete removes a blob by id.
func (r *blobRepository) Delete(ctx context.Context, id uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).DeleteBlob(ctx, id)
	if err != nil {
		return fmt.Errorf("delete blob: %w", translateError(err))
	}
	if n == 0 {
		return fmt.Errorf("delete blob: %w", ErrNotFound)
	}
	return nil
}

// Count returns the number of blobs.
func (r *blobRepository) Count(ctx context.Context) (int64, error) {
	db := database.FromContext(ctx, r.pool)
	n, err := sqlc.New(db).CountBlobs(ctx)
	if err != nil {
		return 0, fmt.Errorf("count blobs: %w", translateError(err))
	}
	return n, nil
}
