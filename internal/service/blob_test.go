package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/client/telegram"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
)

// ---- fakes -----------------------------------------------------------------

// fakeBlobs is an in-memory blobStore.
type fakeBlobs struct {
	mu    sync.Mutex
	items map[uuid.UUID]*model.Blob
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{items: map[uuid.UUID]*model.Blob{}} }

func (f *fakeBlobs) put(b *model.Blob) { f.mu.Lock(); f.items[b.ID] = b; f.mu.Unlock() }

func (f *fakeBlobs) GetByID(_ context.Context, id uuid.UUID) (*model.Blob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *b
	return &cp, nil
}

func (f *fakeBlobs) SetState(_ context.Context, id uuid.UUID, state model.BlobState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return repository.ErrNotFound
	}
	b.State = state
	return nil
}

// fakeChannels is an in-memory channelStore.
type fakeChannels struct {
	items map[uuid.UUID]*model.Channel
}

func newFakeChannels() *fakeChannels {
	return &fakeChannels{items: map[uuid.UUID]*model.Channel{}}
}
func (f *fakeChannels) put(c *model.Channel) { f.items[c.ID] = c }

func (f *fakeChannels) GetByID(_ context.Context, id uuid.UUID) (*model.Channel, error) {
	c, ok := f.items[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

// fakeBots is an in-memory botStore.
type fakeBots struct {
	mu    sync.Mutex
	items map[uuid.UUID]*model.Bot
}

func newFakeBots() *fakeBots { return &fakeBots{items: map[uuid.UUID]*model.Bot{}} }
func (f *fakeBots) put(b *model.Bot) {
	f.mu.Lock()
	f.items[b.ID] = b
	f.mu.Unlock()
}

func (f *fakeBots) GetByID(_ context.Context, id uuid.UUID) (*model.Bot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *b
	return &cp, nil
}

func (f *fakeBots) SetUnavailableUntil(_ context.Context, id uuid.UUID, until *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return repository.ErrNotFound
	}
	b.UnavailableUntil = until
	return nil
}

// fakeBotChannels is an in-memory botChannelStore.
type fakeBotChannels struct {
	mu sync.Mutex
	// keyed by channelID -> ordered list of BotChannel
	byChannel map[uuid.UUID][]model.BotChannel
}

func newFakeBotChannels() *fakeBotChannels {
	return &fakeBotChannels{byChannel: map[uuid.UUID][]model.BotChannel{}}
}

func (f *fakeBotChannels) add(bc model.BotChannel) {
	f.byChannel[bc.ChannelID] = append(f.byChannel[bc.ChannelID], bc)
}

func (f *fakeBotChannels) ListByChannel(_ context.Context, channelID uuid.UUID) ([]model.BotChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.BotChannel, len(f.byChannel[channelID]))
	copy(out, f.byChannel[channelID])
	return out, nil
}

