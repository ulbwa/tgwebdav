package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/client/telegram"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
)

// ---- maintenance fakes -----------------------------------------------------
//
// All fakes here are maint-prefixed to avoid colliding with the identically
// named fakes the other *_test.go files in this package already declare.

// maintFakeBlobs is an in-memory maintBlobStore that records reap/evict calls.
type maintFakeBlobs struct {
	collectable []model.Blob
	listErr     error

	deleted []uuid.UUID // ids passed to Delete, in order

	// evictReturns maps a channel id to the count EvictOlderThan returns.
	evictReturns map[uuid.UUID]int64
	evictCalls   []maintEvictCall
}

type maintEvictCall struct {
	channelID uuid.UUID
	minSeq    int64
}

func newMaintFakeBlobs() *maintFakeBlobs {
	return &maintFakeBlobs{evictReturns: map[uuid.UUID]int64{}}
}

func (f *maintFakeBlobs) ListCollectable(_ context.Context, _ int) ([]model.Blob, error) {
	return f.collectable, f.listErr
}

func (f *maintFakeBlobs) Delete(_ context.Context, id uuid.UUID) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *maintFakeBlobs) EvictOlderThan(_ context.Context, channelID uuid.UUID, minSeq int64) (int64, error) {
	f.evictCalls = append(f.evictCalls, maintEvictCall{channelID, minSeq})
	return f.evictReturns[channelID], nil
}

// maintFakeChannels is an in-memory maintChannelStore.
type maintFakeChannels struct {
	items map[uuid.UUID]*model.Channel
}

func newMaintFakeChannels() *maintFakeChannels {
	return &maintFakeChannels{items: map[uuid.UUID]*model.Channel{}}
}

