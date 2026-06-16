package blob

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// ---- fakes -----------------------------------------------------------------

// fakeBlobs is an in-memory BlobRepository (only methods the reader needs).
type fakeBlobs struct {
	mu    sync.Mutex
	items map[uuid.UUID]*domain.Blob
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{items: map[uuid.UUID]*domain.Blob{}} }

func (f *fakeBlobs) put(b *domain.Blob) { f.mu.Lock(); f.items[b.ID] = b; f.mu.Unlock() }

func (f *fakeBlobs) GetByID(_ context.Context, id uuid.UUID) (*domain.Blob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *b
	return &cp, nil
}

func (f *fakeBlobs) SetState(_ context.Context, id uuid.UUID, state domain.BlobState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return domain.ErrNotFound
	}
	b.State = state
	return nil
}

// Unused BlobRepository methods.
func (f *fakeBlobs) Create(context.Context, *domain.Blob) error          { return nil }
func (f *fakeBlobs) Update(context.Context, *domain.Blob) error          { return nil }
func (f *fakeBlobs) AddRefcount(context.Context, uuid.UUID, int64) error { return nil }
func (f *fakeBlobs) ListByChannel(context.Context, uuid.UUID) ([]domain.Blob, error) {
	return nil, nil
}
func (f *fakeBlobs) ListByState(context.Context, domain.BlobState) ([]domain.Blob, error) {
	return nil, nil
}
func (f *fakeBlobs) ListCollectable(context.Context, int) ([]domain.Blob, error) { return nil, nil }
func (f *fakeBlobs) MarkChannelUnavailable(context.Context, uuid.UUID) error     { return nil }
func (f *fakeBlobs) MarkChannelAvailable(context.Context, uuid.UUID) error       { return nil }
func (f *fakeBlobs) EvictOlderThan(context.Context, uuid.UUID, int64) (int64, error) {
	return 0, nil
}
func (f *fakeBlobs) Delete(context.Context, uuid.UUID) error { return nil }
func (f *fakeBlobs) Count(context.Context) (int64, error)    { return 0, nil }

// fakeChannels is an in-memory ChannelRepository.
type fakeChannels struct {
	items map[uuid.UUID]*domain.Channel
}

func newFakeChannels() *fakeChannels {
	return &fakeChannels{items: map[uuid.UUID]*domain.Channel{}}
}
func (f *fakeChannels) put(c *domain.Channel) { f.items[c.ID] = c }

func (f *fakeChannels) GetByID(_ context.Context, id uuid.UUID) (*domain.Channel, error) {
	c, ok := f.items[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (f *fakeChannels) Create(context.Context, *domain.Channel) error { return nil }
func (f *fakeChannels) Update(context.Context, *domain.Channel) error { return nil }
func (f *fakeChannels) Delete(context.Context, uuid.UUID) error       { return nil }
func (f *fakeChannels) GetByChatID(context.Context, int64) (*domain.Channel, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeChannels) List(context.Context) ([]domain.Channel, error) { return nil, nil }
func (f *fakeChannels) IncrementCounter(context.Context, uuid.UUID, int64) (int64, error) {
	return 0, nil
}
func (f *fakeChannels) SetAvailable(context.Context, uuid.UUID, bool) error { return nil }

// fakeBots is an in-memory BotRepository.
type fakeBots struct {
	mu    sync.Mutex
	items map[uuid.UUID]*domain.Bot
}

func newFakeBots() *fakeBots { return &fakeBots{items: map[uuid.UUID]*domain.Bot{}} }
func (f *fakeBots) put(b *domain.Bot) {
	f.mu.Lock()
	f.items[b.ID] = b
	f.mu.Unlock()
}

func (f *fakeBots) GetByID(_ context.Context, id uuid.UUID) (*domain.Bot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *b
	return &cp, nil
}

func (f *fakeBots) SetUnavailableUntil(_ context.Context, id uuid.UUID, until *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return domain.ErrNotFound
	}
	b.UnavailableUntil = until
	return nil
}

func (f *fakeBots) Create(context.Context, *domain.Bot) error { return nil }
func (f *fakeBots) Update(context.Context, *domain.Bot) error { return nil }
func (f *fakeBots) Delete(context.Context, uuid.UUID) error   { return nil }
func (f *fakeBots) GetByUsername(context.Context, string) (*domain.Bot, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeBots) List(context.Context) ([]domain.Bot, error) { return nil, nil }

// fakeBotChannels is an in-memory BotChannelRepository.
type fakeBotChannels struct {
	mu sync.Mutex
	// keyed by channelID -> ordered list of BotChannel
	byChannel map[uuid.UUID][]domain.BotChannel
}

func newFakeBotChannels() *fakeBotChannels {
	return &fakeBotChannels{byChannel: map[uuid.UUID][]domain.BotChannel{}}
}

func (f *fakeBotChannels) add(bc domain.BotChannel) {
	f.byChannel[bc.ChannelID] = append(f.byChannel[bc.ChannelID], bc)
}

func (f *fakeBotChannels) ListByChannel(_ context.Context, channelID uuid.UUID) ([]domain.BotChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.BotChannel, len(f.byChannel[channelID]))
	copy(out, f.byChannel[channelID])
	return out, nil
}

