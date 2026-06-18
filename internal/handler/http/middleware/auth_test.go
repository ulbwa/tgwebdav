package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/handler/http/middleware"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/service"
)

// ---- fake authenticator -----------------------------------------------------

// fakeAuth is a hand-written authenticator implementing both the Basic and the
// Bearer surfaces the middleware needs.
type fakeAuth struct {
	// basicFn resolves Basic credentials.
	basicFn func(ctx context.Context, username, password string) (*model.Principal, error)
	// bearerFn resolves a Bearer token.
	bearerFn func(ctx context.Context, token string) (*model.User, error)
}

func (f *fakeAuth) AuthenticateBasic(ctx context.Context, username, password string) (*model.Principal, error) {
	return f.basicFn(ctx, username, password)
}

func (f *fakeAuth) AuthenticateBearer(ctx context.Context, token string) (*model.User, error) {
	return f.bearerFn(ctx, token)
}

// principalSink is a terminal handler that records the principal off the context
// and writes 200, so tests can assert what auth placed there.
func principalSink(out **model.Principal) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := model.PrincipalFromContext(r.Context()); ok {
			*out = p
		}
		w.WriteHeader(http.StatusOK)
	})
}

// ---- BasicAuth (WebDAV) -----------------------------------------------------

func TestBasicAuthMissingCredentials(t *testing.T) {
	auth := &fakeAuth{basicFn: func(context.Context, string, string) (*model.Principal, error) {
		t.Fatal("AuthenticateBasic must not be called without credentials")
		return nil, nil
	}}
	var captured *model.Principal
	h := middleware.BasicAuth(auth)(principalSink(&captured))

	req := httptest.NewRequest(http.MethodGet, "/", nil) // no Authorization header
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if wa := rec.Header().Get("WWW-Authenticate"); !strings.Contains(wa, `Basic realm="tgwebdav"`) {
		t.Fatalf("WWW-Authenticate = %q, want a Basic challenge", wa)
	}
	if captured != nil {
		t.Fatal("next handler ran despite missing credentials")
	}
}

func TestBasicAuthSuccessStoresPrincipal(t *testing.T) {
	acting := &model.User{ID: uuid.New(), Login: "u"}
	want := &model.Principal{Acting: acting, Auth: acting}
	auth := &fakeAuth{basicFn: func(_ context.Context, user, pass string) (*model.Principal, error) {
		if user != "u" || pass != "pw" {
			t.Fatalf("unexpected creds: %q/%q", user, pass)
		}
		return want, nil
	}}
	var captured *model.Principal
	h := middleware.BasicAuth(auth)(principalSink(&captured))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("u", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if captured != want {
		t.Fatalf("principal on context = %v, want %v", captured, want)
	}
}

func TestBasicAuthUnauthorized(t *testing.T) {
	auth := &fakeAuth{basicFn: func(context.Context, string, string) (*model.Principal, error) {
		return nil, service.ErrUnauthorized
	}}
	h := middleware.BasicAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next ran despite unauthorized")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("u", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if wa := rec.Header().Get("WWW-Authenticate"); !strings.Contains(wa, "Basic") {
		t.Fatalf("WWW-Authenticate = %q, want Basic challenge on 401", wa)
	}
}

func TestBasicAuthForbiddenImpersonation(t *testing.T) {
	auth := &fakeAuth{basicFn: func(context.Context, string, string) (*model.Principal, error) {
		return nil, service.ErrForbidden
	}}
	h := middleware.BasicAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next ran despite forbidden")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("nonadmin/victim", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// ---- AdminAuth (Management) -------------------------------------------------

func TestAdminAuthNoCredentials(t *testing.T) {
	auth := &fakeAuth{
		basicFn:  func(context.Context, string, string) (*model.Principal, error) { return nil, service.ErrUnauthorized },
		bearerFn: func(context.Context, string) (*model.User, error) { return nil, service.ErrUnauthorized },
	}
	h := middleware.AdminAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next ran without credentials")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(wa, "Basic") || !strings.Contains(wa, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want combined Basic+Bearer challenge", wa)
	}
	assertJSONError(t, rec)
}

func TestAdminAuthBearerAdminOK(t *testing.T) {
	admin := &model.User{ID: uuid.New(), Login: "root", IsAdmin: true}
	auth := &fakeAuth{
		basicFn: func(context.Context, string, string) (*model.Principal, error) {
			t.Fatal("Basic must not be tried")
			return nil, nil
		},
		bearerFn: func(_ context.Context, tok string) (*model.User, error) {
			if tok != "good-token" {
				t.Fatalf("token = %q", tok)
			}
			return admin, nil
		},
	}
	var captured *model.Principal
	h := middleware.AdminAuth(auth)(principalSink(&captured))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if captured == nil || captured.Auth != admin || captured.Acting != admin {
		t.Fatalf("principal = %v, want Acting==Auth==admin", captured)
	}
}

func TestAdminAuthBearerNonAdminForbidden(t *testing.T) {
	user := &model.User{ID: uuid.New(), Login: "alice", IsAdmin: false}
	auth := &fakeAuth{
		bearerFn: func(context.Context, string) (*model.User, error) { return user, nil },
	}
	h := middleware.AdminAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next ran for a non-admin bearer")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer alice-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	assertJSONError(t, rec)
}