func (f *maintFakeChannels) GetByID(_ context.Context, id uuid.UUID) (*model.Channel, error) {
	c, ok := f.items[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (f *maintFakeChannels) List(_ context.Context) ([]model.Channel, error) {
	out := make([]model.Channel, 0, len(f.items))
	for _, c := range f.items {
		out = append(out, *c)
	}
	return out, nil
}

// maintFakeBots is an in-memory maintBotStore recording parked bots.
type maintFakeBots struct {
	unavailable map[uuid.UUID]*time.Time
}

func newMaintFakeBots() *maintFakeBots {
	return &maintFakeBots{unavailable: map[uuid.UUID]*time.Time{}}
}

func (f *maintFakeBots) SetUnavailableUntil(_ context.Context, id uuid.UUID, until *time.Time) error {
	f.unavailable[id] = until
	return nil
}

// maintFakeTelegram is a maintTelegram returning a canned error per delete.
type maintFakeTelegram struct {
	// deleteErr, if set, is returned by every DeleteMessage call.
	deleteErr error
	calls     []maintDeleteCall
}

type maintDeleteCall struct {
	botID     uuid.UUID
	chatID    int64
	messageID int64
}

func (f *maintFakeTelegram) DeleteMessage(_ context.Context, bot *model.Bot, chatID, messageID int64) error {
	f.calls = append(f.calls, maintDeleteCall{bot.ID, chatID, messageID})
	return f.deleteErr
}

// maintFakePicker is a maintBotPicker returning a fixed bot (or error).
type maintFakePicker struct {
	bot *model.Bot
	err error
}

func (f *maintFakePicker) PickForUpload(_ context.Context, _ uuid.UUID) (*model.Bot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bot, nil
}

// maintFakeRefresher is a maintBotRefresher recording calls.
type maintFakeRefresher struct {
	calls int
	err   error
}

func (f *maintFakeRefresher) RefreshMembership(_ context.Context) error {
	f.calls++
	return f.err
}

// maintFakeEvents is a maintEventLogger recording logged events.
type maintFakeEvents struct {
	events []loggedEvent // reuses the loggedEvent type from service_test.go
}

func (f *maintFakeEvents) Log(_ context.Context, kind, message, ref string) error {
	f.events = append(f.events, loggedEvent{kind, message, ref})
	return nil
}

// ---- harness ---------------------------------------------------------------

type maintHarness struct {
	blobs     *maintFakeBlobs
	channels  *maintFakeChannels
	bots      *maintFakeBots
	picker    *maintFakePicker
	refresher *maintFakeRefresher
	tg        *maintFakeTelegram
	events    *maintFakeEvents
}

func newMaintHarness() *maintHarness {
	return &maintHarness{
		blobs:     newMaintFakeBlobs(),
		channels:  newMaintFakeChannels(),
		bots:      newMaintFakeBots(),
		picker:    &maintFakePicker{},
		refresher: &maintFakeRefresher{},
		tg:        &maintFakeTelegram{},
		events:    &maintFakeEvents{},
	}
}

func (h *maintHarness) svc() *MaintenanceService {
	return NewMaintenanceService(h.blobs, h.channels, h.bots, h.picker, h.refresher, h.tg, h.events, testLogger())
}

func (h *maintHarness) countEvents(kind string) int {
	n := 0
	for _, e := range h.events.events {
		if e.kind == kind {
			n++
		}
	}
	return n
}

// ---- reap tests ------------------------------------------------------------

// TestReapDeletesMessageRowAndLogsEvent verifies the happy path: a collectable
// blob with an available bot has its Telegram message deleted, its row removed
// and a reaped event logged.
func TestReapDeletesMessageRowAndLogsEvent(t *testing.T) {
	h := newMaintHarness()
	ctx := context.Background()

	channelID := uuid.New()
	const chatID = int64(-100123)
	h.channels.items[channelID] = &model.Channel{ID: channelID, TGChatID: chatID}

	botID := uuid.New()
	h.picker.bot = &model.Bot{ID: botID, Username: "a", Enabled: true}

	blobID := uuid.New()
	h.blobs.collectable = []model.Blob{
		{ID: blobID, ChannelID: channelID, MessageID: 4242},
	}

	h.svc().reap(ctx)

	if len(h.tg.calls) != 1 {
		t.Fatalf("DeleteMessage calls = %d, want 1", len(h.tg.calls))
	}
	if got := h.tg.calls[0]; got.chatID != chatID || got.messageID != 4242 || got.botID != botID {
		t.Fatalf("DeleteMessage called with %+v, want chat=%d msg=4242 bot=%s", got, chatID, botID)
	}
	if len(h.blobs.deleted) != 1 || h.blobs.deleted[0] != blobID {
		t.Fatalf("blob row deletions = %v, want [%s]", h.blobs.deleted, blobID)
	}
	if n := h.countEvents(model.EventBlobReaped); n != 1 {
		t.Fatalf("EventBlobReaped count = %d, want 1", n)
	}
}

// TestReapRateLimitParksBotAndStops verifies that a *telegram.RateLimitError on the
// first delete parks the bot and stops the cycle without touching the remaining
// blob (uploads keep priority).
func TestReapRateLimitParksBotAndStops(t *testing.T) {
	h := newMaintHarness()
	ctx := context.Background()

	channelID := uuid.New()
	h.channels.items[channelID] = &model.Channel{ID: channelID, TGChatID: -100}

	botID := uuid.New()
	h.picker.bot = &model.Bot{ID: botID, Username: "a", Enabled: true}

	// Two collectable blobs; the first delete rate-limits, the cycle must stop.
	h.blobs.collectable = []model.Blob{
		{ID: uuid.New(), ChannelID: channelID, MessageID: 1},
		{ID: uuid.New(), ChannelID: channelID, MessageID: 2},
	}
	h.tg.deleteErr = &telegram.RateLimitError{RetryAfter: 30 * time.Second}

	before := time.Now()
	h.svc().reap(ctx)

	// Exactly one delete attempt; the cycle stopped after the rate limit.
	if len(h.tg.calls) != 1 {
		t.Fatalf("DeleteMessage calls = %d, want 1 (cycle should stop)", len(h.tg.calls))
	}
	// The bot was parked until ~now+RetryAfter.
	until, ok := h.bots.unavailable[botID]
	if !ok || until == nil {
		t.Fatalf("bot was not parked")
	}
	if until.Before(before.Add(30 * time.Second)) {
		t.Fatalf("park until = %v, want >= %v", until, before.Add(30*time.Second))
	}
	// No rows deleted, no events logged.
	if len(h.blobs.deleted) != 0 {
		t.Fatalf("blob rows deleted = %v, want none after rate limit", h.blobs.deleted)
	}
	if n := h.countEvents(model.EventBlobReaped); n != 0 {
		t.Fatalf("EventBlobReaped count = %d, want 0 after rate limit", n)
	}
}

// TestReapNotFoundStillDeletesRow verifies that a Telegram "message gone"
// (ErrMessageNotFound) is treated as already-deleted: the row is still removed
// and the event logged.
func TestReapNotFoundStillDeletesRow(t *testing.T) {
	h := newMaintHarness()
	ctx := context.Background()

	channelID := uuid.New()
	h.channels.items[channelID] = &model.Channel{ID: channelID, TGChatID: -100}
	h.picker.bot = &model.Bot{ID: uuid.New(), Username: "a", Enabled: true}

	blobID := uuid.New()
	h.blobs.collectable = []model.Blob{{ID: blobID, ChannelID: channelID, MessageID: 7}}
	h.tg.deleteErr = telegram.ErrMessageNotFound

	h.svc().reap(ctx)

	if len(h.blobs.deleted) != 1 || h.blobs.deleted[0] != blobID {
		t.Fatalf("blob row deletions = %v, want [%s] even when message already gone", h.blobs.deleted, blobID)
	}
	if n := h.countEvents(model.EventBlobReaped); n != 1 {
		t.Fatalf("EventBlobReaped count = %d, want 1", n)
	}
}

// ---- evict tests -----------------------------------------------------------

// TestEvictPastRetentionCallsEvictAndLogs verifies a channel whose counter is
// past its retention window evicts with the correct minSeq and logs the event.
func TestEvictPastRetentionCallsEvictAndLogs(t *testing.T) {
	h := newMaintHarness()
	ctx := context.Background()

	channelID := uuid.New()
	// counter 100, threshold 30 → minSeq 70 (> 0).
	h.channels.items[channelID] = &model.Channel{
		ID: channelID, TGChatID: -100, MessageCounter: 100, EvictionThreshold: 30,
	}
	h.blobs.evictReturns[channelID] = 5 // 5 blobs evicted → event logged

	h.svc().evict(ctx)

	if len(h.blobs.evictCalls) != 1 {
		t.Fatalf("EvictOlderThan calls = %d, want 1", len(h.blobs.evictCalls))
	}
	if got := h.blobs.evictCalls[0]; got.channelID != channelID || got.minSeq != 70 {
		t.Fatalf("EvictOlderThan called with %+v, want channel=%s minSeq=70", got, channelID)
	}
	if n := h.countEvents(model.EventChannelEvicted); n != 1 {
		t.Fatalf("EventChannelEvicted count = %d, want 1", n)
	}
}

// TestEvictWithinWindowSkips verifies a channel still inside its retention
// window (minSeq <= 0) is skipped entirely.
func TestEvictWithinWindowSkips(t *testing.T) {
	h := newMaintHarness()
	ctx := context.Background()

	channelID := uuid.New()
	// counter 10, threshold 30 → minSeq -20 (<= 0): skip.
	h.channels.items[channelID] = &model.Channel{
		ID: channelID, TGChatID: -100, MessageCounter: 10, EvictionThreshold: 30,
	}

	h.svc().evict(ctx)

	if len(h.blobs.evictCalls) != 0 {
		t.Fatalf("EvictOlderThan calls = %d, want 0 within retention window", len(h.blobs.evictCalls))
	}
	if n := h.countEvents(model.EventChannelEvicted); n != 0 {
		t.Fatalf("EventChannelEvicted count = %d, want 0", n)
	}
}

// TestEvictNoBlobsEvictedSkipsEvent verifies that when EvictOlderThan reports 0
// rows changed, no event is logged even though the channel is past retention.
func TestEvictNoBlobsEvictedSkipsEvent(t *testing.T) {
	h := newMaintHarness()
	ctx := context.Background()

	channelID := uuid.New()
	h.channels.items[channelID] = &model.Channel{
		ID: channelID, TGChatID: -100, MessageCounter: 100, EvictionThreshold: 30,
	}
	// evictReturns defaults to 0 for this channel → no event.

	h.svc().evict(ctx)

	if len(h.blobs.evictCalls) != 1 {
		t.Fatalf("EvictOlderThan calls = %d, want 1", len(h.blobs.evictCalls))
	}
	if n := h.countEvents(model.EventChannelEvicted); n != 0 {
		t.Fatalf("EventChannelEvicted count = %d, want 0 when nothing evicted", n)
	}
}