func (f *fakeBotChannels) Upsert(_ context.Context, bc *domain.BotChannel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.byChannel[bc.ChannelID]
	for i := range list {
		if list[i].BotID == bc.BotID {
			list[i] = *bc
			f.byChannel[bc.ChannelID] = list
			return nil
		}
	}
	f.byChannel[bc.ChannelID] = append(list, *bc)
	return nil
}

func (f *fakeBotChannels) Get(context.Context, uuid.UUID, uuid.UUID) (*domain.BotChannel, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeBotChannels) ListByBot(context.Context, uuid.UUID) ([]domain.BotChannel, error) {
	return nil, nil
}
func (f *fakeBotChannels) DeleteByBot(context.Context, uuid.UUID) error     { return nil }
func (f *fakeBotChannels) DeleteByChannel(context.Context, uuid.UUID) error { return nil }

// fakeBlobBotFiles is an in-memory BlobBotFileRepository.
type fakeBlobBotFiles struct {
	mu    sync.Mutex
	items map[[2]uuid.UUID]domain.BlobBotFile // key: {blobID, botID}
}

func newFakeBlobBotFiles() *fakeBlobBotFiles {
	return &fakeBlobBotFiles{items: map[[2]uuid.UUID]domain.BlobBotFile{}}
}

func (f *fakeBlobBotFiles) Upsert(_ context.Context, file *domain.BlobBotFile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[[2]uuid.UUID{file.BlobID, file.BotID}] = *file
	return nil
}

