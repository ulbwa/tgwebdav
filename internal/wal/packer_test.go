package wal

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// --- shared in-memory store backing the fakes ------------------------------

type store struct {
	mu      sync.Mutex
	nodes   map[uuid.UUID]*domain.Node
	wal     map[uuid.UUID][]domain.WALChunk
	leased  map[uuid.UUID]bool
	blobs   []*domain.Blob
	extents []domain.Extent
	counter int64
	msgID   int64
}

func newStore() *store {
	return &store{nodes: map[uuid.UUID]*domain.Node{}, wal: map[uuid.UUID][]domain.WALChunk{}, leased: map[uuid.UUID]bool{}}
}

func (s *store) addBuffered(size int64, chunks ...[]byte) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	s.nodes[id] = &domain.Node{ID: id, Size: size, State: domain.NodeBuffered, IsDir: false}
	var wal []domain.WALChunk
	for i, c := range chunks {
		wal = append(wal, domain.WALChunk{ID: uuid.New(), NodeID: id, Seq: int64(i), Data: c})
	}
	s.wal[id] = wal
	return id
}

// --- fakes (embed the interface so unused methods compile but panic) -------

type fakeNodes struct {
	domain.NodeRepository
	s *store
}

func (f *fakeNodes) ClaimBufferedForPacking(_ context.Context, _ string, leaseFor time.Duration, limit int) ([]domain.Node, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	var out []domain.Node
	for id, n := range f.s.nodes {
		if n.State == domain.NodeBuffered && !f.s.leased[id] {
			f.s.leased[id] = true
			out = append(out, *n)
			if len(out) >= limit {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}

func (f *fakeNodes) GetByID(_ context.Context, id uuid.UUID) (*domain.Node, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	n, ok := f.s.nodes[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *n
	return &cp, nil
}

func (f *fakeNodes) Update(_ context.Context, n *domain.Node) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.nodes[n.ID] = n
	return nil
}

func (f *fakeNodes) ReleaseLease(_ context.Context, id uuid.UUID) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	delete(f.s.leased, id)
	return nil
}

type fakeWAL struct {
	domain.WALRepository
	s *store
}

func (f *fakeWAL) EachChunk(_ context.Context, nodeID uuid.UUID, fn func(domain.WALChunk) error) error {
	f.s.mu.Lock()
	chunks := append([]domain.WALChunk(nil), f.s.wal[nodeID]...)
	f.s.mu.Unlock()
	for _, c := range chunks {
		if err := fn(c); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeWAL) DeleteByNode(_ context.Context, nodeID uuid.UUID) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	delete(f.s.wal, nodeID)
	return nil
}

type fakeBlobs struct {
	domain.BlobRepository
	s *store
}

func (f *fakeBlobs) Create(_ context.Context, b *domain.Blob) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	cp := *b
	f.s.blobs = append(f.s.blobs, &cp)
	return nil
}

type fakeBlobFiles struct {
	domain.BlobBotFileRepository
}

func (fakeBlobFiles) Upsert(_ context.Context, _ *domain.BlobBotFile) error { return nil }

type fakeExtents struct {
	domain.ExtentRepository
	s *store
}

func (f *fakeExtents) CreateBatch(_ context.Context, ex []domain.Extent) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.extents = append(f.s.extents, ex...)
	return nil
}

type fakeChannels struct {
	domain.ChannelRepository
	s *store
}

func (f *fakeChannels) IncrementCounter(_ context.Context, _ uuid.UUID, d int64) (int64, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.counter += d
	return f.s.counter, nil
}

type fakeEvents struct{ domain.EventRepository }

func (fakeEvents) Log(_ context.Context, _, _, _ string) error { return nil }

// services / tx / tg / stats fakes

type fakeTx struct{ repos *domain.Repositories }

func (f fakeTx) WithTx(ctx context.Context, fn func(context.Context, *domain.Repositories) error) error {
	return fn(ctx, f.repos)
}

type fakeTG struct{ s *store }

func (f *fakeTG) SendDocument(_ context.Context, _ *domain.Bot, _ int64, _ string, data []byte) (domain.TGSendResult, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.msgID++
	return domain.TGSendResult{MessageID: f.s.msgID, FileID: "file", FileUniqueID: "uniq"}, nil
}
func (f *fakeTG) GetMe(context.Context, *domain.Bot) (string, error) { return "", nil }
func (f *fakeTG) GetChat(context.Context, *domain.Bot, int64) (string, bool, error) {
	return "", false, nil
}
func (f *fakeTG) SendByFileID(context.Context, *domain.Bot, int64, string) (domain.TGSendResult, error) {
	return domain.TGSendResult{}, nil
}
func (f *fakeTG) ForwardMessage(context.Context, *domain.Bot, int64, int64, int64) (domain.TGSendResult, error) {
	return domain.TGSendResult{}, nil
}
func (f *fakeTG) DeleteMessage(context.Context, *domain.Bot, int64, int64) error { return nil }
func (f *fakeTG) DownloadFile(context.Context, *domain.Bot, string) ([]byte, error) {
	return nil, nil
}

