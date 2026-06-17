package service

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// --- shared in-memory store backing the packer fakes -----------------------

type packerStore struct {
	mu      sync.Mutex
	nodes   map[uuid.UUID]*model.Node
	wal     map[uuid.UUID][]model.WALChunk
	leased  map[uuid.UUID]bool
	owner   map[uuid.UUID]string
	blobs   []*model.Blob
	extents []model.Extent
	counter int64
	msgID   int64
}

func newPackerStore() *packerStore {
	return &packerStore{
		nodes:  map[uuid.UUID]*model.Node{},
		wal:    map[uuid.UUID][]model.WALChunk{},
		leased: map[uuid.UUID]bool{},
		owner:  map[uuid.UUID]string{},
	}
}

func (s *packerStore) addBuffered(size int64, chunks ...[]byte) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	s.nodes[id] = &model.Node{ID: id, Size: size, State: model.NodeStateBuffered, IsDir: false}
	var wal []model.WALChunk
	for i, c := range chunks {
		wal = append(wal, model.WALChunk{ID: uuid.New(), NodeID: id, Seq: int64(i), Data: c})
	}
	s.wal[id] = wal
	return id
}

// --- packer fakes (only the tiny-interface methods) ------------------------

type pkNodes struct{ s *packerStore }

func (f *pkNodes) ClaimBufferedForPacking(_ context.Context, owner string, leaseFor time.Duration, limit int) ([]model.Node, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	var out []model.Node
	for id, n := range f.s.nodes {
		if n.State == model.NodeStateBuffered && !f.s.leased[id] {
			f.s.leased[id] = true
			f.s.owner[id] = owner
			out = append(out, *n)
			if len(out) >= limit {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}

func (f *pkNodes) MarkStoredIfOwner(_ context.Context, id uuid.UUID, owner string) (bool, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	n, ok := f.s.nodes[id]
	if !ok || n.State != model.NodeStateBuffered || f.s.owner[id] != owner {
		return false, nil
	}
	n.State = model.NodeStateStored
	delete(f.s.leased, id)
	delete(f.s.owner, id)
	return true, nil
}

func (f *pkNodes) ReleaseLease(_ context.Context, id uuid.UUID) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	delete(f.s.leased, id)
	return nil
}

type pkWAL struct{ s *packerStore }

func (f *pkWAL) DeleteByNode(_ context.Context, nodeID uuid.UUID) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	delete(f.s.wal, nodeID)
	return nil
}

func (f *pkWAL) ReadRange(_ context.Context, nodeID uuid.UUID, offset, length int64) ([]byte, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	var all []byte
	for _, c := range f.s.wal[nodeID] {
		all = append(all, c.Data...)
	}
	if offset >= int64(len(all)) {
		return nil, nil
	}
	end := offset + length
	if end > int64(len(all)) {
		end = int64(len(all))
	}
	out := make([]byte, end-offset)
	copy(out, all[offset:end])
	return out, nil
}

type pkBlobs struct{ s *packerStore }

func (f *pkBlobs) Create(_ context.Context, b *model.Blob) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	cp := *b
	f.s.blobs = append(f.s.blobs, &cp)
	return nil
}

func (f *pkBlobs) AddRefcount(_ context.Context, id uuid.UUID, delta int64) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	for _, b := range f.s.blobs {
		if b.ID == id {
			b.Refcount += delta
			return nil
		}
	}
	return model.ErrNotFound
}

type pkBlobFiles struct{}

func (pkBlobFiles) Upsert(_ context.Context, _ *model.BlobBotFile) error { return nil }

type pkExtents struct{ s *packerStore }

func (f *pkExtents) CreateBatch(_ context.Context, ex []model.Extent) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.extents = append(f.s.extents, ex...)
	return nil
}

type pkChannels struct{ s *packerStore }

func (f *pkChannels) IncrementCounter(_ context.Context, _ uuid.UUID, d int64) (int64, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.counter += d
	return f.s.counter, nil
}

// pkBotRepo / pkBotChans satisfy the packer's bot / bot-channel stores; the
// happy-path tests never hit rate-limit/forbidden so these are no-ops.
type pkBotRepo struct{}