func (f *fakeBlobBotFiles) ListByBlob(_ context.Context, blobID uuid.UUID) ([]domain.BlobBotFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.BlobBotFile
	for k, v := range f.items {
		if k[0] == blobID {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeBlobBotFiles) DeleteByBlob(_ context.Context, blobID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.items {
		if k[0] == blobID {
			delete(f.items, k)
		}
	}
	return nil
}

func (f *fakeBlobBotFiles) Get(context.Context, uuid.UUID, uuid.UUID) (*domain.BlobBotFile, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeBlobBotFiles) DeleteByBot(context.Context, uuid.UUID) error { return nil }

// fakeExtents is an in-memory ExtentRepository (only ListNodesSolelyOnBlob used).
type fakeExtents struct {
	solelyOnBlob map[uuid.UUID][]uuid.UUID // blobID -> node ids living solely on it
}

func newFakeExtents() *fakeExtents {
	return &fakeExtents{solelyOnBlob: map[uuid.UUID][]uuid.UUID{}}
}

func (f *fakeExtents) ListNodesSolelyOnBlob(_ context.Context, blobID uuid.UUID) ([]uuid.UUID, error) {
	return f.solelyOnBlob[blobID], nil
}

func (f *fakeExtents) CreateBatch(context.Context, []domain.Extent) error { return nil }
func (f *fakeExtents) ListByNode(context.Context, uuid.UUID) ([]domain.Extent, error) {
	return nil, nil
}
func (f *fakeExtents) DeleteByNode(context.Context, uuid.UUID) error { return nil }
func (f *fakeExtents) ListBlobIDsByNode(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (f *fakeExtents) CopyForNode(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// fakeNodes records deletions.
type fakeNodes struct {
	mu      sync.Mutex
	deleted []uuid.UUID
}

func (f *fakeNodes) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeNodes) deletedIDs() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uuid.UUID, len(f.deleted))
	copy(out, f.deleted)
	return out
}

func (f *fakeNodes) Create(context.Context, *domain.Node) error { return nil }
func (f *fakeNodes) Update(context.Context, *domain.Node) error { return nil }
func (f *fakeNodes) GetByID(context.Context, uuid.UUID) (*domain.Node, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeNodes) GetByPath(context.Context, uuid.UUID, string) (*domain.Node, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeNodes) ListChildren(context.Context, uuid.UUID, uuid.UUID) ([]domain.Node, error) {
	return nil, nil
}
func (f *fakeNodes) ListSubtree(context.Context, uuid.UUID, string) ([]domain.Node, error) {
	return nil, nil
}
func (f *fakeNodes) CountChildren(context.Context, uuid.UUID) (int64, error) { return 0, nil }
func (f *fakeNodes) SumSizeByUser(context.Context, uuid.UUID) (int64, error) { return 0, nil }
func (f *fakeNodes) ClaimBufferedForPacking(context.Context, string, time.Duration, int) ([]domain.Node, error) {
	return nil, nil
}
func (f *fakeNodes) ReleaseLease(context.Context, uuid.UUID) error { return nil }
func (f *fakeNodes) MarkStoredIfOwner(context.Context, uuid.UUID, string) (bool, error) {
	return false, nil
}

// fakeEvents records logged events.
type fakeEvents struct {
	mu     sync.Mutex
	logged []domain.Event
}

func (f *fakeEvents) Log(_ context.Context, kind, message, ref string) error {
	f.mu.Lock()
	f.logged = append(f.logged, domain.Event{Kind: kind, Message: message, Ref: ref})
	f.mu.Unlock()
	return nil
}
func (f *fakeEvents) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.logged {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
func (f *fakeEvents) List(context.Context, string, int, int) ([]domain.Event, int64, error) {
	return nil, 0, nil
}

// fakeTx runs fn directly against the same repos (no real transaction).
type fakeTx struct {
	repos *domain.Repositories
}

func (f *fakeTx) WithTx(ctx context.Context, fn func(ctx context.Context, r *domain.Repositories) error) error {
	return fn(ctx, f.repos)
}

// fakeCache is an in-memory BlobCache.
type fakeCache struct {
	mu    sync.Mutex
	items map[uuid.UUID][]byte
}

func newFakeCache() *fakeCache { return &fakeCache{items: map[uuid.UUID][]byte{}} }

func (c *fakeCache) Get(id uuid.UUID) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.items[id]
	return d, ok
}
func (c *fakeCache) Put(id uuid.UUID, data []byte) error {
	c.mu.Lock()
	c.items[id] = data
	c.mu.Unlock()
	return nil
}
func (c *fakeCache) Remove(id uuid.UUID) {
	c.mu.Lock()
	delete(c.items, id)
	c.mu.Unlock()
}
func (c *fakeCache) Stats() (int64, int) { return 0, 0 }

// noopStats implements domain.StatRecorder doing nothing.
type noopStats struct{}

func (noopStats) AddReadBytes(int64)  {}
func (noopStats) AddWriteBytes(int64) {}
func (noopStats) IncReadOps()         {}
func (noopStats) IncWriteOps()        {}
func (noopStats) IncCacheHit()        {}
func (noopStats) IncCacheMiss()       {}
func (noopStats) IncTelegramReq()     {}

// fakeTG is a programmable TelegramAPI.
type fakeTG struct {
	mu sync.Mutex

	// downloadByFileID maps file_id -> (data, err). A nil entry means "not set".
	downloadByFileID map[string]tgResult
	// forwardByBot maps bot username -> result/err for ForwardMessage.
	forwardByBot map[string]tgForward
	// downloadCalls records the file_ids passed to DownloadFile in order.
	downloadCalls []string
	// forwardCalls records the bot usernames passed to ForwardMessage in order.
	forwardCalls []string
	// deleteCalls records message ids deleted.
	deleteCalls []int64
}

type tgResult struct {
	data []byte
	err  error
}
type tgForward struct {
	res domain.TGSendResult
	err error
}

func newFakeTG() *fakeTG {
	return &fakeTG{
		downloadByFileID: map[string]tgResult{},
		forwardByBot:     map[string]tgForward{},
	}
}

func (t *fakeTG) DownloadFile(_ context.Context, _ *domain.Bot, fileID string) ([]byte, error) {
	t.mu.Lock()
	t.downloadCalls = append(t.downloadCalls, fileID)
	r, ok := t.downloadByFileID[fileID]
	t.mu.Unlock()
	if !ok {
		return nil, domain.ErrTelegramNotFound
	}
	return r.data, r.err
}

func (t *fakeTG) ForwardMessage(_ context.Context, bot *domain.Bot, _, _, _ int64) (domain.TGSendResult, error) {
	t.mu.Lock()
	t.forwardCalls = append(t.forwardCalls, bot.Username)
	f, ok := t.forwardByBot[bot.Username]
	t.mu.Unlock()
	if !ok {
		return domain.TGSendResult{}, domain.ErrTelegramNotFound
	}
	return f.res, f.err
}

func (t *fakeTG) DeleteMessage(_ context.Context, _ *domain.Bot, _, messageID int64) error {
	t.mu.Lock()
	t.deleteCalls = append(t.deleteCalls, messageID)
	t.mu.Unlock()
	return nil
}

func (t *fakeTG) GetMe(context.Context, *domain.Bot) (string, error) { return "", nil }
func (t *fakeTG) GetChat(context.Context, *domain.Bot, int64) (string, bool, error) {
	return "", false, nil
}
func (t *fakeTG) SendDocument(context.Context, *domain.Bot, int64, string, []byte) (domain.TGSendResult, error) {
	return domain.TGSendResult{}, nil
}
func (t *fakeTG) SendByFileID(context.Context, *domain.Bot, int64, string) (domain.TGSendResult, error) {
	return domain.TGSendResult{}, nil
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	blobs    *fakeBlobs
	channels *fakeChannels
	bots     *fakeBots
	botChans *fakeBotChannels
	files    *fakeBlobBotFiles
	extents  *fakeExtents
	nodes    *fakeNodes
	events   *fakeEvents
	cache    *fakeCache
	tg       *fakeTG
	reader   *Reader

	channelID uuid.UUID
	chatID    int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		blobs:     newFakeBlobs(),
		channels:  newFakeChannels(),
		bots:      newFakeBots(),
		botChans:  newFakeBotChannels(),
		files:     newFakeBlobBotFiles(),
		extents:   newFakeExtents(),
		nodes:     &fakeNodes{},
		events:    &fakeEvents{},
		cache:     newFakeCache(),
		tg:        newFakeTG(),
		channelID: uuid.New(),
		chatID:    -1001234567890,
	}
	h.channels.put(&domain.Channel{ID: h.channelID, TGChatID: h.chatID, Available: true})

	repos := &domain.Repositories{
		Bots:         h.bots,
		Channels:     h.channels,
		BotChannels:  h.botChans,
		Blobs:        h.blobs,
		BlobBotFiles: h.files,
		Nodes:        h.nodes,
		Extents:      h.extents,
		Events:       h.events,
	}
	tx := &fakeTx{repos: repos}
	h.reader = NewReader(repos, tx, h.tg, h.cache, noopStats{}, slog.New(slog.DiscardHandler))
	return h
}

