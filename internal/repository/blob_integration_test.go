package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// insertTestChannel inserts a channel row (the FK parent of blobs) and returns
// its id.
func insertTestChannel(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := sqlc.New(pool).CreateChannel(ctx, sqlc.CreateChannelParams{
		ID:                id,
		TgChatID:          int64(uuid.New().ID()), // unique-ish chat id
		Title:             "test-channel",
		MessageCounter:    0,
		EvictionThreshold: 900000,
		Available:         true,
		CreatedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("insert test channel: %v", err)
	}
	return id
}

func TestBlobRepository_CreateGetUpdate(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobRepository(pool)
	channelID := insertTestChannel(ctx, t, pool)

	sealed := time.Now().UTC().Truncate(time.Microsecond)
	b := &model.Blob{
		ChannelID:  channelID,
		MessageID:  42,
		MessageSeq: 7,
		Size:       1024,
		State:      model.BlobStateSealed,
		Refcount:   0,
		SealedAt:   &sealed,
	}
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.ID == uuid.Nil {
		t.Fatal("Create did not assign an id")
	}
	if b.CreatedAt.IsZero() {
		t.Fatal("Create did not set CreatedAt")
	}

	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.MessageID != 42 || got.MessageSeq != 7 || got.Size != 1024 {
		t.Fatalf("GetByID mismatch: %+v", got)
	}
	if got.State != model.BlobStateSealed {
		t.Fatalf("State = %v, want Sealed", got.State)
	}
	if got.SealedAt == nil || !got.SealedAt.Equal(sealed) {
		t.Fatalf("SealedAt = %v, want %v", got.SealedAt, sealed)
	}

	// Update mutable columns.
	got.Size = 2048
	got.MessageID = 99
	got.State = model.BlobStateStored
	got.Refcount = 3
	got.SealedAt = nil
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reloaded, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if reloaded.Size != 2048 || reloaded.MessageID != 99 || reloaded.Refcount != 3 {
		t.Fatalf("Update not persisted: %+v", reloaded)
	}
	if reloaded.State != model.BlobStateStored {
		t.Fatalf("State after update = %v, want Stored", reloaded.State)
	}
	if reloaded.SealedAt != nil {
		t.Fatalf("SealedAt after update = %v, want nil", reloaded.SealedAt)
	}

	// Update of a missing row → ErrNotFound.
	ghost := &model.Blob{ID: uuid.New(), ChannelID: channelID}
	if err := repo.Update(ctx, ghost); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Update missing: err = %v, want ErrNotFound", err)
	}
}

func TestBlobRepository_SetState(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobRepository(pool)
	channelID := insertTestChannel(ctx, t, pool)

	b := &model.Blob{ChannelID: channelID, State: model.BlobStateOpen}
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.SetState(ctx, b.ID, model.BlobStateStored); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != model.BlobStateStored {
		t.Fatalf("State = %v, want Stored", got.State)
	}
	if err := repo.SetState(ctx, uuid.New(), model.BlobStateStored); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("SetState missing: err = %v, want ErrNotFound", err)
	}
}

// TestBlobRepository_AddRefcountAtomicity fires N concurrent +1 increments at a
// single blob and asserts the final refcount is exactly N, proving the UPDATE is
// atomic (no lost updates).
func TestBlobRepository_AddRefcountAtomicity(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobRepository(pool)
	channelID := insertTestChannel(ctx, t, pool)

	b := &model.Blob{ChannelID: channelID, State: model.BlobStateStored}
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := repo.AddRefcount(ctx, b.ID, 1); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("AddRefcount: %v", err)
	}

	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Refcount != n {
		t.Fatalf("refcount = %d, want %d", got.Refcount, n)
	}

	// Negative delta and missing-row behavior.
	if err := repo.AddRefcount(ctx, b.ID, -10); err != nil {
		t.Fatalf("AddRefcount negative: %v", err)
	}
	got, _ = repo.GetByID(ctx, b.ID)
	if got.Refcount != n-10 {
		t.Fatalf("refcount after -10 = %d, want %d", got.Refcount, n-10)
	}
	if err := repo.AddRefcount(ctx, uuid.New(), 1); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("AddRefcount missing: err = %v, want ErrNotFound", err)
	}
}