func (pkBotRepo) SetUnavailableUntil(context.Context, uuid.UUID, *time.Time) error { return nil }

type pkBotChans struct{}

func (pkBotChans) Upsert(context.Context, *model.BotChannel) error { return nil }

// pkTG is a programmable packerTelegramClient.
type pkTG struct {
	s              *packerStore
	mu             sync.Mutex
	calls          int
	failLo, failHi int           // 1-based inclusive call range that returns an error
	delay          time.Duration // simulated upload duration (to expose concurrency)
	conc, maxConc  int           // current / observed-max concurrent SendDocument calls
}

func (f *pkTG) SendDocument(_ context.Context, _ *model.Bot, _ int64, _ string, data []byte) (model.TGSendResult, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.conc++
	if f.conc > f.maxConc {
		f.maxConc = f.conc
	}
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	f.conc--
	f.mu.Unlock()

	if f.failLo != 0 && n >= f.failLo && n <= f.failHi {
		return model.TGSendResult{}, fmt.Errorf("simulated upload failure on call %d", n)
	}
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.msgID++
	return model.TGSendResult{MessageID: f.s.msgID, FileID: "file", FileUniqueID: "uniq"}, nil
}

func (f *pkTG) maxConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxConc
}

// fakeChanSvc round-robins PickForUpload across chans, so a test can verify the
// packer pins one file to a single channel even when several are available.
type fakeChanSvc struct {
	chans []model.Channel
	n     atomic.Int64
}

func (f *fakeChanSvc) PickForUpload(context.Context) (*model.Channel, error) {
	if len(f.chans) == 0 {
		return nil, model.ErrNoBot
	}
	i := int(f.n.Add(1)-1) % len(f.chans)
	c := f.chans[i]
	return &c, nil
}

// fakeBotSvc satisfies botPicker: one bot, always picked.
type fakeBotSvc struct{ bot model.Bot }

func (f fakeBotSvc) PickForUpload(context.Context, uuid.UUID) (*model.Bot, error) {
	b := f.bot
	return &b, nil
}
func (f fakeBotSvc) List(context.Context) ([]model.Bot, error) {
	return []model.Bot{f.bot}, nil
}

type fakeSettings struct{ s model.Settings }

func (f fakeSettings) Get(context.Context) (model.Settings, error) { return f.s, nil }

func newPacker(s *packerStore, blobMax int64) (*Packer, *packerStore) {
	return newPackerTG(s, blobMax, &pkTG{s: s})
}

func newPackerTG(s *packerStore, blobMax int64, tg *pkTG) (*Packer, *packerStore) {
	chID := uuid.New()
	p := NewPacker(
		&pkNodes{s: s},
		&pkWAL{s: s},
		&pkBlobs{s: s},
		&pkExtents{s: s},
		&pkChannels{s: s},
		pkBotRepo{},
		pkBotChans{},
		pkBlobFiles{},
		fakeTx{},
		tg,
		&fakeChanSvc{chans: []model.Channel{{ID: chID, TGChatID: -100123}}},
		fakeBotSvc{bot: model.Bot{ID: uuid.New(), Enabled: true}},
		fakeSettings{s: model.Settings{BlobMaxSize: blobMax, WALIdleTimeout: time.Millisecond}},
		noopStats{},
		&fakeEvents{},
		nil,
	)
	p.pollInterval = 3 * time.Millisecond
	return p, s
}

func runUntilBlobs(t *testing.T, p *Packer, s *packerStore, want int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.blobs)
		buffered := 0
		for _, nd := range s.nodes {
			if nd.State == model.NodeStateBuffered {
				buffered++
			}
		}
		s.mu.Unlock()
		if n >= want && buffered == 0 {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timeout: got %d blobs, want %d", n, want)
		case <-time.After(3 * time.Millisecond):
		}
	}
}