// addBot registers an enabled, available bot that is a member of the channel.
func (h *harness) addBot(username string) *domain.Bot {
	b := &domain.Bot{ID: uuid.New(), Username: username, Enabled: true}
	h.bots.put(b)
	h.botChans.add(domain.BotChannel{BotID: b.ID, ChannelID: h.channelID, Member: true})
	return b
}

// addStoredBlob creates a stored blob in the channel.
func (h *harness) addStoredBlob(messageID int64) *domain.Blob {
	b := &domain.Blob{
		ID:        uuid.New(),
		ChannelID: h.channelID,
		MessageID: messageID,
		State:     domain.BlobStored,
	}
	h.blobs.put(b)
	return b
}

// ---- tests -----------------------------------------------------------------

func TestReadBlob_NotReadable(t *testing.T) {
	h := newHarness(t)
	blob := h.addStoredBlob(10)
	blob.State = domain.BlobUnavailable
	h.blobs.put(blob)

	_, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if !errors.Is(err, domain.ErrBlobUnavailable) {
		t.Fatalf("expected ErrBlobUnavailable, got %v", err)
	}
}

func TestReadBlob_CacheHitShortCircuits(t *testing.T) {
	h := newHarness(t)
	blob := h.addStoredBlob(10)
	want := []byte("cached-bytes")
	if err := h.cache.Put(blob.ID, want); err != nil {
		t.Fatal(err)
	}

	got, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	// No Telegram traffic should have occurred.
	if len(h.tg.downloadCalls) != 0 || len(h.tg.forwardCalls) != 0 {
		t.Fatalf("expected no telegram calls, got download=%v forward=%v",
			h.tg.downloadCalls, h.tg.forwardCalls)
	}
}

