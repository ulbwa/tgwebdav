package services

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// testLogger returns a logger that discards output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- fake TxManager --------------------------------------------------------

// fakeTx runs fn against the same repositories with no real transaction.
type fakeTx struct {
	repos *domain.Repositories
}

func (t *fakeTx) WithTx(ctx context.Context, fn func(ctx context.Context, r *domain.Repositories) error) error {
	return fn(ctx, t.repos)
}

// ---- fake repositories -----------------------------------------------------

type fakeBotRepo struct {
	bots map[uuid.UUID]*domain.Bot
}

func newFakeBotRepo() *fakeBotRepo { return &fakeBotRepo{bots: map[uuid.UUID]*domain.Bot{}} }

func (r *fakeBotRepo) Create(_ context.Context, b *domain.Bot) error {
	cp := *b
	r.bots[b.ID] = &cp
	return nil
}
func (r *fakeBotRepo) Update(_ context.Context, b *domain.Bot) error {
	if _, ok := r.bots[b.ID]; !ok {
		return domain.ErrNotFound
	}
	cp := *b
	r.bots[b.ID] = &cp
	return nil
}
func (r *fakeBotRepo) Delete(_ context.Context, id uuid.UUID) error {
	delete(r.bots, id)
	return nil
}
func (r *fakeBotRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Bot, error) {
	b, ok := r.bots[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *b
	return &cp, nil
}
func (r *fakeBotRepo) GetByUsername(_ context.Context, username string) (*domain.Bot, error) {
	for _, b := range r.bots {
		if b.Username == username {
			cp := *b
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (r *fakeBotRepo) List(_ context.Context) ([]domain.Bot, error) {
	out := make([]domain.Bot, 0, len(r.bots))
	for _, b := range r.bots {
		out = append(out, *b)
	}
	sortBots(out)
	return out, nil
}
func (r *fakeBotRepo) SetUnavailableUntil(_ context.Context, id uuid.UUID, until *time.Time) error {
	b, ok := r.bots[id]
	if !ok {
		return domain.ErrNotFound
	}
	b.UnavailableUntil = until
	return nil
}

// sortBots orders bots by id string so List is deterministic.
func sortBots(bots []domain.Bot) {
	for i := 1; i < len(bots); i++ {
		for j := i; j > 0 && bots[j-1].ID.String() > bots[j].ID.String(); j-- {
			bots[j-1], bots[j] = bots[j], bots[j-1]
		}
	}
}

type fakeChannelRepo struct {
	channels map[uuid.UUID]*domain.Channel
}

func newFakeChannelRepo() *fakeChannelRepo {
	return &fakeChannelRepo{channels: map[uuid.UUID]*domain.Channel{}}
}

func (r *fakeChannelRepo) Create(_ context.Context, c *domain.Channel) error {
	cp := *c
	r.channels[c.ID] = &cp
	return nil
}
func (r *fakeChannelRepo) Update(_ context.Context, c *domain.Channel) error {
	if _, ok := r.channels[c.ID]; !ok {
		return domain.ErrNotFound
	}
	cp := *c
	r.channels[c.ID] = &cp
	return nil
}
func (r *fakeChannelRepo) Delete(_ context.Context, id uuid.UUID) error {
	delete(r.channels, id)
	return nil
}
func (r *fakeChannelRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Channel, error) {
	c, ok := r.channels[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *c
	return &cp, nil
}
func (r *fakeChannelRepo) GetByChatID(_ context.Context, chatID int64) (*domain.Channel, error) {
	for _, c := range r.channels {
		if c.TGChatID == chatID {
			cp := *c
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (r *fakeChannelRepo) List(_ context.Context) ([]domain.Channel, error) {
	out := make([]domain.Channel, 0, len(r.channels))
	for _, c := range r.channels {
		out = append(out, *c)
	}
	sortChannels(out)
	return out, nil
}
func (r *fakeChannelRepo) IncrementCounter(_ context.Context, id uuid.UUID, delta int64) (int64, error) {
	c, ok := r.channels[id]
	if !ok {
		return 0, domain.ErrNotFound
	}
	c.MessageCounter += delta
	return c.MessageCounter, nil
}
func (r *fakeChannelRepo) SetAvailable(_ context.Context, id uuid.UUID, available bool) error {
	c, ok := r.channels[id]
	if !ok {
		return domain.ErrNotFound
	}
	c.Available = available
	return nil
}

func sortChannels(channels []domain.Channel) {
	for i := 1; i < len(channels); i++ {
		for j := i; j > 0 && channels[j-1].ID.String() > channels[j].ID.String(); j-- {
			channels[j-1], channels[j] = channels[j], channels[j-1]
		}
	}
}

type botChannelKey struct {
	bot, channel uuid.UUID
}

type fakeBotChannelRepo struct {
	m map[botChannelKey]*domain.BotChannel
}

func newFakeBotChannelRepo() *fakeBotChannelRepo {
	return &fakeBotChannelRepo{m: map[botChannelKey]*domain.BotChannel{}}
}

func (r *fakeBotChannelRepo) Upsert(_ context.Context, bc *domain.BotChannel) error {
	cp := *bc
	r.m[botChannelKey{bc.BotID, bc.ChannelID}] = &cp
	return nil
}
func (r *fakeBotChannelRepo) Get(_ context.Context, botID, channelID uuid.UUID) (*domain.BotChannel, error) {
	bc, ok := r.m[botChannelKey{botID, channelID}]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *bc
	return &cp, nil
}
func (r *fakeBotChannelRepo) ListByChannel(_ context.Context, channelID uuid.UUID) ([]domain.BotChannel, error) {
	var out []domain.BotChannel
	for _, bc := range r.m {
		if bc.ChannelID == channelID {
			out = append(out, *bc)
		}
	}
	return out, nil
}
func (r *fakeBotChannelRepo) ListByBot(_ context.Context, botID uuid.UUID) ([]domain.BotChannel, error) {
	var out []domain.BotChannel
	for _, bc := range r.m {
		if bc.BotID == botID {
			out = append(out, *bc)
		}
	}
	return out, nil
}
func (r *fakeBotChannelRepo) DeleteByBot(_ context.Context, botID uuid.UUID) error {
	for k := range r.m {
		if k.bot == botID {
			delete(r.m, k)
		}
	}
	return nil
}
func (r *fakeBotChannelRepo) DeleteByChannel(_ context.Context, channelID uuid.UUID) error {
	for k := range r.m {
		if k.channel == channelID {
			delete(r.m, k)
		}
	}
	return nil
}

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

func (r *fakeBlobRepo) Create(context.Context, *domain.Blob) error { return nil }
func (r *fakeBlobRepo) GetByID(context.Context, uuid.UUID) (*domain.Blob, error) {
	return nil, domain.ErrNotFound
}
func (r *fakeBlobRepo) Update(context.Context, *domain.Blob) error                  { return nil }
func (r *fakeBlobRepo) SetState(context.Context, uuid.UUID, domain.BlobState) error { return nil }
func (r *fakeBlobRepo) AddRefcount(context.Context, uuid.UUID, int64) error         { return nil }
func (r *fakeBlobRepo) ListByChannel(context.Context, uuid.UUID) ([]domain.Blob, error) {
	return nil, nil
}
func (r *fakeBlobRepo) ListByState(context.Context, domain.BlobState) ([]domain.Blob, error) {
	return nil, nil
}
func (r *fakeBlobRepo) ListCollectable(context.Context, int) ([]domain.Blob, error) {
	return nil, nil
}
func (r *fakeBlobRepo) MarkChannelUnavailable(_ context.Context, channelID uuid.UUID) error {
	r.unavailableCalls[channelID]++
	return nil
}
func (r *fakeBlobRepo) MarkChannelAvailable(_ context.Context, channelID uuid.UUID) error {
	r.availableCalls[channelID]++
	return nil
}
func (r *fakeBlobRepo) EvictOlderThan(context.Context, uuid.UUID, int64) (int64, error) {
	return 0, nil
}
func (r *fakeBlobRepo) Delete(context.Context, uuid.UUID) error { return nil }
func (r *fakeBlobRepo) Count(context.Context) (int64, error)    { return 0, nil }

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
func (r *fakeEventRepo) List(context.Context, string, int, int) ([]domain.Event, int64, error) {
	return nil, 0, nil
}

type fakeSettingsRepo struct {
	settings domain.Settings
}

func newFakeSettingsRepo() *fakeSettingsRepo {
	return &fakeSettingsRepo{settings: domain.DefaultSettings()}
}

func (r *fakeSettingsRepo) Get(context.Context) (domain.Settings, error) {
	return r.settings, nil
}
func (r *fakeSettingsRepo) Update(_ context.Context, s domain.Settings) error {
	r.settings = s
	return nil
}

// ---- fake TelegramAPI ------------------------------------------------------

// fakeTelegram returns canned getMe/getChat responses.
type fakeTelegram struct {
	// usernameByToken maps a bot token to the username getMe should return.
	usernameByToken map[string]string
	// memberByChat maps a tg chat id to whether the bot is a member.
	memberByChat map[int64]bool
	// titleByChat maps a tg chat id to its title.
	titleByChat map[int64]string
}

func newFakeTelegram() *fakeTelegram {
	return &fakeTelegram{
		usernameByToken: map[string]string{},
		memberByChat:    map[int64]bool{},
		titleByChat:     map[int64]string{},
	}
}

func (t *fakeTelegram) GetMe(_ context.Context, bot *domain.Bot) (string, error) {
	if u, ok := t.usernameByToken[bot.Token]; ok {
		return u, nil
	}
	return "", errors.New("unknown token")
}
func (t *fakeTelegram) GetChat(_ context.Context, _ *domain.Bot, chatID int64) (string, bool, error) {
	return t.titleByChat[chatID], t.memberByChat[chatID], nil
}
func (t *fakeTelegram) SendDocument(context.Context, *domain.Bot, int64, string, []byte) (domain.TGSendResult, error) {
	return domain.TGSendResult{}, errors.New("not implemented")
}
func (t *fakeTelegram) SendByFileID(context.Context, *domain.Bot, int64, string) (domain.TGSendResult, error) {
	return domain.TGSendResult{}, errors.New("not implemented")
}
func (t *fakeTelegram) ForwardMessage(context.Context, *domain.Bot, int64, int64, int64) (domain.TGSendResult, error) {
	return domain.TGSendResult{}, errors.New("not implemented")
}
func (t *fakeTelegram) DeleteMessage(context.Context, *domain.Bot, int64, int64) error {
	return nil
}
func (t *fakeTelegram) DownloadFile(context.Context, *domain.Bot, string) ([]byte, error) {
	return nil, errors.New("not implemented")
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	repos    *domain.Repositories
	tx       *fakeTx
	tg       *fakeTelegram
	blobs    *fakeBlobRepo
	bots     *fakeBotRepo
	channels *fakeChannelRepo
	bc       *fakeBotChannelRepo
	events   *fakeEventRepo
	settings *fakeSettingsRepo
}

func newHarness() *harness {
	bots := newFakeBotRepo()
	channels := newFakeChannelRepo()
	bc := newFakeBotChannelRepo()
	blobs := newFakeBlobRepo()
	events := newFakeEventRepo()
	settings := newFakeSettingsRepo()

	repos := &domain.Repositories{
		Bots:        bots,
		Channels:    channels,
		BotChannels: bc,
		Blobs:       blobs,
		Events:      events,
		Settings:    settings,
	}
	return &harness{
		repos:    repos,
		tx:       &fakeTx{repos: repos},
		tg:       newFakeTelegram(),
		blobs:    blobs,
		bots:     bots,
		channels: channels,
		bc:       bc,
		events:   events,
		settings: settings,
	}
}

// ---- tests -----------------------------------------------------------------

func TestChannelAddChatIDPrefixAndIdempotency(t *testing.T) {
	h := newHarness()
	svc := NewChannelService(h.repos, h.tx, h.tg, NewSettingsService(h.repos), testLogger())

	const bareID = int64(1234567890)
	const wantChatID = int64(-1001234567890)

	ch, err := svc.Add(context.Background(), bareID)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if ch.TGChatID != wantChatID {
		t.Fatalf("chat id = %d, want %d", ch.TGChatID, wantChatID)
	}
	if ch.EvictionThreshold != domain.DefaultSettings().DefaultEvictionThreshold {
		t.Fatalf("eviction threshold = %d, want %d", ch.EvictionThreshold, domain.DefaultSettings().DefaultEvictionThreshold)
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

func TestBotPickForUploadRoundRobin(t *testing.T) {
	h := newHarness()

	// One channel, two enabled member bots.
	channelID := uuid.New()
	h.channels.channels[channelID] = &domain.Channel{ID: channelID, TGChatID: -100, Available: true}

	botA := uuid.New()
	botB := uuid.New()
	h.bots.bots[botA] = &domain.Bot{ID: botA, Username: "a", Enabled: true}
	h.bots.bots[botB] = &domain.Bot{ID: botB, Username: "b", Enabled: true}
	h.bc.m[botChannelKey{botA, channelID}] = &domain.BotChannel{BotID: botA, ChannelID: channelID, Member: true}
	h.bc.m[botChannelKey{botB, channelID}] = &domain.BotChannel{BotID: botB, ChannelID: channelID, Member: true}

	svc := NewBotService(h.repos, h.tx, h.tg, testLogger())
	ctx := context.Background()

	// Round-robin: across consecutive picks both bots must appear, and over a
	// full cycle each bot must be returned an equal number of times.
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
	h := newHarness()
	channelID := uuid.New()
	h.channels.channels[channelID] = &domain.Channel{ID: channelID, TGChatID: -100, Available: true}

	svc := NewBotService(h.repos, h.tx, h.tg, testLogger())
	ctx := context.Background()

	// No bots at all.
	if _, err := svc.PickForUpload(ctx, channelID); !errors.Is(err, domain.ErrNoBot) {
		t.Fatalf("want ErrNoBot, got %v", err)
	}

	// A disabled member bot is not eligible.
	botID := uuid.New()
	h.bots.bots[botID] = &domain.Bot{ID: botID, Username: "x", Enabled: false}
	h.bc.m[botChannelKey{botID, channelID}] = &domain.BotChannel{BotID: botID, ChannelID: channelID, Member: true}
	if _, err := svc.PickForUpload(ctx, channelID); !errors.Is(err, domain.ErrNoBot) {
		t.Fatalf("disabled bot: want ErrNoBot, got %v", err)
	}

	// A non-member enabled bot is not eligible.
	h.bots.bots[botID].Enabled = true
	h.bc.m[botChannelKey{botID, channelID}].Member = false
	if _, err := svc.PickForUpload(ctx, channelID); !errors.Is(err, domain.ErrNoBot) {
		t.Fatalf("non-member bot: want ErrNoBot, got %v", err)
	}
}

func TestReevaluateAvailabilityFlipsChannelAndBlobs(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	// Channel currently marked available with a member bot.
	channelID := uuid.New()
	h.channels.channels[channelID] = &domain.Channel{ID: channelID, TGChatID: -100, Available: true}
	botID := uuid.New()
	h.bots.bots[botID] = &domain.Bot{ID: botID, Username: "a", Enabled: true}
	h.bc.m[botChannelKey{botID, channelID}] = &domain.BotChannel{BotID: botID, ChannelID: channelID, Member: true}

	chSvc := NewChannelService(h.repos, h.tx, h.tg, NewSettingsService(h.repos), testLogger())

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

func TestBotAddIdempotentByUsername(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	// A channel exists so Add checks membership of it.
	channelID := uuid.New()
	chatID := int64(-1009999)
	h.channels.channels[channelID] = &domain.Channel{ID: channelID, TGChatID: chatID, Available: true}
	h.tg.memberByChat[chatID] = true

	h.tg.usernameByToken["tok1"] = "samebot"
	h.tg.usernameByToken["tok2"] = "samebot"

	svc := NewBotService(h.repos, h.tx, h.tg, testLogger())

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

	// Membership of the existing channel must have been recorded.
	bc, err := h.bc.Get(ctx, b1.ID, channelID)
	if err != nil {
		t.Fatalf("bot_channel not recorded: %v", err)
	}
	if !bc.Member {
		t.Fatalf("expected bot recorded as member")
	}
}

func TestBotRemoveLogsEventAndReevaluates(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	channelID := uuid.New()
	h.channels.channels[channelID] = &domain.Channel{ID: channelID, TGChatID: -100, Available: true}
	botID := uuid.New()
	h.bots.bots[botID] = &domain.Bot{ID: botID, Username: "a", Enabled: true}
	h.bc.m[botChannelKey{botID, channelID}] = &domain.BotChannel{BotID: botID, ChannelID: channelID, Member: true}

	svc := NewBotService(h.repos, h.tx, h.tg, testLogger())
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
		if e.kind == domain.EventBotDisabled && e.ref == botID.String() {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EventBotDisabled logged for removed bot")
	}
}

func TestChannelRemoveDecommissions(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	channelID := uuid.New()
	h.channels.channels[channelID] = &domain.Channel{ID: channelID, TGChatID: -100, Available: true}

	svc := NewChannelService(h.repos, h.tx, h.tg, NewSettingsService(h.repos), testLogger())
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
	found := false
	for _, e := range h.events.events {
		if e.kind == domain.EventChannelDisabled && e.ref == channelID.String() {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EventChannelDisabled logged")
	}
}

func TestChannelPickForUploadRoundRobinAndErrNoBot(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	svc := NewChannelService(h.repos, h.tx, h.tg, NewSettingsService(h.repos), testLogger())

	// No channels → ErrNoBot.
	if _, err := svc.PickForUpload(ctx); !errors.Is(err, domain.ErrNoBot) {
		t.Fatalf("want ErrNoBot, got %v", err)
	}

	// Two available channels each with an enabled member bot.
	ch1 := uuid.New()
	ch2 := uuid.New()
	h.channels.channels[ch1] = &domain.Channel{ID: ch1, TGChatID: -101, Available: true}
	h.channels.channels[ch2] = &domain.Channel{ID: ch2, TGChatID: -102, Available: true}
	botID := uuid.New()
	h.bots.bots[botID] = &domain.Bot{ID: botID, Username: "a", Enabled: true}
	h.bc.m[botChannelKey{botID, ch1}] = &domain.BotChannel{BotID: botID, ChannelID: ch1, Member: true}
	h.bc.m[botChannelKey{botID, ch2}] = &domain.BotChannel{BotID: botID, ChannelID: ch2, Member: true}

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
	if _, err := svc.PickForUpload(ctx); !errors.Is(err, domain.ErrNoBot) {
		t.Fatalf("want ErrNoBot when no eligible channel, got %v", err)
	}
}

// TestChannelPickForUploadSkipsRateLimitedOnlyChannel verifies that a channel
// whose only member bot is currently rate-limited (UnavailableUntil in the
// future) is skipped, while a channel with a usable member bot is still picked,
// even though the channel itself is marked available. This guards the fix for
// PickForUpload returning a channel whose member bots are all parked.
func TestChannelPickForUploadSkipsRateLimitedOnlyChannel(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	svc := NewChannelService(h.repos, h.tx, h.tg, NewSettingsService(h.repos), testLogger())

	// Two available channels. chParked's only bot is rate-limited; chOK's bot is
	// usable right now.
	chParked := uuid.New()
	chOK := uuid.New()
	h.channels.channels[chParked] = &domain.Channel{ID: chParked, TGChatID: -201, Available: true}
	h.channels.channels[chOK] = &domain.Channel{ID: chOK, TGChatID: -202, Available: true}

	future := time.Now().Add(time.Hour)
	parkedBot := uuid.New()
	usableBot := uuid.New()
	// Both bots are enabled; parkedBot is rate-limited via UnavailableUntil.
	h.bots.bots[parkedBot] = &domain.Bot{ID: parkedBot, Username: "parked", Enabled: true, UnavailableUntil: &future}
	h.bots.bots[usableBot] = &domain.Bot{ID: usableBot, Username: "usable", Enabled: true}
	h.bc.m[botChannelKey{parkedBot, chParked}] = &domain.BotChannel{BotID: parkedBot, ChannelID: chParked, Member: true}
	h.bc.m[botChannelKey{usableBot, chOK}] = &domain.BotChannel{BotID: usableBot, ChannelID: chOK, Member: true}

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
	if _, err := svc.PickForUpload(ctx); !errors.Is(err, domain.ErrNoBot) {
		t.Fatalf("want ErrNoBot when every member bot is rate-limited, got %v", err)
	}
}

func TestSettingsService(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	svc := NewSettingsService(h.repos)

	got, err := svc.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DefaultEvictionThreshold != domain.DefaultSettings().DefaultEvictionThreshold {
		t.Fatalf("unexpected default settings: %+v", got)
	}

	got.MaxFileSize = 4242
	if err := svc.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, _ := svc.Get(ctx)
	if after.MaxFileSize != 4242 {
		t.Fatalf("settings not updated: %d", after.MaxFileSize)
	}
}

// compile-time assertion that the fakes satisfy the domain interfaces.
var (
	_ domain.BotRepository        = (*fakeBotRepo)(nil)
	_ domain.ChannelRepository    = (*fakeChannelRepo)(nil)
	_ domain.BotChannelRepository = (*fakeBotChannelRepo)(nil)
	_ domain.BlobRepository       = (*fakeBlobRepo)(nil)
	_ domain.EventRepository      = (*fakeEventRepo)(nil)
	_ domain.SettingsRepository   = (*fakeSettingsRepo)(nil)
	_ domain.TelegramAPI          = (*fakeTelegram)(nil)
	_ domain.TxManager            = (*fakeTx)(nil)
)