func TestPackerPacksSmallFilesIntoOneBlob(t *testing.T) {
	s := newPackerStore()
	id1 := s.addBuffered(10, make([]byte, 10))
	id2 := s.addBuffered(20, make([]byte, 20))
	id3 := s.addBuffered(30, make([]byte, 30))
	p, _ := newPacker(s, 1000)
	runUntilBlobs(t, p, s, 1)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.blobs) != 1 {
		t.Fatalf("blobs = %d, want 1", len(s.blobs))
	}
	if s.blobs[0].Size != 60 {
		t.Errorf("blob size = %d, want 60", s.blobs[0].Size)
	}
	if s.blobs[0].Refcount != 3 {
		t.Errorf("refcount = %d, want 3", s.blobs[0].Refcount)
	}
	if len(s.extents) != 3 {
		t.Fatalf("extents = %d, want 3", len(s.extents))
	}
	for _, id := range []uuid.UUID{id1, id2, id3} {
		if s.nodes[id].State != model.NodeStateStored {
			t.Errorf("node %s state = %s, want stored", id, s.nodes[id].State)
		}
		if len(s.wal[id]) != 0 {
			t.Errorf("node %s WAL not deleted", id)
		}
	}
	// All extents reference the single blob.
	for _, e := range s.extents {
		if e.BlobID != s.blobs[0].ID {
			t.Errorf("extent blob mismatch")
		}
	}
}

func TestPackerSplitsLargeFileAcrossBlobs(t *testing.T) {
	s := newPackerStore()
	// 2500 bytes across chunks, blobMax 1000 → 3 blobs (1000,1000,500).
	id := s.addBuffered(2500, make([]byte, 1000), make([]byte, 1000), make([]byte, 500))
	p, _ := newPacker(s, 1000)
	runUntilBlobs(t, p, s, 3)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.blobs) != 3 {
		t.Fatalf("blobs = %d, want 3", len(s.blobs))
	}
	sort.Slice(s.extents, func(i, j int) bool { return s.extents[i].FileOffset < s.extents[j].FileOffset })
	if len(s.extents) != 3 {
		t.Fatalf("extents = %d, want 3", len(s.extents))
	}
	wantOff := []int64{0, 1000, 2000}
	wantLen := []int64{1000, 1000, 500}
	for i, e := range s.extents {
		if e.FileOffset != wantOff[i] || e.Length != wantLen[i] {
			t.Errorf("extent %d = off %d len %d, want off %d len %d", i, e.FileOffset, e.Length, wantOff[i], wantLen[i])
		}
		if e.NodeID != id {
			t.Errorf("extent %d node mismatch", i)
		}
		if e.Seq != int64(i) {
			t.Errorf("extent %d seq = %d, want %d", i, e.Seq, i)
		}
	}
	if s.nodes[id].State != model.NodeStateStored {
		t.Errorf("node not stored")
	}
	if len(s.wal[id]) != 0 {
		t.Errorf("WAL not deleted")
	}
}

// TestPackerRepackAfterPartialFailureNoDuplicateExtents reproduces the critical
// bug the review found: a file that spans several blobs whose later flush fails
// must NOT leave behind extents from the partially-packed run when it is
// re-packed. After the refactor, extents are only written at finalization, so a
// failed run leaves only an orphan blob (refcount 0) and the re-pack produces
// exactly one clean extent set.
func TestPackerRepackAfterPartialFailureNoDuplicateExtents(t *testing.T) {
	s := newPackerStore()
	id := s.addBuffered(2500, make([]byte, 1000), make([]byte, 1000), make([]byte, 500))
	// Calls: run1 blob1 = call 1 (ok); run1 blob2 attempts = calls 2..7 (fail, 6
	// upload retries) → run1 aborts after blob1. run2 starts fresh at call 8+.
	tg := &pkTG{s: s, failLo: 2, failHi: 7}
	p, _ := newPackerTG(s, 1000, tg)
	runUntilBlobs(t, p, s, 3) // wait until node stored with 3 good blobs

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodes[id].State != model.NodeStateStored {
		t.Fatalf("node not stored")
	}
	// Exactly 3 extents (no duplicates from the aborted run).
	var nodeExt []model.Extent
	for _, e := range s.extents {
		if e.NodeID == id {
			nodeExt = append(nodeExt, e)
		}
	}
	if len(nodeExt) != 3 {
		t.Fatalf("got %d extents, want 3 (duplicate extents from aborted run!)", len(nodeExt))
	}
	sort.Slice(nodeExt, func(i, j int) bool { return nodeExt[i].FileOffset < nodeExt[j].FileOffset })
	wantOff := []int64{0, 1000, 2000}
	for i, e := range nodeExt {
		if e.FileOffset != wantOff[i] {
			t.Errorf("extent %d offset %d, want %d", i, e.FileOffset, wantOff[i])
		}
	}
	// The 3 good blobs are referenced exactly once each; the pieces uploaded by
	// the aborted run before it failed are refcount-0 orphans (GC-collectable).
	orphans, good := 0, 0
	for _, b := range s.blobs {
		switch {
		case b.Refcount == 0:
			orphans++
		case b.Refcount == 1:
			good++
		default:
			t.Errorf("blob %s refcount %d, want 0 (orphan) or 1 (referenced)", b.ID, b.Refcount)
		}
	}
	if good != 3 {
		t.Errorf("got %d referenced blobs, want 3 (the re-packed file)", good)
	}
	if orphans < 1 {
		t.Errorf("got %d orphan blobs, want >=1 (from the aborted run)", orphans)
	}
}