func TestReadBlob_CachedFileIDFastPath(t *testing.T) {
	h := newHarness(t)
	bot := h.addBot("fastbot")
	blob := h.addStoredBlob(10)

	const fileID = "FILE_FAST"
	want := []byte("fast-path-bytes")
	if err := h.files.Upsert(context.Background(), &domain.BlobBotFile{
		BlobID: blob.ID, BotID: bot.ID, FileID: fileID,
	}); err != nil {
		t.Fatal(err)
	}
	h.tg.downloadByFileID[fileID] = tgResult{data: want}

	got, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Fast path: exactly one download of the cached file_id, no forward.
	if len(h.tg.downloadCalls) != 1 || h.tg.downloadCalls[0] != fileID {
		t.Fatalf("expected single download of %q, got %v", fileID, h.tg.downloadCalls)
	}
	if len(h.tg.forwardCalls) != 0 {
		t.Fatalf("expected no forward, got %v", h.tg.forwardCalls)
	}
	// Result was cached.
	if _, ok := h.cache.Get(blob.ID); !ok {
		t.Fatal("expected result to be cached")
	}
}

func TestReadBlob_StaleFileIDFallsBackToRecovery(t *testing.T) {
	h := newHarness(t)
	bot := h.addBot("recbot")
	blob := h.addStoredBlob(10)

	const staleID = "FILE_STALE"
	if err := h.files.Upsert(context.Background(), &domain.BlobBotFile{
		BlobID: blob.ID, BotID: bot.ID, FileID: staleID,
	}); err != nil {
		t.Fatal(err)
	}
	// Cached file_id download returns not-found -> stale.
	h.tg.downloadByFileID[staleID] = tgResult{err: domain.ErrTelegramNotFound}

	// Forward-recovery succeeds and yields a fresh file_id.
	const freshID = "FILE_FRESH"
	want := []byte("recovered-bytes")
	h.tg.forwardByBot[bot.Username] = tgForward{res: domain.TGSendResult{
		MessageID: 999, FileID: freshID, FileUniqueID: "u1",
	}}
	h.tg.downloadByFileID[freshID] = tgResult{data: want}

	got, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Stale row must have been deleted, fresh one cached.
	files, _ := h.files.ListByBlob(context.Background(), blob.ID)
	if len(files) != 1 || files[0].FileID != freshID {
		t.Fatalf("expected only fresh file_id cached, got %+v", files)
	}
	// Forward happened, forwarded copy was deleted.
	if len(h.tg.forwardCalls) != 1 {
		t.Fatalf("expected one forward, got %v", h.tg.forwardCalls)
	}
	if len(h.tg.deleteCalls) != 1 || h.tg.deleteCalls[0] != 999 {
		t.Fatalf("expected delete of forwarded copy 999, got %v", h.tg.deleteCalls)
	}
	// Blob must NOT have been perm-deleted on a stale-only signal.
	b, _ := h.blobs.GetByID(context.Background(), blob.ID)
	if b.State != domain.BlobStored {
		t.Fatalf("expected blob still stored, got %s", b.State)
	}
}