func TestAdminAuthBearerInvalidUnauthorized(t *testing.T) {
	auth := &fakeAuth{
		bearerFn: func(context.Context, string) (*model.User, error) { return nil, errors.New("nope") },
	}
	h := middleware.AdminAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next ran for an invalid bearer")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer bad")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminAuthBasicAdminOK(t *testing.T) {
	admin := &model.User{ID: uuid.New(), Login: "root", IsAdmin: true}
	auth := &fakeAuth{
		basicFn: func(context.Context, string, string) (*model.Principal, error) {
			return &model.Principal{Acting: admin, Auth: admin}, nil
		},
	}
	var captured *model.Principal
	h := middleware.AdminAuth(auth)(principalSink(&captured))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.SetBasicAuth("root", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if captured == nil || !captured.IsAdmin() {
		t.Fatalf("principal = %v, want admin", captured)
	}
}

func TestAdminAuthBasicNonAdminForbidden(t *testing.T) {
	user := &model.User{ID: uuid.New(), Login: "alice", IsAdmin: false}
	auth := &fakeAuth{
		basicFn: func(context.Context, string, string) (*model.Principal, error) {
			return &model.Principal{Acting: user, Auth: user}, nil
		},
	}
	h := middleware.AdminAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next ran for a non-admin")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.SetBasicAuth("alice", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAdminAuthBasicForbiddenFromService(t *testing.T) {
	// AuthenticateBasic itself returns ErrForbidden (e.g. non-admin impersonation
	// attempt) — must surface 403, not 401.
	auth := &fakeAuth{
		basicFn: func(context.Context, string, string) (*model.Principal, error) {
			return nil, service.ErrForbidden
		},
	}
	h := middleware.AdminAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next ran")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.SetBasicAuth("alice/victim", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAdminAuthBasicWrongPasswordUnauthorized(t *testing.T) {
	auth := &fakeAuth{
		basicFn: func(context.Context, string, string) (*model.Principal, error) {
			return nil, service.ErrUnauthorized
		},
	}
	h := middleware.AdminAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next ran")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.SetBasicAuth("root", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestAdminAuthBearerPreferredOverBasic verifies a Bearer header short-circuits
// before Basic credentials are even inspected.
func TestAdminAuthBearerPreferredOverBasic(t *testing.T) {
	admin := &model.User{ID: uuid.New(), IsAdmin: true}
	auth := &fakeAuth{
		basicFn: func(context.Context, string, string) (*model.Principal, error) {
			t.Fatal("Basic must not be consulted when Bearer present")
			return nil, nil
		},
		bearerFn: func(context.Context, string) (*model.User, error) { return admin, nil },
	}
	h := middleware.AdminAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	// A single Authorization header can carry only one scheme; a Bearer header is
	// resolved via the bearer path and Basic is never consulted.
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestBearerTokenParsing exercises the header-parsing edge cases by routing
// requests through AdminAuth: an empty / whitespace-only / case-variant Bearer
// header falls back to Basic (and, absent Basic, yields 401).
func TestBearerTokenParsing(t *testing.T) {
	bearerCalls := 0
	auth := &fakeAuth{
		basicFn: func(context.Context, string, string) (*model.Principal, error) { return nil, service.ErrUnauthorized },
		bearerFn: func(_ context.Context, tok string) (*model.User, error) {
			bearerCalls++
			return &model.User{IsAdmin: true}, nil
		},
	}

	cases := []struct {
		name       string
		header     string
		wantBearer bool // whether the bearer path should be taken
		wantStatus int
	}{
		{"empty bearer token", "Bearer ", false, http.StatusUnauthorized},
		{"whitespace-only token", "Bearer    ", false, http.StatusUnauthorized},
		{"too short header", "Bear", false, http.StatusUnauthorized},
		{"lowercase scheme accepted", "bearer abc", true, http.StatusOK},
		{"valid token", "Bearer abc", true, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bearerCalls = 0
			h := middleware.AdminAuth(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
			req.Header.Set("Authorization", c.header)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if (bearerCalls > 0) != c.wantBearer {
				t.Fatalf("bearer path taken = %v, want %v", bearerCalls > 0, c.wantBearer)
			}
		})
	}
}

// ---- Logging middleware -----------------------------------------------------

func TestLoggingEmitsOneRequestLine(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	h := middleware.Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/some/path", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 (logging must not alter the response)", rec.Code)
	}

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("log line is not valid JSON: %v\n%s", err, buf.String())
	}
	if line["msg"] != "http request" {
		t.Errorf("msg = %v, want %q", line["msg"], "http request")
	}
	if line["method"] != http.MethodGet {
		t.Errorf("method = %v, want GET", line["method"])
	}
	if line["path"] != "/some/path" {
		t.Errorf("path = %v, want /some/path", line["path"])
	}
	if status, _ := line["status"].(float64); int(status) != http.StatusTeapot {
		t.Errorf("logged status = %v, want 418", line["status"])
	}
}

// TestLoggingNilLoggerFallsBack verifies a nil logger does not panic (it falls
// back to slog.Default).
func TestLoggingNilLoggerFallsBack(t *testing.T) {
	h := middleware.Logging(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // must not panic
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// ---- helpers ----------------------------------------------------------------

func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("error body has no error message: %v", body)
	}
}