// TestBlobRepository_ListCollectable covers the refcount/state/grace-period
// filter, the explicit limit, and the unlimited (limit <= 0) case.
func TestBlobRepository_ListCollectable(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobRepository(pool)
	channelID := insertTestChannel(ctx, t, pool)

	old := time.Now().Add(-time.Hour)

	// Collectable: stored, refcount <= 0, older than the 10-minute grace period.
	collectable := make([]uuid.UUID, 0, 5)
	for i := 0; i < 5; i++ {
		b := &model.Blob{ChannelID: channelID, State: model.BlobStateStored, Refcount: 0, CreatedAt: old}
		if err := repo.Create(ctx, b); err != nil {
			t.Fatalf("Create collectable: %v", err)
		}
		collectable = append(collectable, b.ID)
	}

	// Not collectable: positive refcount.
	if err := repo.Create(ctx, &model.Blob{ChannelID: channelID, State: model.BlobStateStored, Refcount: 1, CreatedAt: old}); err != nil {
		t.Fatalf("Create refcounted: %v", err)
	}
	// Not collectable: wrong state.
	if err := repo.Create(ctx, &model.Blob{ChannelID: channelID, State: model.BlobStateSealed, Refcount: 0, CreatedAt: old}); err != nil {
		t.Fatalf("Create sealed: %v", err)
	}
	// Not collectable: inside the grace period (created just now).
	if err := repo.Create(ctx, &model.Blob{ChannelID: channelID, State: model.BlobStateStored, Refcount: 0, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create fresh: %v", err)
	}

	// Unlimited (limit <= 0) returns all 5 collectable blobs and nothing else.
	all, err := repo.ListCollectable(ctx, 0)
	if err != nil {
		t.Fatalf("ListCollectable(0): %v", err)
	}
	if len(all) != len(collectable) {
		t.Fatalf("ListCollectable(0) returned %d, want %d", len(all), len(collectable))
	}
	for _, b := range all {
		if b.State != model.BlobStateStored || b.Refcount > 0 {
			t.Fatalf("ListCollectable returned ineligible blob: %+v", b)
		}
	}

	// Negative limit is also unlimited.
	neg, err := repo.ListCollectable(ctx, -1)
	if err != nil {
		t.Fatalf("ListCollectable(-1): %v", err)
	}
	if len(neg) != len(collectable) {
		t.Fatalf("ListCollectable(-1) returned %d, want %d", len(neg), len(collectable))
	}

	// Explicit limit caps the result.
	limited, err := repo.ListCollectable(ctx, 2)
	if err != nil {
		t.Fatalf("ListCollectable(2): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("ListCollectable(2) returned %d, want 2", len(limited))
	}
}

func TestBlobRepository_EvictOlderThan(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobRepository(pool)
	channelID := insertTestChannel(ctx, t, pool)

	// seq 1,2 should be evicted (< minSeq=3); seq 3,4 must remain stored.
	mk := func(seq int64, state model.BlobState) uuid.UUID {
		b := &model.Blob{ChannelID: channelID, MessageSeq: seq, State: state}
		if err := repo.Create(ctx, b); err != nil {
			t.Fatalf("Create seq=%d: %v", seq, err)
		}
		return b.ID
	}
	low1 := mk(1, model.BlobStateStored)
	low2 := mk(2, model.BlobStateStored)
	high3 := mk(3, model.BlobStateStored)
	high4 := mk(4, model.BlobStateStored)
	// A perm-unavailable blob below the threshold must NOT be touched/counted.
	perm := mk(0, model.BlobStatePermUnavailable)

	n, err := repo.EvictOlderThan(ctx, channelID, 3)
	if err != nil {
		t.Fatalf("EvictOlderThan: %v", err)
	}
	if n != 2 {
		t.Fatalf("EvictOlderThan affected %d, want 2", n)
	}

	assertState := func(id uuid.UUID, want model.BlobState) {
		got, err := repo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.State != want {
			t.Fatalf("blob %s state = %v, want %v", id, got.State, want)
		}
	}
	assertState(low1, model.BlobStateUnavailable)
	assertState(low2, model.BlobStateUnavailable)
	assertState(high3, model.BlobStateStored)
	assertState(high4, model.BlobStateStored)
	assertState(perm, model.BlobStatePermUnavailable)
}

func TestBlobRepository_MarkChannelAvailability(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobRepository(pool)
	channelID := insertTestChannel(ctx, t, pool)

	stored := &model.Blob{ChannelID: channelID, State: model.BlobStateStored}
	perm := &model.Blob{ChannelID: channelID, State: model.BlobStatePermUnavailable}
	for _, b := range []*model.Blob{stored, perm} {
		if err := repo.Create(ctx, b); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	// Unavailable flips stored → unavailable but leaves perm untouched.
	if err := repo.MarkChannelUnavailable(ctx, channelID); err != nil {
		t.Fatalf("MarkChannelUnavailable: %v", err)
	}
	if got, _ := repo.GetByID(ctx, stored.ID); got.State != model.BlobStateUnavailable {
		t.Fatalf("stored state = %v, want Unavailable", got.State)
	}
	if got, _ := repo.GetByID(ctx, perm.ID); got.State != model.BlobStatePermUnavailable {
		t.Fatalf("perm state = %v, want PermUnavailable", got.State)
	}

	// Available restores unavailable → stored but still leaves perm untouched.
	if err := repo.MarkChannelAvailable(ctx, channelID); err != nil {
		t.Fatalf("MarkChannelAvailable: %v", err)
	}
	if got, _ := repo.GetByID(ctx, stored.ID); got.State != model.BlobStateStored {
		t.Fatalf("stored state after restore = %v, want Stored", got.State)
	}
	if got, _ := repo.GetByID(ctx, perm.ID); got.State != model.BlobStatePermUnavailable {
		t.Fatalf("perm state after restore = %v, want PermUnavailable", got.State)
	}
}

func TestBlobRepository_ListByChannelAndState(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobRepository(pool)
	channelID := insertTestChannel(ctx, t, pool)
	otherChannel := insertTestChannel(ctx, t, pool)

	if err := repo.Create(ctx, &model.Blob{ChannelID: channelID, MessageSeq: 2, State: model.BlobStateStored}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Create(ctx, &model.Blob{ChannelID: channelID, MessageSeq: 1, State: model.BlobStateSealed}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Create(ctx, &model.Blob{ChannelID: otherChannel, MessageSeq: 5, State: model.BlobStateStored}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	byChannel, err := repo.ListByChannel(ctx, channelID)
	if err != nil {
		t.Fatalf("ListByChannel: %v", err)
	}
	if len(byChannel) != 2 {
		t.Fatalf("ListByChannel returned %d, want 2", len(byChannel))
	}
	// Ordered by message_seq ascending.
	if byChannel[0].MessageSeq != 1 || byChannel[1].MessageSeq != 2 {
		t.Fatalf("ListByChannel not ordered by message_seq: %+v", byChannel)
	}

	byState, err := repo.ListByState(ctx, model.BlobStateStored)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	if len(byState) != 2 {
		t.Fatalf("ListByState(Stored) returned %d, want 2", len(byState))
	}
}

func TestBlobRepository_DeleteAndCount(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobRepository(pool)
	channelID := insertTestChannel(ctx, t, pool)

	if n, err := repo.Count(ctx); err != nil || n != 0 {
		t.Fatalf("Count empty = %d, %v; want 0, nil", n, err)
	}

	b := &model.Blob{ChannelID: channelID, State: model.BlobStateStored}
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n, err := repo.Count(ctx); err != nil || n != 1 {
		t.Fatalf("Count = %d, %v; want 1, nil", n, err)
	}

	if err := repo.Delete(ctx, b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, b.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetByID after delete: err = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, b.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Delete missing: err = %v, want ErrNotFound", err)
	}
	if n, err := repo.Count(ctx); err != nil || n != 0 {
		t.Fatalf("Count after delete = %d, %v; want 0, nil", n, err)
	}
}

func TestBlobRepository_GetByID_NotFound(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBlobRepository(pool)
	if _, err := repo.GetByID(ctx, uuid.New()); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetByID missing: err = %v, want ErrNotFound", err)
	}
}