func TestReadBlob_PermNotFound_CachedThenRecovery_CascadeDeletes(t *testing.T) {
	h := newHarness(t)
	bot := h.addBot("gonebot")
	blob := h.addStoredBlob(10)

	const staleID = "FILE_STALE"
	if err := h.files.Upsert(context.Background(), &domain.BlobBotFile{
		BlobID: blob.ID, BotID: bot.ID, FileID: staleID,
	}); err != nil {
		t.Fatal(err)
	}
	// Cached download not found (stale) AND forward also not found (gone).
	h.tg.downloadByFileID[staleID] = tgResult{err: domain.ErrTelegramNotFound}
	h.tg.forwardByBot[bot.Username] = tgForward{err: domain.ErrTelegramNotFound}

	// A node lives solely on this blob.
	soleNode := uuid.New()
	h.extents.solelyOnBlob[blob.ID] = []uuid.UUID{soleNode}

	_, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if !errors.Is(err, domain.ErrBlobUnavailable) {
		t.Fatalf("expected ErrBlobUnavailable, got %v", err)
	}

	// Blob marked perm_unavailable.
	b, _ := h.blobs.GetByID(context.Background(), blob.ID)
	if b.State != domain.BlobPermUnavailable {
		t.Fatalf("expected perm_unavailable, got %s", b.State)
	}
	// Node solely on the blob was deleted.
	deleted := h.nodes.deletedIDs()
	if len(deleted) != 1 || deleted[0] != soleNode {
		t.Fatalf("expected node %s deleted, got %v", soleNode, deleted)
	}
	// Events logged.
	if h.events.count(domain.EventBlobPermDeleted) != 1 {
		t.Fatalf("expected one EventBlobPermDeleted, got %d", h.events.count(domain.EventBlobPermDeleted))
	}
	if h.events.count(domain.EventCascadeDelete) != 1 {
		t.Fatalf("expected one EventCascadeDelete, got %d", h.events.count(domain.EventCascadeDelete))
	}
}

func TestReadBlob_PermNotFound_RecoveryOnlyPath(t *testing.T) {
	h := newHarness(t)
	bot := h.addBot("gonebot2")
	blob := h.addStoredBlob(10)

	// No cached file_id; recovery path used directly. Forward not found -> gone.
	h.tg.forwardByBot[bot.Username] = tgForward{err: domain.ErrTelegramNotFound}

	soleNode := uuid.New()
	h.extents.solelyOnBlob[blob.ID] = []uuid.UUID{soleNode}

	_, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if !errors.Is(err, domain.ErrBlobUnavailable) {
		t.Fatalf("expected ErrBlobUnavailable, got %v", err)
	}
	b, _ := h.blobs.GetByID(context.Background(), blob.ID)
	if b.State != domain.BlobPermUnavailable {
		t.Fatalf("expected perm_unavailable, got %s", b.State)
	}
	if got := h.nodes.deletedIDs(); len(got) != 1 || got[0] != soleNode {
		t.Fatalf("expected node %s deleted, got %v", soleNode, got)
	}
}

