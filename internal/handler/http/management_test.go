package http_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	canonhttp "github.com/ulbwa/tgwebdav/internal/handler/http"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
	"github.com/ulbwa/tgwebdav/internal/service"
)

// ---- in-memory stores ------------------------------------------------------

// memUserStore is an in-memory user repository sufficient for AuthService and
// UserService. Login uniqueness mirrors the real unique constraint.
type memUserStore struct {
	byID map[uuid.UUID]*model.User
}

func newMemUserStore() *memUserStore {
	return &memUserStore{byID: make(map[uuid.UUID]*model.User)}
}

func (s *memUserStore) Create(_ context.Context, u *model.User) error {
	for _, e := range s.byID {
		if e.Login == u.Login {
			return repository.ErrAlreadyExists
		}
	}
	cp := *u
	s.byID[u.ID] = &cp
	return nil
}

func (s *memUserStore) Update(_ context.Context, u *model.User) error {
	if _, ok := s.byID[u.ID]; !ok {
		return repository.ErrNotFound
	}
	cp := *u
	s.byID[u.ID] = &cp
	return nil
}

func (s *memUserStore) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := s.byID[id]; !ok {
		return repository.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

func (s *memUserStore) GetByID(_ context.Context, id uuid.UUID) (*model.User, error) {
	u, ok := s.byID[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *memUserStore) GetByLogin(_ context.Context, login string) (*model.User, error) {
	for _, u := range s.byID {
		if u.Login == login {
			cp := *u
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (s *memUserStore) List(_ context.Context) ([]model.User, error) {
	out := make([]model.User, 0, len(s.byID))
	for _, u := range s.byID {
		out = append(out, *u)
	}
	return out, nil
}

// memTokenStore is an in-memory token repository for AuthService + UserService.
type memTokenStore struct {
	byID map[uuid.UUID]*model.APIToken
}

func newMemTokenStore() *memTokenStore {
	return &memTokenStore{byID: make(map[uuid.UUID]*model.APIToken)}
}

func (s *memTokenStore) Create(_ context.Context, t *model.APIToken) error {
	cp := *t
	s.byID[t.ID] = &cp
	return nil
}

func (s *memTokenStore) ListByUser(_ context.Context, userID uuid.UUID) ([]model.APIToken, error) {
	out := make([]model.APIToken, 0)
	for _, t := range s.byID {
		if t.UserID == userID {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (s *memTokenStore) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := s.byID[id]; !ok {
		return repository.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

func (s *memTokenStore) GetByHash(_ context.Context, hash string) (*model.APIToken, error) {
	for _, t := range s.byID {
		if t.TokenHash == hash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (s *memTokenStore) TouchLastUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	if t, ok := s.byID[id]; ok {
		t.LastUsedAt = &at
	}
	return nil
}

// memSettingsStore is an in-memory settings repository.
type memSettingsStore struct{ s model.Settings }

func newMemSettingsStore() *memSettingsStore {
	return &memSettingsStore{s: model.DefaultSettings()}
}

func (m *memSettingsStore) Get(context.Context) (model.Settings, error) { return m.s, nil }
func (m *memSettingsStore) Update(_ context.Context, s model.Settings) error {
	s.UpdatedAt = time.Now().UTC()
	m.s = s
	return nil
}

// memEventStore is an in-memory event repository.
type memEventStore struct{ events []model.Event }

func (m *memEventStore) List(_ context.Context, kind string, limit, offset int) ([]model.Event, int64, error) {
	var filtered []model.Event
	for _, e := range m.events {
		if kind == "" || e.Kind == kind {
			filtered = append(filtered, e)
		}
	}
	total := int64(len(filtered))
	if offset > len(filtered) {
		offset = len(filtered)
	}
	filtered = filtered[offset:]
	if limit >= 0 && limit < len(filtered) {
		filtered = filtered[:limit]
	}
	return filtered, total, nil
}

// memStatStore is an in-memory stat repository.
type memStatStore struct{ samples []model.StatSample }

func (m *memStatStore) Record(_ context.Context, metric, label string, value float64) error {
	m.samples = append(m.samples, model.StatSample{
		ID: uuid.New(), TS: time.Now().UTC(), Metric: metric, Label: label, Value: value,
	})
	return nil
}

func (m *memStatStore) Query(_ context.Context, metric, label string, from, to time.Time) ([]model.StatSample, error) {
	var out []model.StatSample
	for _, s := range m.samples {
		if s.Metric == metric && s.Label == label {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memStatStore) Latest(_ context.Context, metric, label string) (*model.StatSample, error) {
	for i := len(m.samples) - 1; i >= 0; i-- {
		if m.samples[i].Metric == metric && m.samples[i].Label == label {
			cp := m.samples[i]
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}

// ---- test harness ----------------------------------------------------------

type mgmtFixture struct {
	handler    http.Handler
	users      *memUserStore
	tokens     *memTokenStore
	auth       *service.AuthService
	adminLogin string
	adminPass  string
	adminID    uuid.UUID
}

func newMgmtFixture(t *testing.T) *mgmtFixture {
	t.Helper()

	users := newMemUserStore()
	tokens := newMemTokenStore()
	settings := newMemSettingsStore()
	events := &memEventStore{}
	statStore := &memStatStore{}

	auth := service.NewAuthService(users, tokens)
	userSvc := service.NewUserService(users, tokens)
	settingsSvc := service.NewSettingsService(settings)
	eventSvc := service.NewEventService(events)
	statRec := service.NewStatRecorder(statStore, time.Minute, nil)

	// Seed an admin user directly via the service so the password hashes match.
	const adminPass = "s3cret-admin"
	admin, err := userSvc.Create(context.Background(), service.CreateUserParams{
		Login:    "admin",
		Password: adminPass,
		IsAdmin:  true,
	})
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	h := canonhttp.NewManagementHandler(canonhttp.ManagementDeps{
		Auth:     auth,
		Users:    userSvc,
		Settings: settingsSvc,
		Events:   eventSvc,
		Stats:    statRec,
		Logger:   nil,
		// Bots and Channels are not exercised by these tests; the routes that
		// need them are not called here.
	})

	return &mgmtFixture{
		handler:    h,
		users:      users,
		tokens:     tokens,
		auth:       auth,
		adminLogin: "admin",
		adminPass:  adminPass,
		adminID:    admin.ID,
	}
}

func (f *mgmtFixture) do(t *testing.T, method, target string, body string, basic [2]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rdr)
	if basic[0] != "" || basic[1] != "" {
		req.SetBasicAuth(basic[0], basic[1])
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// ---- tests -----------------------------------------------------------------

func TestHealthzPublic(t *testing.T) {
	f := newMgmtFixture(t)
	rec := f.do(t, http.MethodGet, "/healthz", "", [2]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("healthz body = %v, want status=ok", got)
	}
}

func TestSpecServed(t *testing.T) {
	f := newMgmtFixture(t)
	rec := f.do(t, http.MethodGet, "/openapi.yaml", "", [2]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.yaml status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Fatalf("spec Content-Type = %q, want application/yaml", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("spec body is empty")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("openapi")) {
		t.Fatal("spec body does not look like an OpenAPI document")
	}
}

func TestAPIRequiresAuth(t *testing.T) {
	f := newMgmtFixture(t)
	rec := f.do(t, http.MethodGet, "/api/v1/users", "", [2]string{})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/v1/users status = %d, want 401", rec.Code)
	}
	if wa := rec.Header().Get("WWW-Authenticate"); !strings.Contains(wa, "Basic") || !strings.Contains(wa, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want Basic + Bearer challenge", wa)
	}
}

func TestAPIForbiddenForNonAdmin(t *testing.T) {
	f := newMgmtFixture(t)
	// Create a non-admin user via the API as admin.
	rec := f.do(t, http.MethodPost, "/api/v1/users",
		`{"login":"alice","password":"pw-alice"}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create non-admin status = %d, want 201", rec.Code)
	}

	// Non-admin Basic credentials must be forbidden on /api/v1.
	rec = f.do(t, http.MethodGet, "/api/v1/users", "", [2]string{"alice", "pw-alice"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /api/v1/users status = %d, want 403", rec.Code)
	}
}

func TestAPIOKForAdmin(t *testing.T) {
	f := newMgmtFixture(t)
	rec := f.do(t, http.MethodGet, "/api/v1/users", "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /api/v1/users status = %d, want 200", rec.Code)
	}
	var users []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users, want 1 (the seeded admin)", len(users))
	}
}

func TestBearerTokenAuth(t *testing.T) {
	f := newMgmtFixture(t)
	// Mint a bearer token for the admin via the API.
	rec := f.do(t, http.MethodPost, "/api/v1/users/"+f.adminID.String()+"/tokens",
		`{"name":"ci"}`, [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token status = %d, want 201", rec.Code)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created token: %v", err)
	}
	if created.Token == "" {
		t.Fatal("created token plaintext is empty")
	}
	// The persisted hash must be sha-256 of the plaintext (contract with auth).
	sum := sha256.Sum256([]byte(created.Token))
	wantHash := hex.EncodeToString(sum[:])
	found := false
	for _, tok := range f.tokens.byID {
		if tok.TokenHash == wantHash {
			found = true
		}
	}
	if !found {
		t.Fatal("persisted token hash does not match sha-256(plaintext)")
	}

	// Use the bearer token to reach an admin-only route.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	bearerRec := httptest.NewRecorder()
	f.handler.ServeHTTP(bearerRec, req)
	if bearerRec.Code != http.StatusOK {
		t.Fatalf("bearer GET /api/v1/users status = %d, want 200", bearerRec.Code)
	}
}

func TestCreateUserAndGetRoundTrip(t *testing.T) {
	f := newMgmtFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/users",
		`{"login":"bob","password":"pw-bob","quota_bytes":1024,"rate_per_min":60}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("create user Content-Type = %q, want application/json", ct)
	}
	var created struct {
		Id         string `json:"id"`
		Login      string `json:"login"`
		IsAdmin    bool   `json:"is_admin"`
		QuotaBytes int64  `json:"quota_bytes"`
		RatePerMin int32  `json:"rate_per_min"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created user: %v", err)
	}
	if created.Login != "bob" || created.IsAdmin || created.QuotaBytes != 1024 || created.RatePerMin != 60 {
		t.Fatalf("created user shape unexpected: %+v", created)
	}
	if created.Id == "" {
		t.Fatal("created user has no id")
	}

	// GetUser round-trip.
	rec = f.do(t, http.MethodGet, "/api/v1/users/"+created.Id, "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("get created user status = %d, want 200", rec.Code)
	}

	// GetUser for a random (missing) id → 404.
	rec = f.do(t, http.MethodGet, "/api/v1/users/"+uuid.New().String(), "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing user status = %d, want 404", rec.Code)
	}
}
