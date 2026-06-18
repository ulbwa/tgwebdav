package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
)

// testLogger returns a logger that discards output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- fake TxManager --------------------------------------------------------

// recordingTx runs fn against the same context with no real transaction; the fake
// repositories ignore the (absent) transaction in ctx and mutate their maps
// directly, which mirrors the canon repos resolving the active tx from ctx.
type recordingTx struct {
	// withTxErr, if set, is returned without running fn (to exercise rollback
	// error paths).
	withTxErr error
	calls     int
}

func (t *recordingTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	t.calls++
	if t.withTxErr != nil {
		return t.withTxErr
	}
	return fn(ctx)
}

// ---- fake bot store --------------------------------------------------------

type fakeBotRepo struct {
	bots map[uuid.UUID]*model.Bot
}

func newFakeBotRepo() *fakeBotRepo { return &fakeBotRepo{bots: map[uuid.UUID]*model.Bot{}} }

func (r *fakeBotRepo) Create(_ context.Context, b *model.Bot) error {
	cp := *b
	r.bots[b.ID] = &cp
	return nil
}
func (r *fakeBotRepo) Update(_ context.Context, b *model.Bot) error {
	if _, ok := r.bots[b.ID]; !ok {
		return repository.ErrNotFound
	}
	cp := *b
	r.bots[b.ID] = &cp
	return nil
}
func (r *fakeBotRepo) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := r.bots[id]; !ok {
		return repository.ErrNotFound
	}
	delete(r.bots, id)
	return nil
}
func (r *fakeBotRepo) GetByID(_ context.Context, id uuid.UUID) (*model.Bot, error) {
	b, ok := r.bots[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *b
	return &cp, nil
}
func (r *fakeBotRepo) GetByUsername(_ context.Context, username string) (*model.Bot, error) {
	for _, b := range r.bots {
		if b.Username == username {
			cp := *b
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (r *fakeBotRepo) List(_ context.Context) ([]model.Bot, error) {
	out := make([]model.Bot, 0, len(r.bots))
	for _, b := range r.bots {
		out = append(out, *b)
	}
	sortBots(out)
	return out, nil
}

// sortBots orders bots by id string so List is deterministic.
func sortBots(bots []model.Bot) {
	for i := 1; i < len(bots); i++ {
		for j := i; j > 0 && bots[j-1].ID.String() > bots[j].ID.String(); j-- {
			bots[j-1], bots[j] = bots[j], bots[j-1]
		}
	}
}

// ---- fake channel store ----------------------------------------------------

type fakeChannelRepo struct {
	channels map[uuid.UUID]*model.Channel
}

func newFakeChannelRepo() *fakeChannelRepo {
	return &fakeChannelRepo{channels: map[uuid.UUID]*model.Channel{}}
}

func (r *fakeChannelRepo) Create(_ context.Context, c *model.Channel) error {
	cp := *c
	r.channels[c.ID] = &cp
	return nil
}
func (r *fakeChannelRepo) Update(_ context.Context, c *model.Channel) error {
	if _, ok := r.channels[c.ID]; !ok {
		return repository.ErrNotFound
	}
	cp := *c
	r.channels[c.ID] = &cp
	return nil
}
func (r *fakeChannelRepo) GetByID(_ context.Context, id uuid.UUID) (*model.Channel, error) {
	c, ok := r.channels[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *c
	return &cp, nil
}
func (r *fakeChannelRepo) GetByChatID(_ context.Context, chatID int64) (*model.Channel, error) {
	for _, c := range r.channels {
		if c.TGChatID == chatID {
			cp := *c
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (r *fakeChannelRepo) List(_ context.Context) ([]model.Channel, error) {
	out := make([]model.Channel, 0, len(r.channels))
	for _, c := range r.channels {
		out = append(out, *c)
	}
	sortChannels(out)
	return out, nil
}
func (r *fakeChannelRepo) SetAvailable(_ context.Context, id uuid.UUID, available bool) error {
	c, ok := r.channels[id]
	if !ok {
		return repository.ErrNotFound
	}
	c.Available = available
	return nil
}

func sortChannels(channels []model.Channel) {
	for i := 1; i < len(channels); i++ {
		for j := i; j > 0 && channels[j-1].ID.String() > channels[j].ID.String(); j-- {
			channels[j-1], channels[j] = channels[j], channels[j-1]
		}
	}
}

// ---- fake bot-channel store ------------------------------------------------

type botChannelKey struct {
	bot, channel uuid.UUID
}

type fakeBotChannelRepo struct {
	m map[botChannelKey]*model.BotChannel
}

func newFakeBotChannelRepo() *fakeBotChannelRepo {
	return &fakeBotChannelRepo{m: map[botChannelKey]*model.BotChannel{}}
}

func (r *fakeBotChannelRepo) Upsert(_ context.Context, bc *model.BotChannel) error {
	cp := *bc
	r.m[botChannelKey{bc.BotID, bc.ChannelID}] = &cp
	return nil
}
func (r *fakeBotChannelRepo) Get(_ context.Context, botID, channelID uuid.UUID) (*model.BotChannel, error) {
	bc, ok := r.m[botChannelKey{botID, channelID}]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *bc
	return &cp, nil
}
func (r *fakeBotChannelRepo) ListByChannel(_ context.Context, channelID uuid.UUID) ([]model.BotChannel, error) {
	var out []model.BotChannel
	for _, bc := range r.m {
		if bc.ChannelID == channelID {
			out = append(out, *bc)
		}
	}
	return out, nil
}

// ---- fake blob store -------------------------------------------------------

type fakeBlobRepo struct {
	availableCalls   map[uuid.UUID]int
	unavailableCalls map[uuid.UUID]int
}

func newFakeBlobRepo() *fakeBlobRepo {
	return &fakeBlobRepo{
		availableCalls:   map[uuid.UUID]int{},
		unavailableCalls: map[uuid.UUID]int{},
	}
}

func (r *fakeBlobRepo) MarkChannelUnavailable(_ context.Context, channelID uuid.UUID) error {
	r.unavailableCalls[channelID]++
	return nil
}
func (r *fakeBlobRepo) MarkChannelAvailable(_ context.Context, channelID uuid.UUID) error {
	r.availableCalls[channelID]++
	return nil
}

// ---- fake event logger -----------------------------------------------------

type loggedEvent struct {
	kind, message, ref string
}

type fakeEventRepo struct {
	events []loggedEvent
}

func newFakeEventRepo() *fakeEventRepo { return &fakeEventRepo{} }

func (r *fakeEventRepo) Log(_ context.Context, kind, message, ref string) error {
	r.events = append(r.events, loggedEvent{kind, message, ref})
	return nil
}

// ---- fake settings getter --------------------------------------------------

type fakeSettingsRepo struct {
	settings model.Settings
}

func newFakeSettingsRepo() *fakeSettingsRepo {
	return &fakeSettingsRepo{settings: model.DefaultSettings()}
}

func (r *fakeSettingsRepo) Get(context.Context) (model.Settings, error) {
	return r.settings, nil
}

// ---- fake telegram client --------------------------------------------------

// fakeTelegram returns canned getMe/getChat responses.
type fakeTelegram struct {
	// usernameByToken maps a bot token to the username getMe should return.
	usernameByToken map[string]string
	// memberByChat maps a tg chat id to whether the bot is a member.
	memberByChat map[int64]bool
	// titleByChat maps a tg chat id to its title.
	titleByChat map[int64]string
	// getMeErr, if set, is returned by GetMe regardless of token.
	getMeErr error
}

func newFakeTelegram() *fakeTelegram {
	return &fakeTelegram{
		usernameByToken: map[string]string{},
		memberByChat:    map[int64]bool{},
		titleByChat:     map[int64]string{},
	}
}

func (t *fakeTelegram) GetMe(_ context.Context, bot *model.Bot) (string, error) {
	if t.getMeErr != nil {
		return "", t.getMeErr
	}
	if u, ok := t.usernameByToken[bot.Token]; ok {
		return u, nil
	}
	return "", errors.New("unknown token")
}
func (t *fakeTelegram) GetChat(_ context.Context, _ *model.Bot, chatID int64) (string, bool, error) {
	return t.titleByChat[chatID], t.memberByChat[chatID], nil
}

// ---- svcHarness ---------------------------------------------------------------

type svcHarness struct {
	tx       *recordingTx
	tg       *fakeTelegram
	blobs    *fakeBlobRepo
	bots     *fakeBotRepo
	channels *fakeChannelRepo
	bc       *fakeBotChannelRepo
	events   *fakeEventRepo
	settings *fakeSettingsRepo
}

func newSvcHarness() *svcHarness {
	return &svcHarness{
		tx:       &recordingTx{},
		tg:       newFakeTelegram(),
		blobs:    newFakeBlobRepo(),
		bots:     newFakeBotRepo(),
		channels: newFakeChannelRepo(),
		bc:       newFakeBotChannelRepo(),
		events:   newFakeEventRepo(),
		settings: newFakeSettingsRepo(),
	}
}

func (h *svcHarness) botSvc() *BotService {
	return NewBotService(h.bots, h.channels, h.bc, h.blobs, h.events, h.tx, h.tg, testLogger())
}

func (h *svcHarness) channelSvc() *ChannelService {
	return NewChannelService(h.channels, h.bots, h.bc, h.blobs, h.events, h.settings, h.tx, h.tg, testLogger())
}

// ---- tests -----------------------------------------------------------------

func TestChannelAddChatIDPrefixAndIdempotency(t *testing.T) {
	h := newSvcHarness()
	svc := h.channelSvc()

	const bareID = int64(1234567890)
	const wantChatID = int64(-1001234567890)

	ch, err := svc.Add(context.Background(), bareID)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if ch.TGChatID != wantChatID {
		t.Fatalf("chat id = %d, want %d", ch.TGChatID, wantChatID)
	}
	if ch.EvictionThreshold != model.DefaultSettings().DefaultEvictionThreshold {
		t.Fatalf("eviction threshold = %d, want %d", ch.EvictionThreshold, model.DefaultSettings().DefaultEvictionThreshold)
	}

	// Re-adding the same bare id must reuse the row, not create a second one.
	ch2, err := svc.Add(context.Background(), bareID)
	if err != nil {
		t.Fatalf("Add (idempotent): %v", err)
	}
	if ch2.ID != ch.ID {
		t.Fatalf("idempotent Add returned new id %s, want %s", ch2.ID, ch.ID)
	}
	if got := len(h.channels.channels); got != 1 {
		t.Fatalf("channel count = %d, want 1", got)
	}
}

// TestChannelAddRecordsTitleAndMembership verifies the -100 prefix is applied,
// the channel title is taken from the first member bot, and the membership
// matrix is populated.
func TestChannelAddRecordsTitleAndMembership(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	const bareID = int64(555)
	const chatID = int64(-100555)
	h.tg.memberByChat[chatID] = true
	h.tg.titleByChat[chatID] = "My Channel"

	botID := uuid.New()
	h.bots.bots[botID] = &model.Bot{ID: botID, Username: "a", Token: "t", Enabled: true}

	svc := h.channelSvc()
	ch, err := svc.Add(ctx, bareID)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if ch.Title != "My Channel" {
		t.Fatalf("title = %q, want %q", ch.Title, "My Channel")
	}
	bc, err := h.bc.Get(ctx, botID, ch.ID)
	if err != nil {
		t.Fatalf("membership not recorded: %v", err)
	}
	if !bc.Member {
		t.Fatalf("expected bot recorded as member")
	}
}

func TestBotPickForUploadRoundRobin(t *testing.T) {
	h := newSvcHarness()

	// One channel, two enabled member bots.
	channelID := uuid.New()
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: -100, Available: true}

	botA := uuid.New()
	botB := uuid.New()
	h.bots.bots[botA] = &model.Bot{ID: botA, Username: "a", Enabled: true}
	h.bots.bots[botB] = &model.Bot{ID: botB, Username: "b", Enabled: true}
	h.bc.m[botChannelKey{botA, channelID}] = &model.BotChannel{BotID: botA, ChannelID: channelID, Member: true}
	h.bc.m[botChannelKey{botB, channelID}] = &model.BotChannel{BotID: botB, ChannelID: channelID, Member: true}

	svc := h.botSvc()
	ctx := context.Background()

	// Round-robin: over a full cycle each bot must be returned an equal number
	// of times.
	counts := map[uuid.UUID]int{}
	const rounds = 6
	for i := 0; i < rounds; i++ {
		bot, err := svc.PickForUpload(ctx, channelID)
		if err != nil {
			t.Fatalf("PickForUpload: %v", err)
		}
		counts[bot.ID]++
	}
	if counts[botA] != rounds/2 || counts[botB] != rounds/2 {
		t.Fatalf("round-robin not balanced: a=%d b=%d", counts[botA], counts[botB])
	}

	// Consecutive picks must alternate (not return the same bot twice in a row).
	first, _ := svc.PickForUpload(ctx, channelID)
	second, _ := svc.PickForUpload(ctx, channelID)
	if first.ID == second.ID {
		t.Fatalf("expected round-robin to alternate, got %s twice", first.ID)
	}
}

func TestBotPickForUploadErrNoBot(t *testing.T) {
	h := newSvcHarness()
	channelID := uuid.New()
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: -100, Available: true}

	svc := h.botSvc()
	ctx := context.Background()

	// No bots at all.
	if _, err := svc.PickForUpload(ctx, channelID); !errors.Is(err, ErrNoBot) {
		t.Fatalf("want ErrNoBot, got %v", err)
	}

	// A disabled member bot is not eligible.
	botID := uuid.New()
	h.bots.bots[botID] = &model.Bot{ID: botID, Username: "x", Enabled: false}
	h.bc.m[botChannelKey{botID, channelID}] = &model.BotChannel{BotID: botID, ChannelID: channelID, Member: true}
	if _, err := svc.PickForUpload(ctx, channelID); !errors.Is(err, ErrNoBot) {
		t.Fatalf("disabled bot: want ErrNoBot, got %v", err)
	}

	// A non-member enabled bot is not eligible.
	h.bots.bots[botID].Enabled = true
	h.bc.m[botChannelKey{botID, channelID}].Member = false
	if _, err := svc.PickForUpload(ctx, channelID); !errors.Is(err, ErrNoBot) {
		t.Fatalf("non-member bot: want ErrNoBot, got %v", err)
	}

	// A rate-limited (UnavailableUntil in the future) member bot is not eligible.
	future := time.Now().Add(time.Hour)
	h.bc.m[botChannelKey{botID, channelID}].Member = true
	h.bots.bots[botID].UnavailableUntil = &future
	if _, err := svc.PickForUpload(ctx, channelID); !errors.Is(err, ErrNoBot) {
		t.Fatalf("rate-limited bot: want ErrNoBot, got %v", err)
	}
}

func TestReevaluateAvailabilityFlipsChannelAndBlobs(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	// Channel currently marked available with a member bot.
	channelID := uuid.New()
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: -100, Available: true}
	botID := uuid.New()
	h.bots.bots[botID] = &model.Bot{ID: botID, Username: "a", Enabled: true}
	h.bc.m[botChannelKey{botID, channelID}] = &model.BotChannel{BotID: botID, ChannelID: channelID, Member: true}

	chSvc := h.channelSvc()

	// Initially: one enabled member → channel stays available, blobs marked available.
	if err := chSvc.ReevaluateAvailability(ctx); err != nil {
		t.Fatalf("ReevaluateAvailability: %v", err)
	}
	if !h.channels.channels[channelID].Available {
		t.Fatalf("channel should be available")
	}
	if h.blobs.availableCalls[channelID] == 0 {
		t.Fatalf("expected MarkChannelAvailable to be called")
	}
	// The channel-row + blobs mutation must have gone through a transaction.
	if h.tx.calls == 0 {
		t.Fatalf("expected availability change to run inside a transaction")
	}

	// Disable the only member bot → channel must flip unavailable + blobs unavailable.
	h.bots.bots[botID].Enabled = false
	if err := chSvc.ReevaluateAvailability(ctx); err != nil {
		t.Fatalf("ReevaluateAvailability (after disable): %v", err)
	}
	if h.channels.channels[channelID].Available {
		t.Fatalf("channel should be unavailable after disabling its only bot")
	}
	if h.blobs.unavailableCalls[channelID] == 0 {
		t.Fatalf("expected MarkChannelUnavailable to be called")
	}
}

// TestReevaluateAvailabilityTxError verifies a transaction failure propagates
// out of the re-evaluation rather than being swallowed.
func TestReevaluateAvailabilityTxError(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	channelID := uuid.New()
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: -100, Available: true}
	botID := uuid.New()
	h.bots.bots[botID] = &model.Bot{ID: botID, Username: "a", Enabled: true}
	h.bc.m[botChannelKey{botID, channelID}] = &model.BotChannel{BotID: botID, ChannelID: channelID, Member: true}

	h.tx.withTxErr = errors.New("boom")

	chSvc := h.channelSvc()
	if err := chSvc.ReevaluateAvailability(ctx); err == nil {
		t.Fatalf("expected error when transaction fails")
	}
}

func TestBotAddValidatesTokenAndChecksMembership(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	// A channel exists so Add checks membership of it.
	channelID := uuid.New()
	chatID := int64(-1009999)
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: chatID, Available: true}
	h.tg.memberByChat[chatID] = true
	h.tg.usernameByToken["tok1"] = "bot1"

	svc := h.botSvc()
	b, err := svc.Add(ctx, "tok1")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if b.Username != "bot1" {
		t.Fatalf("username = %q, want %q", b.Username, "bot1")
	}
	// Membership of the existing channel must have been recorded.
	bc, err := h.bc.Get(ctx, b.ID, channelID)
	if err != nil {
		t.Fatalf("bot_channel not recorded: %v", err)
	}
	if !bc.Member {
		t.Fatalf("expected bot recorded as member")
	}
}

// TestBotAddInvalidToken verifies a getMe failure aborts Add (no bot created).
func TestBotAddInvalidToken(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()
	h.tg.getMeErr = errors.New("invalid token")

	svc := h.botSvc()
	if _, err := svc.Add(ctx, "bad"); err == nil {
		t.Fatalf("expected error for invalid token")
	}
	if got := len(h.bots.bots); got != 0 {
		t.Fatalf("bot count = %d, want 0 after invalid token", got)
	}
}

func TestBotAddIdempotentByUsername(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	channelID := uuid.New()
	chatID := int64(-1009999)
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: chatID, Available: true}
	h.tg.memberByChat[chatID] = true

	h.tg.usernameByToken["tok1"] = "samebot"
	h.tg.usernameByToken["tok2"] = "samebot"

	svc := h.botSvc()

	b1, err := svc.Add(ctx, "tok1")
	if err != nil {
		t.Fatalf("Add tok1: %v", err)
	}

	b2, err := svc.Add(ctx, "tok2")
	if err != nil {
		t.Fatalf("Add tok2: %v", err)
	}
	if b2.ID != b1.ID {
		t.Fatalf("re-adding same username produced new id %s, want %s", b2.ID, b1.ID)
	}
	if got := len(h.bots.bots); got != 1 {
		t.Fatalf("bot count = %d, want 1", got)
	}
	if stored := h.bots.bots[b1.ID]; stored.Token != "tok2" {
		t.Fatalf("token not updated on idempotent add: got %q", stored.Token)
	}
}

func TestBotRemoveLogsEventAndReevaluates(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	channelID := uuid.New()
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: -100, Available: true}
	botID := uuid.New()
	h.bots.bots[botID] = &model.Bot{ID: botID, Username: "a", Enabled: true}
	h.bc.m[botChannelKey{botID, channelID}] = &model.BotChannel{BotID: botID, ChannelID: channelID, Member: true}

	svc := h.botSvc()
	if err := svc.Remove(ctx, botID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, ok := h.bots.bots[botID]; ok {
		t.Fatalf("bot should be deleted")
	}
	// Channel had only this bot; after removal it must be unavailable.
	if h.channels.channels[channelID].Available {
		t.Fatalf("channel should be unavailable after removing its only bot")
	}
	found := false
	for _, e := range h.events.events {
		if e.kind == model.EventBotDisabled && e.ref == botID.String() {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EventBotDisabled logged for removed bot")
	}
}

// TestBotRemoveNotFound verifies removing a missing bot surfaces the error.
func TestBotRemoveNotFound(t *testing.T) {
	h := newSvcHarness()
	svc := h.botSvc()
	if err := svc.Remove(context.Background(), uuid.New()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestBotSetEnabledLogsOnDisable verifies disabling a bot re-evaluates
// availability and logs a disabled event, while enabling does not log.
func TestBotSetEnabledLogsOnDisable(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	channelID := uuid.New()
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: -100, Available: true}
	botID := uuid.New()
	h.bots.bots[botID] = &model.Bot{ID: botID, Username: "a", Enabled: true}
	h.bc.m[botChannelKey{botID, channelID}] = &model.BotChannel{BotID: botID, ChannelID: channelID, Member: true}

	svc := h.botSvc()
	if err := svc.SetEnabled(ctx, botID, false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if h.bots.bots[botID].Enabled {
		t.Fatalf("bot should be disabled")
	}
	if h.channels.channels[channelID].Available {
		t.Fatalf("channel should flip unavailable after disabling its only bot")
	}
	disabledEvents := 0
	for _, e := range h.events.events {
		if e.kind == model.EventBotDisabled {
			disabledEvents++
		}
	}
	if disabledEvents != 1 {
		t.Fatalf("want exactly 1 disabled event, got %d", disabledEvents)
	}

	// Re-enabling must not log a disabled event.
	if err := svc.SetEnabled(ctx, botID, true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	disabledEvents = 0
	for _, e := range h.events.events {
		if e.kind == model.EventBotDisabled {
			disabledEvents++
		}
	}
	if disabledEvents != 1 {
		t.Fatalf("enabling should not log a disabled event; got %d total", disabledEvents)
	}
}

// TestBotGetAndList exercise the simple read paths and not-found.
func TestBotGetAndList(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()
	svc := h.botSvc()

	if _, err := svc.Get(ctx, uuid.New()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}

	id := uuid.New()
	h.bots.bots[id] = &model.Bot{ID: id, Username: "a", Enabled: true}
	got, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != id {
		t.Fatalf("Get returned id %s, want %s", got.ID, id)
	}
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
}

// TestBotRefreshMembership verifies every (bot, channel) pair is re-checked and
// the matrix updated.
func TestBotRefreshMembership(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	channelID := uuid.New()
	chatID := int64(-100777)
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: chatID, Available: false}
	botID := uuid.New()
	h.bots.bots[botID] = &model.Bot{ID: botID, Username: "a", Enabled: true}
	// Initially recorded as non-member; telegram now reports membership.
	h.bc.m[botChannelKey{botID, channelID}] = &model.BotChannel{BotID: botID, ChannelID: channelID, Member: false}
	h.tg.memberByChat[chatID] = true

	svc := h.botSvc()
	if err := svc.RefreshMembership(ctx); err != nil {
		t.Fatalf("RefreshMembership: %v", err)
	}
	bc, _ := h.bc.Get(ctx, botID, channelID)
	if !bc.Member {
		t.Fatalf("membership should be refreshed to true")
	}
	// Now that there is an enabled member, availability must flip on.
	if !h.channels.channels[channelID].Available {
		t.Fatalf("channel should be available after membership refresh")
	}
}

func TestChannelRemoveDecommissions(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	channelID := uuid.New()
	h.channels.channels[channelID] = &model.Channel{ID: channelID, TGChatID: -100, Available: true}

	svc := h.channelSvc()
	if err := svc.Remove(ctx, channelID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Row must NOT be deleted, only marked unavailable.
	c, ok := h.channels.channels[channelID]
	if !ok {
		t.Fatalf("channel row should not be deleted")
	}
	if c.Available {
		t.Fatalf("channel should be marked unavailable")
	}
	if h.blobs.unavailableCalls[channelID] == 0 {
		t.Fatalf("expected MarkChannelUnavailable")
	}
	// The two mutations must have run inside a transaction.
	if h.tx.calls == 0 {
		t.Fatalf("expected Remove to run inside a transaction")
	}
	found := false
	for _, e := range h.events.events {
		if e.kind == model.EventChannelDisabled && e.ref == channelID.String() {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EventChannelDisabled logged")
	}
}

// TestChannelSetEvictionThreshold covers the threshold setter and not-found.
func TestChannelSetEvictionThreshold(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()
	svc := h.channelSvc()

	if err := svc.SetEvictionThreshold(ctx, uuid.New(), 10); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing channel: want ErrNotFound, got %v", err)
	}

	id := uuid.New()
	h.channels.channels[id] = &model.Channel{ID: id, TGChatID: -100, EvictionThreshold: 1}
	if err := svc.SetEvictionThreshold(ctx, id, 4242); err != nil {
		t.Fatalf("SetEvictionThreshold: %v", err)
	}
	if got := h.channels.channels[id].EvictionThreshold; got != 4242 {
		t.Fatalf("threshold = %d, want 4242", got)
	}
}

func TestChannelPickForUploadRoundRobinAndErrNoBot(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	svc := h.channelSvc()

	// No channels → ErrNoBot.
	if _, err := svc.PickForUpload(ctx); !errors.Is(err, ErrNoBot) {
		t.Fatalf("want ErrNoBot, got %v", err)
	}

	// Two available channels each with an enabled member bot.
	ch1 := uuid.New()
	ch2 := uuid.New()
	h.channels.channels[ch1] = &model.Channel{ID: ch1, TGChatID: -101, Available: true}
	h.channels.channels[ch2] = &model.Channel{ID: ch2, TGChatID: -102, Available: true}
	botID := uuid.New()
	h.bots.bots[botID] = &model.Bot{ID: botID, Username: "a", Enabled: true}
	h.bc.m[botChannelKey{botID, ch1}] = &model.BotChannel{BotID: botID, ChannelID: ch1, Member: true}
	h.bc.m[botChannelKey{botID, ch2}] = &model.BotChannel{BotID: botID, ChannelID: ch2, Member: true}

	counts := map[uuid.UUID]int{}
	for i := 0; i < 4; i++ {
		c, err := svc.PickForUpload(ctx)
		if err != nil {
			t.Fatalf("PickForUpload: %v", err)
		}
		counts[c.ID]++
	}
	if counts[ch1] != 2 || counts[ch2] != 2 {
		t.Fatalf("round-robin not balanced: ch1=%d ch2=%d", counts[ch1], counts[ch2])
	}

	// A channel that is unavailable or has no enabled member bot is excluded.
	h.channels.channels[ch1].Available = false
	h.bots.bots[botID].Enabled = false // disables ch2's only bot
	if _, err := svc.PickForUpload(ctx); !errors.Is(err, ErrNoBot) {
		t.Fatalf("want ErrNoBot when no eligible channel, got %v", err)
	}
}

// TestChannelPickForUploadSkipsRateLimitedOnlyChannel verifies that a channel
// whose only member bot is currently rate-limited (UnavailableUntil in the
// future) is skipped, while a channel with a usable member bot is still picked,
// even though the channel itself is marked available.
func TestChannelPickForUploadSkipsRateLimitedOnlyChannel(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()

	svc := h.channelSvc()

	chParked := uuid.New()
	chOK := uuid.New()
	h.channels.channels[chParked] = &model.Channel{ID: chParked, TGChatID: -201, Available: true}
	h.channels.channels[chOK] = &model.Channel{ID: chOK, TGChatID: -202, Available: true}

	future := time.Now().Add(time.Hour)
	parkedBot := uuid.New()
	usableBot := uuid.New()
	h.bots.bots[parkedBot] = &model.Bot{ID: parkedBot, Username: "parked", Enabled: true, UnavailableUntil: &future}
	h.bots.bots[usableBot] = &model.Bot{ID: usableBot, Username: "usable", Enabled: true}
	h.bc.m[botChannelKey{parkedBot, chParked}] = &model.BotChannel{BotID: parkedBot, ChannelID: chParked, Member: true}
	h.bc.m[botChannelKey{usableBot, chOK}] = &model.BotChannel{BotID: usableBot, ChannelID: chOK, Member: true}

	// Every pick must be chOK; chParked must never be returned.
	for i := 0; i < 5; i++ {
		c, err := svc.PickForUpload(ctx)
		if err != nil {
			t.Fatalf("PickForUpload: %v", err)
		}
		if c.ID != chOK {
			t.Fatalf("pick %d returned %s, want the channel with a usable bot %s", i, c.ID, chOK)
		}
	}

	// If chOK's bot also becomes rate-limited, no channel has a usable member bot
	// even though both channels are still marked available → ErrNoBot.
	h.bots.bots[usableBot].UnavailableUntil = &future
	if _, err := svc.PickForUpload(ctx); !errors.Is(err, ErrNoBot) {
		t.Fatalf("want ErrNoBot when every member bot is rate-limited, got %v", err)
	}
}

// TestChannelGetAndList exercise the simple read paths and not-found.
func TestChannelGetAndList(t *testing.T) {
	h := newSvcHarness()
	ctx := context.Background()
	svc := h.channelSvc()

	if _, err := svc.Get(ctx, uuid.New()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}

	id := uuid.New()
	h.channels.channels[id] = &model.Channel{ID: id, TGChatID: -100, Available: true}
	if _, err := svc.Get(ctx, id); err != nil {
		t.Fatalf("Get: %v", err)
	}
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
}