// TestPackerUploadsBlobsInParallel verifies that a large file's blobs upload
// concurrently across the worker pool rather than one at a time.
func TestPackerUploadsBlobsInParallel(t *testing.T) {
	s := newPackerStore()
	// 10 blobs worth (blobMax 1000) in a single large file.
	chunks := make([][]byte, 10)
	for i := range chunks {
		chunks[i] = make([]byte, 1000)
	}
	id := s.addBuffered(10000, chunks...)

	tg := &pkTG{s: s, delay: 30 * time.Millisecond}
	p, _ := newPackerTG(s, 1000, tg)
	p.uploadConcurrency = 5 // 5 parallel upload workers
	runUntilBlobs(t, p, s, 10)

	if mc := tg.maxConcurrency(); mc < 2 {
		t.Errorf("max concurrent uploads = %d, want >= 2 (uploads ran sequentially)", mc)
	} else {
		t.Logf("observed max concurrent uploads = %d", mc)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodes[id].State != model.NodeStateStored {
		t.Fatalf("node not stored")
	}
	if len(s.blobs) != 10 {
		t.Errorf("blobs = %d, want 10", len(s.blobs))
	}
	var nodeExt []model.Extent
	for _, e := range s.extents {
		if e.NodeID == id {
			nodeExt = append(nodeExt, e)
		}
	}
	if len(nodeExt) != 10 {
		t.Fatalf("extents = %d, want 10", len(nodeExt))
	}
	sort.Slice(nodeExt, func(i, j int) bool { return nodeExt[i].FileOffset < nodeExt[j].FileOffset })
	for i, e := range nodeExt {
		if e.FileOffset != int64(i*1000) || e.Length != 1000 {
			t.Errorf("extent %d = off %d len %d, want off %d len 1000", i, e.FileOffset, e.Length, i*1000)
		}
	}
	for _, b := range s.blobs {
		if b.Refcount != 1 {
			t.Errorf("blob %s refcount %d, want 1", b.ID, b.Refcount)
		}
	}
}

// TestPackerKeepsFileBlobsInOneChannel verifies that all blobs of a single file
// land in the same channel even when several channels are available, so losing
// one channel never partially corrupts a file.
func TestPackerKeepsFileBlobsInOneChannel(t *testing.T) {
	s := newPackerStore()
	chunks := make([][]byte, 8)
	for i := range chunks {
		chunks[i] = make([]byte, 1000)
	}
	s.addBuffered(8000, chunks...) // 8 blobs
	p, _ := newPacker(s, 1000)
	// Three channels round-robin; the file must still go to exactly one.
	p.chanSvc = &fakeChanSvc{chans: []model.Channel{
		{ID: uuid.New(), TGChatID: -1001},
		{ID: uuid.New(), TGChatID: -1002},
		{ID: uuid.New(), TGChatID: -1003},
	}}
	runUntilBlobs(t, p, s, 8)

	s.mu.Lock()
	defer s.mu.Unlock()
	channels := map[uuid.UUID]bool{}
	for _, b := range s.blobs {
		channels[b.ChannelID] = true
	}
	if len(channels) != 1 {
		t.Errorf("file's blobs spread across %d channels, want 1", len(channels))
	}
}

func TestPackerFinalizesZeroByteFile(t *testing.T) {
	s := newPackerStore()
	id := s.addBuffered(0)
	p, _ := newPacker(s, 1000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.After(3 * time.Second)
	for {
		s.mu.Lock()
		stored := s.nodes[id].State == model.NodeStateStored
		s.mu.Unlock()
		if stored {
			cancel()
			<-done
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("zero-byte node never stored")
		case <-time.After(3 * time.Millisecond):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.blobs) != 0 {
		t.Errorf("zero-byte file created %d blobs, want 0", len(s.blobs))
	}
}

// TestPackerLostLeaseDoesNotDoubleWrite verifies the MarkStoredIfOwner guard:
// if a node's lease is stolen (a different owner) before finalize runs, the
// packer must NOT write extents or bump refcounts for it — a re-pack would then
// be the one to write them exactly once. Here the only worker loses the lease,
// so finalize is a no-op: no extents, the blob stays a refcount-0 orphan, and
// the WAL is preserved for the eventual re-pack.
func TestPackerLostLeaseDoesNotDoubleWrite(t *testing.T) {
	s := newPackerStore()
	id := s.addBuffered(500, make([]byte, 500))

	// Build a packer whose nodeStore steals the lease at claim time, so the
	// owner recorded never matches the packer's leaseOwner and MarkStoredIfOwner
	// returns (false, nil).
	chID := uuid.New()
	stealing := &leaseStealingNodes{pkNodes: pkNodes{s: s}}
	p := NewPacker(
		stealing,
		&pkWAL{s: s},
		&pkBlobs{s: s},
		&pkExtents{s: s},
		&pkChannels{s: s},
		pkBotRepo{},
		pkBotChans{},
		pkBlobFiles{},
		fakeTx{},
		&pkTG{s: s},
		&fakeChanSvc{chans: []model.Channel{{ID: chID, TGChatID: -100123}}},
		fakeBotSvc{bot: model.Bot{ID: uuid.New(), Enabled: true}},
		fakeSettings{s: model.Settings{BlobMaxSize: 1000, WALIdleTimeout: time.Millisecond}},
		noopStats{},
		&fakeEvents{},
		nil,
	)
	p.pollInterval = 3 * time.Millisecond

	// Run until a blob is uploaded, then stop. The blob is created (orphan), but
	// finalize cannot store the node because the lease is owned by "thief".
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.After(3 * time.Second)
	for {
		s.mu.Lock()
		uploaded := len(s.blobs) >= 1
		s.mu.Unlock()
		if uploaded {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("blob never uploaded")
		case <-time.After(3 * time.Millisecond):
		}
	}
	cancel()
	<-done

	s.mu.Lock()
	defer s.mu.Unlock()
	// Node must NOT be stored (lease lost).
	if s.nodes[id].State == model.NodeStateStored {
		t.Fatalf("node stored despite lost lease (double-write hazard)")
	}
	// No extents written.
	if len(s.extents) != 0 {
		t.Fatalf("extents written for lost-lease node = %d, want 0", len(s.extents))
	}
	// Uploaded blob is a refcount-0 orphan.
	for _, b := range s.blobs {
		if b.Refcount != 0 {
			t.Errorf("blob %s refcount %d, want 0 (orphan; lease was lost)", b.ID, b.Refcount)
		}
	}
	// WAL preserved for re-pack.
	if len(s.wal[id]) == 0 {
		t.Errorf("WAL deleted for lost-lease node; it would be unrecoverable")
	}
}

// leaseStealingNodes hands out claims under a foreign owner so the packer's
// MarkStoredIfOwner always returns (false, nil).
type leaseStealingNodes struct{ pkNodes }

func (f *leaseStealingNodes) ClaimBufferedForPacking(ctx context.Context, _ string, leaseFor time.Duration, limit int) ([]model.Node, error) {
	// Claim under a different owner so finalize's MarkStoredIfOwner never matches.
	return f.pkNodes.ClaimBufferedForPacking(ctx, "thief", leaseFor, limit)
}