func TestReadBlob_RateLimitMovesToNextBot(t *testing.T) {
	h := newHarness(t)
	limited := h.addBot("limited")
	good := h.addBot("good")
	blob := h.addStoredBlob(10)

	// First bot (by member order) is rate-limited on forward.
	h.tg.forwardByBot[limited.Username] = tgForward{err: &domain.RateLimitError{RetryAfter: 30 * time.Second}}
	// Second bot recovers successfully.
	const freshID = "FILE_OK"
	want := []byte("second-bot-bytes")
	h.tg.forwardByBot[good.Username] = tgForward{res: domain.TGSendResult{MessageID: 5, FileID: freshID}}
	h.tg.downloadByFileID[freshID] = tgResult{data: want}

	got, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Both bots attempted: limited first, then good.
	if len(h.tg.forwardCalls) != 2 ||
		h.tg.forwardCalls[0] != limited.Username ||
		h.tg.forwardCalls[1] != good.Username {
		t.Fatalf("expected forwards [%s %s], got %v", limited.Username, good.Username, h.tg.forwardCalls)
	}
	// Limited bot was parked (UnavailableUntil set).
	parked, _ := h.bots.GetByID(context.Background(), limited.ID)
	if parked.UnavailableUntil == nil {
		t.Fatal("expected limited bot to be parked with UnavailableUntil set")
	}
	// A bot-unavailable event was logged.
	if h.events.count(domain.EventBotUnavailable) != 1 {
		t.Fatalf("expected one EventBotUnavailable, got %d", h.events.count(domain.EventBotUnavailable))
	}
}

func TestReadBlob_ForbiddenMovesToNextBotAndRecordsNonMember(t *testing.T) {
	h := newHarness(t)
	forbidden := h.addBot("forbidden")
	good := h.addBot("good2")
	blob := h.addStoredBlob(10)

	h.tg.forwardByBot[forbidden.Username] = tgForward{err: domain.ErrTelegramForbidden}
	const freshID = "FILE_OK2"
	want := []byte("ok-bytes")
	h.tg.forwardByBot[good.Username] = tgForward{res: domain.TGSendResult{MessageID: 7, FileID: freshID}}
	h.tg.downloadByFileID[freshID] = tgResult{data: want}

	got, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Forbidden bot recorded as non-member of the channel.
	bcs, _ := h.botChans.ListByChannel(context.Background(), h.channelID)
	var foundNonMember bool
	for _, bc := range bcs {
		if bc.BotID == forbidden.ID && !bc.Member {
			foundNonMember = true
		}
	}
	if !foundNonMember {
		t.Fatalf("expected forbidden bot recorded as non-member, got %+v", bcs)
	}
}

func TestReadBlob_NoMemberBot(t *testing.T) {
	h := newHarness(t)
	blob := h.addStoredBlob(10) // no bots added

	_, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if !errors.Is(err, domain.ErrBlobUnavailable) {
		t.Fatalf("expected ErrBlobUnavailable, got %v", err)
	}
}

func TestReadBlob_FastPathPreferredOverNonCachedBot(t *testing.T) {
	h := newHarness(t)
	// plainBot is a member with no cached file_id; cachedBot has a cached one.
	plainBot := h.addBot("plain")
	cachedBot := h.addBot("cached")
	blob := h.addStoredBlob(10)

	const fileID = "FILE_PREFERRED"
	want := []byte("preferred-bytes")
	if err := h.files.Upsert(context.Background(), &domain.BlobBotFile{
		BlobID: blob.ID, BotID: cachedBot.ID, FileID: fileID,
	}); err != nil {
		t.Fatal(err)
	}
	h.tg.downloadByFileID[fileID] = tgResult{data: want}

	got, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	// The cached bot's fast path was used; plainBot never forwarded.
	if len(h.tg.forwardCalls) != 0 {
		t.Fatalf("expected no forwards (fast path), got %v", h.tg.forwardCalls)
	}
	_ = plainBot
}