type fakeChanSvc struct{ ch domain.Channel }

func (f fakeChanSvc) PickForUpload(context.Context) (*domain.Channel, error) { c := f.ch; return &c, nil }
func (f fakeChanSvc) Add(context.Context, int64) (*domain.Channel, error)    { return nil, nil }
func (f fakeChanSvc) Remove(context.Context, uuid.UUID) error                { return nil }
func (f fakeChanSvc) SetEvictionThreshold(context.Context, uuid.UUID, int64) error {
	return nil
}
func (f fakeChanSvc) List(context.Context) ([]domain.Channel, error)        { return nil, nil }
func (f fakeChanSvc) Get(context.Context, uuid.UUID) (*domain.Channel, error) { return nil, nil }
func (f fakeChanSvc) ReevaluateAvailability(context.Context) error          { return nil }

type fakeBotSvc struct{ bot domain.Bot }

func (f fakeBotSvc) PickForUpload(context.Context, uuid.UUID) (*domain.Bot, error) {
	b := f.bot
	return &b, nil
}
func (f fakeBotSvc) Add(context.Context, string) (*domain.Bot, error)     { return nil, nil }
func (f fakeBotSvc) Remove(context.Context, uuid.UUID) error              { return nil }
func (f fakeBotSvc) SetEnabled(context.Context, uuid.UUID, bool) error    { return nil }
func (f fakeBotSvc) List(context.Context) ([]domain.Bot, error)           { return nil, nil }
func (f fakeBotSvc) Get(context.Context, uuid.UUID) (*domain.Bot, error)  { return nil, nil }
func (f fakeBotSvc) RefreshMembership(context.Context) error              { return nil }

type fakeSettings struct{ s domain.Settings }

func (f fakeSettings) Get(context.Context) (domain.Settings, error)     { return f.s, nil }
func (f fakeSettings) Update(context.Context, domain.Settings) error    { return nil }

type noopStats struct{}

func (noopStats) AddReadBytes(int64)  {}
func (noopStats) AddWriteBytes(int64) {}
func (noopStats) IncReadOps()         {}
func (noopStats) IncWriteOps()        {}
func (noopStats) IncCacheHit()        {}
func (noopStats) IncCacheMiss()       {}
func (noopStats) IncTelegramReq()     {}

func newPacker(s *store, blobMax int64) (*Packer, *store) {
	repos := &domain.Repositories{
		Nodes:        &fakeNodes{s: s},
		WAL:          &fakeWAL{s: s},
		Blobs:        &fakeBlobs{s: s},
		BlobBotFiles: fakeBlobFiles{},
		Extents:      &fakeExtents{s: s},
		Channels:     &fakeChannels{s: s},
		Events:       fakeEvents{},
	}
	chID := uuid.New()
	p := NewPacker(
		repos,
		fakeTx{repos: repos},
		&fakeTG{s: s},
		fakeChanSvc{ch: domain.Channel{ID: chID, TGChatID: -100123}},
		fakeBotSvc{bot: domain.Bot{ID: uuid.New(), Enabled: true}},
		fakeSettings{s: domain.Settings{BlobMaxSize: blobMax, WALIdleTimeout: time.Millisecond}},
		noopStats{},
		nil,
	)
	p.pollInterval = 3 * time.Millisecond
	return p, s
}

func runUntilBlobs(t *testing.T, p *Packer, s *store, want int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.blobs)
		s.mu.Unlock()
		if n >= want {
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
	s := newStore()
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
		if s.nodes[id].State != domain.NodeStored {
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
	s := newStore()
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
	if s.nodes[id].State != domain.NodeStored {
		t.Errorf("node not stored")
	}
	if len(s.wal[id]) != 0 {
		t.Errorf("WAL not deleted")
	}
}

func TestPackerFinalizesZeroByteFile(t *testing.T) {
	s := newStore()
	id := s.addBuffered(0)
	p, _ := newPacker(s, 1000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.After(3 * time.Second)
	for {
		s.mu.Lock()
		stored := s.nodes[id].State == domain.NodeStored
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