func (f *fakeBotChannels) Upsert(_ context.Context, bc *model.BotChannel) error {
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

// fakeBlobBotFiles is an in-memory blobBotFiles.
type fakeBlobBotFiles struct {
	mu    sync.Mutex
	items map[[2]uuid.UUID]model.BlobBotFile // key: {blobID, botID}
}

func newFakeBlobBotFiles() *fakeBlobBotFiles {
	return &fakeBlobBotFiles{items: map[[2]uuid.UUID]model.BlobBotFile{}}
}

func (f *fakeBlobBotFiles) Upsert(_ context.Context, file *model.BlobBotFile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[[2]uuid.UUID{file.BlobID, file.BotID}] = *file
	return nil
}

func (f *fakeBlobBotFiles) ListByBlob(_ context.Context, blobID uuid.UUID) ([]model.BlobBotFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.BlobBotFile
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

// fakeExtents is an in-memory extentStore (only ListNodesSolelyOnBlob used).
type fakeExtents struct {
	solelyOnBlob map[uuid.UUID][]uuid.UUID // blobID -> node ids living solely on it
}

func newFakeExtents() *fakeExtents {
	return &fakeExtents{solelyOnBlob: map[uuid.UUID][]uuid.UUID{}}
}

func (f *fakeExtents) ListNodesSolelyOnBlob(_ context.Context, blobID uuid.UUID) ([]uuid.UUID, error) {
	return f.solelyOnBlob[blobID], nil
}

// fakeNodes records deletions (only Delete is used by the cascade).
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

// fakeEvents records logged events.
type fakeEvents struct {
	mu     sync.Mutex
	logged []model.Event
}

func (f *fakeEvents) Log(_ context.Context, kind, message, ref string) error {
	f.mu.Lock()
	f.logged = append(f.logged, model.Event{Kind: kind, Message: message, Ref: ref})
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

// fakeTx runs fn directly (no real transaction); the canon WithTx places the tx
// on the context, but the in-memory stores ignore it, so passing ctx through is
// sufficient.
type fakeTx struct{}

func (fakeTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// fakeCache is an in-memory blobCache.
type fakeCache struct {
	mu       sync.Mutex
	items    map[uuid.UUID][]byte
	capacity int64
}

func newFakeCache() *fakeCache {
	return &fakeCache{items: map[uuid.UUID][]byte{}, capacity: 1 << 30}
}

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
func (c *fakeCache) Has(id uuid.UUID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[id]
	return ok
}
func (c *fakeCache) Capacity() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity
}

// noopStats implements statRecorder doing nothing.
type noopStats struct{}

func (noopStats) AddReadBytes(int64)  {}
func (noopStats) AddWriteBytes(int64) {}
func (noopStats) IncCacheHit()        {}
func (noopStats) IncCacheMiss()       {}
func (noopStats) IncTelegramReq()     {}

// fakeTG is a programmable telegramClient.
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
	// downloadDelay/conc/maxConc expose DownloadFile concurrency to tests.
	downloadDelay   time.Duration
	downloadConc    int
	downloadMaxConc int
}

type tgResult struct {
	data []byte
	err  error
}
type tgForward struct {
	res model.TGSendResult
	err error
}

func newFakeTG() *fakeTG {
	return &fakeTG{
		downloadByFileID: map[string]tgResult{},
		forwardByBot:     map[string]tgForward{},
	}
}

func (t *fakeTG) DownloadFile(_ context.Context, _ *model.Bot, fileID string) ([]byte, error) {
	t.mu.Lock()
	t.downloadCalls = append(t.downloadCalls, fileID)
	t.downloadConc++
	if t.downloadConc > t.downloadMaxConc {
		t.downloadMaxConc = t.downloadConc
	}
	r, ok := t.downloadByFileID[fileID]
	delay := t.downloadDelay
	t.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	t.mu.Lock()
	t.downloadConc--
	t.mu.Unlock()
	if !ok {
		return nil, telegram.ErrMessageNotFound
	}
	return r.data, r.err
}

func (t *fakeTG) maxDownloadConcurrency() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.downloadMaxConc
}

func (t *fakeTG) ForwardMessage(_ context.Context, bot *model.Bot, _, _, _ int64) (model.TGSendResult, error) {
	t.mu.Lock()
	t.forwardCalls = append(t.forwardCalls, bot.Username)
	f, ok := t.forwardByBot[bot.Username]
	t.mu.Unlock()
	if !ok {
		return model.TGSendResult{}, telegram.ErrMessageNotFound
	}
	return f.res, f.err
}

func (t *fakeTG) DeleteMessage(_ context.Context, _ *model.Bot, _, messageID int64) error {
	t.mu.Lock()
	t.deleteCalls = append(t.deleteCalls, messageID)
	t.mu.Unlock()
	return nil
}

// ---- blobHarness ---------------------------------------------------------------

type blobHarness struct {
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
	reader   *BlobReader

	channelID uuid.UUID
	chatID    int64
}

func newBlobHarness(t *testing.T) *blobHarness {
	t.Helper()
	h := &blobHarness{
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
	h.channels.put(&model.Channel{ID: h.channelID, TGChatID: h.chatID, Available: true})

	h.reader = NewBlobReader(
		h.blobs, h.channels, h.bots, h.botChans, h.files, h.nodes, h.extents,
		fakeTx{}, h.tg, h.cache, noopStats{}, h.events, slog.New(slog.DiscardHandler),
	)
	return h
}

// addBot registers an enabled, available bot that is a member of the channel.
func (h *blobHarness) addBot(username string) *model.Bot {
	b := &model.Bot{ID: uuid.New(), Username: username, Enabled: true}
	h.bots.put(b)
	h.botChans.add(model.BotChannel{BotID: b.ID, ChannelID: h.channelID, Member: true})
	return b
}

// addStoredBlob creates a stored blob in the channel.
func (h *blobHarness) addStoredBlob(messageID int64) *model.Blob {
	b := &model.Blob{
		ID:        uuid.New(),
		ChannelID: h.channelID,
		MessageID: messageID,
		State:     model.BlobStateStored,
	}
	h.blobs.put(b)
	return b
}

// prefetchable creates n stored blobs, each with a cached file_id for bot that
// downloads to deterministic bytes, and returns their ids in order.
func (h *blobHarness) prefetchable(bot *model.Bot, n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		blob := h.addStoredBlob(int64(100 + i))
		fileID := blob.ID.String()
		_ = h.files.Upsert(context.Background(), &model.BlobBotFile{BlobID: blob.ID, BotID: bot.ID, FileID: fileID})
		h.tg.downloadByFileID[fileID] = tgResult{data: []byte("blob-" + fileID)}
		ids[i] = blob.ID
	}
	return ids
}

// ---- tests -----------------------------------------------------------------

func TestPrefetchDownloadsInParallel(t *testing.T) {
	h := newBlobHarness(t)
	bot := h.addBot("b1")
	h.tg.downloadDelay = 30 * time.Millisecond
	ids := h.prefetchable(bot, 6)

	h.reader.Prefetch(context.Background(), ids)

	if mc := h.tg.maxDownloadConcurrency(); mc < 2 {
		t.Errorf("max concurrent downloads = %d, want >= 2 (prefetch ran sequentially)", mc)
	} else {
		t.Logf("observed max concurrent downloads = %d", mc)
	}
	for _, id := range ids {
		if !h.cache.Has(id) {
			t.Errorf("blob %s not warmed into cache", id)
		}
	}
}

func TestPrefetchBoundedByCacheCapacity(t *testing.T) {
	h := newBlobHarness(t)
	bot := h.addBot("b1")
	// Capacity for ~2 blobs (limit = capacity / 20MiB).
	h.cache.capacity = 2 * (20 << 20)
	ids := h.prefetchable(bot, 6)

	h.reader.Prefetch(context.Background(), ids)

	cached := 0
	for _, id := range ids {
		if h.cache.Has(id) {
			cached++
		}
	}
	if cached != 2 {
		t.Errorf("prefetched %d blobs, want 2 (bounded by cache capacity)", cached)
	}
}

func TestReadBlob_NotReadable(t *testing.T) {
	h := newBlobHarness(t)
	blob := h.addStoredBlob(10)
	blob.State = model.BlobStateUnavailable
	h.blobs.put(blob)

	_, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if !errors.Is(err, ErrBlobUnavailable) {
		t.Fatalf("expected ErrBlobUnavailable, got %v", err)
	}
}

func TestReadBlob_CacheHitShortCircuits(t *testing.T) {
	h := newBlobHarness(t)
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
	h := newBlobHarness(t)
	bot := h.addBot("fastbot")
	blob := h.addStoredBlob(10)

	const fileID = "FILE_FAST"
	want := []byte("fast-path-bytes")
	if err := h.files.Upsert(context.Background(), &model.BlobBotFile{
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
	h := newBlobHarness(t)
	bot := h.addBot("recbot")
	blob := h.addStoredBlob(10)

	const staleID = "FILE_STALE"
	if err := h.files.Upsert(context.Background(), &model.BlobBotFile{
		BlobID: blob.ID, BotID: bot.ID, FileID: staleID,
	}); err != nil {
		t.Fatal(err)
	}
	// Cached file_id download returns not-found -> stale.
	h.tg.downloadByFileID[staleID] = tgResult{err: telegram.ErrMessageNotFound}

	// Forward-recovery succeeds and yields a fresh file_id.
	const freshID = "FILE_FRESH"
	want := []byte("recovered-bytes")
	h.tg.forwardByBot[bot.Username] = tgForward{res: model.TGSendResult{
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
	if b.State != model.BlobStateStored {
		t.Fatalf("expected blob still stored, got %s", b.State)
	}
}

func TestReadBlob_PermNotFound_CachedThenRecovery_CascadeDeletes(t *testing.T) {
	h := newBlobHarness(t)
	bot := h.addBot("gonebot")
	blob := h.addStoredBlob(10)

	const staleID = "FILE_STALE"
	if err := h.files.Upsert(context.Background(), &model.BlobBotFile{
		BlobID: blob.ID, BotID: bot.ID, FileID: staleID,
	}); err != nil {
		t.Fatal(err)
	}
	// Cached download not found (stale) AND forward also not found (gone).
	h.tg.downloadByFileID[staleID] = tgResult{err: telegram.ErrMessageNotFound}
	h.tg.forwardByBot[bot.Username] = tgForward{err: telegram.ErrMessageNotFound}

	// A node lives solely on this blob.
	soleNode := uuid.New()
	h.extents.solelyOnBlob[blob.ID] = []uuid.UUID{soleNode}

	_, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if !errors.Is(err, ErrBlobUnavailable) {
		t.Fatalf("expected ErrBlobUnavailable, got %v", err)
	}

	// Blob marked perm_unavailable.
	b, _ := h.blobs.GetByID(context.Background(), blob.ID)
	if b.State != model.BlobStatePermUnavailable {
		t.Fatalf("expected perm_unavailable, got %s", b.State)
	}
	// Node solely on the blob was deleted.
	deleted := h.nodes.deletedIDs()
	if len(deleted) != 1 || deleted[0] != soleNode {
		t.Fatalf("expected node %s deleted, got %v", soleNode, deleted)
	}
	// Events logged.
	if h.events.count(model.EventBlobPermDeleted) != 1 {
		t.Fatalf("expected one EventBlobPermDeleted, got %d", h.events.count(model.EventBlobPermDeleted))
	}
	if h.events.count(model.EventCascadeDelete) != 1 {
		t.Fatalf("expected one EventCascadeDelete, got %d", h.events.count(model.EventCascadeDelete))
	}
}

func TestReadBlob_PermNotFound_RecoveryOnlyPath(t *testing.T) {
	h := newBlobHarness(t)
	bot := h.addBot("gonebot2")
	blob := h.addStoredBlob(10)

	// No cached file_id; recovery path used directly. Forward not found -> gone.
	h.tg.forwardByBot[bot.Username] = tgForward{err: telegram.ErrMessageNotFound}

	soleNode := uuid.New()
	h.extents.solelyOnBlob[blob.ID] = []uuid.UUID{soleNode}

	_, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if !errors.Is(err, ErrBlobUnavailable) {
		t.Fatalf("expected ErrBlobUnavailable, got %v", err)
	}
	b, _ := h.blobs.GetByID(context.Background(), blob.ID)
	if b.State != model.BlobStatePermUnavailable {
		t.Fatalf("expected perm_unavailable, got %s", b.State)
	}
	if got := h.nodes.deletedIDs(); len(got) != 1 || got[0] != soleNode {
		t.Fatalf("expected node %s deleted, got %v", soleNode, got)
	}
}

func TestReadBlob_RateLimitMovesToNextBot(t *testing.T) {
	h := newBlobHarness(t)
	limited := h.addBot("limited")
	good := h.addBot("good")
	blob := h.addStoredBlob(10)

	// First bot (by member order) is rate-limited on forward.
	h.tg.forwardByBot[limited.Username] = tgForward{err: &telegram.RateLimitError{RetryAfter: 30 * time.Second}}
	// Second bot recovers successfully.
	const freshID = "FILE_OK"
	want := []byte("second-bot-bytes")
	h.tg.forwardByBot[good.Username] = tgForward{res: model.TGSendResult{MessageID: 5, FileID: freshID}}
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
	if h.events.count(model.EventBotUnavailable) != 1 {
		t.Fatalf("expected one EventBotUnavailable, got %d", h.events.count(model.EventBotUnavailable))
	}
}

func TestReadBlob_ForbiddenMovesToNextBotAndRecordsNonMember(t *testing.T) {
	h := newBlobHarness(t)
	forbidden := h.addBot("forbidden")
	good := h.addBot("good2")
	blob := h.addStoredBlob(10)

	h.tg.forwardByBot[forbidden.Username] = tgForward{err: telegram.ErrForbidden}
	const freshID = "FILE_OK2"
	want := []byte("ok-bytes")
	h.tg.forwardByBot[good.Username] = tgForward{res: model.TGSendResult{MessageID: 7, FileID: freshID}}
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
	h := newBlobHarness(t)
	blob := h.addStoredBlob(10) // no bots added

	_, err := h.reader.ReadBlob(context.Background(), blob.ID)
	if !errors.Is(err, ErrBlobUnavailable) {
		t.Fatalf("expected ErrBlobUnavailable, got %v", err)
	}
}

func TestReadBlob_FastPathPreferredOverNonCachedBot(t *testing.T) {
	h := newBlobHarness(t)
	// plainBot is a member with no cached file_id; cachedBot has a cached one.
	plainBot := h.addBot("plain")
	cachedBot := h.addBot("cached")
	blob := h.addStoredBlob(10)

	const fileID = "FILE_PREFERRED"
	want := []byte("preferred-bytes")
	if err := h.files.Upsert(context.Background(), &model.BlobBotFile{
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
