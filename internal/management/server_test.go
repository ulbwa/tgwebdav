package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// fakeAuth is an in-memory domain.AuthService for handler tests.
type fakeAuth struct {
	basic  map[string]*domain.User // "user:pass" -> user
	bearer map[string]*domain.User // token -> user
}

func (f *fakeAuth) AuthenticateBasic(_ context.Context, username, password string) (*domain.Principal, error) {
	u, ok := f.basic[username+":"+password]
	if !ok {
		return nil, domain.ErrUnauthorized
	}
	return &domain.Principal{Acting: u, Auth: u}, nil
}

func (f *fakeAuth) AuthenticateBearer(_ context.Context, token string) (*domain.User, error) {
	u, ok := f.bearer[token]
	if !ok {
		return nil, domain.ErrUnauthorized
	}
	return u, nil
}

func (f *fakeAuth) HashPassword(pw string) (string, error) { return "hash:" + pw, nil }

// fakeUserRepo is a no-op domain.UserRepository returning a fixed list.
type fakeUserRepo struct{ users []domain.User }

func (r *fakeUserRepo) Create(context.Context, *domain.User) error        { return nil }
func (r *fakeUserRepo) Update(context.Context, *domain.User) error        { return nil }
func (r *fakeUserRepo) Delete(context.Context, uuid.UUID) error           { return nil }
func (r *fakeUserRepo) GetByID(context.Context, uuid.UUID) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (r *fakeUserRepo) GetByLogin(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (r *fakeUserRepo) List(context.Context) ([]domain.User, error) { return r.users, nil }
func (r *fakeUserRepo) Count(context.Context) (int64, error)        { return int64(len(r.users)), nil }

func newTestServer(t *testing.T, auth domain.AuthService, repos *domain.Repositories) http.Handler {
	t.Helper()
	h := NewHandlers(Deps{Repos: repos, Auth: auth})
	srv := NewServer(":0", h, auth, nil)
	return srv.Handler
}

func TestHealthzPublic(t *testing.T) {
	auth := &fakeAuth{}
	handler := newTestServer(t, auth, &domain.Repositories{})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("healthz status field = %q, want ok", body["status"])
	}
}

func TestUnauthorizedWithoutCredentials(t *testing.T) {
	auth := &fakeAuth{}
	handler := newTestServer(t, auth, &domain.Repositories{Users: &fakeUserRepo{}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatalf("missing WWW-Authenticate header")
	}
}

func TestForbiddenForNonAdminBearer(t *testing.T) {
	nonAdmin := &domain.User{ID: uuid.New(), Login: "bob", IsAdmin: false}
	auth := &fakeAuth{bearer: map[string]*domain.User{"tok-bob": nonAdmin}}
	handler := newTestServer(t, auth, &domain.Repositories{Users: &fakeUserRepo{}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer tok-bob")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAuthorizedAdminListsUsers(t *testing.T) {
	admin := &domain.User{ID: uuid.New(), Login: "root", IsAdmin: true, CreatedAt: time.Now()}
	auth := &fakeAuth{
		bearer: map[string]*domain.User{"tok-admin": admin},
		basic:  map[string]*domain.User{"root:secret": admin},
	}
	repos := &domain.Repositories{Users: &fakeUserRepo{users: []domain.User{*admin}}}
	handler := newTestServer(t, auth, repos)

	// Bearer path.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer tok-admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var users []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(users) != 1 || users[0]["login"] != "root" {
		t.Fatalf("unexpected users payload: %s", rec.Body.String())
	}

	// Basic path.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req2.SetBasicAuth("root", "secret")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("basic status = %d, want 200", rec2.Code)
	}
}

func TestSpecServedPublicly(t *testing.T) {
	auth := &fakeAuth{}
	handler := newTestServer(t, auth, &domain.Repositories{})

	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("spec status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("spec body empty")
	}
}
