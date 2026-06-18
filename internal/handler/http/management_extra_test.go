package http_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	canonhttp "github.com/ulbwa/tgwebdav/internal/handler/http"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
	"github.com/ulbwa/tgwebdav/internal/service"
)

// testDiscardLogger returns a logger that discards output (services require a
// non-nil logger).
func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- in-memory bot / channel / membership / blob stores --------------------
//
// These satisfy the dependency interfaces BotService and ChannelService declare
// so the Management bots/channels endpoints can be exercised end-to-end against
// real services (no mocks echoing themselves).

type memBotStore struct {
	mu sync.Mutex
	m  map[uuid.UUID]*model.Bot
}

func newMemBotStore() *memBotStore { return &memBotStore{m: map[uuid.UUID]*model.Bot{}} }

func (s *memBotStore) Create(_ context.Context, b *model.Bot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *b
	s.m[b.ID] = &cp
	return nil
}
func (s *memBotStore) Update(_ context.Context, b *model.Bot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[b.ID]; !ok {
		return repository.ErrNotFound
	}
	cp := *b
	s.m[b.ID] = &cp
	return nil
}
func (s *memBotStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return repository.ErrNotFound
	}
	delete(s.m, id)
	return nil
}
func (s *memBotStore) GetByID(_ context.Context, id uuid.UUID) (*model.Bot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *b
	return &cp, nil
}
func (s *memBotStore) GetByUsername(_ context.Context, username string) (*model.Bot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.m {
		if b.Username == username {
			cp := *b
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (s *memBotStore) List(_ context.Context) ([]model.Bot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Bot, 0, len(s.m))
	for _, b := range s.m {
		out = append(out, *b)
	}
	return out, nil
}

type memChannelStore struct {
	mu sync.Mutex
	m  map[uuid.UUID]*model.Channel
}

func newMemChannelStore() *memChannelStore {
	return &memChannelStore{m: map[uuid.UUID]*model.Channel{}}
}

func (s *memChannelStore) Create(_ context.Context, c *model.Channel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	s.m[c.ID] = &cp
	return nil
}
func (s *memChannelStore) Update(_ context.Context, c *model.Channel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[c.ID]; !ok {
		return repository.ErrNotFound
	}
	cp := *c
	s.m[c.ID] = &cp
	return nil
}
func (s *memChannelStore) GetByID(_ context.Context, id uuid.UUID) (*model.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *c
	return &cp, nil
}
func (s *memChannelStore) GetByChatID(_ context.Context, chatID int64) (*model.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.m {
		if c.TGChatID == chatID {
			cp := *c
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (s *memChannelStore) List(_ context.Context) ([]model.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Channel, 0, len(s.m))
	for _, c := range s.m {
		out = append(out, *c)
	}
	return out, nil
}
func (s *memChannelStore) SetAvailable(_ context.Context, id uuid.UUID, available bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[id]
	if !ok {
		return repository.ErrNotFound
	}
	c.Available = available
	return nil
}

type memBotChannelStore struct {
	mu sync.Mutex
	m  map[[2]uuid.UUID]*model.BotChannel
}

func newMemBotChannelStore() *memBotChannelStore {
	return &memBotChannelStore{m: map[[2]uuid.UUID]*model.BotChannel{}}
}

func (s *memBotChannelStore) Upsert(_ context.Context, bc *model.BotChannel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *bc
	s.m[[2]uuid.UUID{bc.BotID, bc.ChannelID}] = &cp
	return nil
}
func (s *memBotChannelStore) ListByChannel(_ context.Context, channelID uuid.UUID) ([]model.BotChannel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.BotChannel
	for _, bc := range s.m {
		if bc.ChannelID == channelID {
			out = append(out, *bc)
		}
	}
	return out, nil
}

// memBlobAvailabilityStore satisfies the blobStore the bot/channel services use:
// only the channel availability flips.
type memBlobAvailabilityStore struct{}

func (memBlobAvailabilityStore) MarkChannelUnavailable(context.Context, uuid.UUID) error { return nil }
func (memBlobAvailabilityStore) MarkChannelAvailable(context.Context, uuid.UUID) error   { return nil }

// memEventLogger satisfies the eventLogger (Log) interface the bot/channel
// services write through.
type memEventLogger struct{}

func (memEventLogger) Log(context.Context, string, string, string) error { return nil }

// memTx runs the closure directly.
type memTx struct{}

func (memTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// fakeTelegramAPI is a programmable telegram client for the bot/channel services.
type fakeTelegramAPI struct {
	usernameByToken map[string]string
	memberByChat    map[int64]bool
	titleByChat     map[int64]string
}

func newFakeTelegramAPI() *fakeTelegramAPI {
	return &fakeTelegramAPI{
		usernameByToken: map[string]string{},
		memberByChat:    map[int64]bool{},
		titleByChat:     map[int64]string{},
	}
}

func (t *fakeTelegramAPI) GetMe(_ context.Context, bot *model.Bot) (string, error) {
	if u, ok := t.usernameByToken[bot.Token]; ok {
		return u, nil
	}
	return "", repository.ErrNotFound // simulates a bad/unknown token
}
func (t *fakeTelegramAPI) GetChat(_ context.Context, _ *model.Bot, chatID int64) (string, bool, error) {
	return t.titleByChat[chatID], t.memberByChat[chatID], nil
}

// ---- extended fixture wiring bots + channels -------------------------------

type fullFixture struct {
	*mgmtFixture
	tg *fakeTelegramAPI
}

func newFullFixture(t *testing.T) *fullFixture {
	t.Helper()

	users := newMemUserStore()
	tokens := newMemTokenStore()
	settings := newMemSettingsStore()
	events := &memEventStore{}
	statStore := &memStatStore{}
	bots := newMemBotStore()
	channels := newMemChannelStore()
	bcs := newMemBotChannelStore()
	blobs := memBlobAvailabilityStore{}
	tx := memTx{}
	tg := newFakeTelegramAPI()

	auth := service.NewAuthService(users, tokens)
	userSvc := service.NewUserService(users, tokens)
	settingsSvc := service.NewSettingsService(settings)
	eventSvc := service.NewEventService(events)
	statRec := service.NewStatRecorder(statStore, time.Minute, nil)
	eventLog := memEventLogger{}
	botSvc := service.NewBotService(bots, channels, bcs, blobs, eventLog, tx, tg, testDiscardLogger())
	channelSvc := service.NewChannelService(channels, bots, bcs, blobs, eventLog, settingsSvc, tx, tg, testDiscardLogger())

	const adminPass = "s3cret-admin"
	admin, err := userSvc.Create(context.Background(), service.CreateUserParams{
		Login: "admin", Password: adminPass, IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	h := canonhttp.NewManagementHandler(canonhttp.ManagementDeps{
		Auth:     auth,
		Users:    userSvc,
		Bots:     botSvc,
		Channels: channelSvc,
		Settings: settingsSvc,
		Events:   eventSvc,
		Stats:    statRec,
		Logger:   nil,
	})

	mf := &mgmtFixture{
		handler:    h,
		users:      users,
		tokens:     tokens,
		auth:       auth,
		adminLogin: "admin",
		adminPass:  adminPass,
		adminID:    admin.ID,
	}
	return &fullFixture{mgmtFixture: mf, tg: tg}
}

// ---- bot endpoints ---------------------------------------------------------

func TestBotsCRUD(t *testing.T) {
	f := newFullFixture(t)
	f.tg.usernameByToken["good-token"] = "blob_bot"

	// List is empty initially.
	rec := f.do(t, http.MethodGet, "/api/v1/bots", "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("list bots status = %d, want 200", rec.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode bots: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("initial bot list len = %d, want 0", len(list))
	}

	// Add a bot.
	rec = f.do(t, http.MethodPost, "/api/v1/bots", `{"token":"good-token"}`, [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add bot status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Id       string `json:"id"`
		Username string `json:"username"`
		Enabled  bool   `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created bot: %v", err)
	}
	if created.Username != "blob_bot" || !created.Enabled || created.Id == "" {
		t.Fatalf("created bot shape unexpected: %+v", created)
	}

	// Empty token → 400.
	rec = f.do(t, http.MethodPost, "/api/v1/bots", `{"token":""}`, [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("add bot empty token status = %d, want 400", rec.Code)
	}

	// Disable the bot.
	rec = f.do(t, http.MethodPut, "/api/v1/bots/"+created.Id+"/enabled", `{"enabled":false}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set bot enabled status = %d, want 204", rec.Code)
	}

	// Set-enabled on a missing bot → 404.
	rec = f.do(t, http.MethodPut, "/api/v1/bots/"+uuid.New().String()+"/enabled", `{"enabled":true}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("set enabled missing bot status = %d, want 404", rec.Code)
	}

	// Delete the bot.
	rec = f.do(t, http.MethodDelete, "/api/v1/bots/"+created.Id, "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete bot status = %d, want 204", rec.Code)
	}

	// Delete missing → 404.
	rec = f.do(t, http.MethodDelete, "/api/v1/bots/"+uuid.New().String(), "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing bot status = %d, want 404", rec.Code)
	}
}

// TestAddBotInvalidToken exercises the upstream-validation failure path: the fake
// telegram GetMe rejects an unknown token, surfacing a 500-class error.
func TestAddBotInvalidToken(t *testing.T) {
	f := newFullFixture(t)
	rec := f.do(t, http.MethodPost, "/api/v1/bots", `{"token":"unknown"}`, [2]string{f.adminLogin, f.adminPass})
	if rec.Code < 400 {
		t.Fatalf("add bot with invalid token status = %d, want an error status", rec.Code)
	}
}

// ---- channel endpoints -----------------------------------------------------

func TestChannelsCRUD(t *testing.T) {
	f := newFullFixture(t)

	// List empty.
	rec := f.do(t, http.MethodGet, "/api/v1/channels", "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("list channels status = %d, want 200", rec.Code)
	}

	// Add a channel by bare id; the -100 prefix is applied internally.
	rec = f.do(t, http.MethodPost, "/api/v1/channels", `{"bare_id":1234567890}`, [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add channel status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Id       string `json:"id"`
		TgChatId int64  `json:"tg_chat_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created channel: %v", err)
	}
	if created.TgChatId != -1001234567890 {
		t.Fatalf("tg_chat_id = %d, want -1001234567890", created.TgChatId)
	}

	// bare_id 0 → 400.
	rec = f.do(t, http.MethodPost, "/api/v1/channels", `{"bare_id":0}`, [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("add channel bare_id=0 status = %d, want 400", rec.Code)
	}

	// Set eviction threshold.
	rec = f.do(t, http.MethodPut, "/api/v1/channels/"+created.Id+"/eviction", `{"threshold":500}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set eviction status = %d, want 204", rec.Code)
	}

	// Negative threshold → 400.
	rec = f.do(t, http.MethodPut, "/api/v1/channels/"+created.Id+"/eviction", `{"threshold":-1}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("set eviction negative status = %d, want 400", rec.Code)
	}

	// Set eviction on missing channel → 404.
	rec = f.do(t, http.MethodPut, "/api/v1/channels/"+uuid.New().String()+"/eviction", `{"threshold":1}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("set eviction missing status = %d, want 404", rec.Code)
	}

	// Delete (decommission) the channel — the service marks it unavailable, 204.
	rec = f.do(t, http.MethodDelete, "/api/v1/channels/"+created.Id, "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete channel status = %d, want 204", rec.Code)
	}
}
