package http_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	canonhttp "github.com/ulbwa/tgwebdav/internal/handler/http"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/service"
	"github.com/ulbwa/tgwebdav/internal/service/webdavfs"
)

// newDavFixtureRate is like newDavFixture but seeds the user's per-minute request
// rate (for exercising the 429 path) in addition to the quota.
func newDavFixtureRate(t *testing.T, quotaBytes int64, ratePerMin int) *davFixture {
	t.Helper()

	store := newMemFSStore()
	users := newMemUserStore()
	tokens := newMemTokenStore()

	auth := service.NewAuthService(users, tokens)
	limiter := service.NewLimiter()

	const pass = "pw-dav"
	hash, err := auth.HashPassword(pass)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	u := &model.User{
		ID:           uuid.New(),
		Login:        "dav",
		PasswordHash: hash,
		QuotaBytes:   quotaBytes,
		RatePerMin:   ratePerMin,
		CreatedAt:    time.Now().UTC(),
	}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	fs := webdavfs.NewFileSystem(
		store, store, store, store,
		fsTx{}, fsBlobReader{}, limiter, fsSettings{}, fsStats{}, nil,
	)
	h := canonhttp.NewWebDAVHandler(canonhttp.WebDAVDeps{FS: fs, Auth: auth, Limiter: limiter, Logger: nil})
	return &davFixture{handler: h, login: "dav", pass: pass}
}

// copyReq issues a COPY with the given Destination/Overwrite/Depth headers.
func (f *davFixture) copyReq(t *testing.T, src, dest, overwrite, depth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("COPY", src, nil)
	req.Header.Set("Destination", dest)
	if overwrite != "" {
		req.Header.Set("Overwrite", overwrite)
	}
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	req.SetBasicAuth(f.login, f.pass)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestWebDAVCopyCreatesAndOverwrites(t *testing.T) {
	f := newDavFixture(t, 0)

	// Seed a source file.
	if rec := f.req(t, http.MethodPut, "/src.txt", "payload", true); rec.Code >= 300 {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}

	// COPY to a new path → 201 Created.
	rec := f.copyReq(t, "/src.txt", "http://x/dst.txt", "", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("COPY new dest status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	// The copy is independently readable.
	if got := f.req(t, http.MethodGet, "/dst.txt", "", true); got.Code != http.StatusOK {
		t.Fatalf("GET copy status = %d, want 200", got.Code)
	}

	// COPY over an existing dest (overwrite default true) → 204 No Content.
	rec = f.copyReq(t, "/src.txt", "http://x/dst.txt", "", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("COPY existing dest status = %d, want 204", rec.Code)
	}

	// COPY over an existing dest with Overwrite: F → 412 Precondition Failed.
	rec = f.copyReq(t, "/src.txt", "http://x/dst.txt", "F", "")
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("COPY overwrite=F status = %d, want 412", rec.Code)
	}
}

func TestWebDAVCopyMissingSourceIs404(t *testing.T) {
	f := newDavFixture(t, 0)
	rec := f.copyReq(t, "/nope.txt", "http://x/dst.txt", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("COPY missing source status = %d, want 404", rec.Code)
	}
}

func TestWebDAVCopyMissingDestinationHeaderIs400(t *testing.T) {
	f := newDavFixture(t, 0)
	if rec := f.req(t, http.MethodPut, "/s.txt", "x", true); rec.Code >= 300 {
		t.Fatalf("seed PUT: %d", rec.Code)
	}
	req := httptest.NewRequest("COPY", "/s.txt", nil) // no Destination
	req.SetBasicAuth(f.login, f.pass)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("COPY without Destination status = %d, want 400", rec.Code)
	}
}

func TestWebDAVCopyQuota507(t *testing.T) {
	// Quota 10 bytes: a 7-byte file fits; copying it (another 7) overflows.
	f := newDavFixture(t, 10)
	if rec := f.req(t, http.MethodPut, "/a.txt", "1234567", true); rec.Code >= 300 {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}
	rec := f.copyReq(t, "/a.txt", "http://x/b.txt", "", "")
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("COPY over quota status = %d, want 507", rec.Code)
	}
}

func TestWebDAVCopyDirectoryRecursive(t *testing.T) {
	f := newDavFixture(t, 0)
	// Build /dir/inner.txt.
	if rec := f.req(t, "MKCOL", "/dir", "", true); rec.Code >= 300 {
		t.Fatalf("MKCOL status = %d", rec.Code)
	}
	if rec := f.req(t, http.MethodPut, "/dir/inner.txt", "inside", true); rec.Code >= 300 {
		t.Fatalf("seed inner PUT status = %d", rec.Code)
	}
	// COPY the directory with Depth (default → recursive).
	rec := f.copyReq(t, "/dir", "http://x/dir2", "", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("COPY dir status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	// The recursively copied child is readable at its new path.
	if got := f.req(t, http.MethodGet, "/dir2/inner.txt", "", true); got.Code != http.StatusOK {
		t.Fatalf("GET recursively-copied child status = %d, want 200", got.Code)
	}
}

func TestWebDAVMove(t *testing.T) {
	f := newDavFixture(t, 0)
	if rec := f.req(t, http.MethodPut, "/old.txt", "content", true); rec.Code >= 300 {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}
	req := httptest.NewRequest("MOVE", "/old.txt", nil)
	// x/net/webdav validates the Destination host against the request host, so it
	// must match (httptest sets the request host to example.com).
	req.Header.Set("Destination", "http://example.com/new.txt")
	req.SetBasicAuth(f.login, f.pass)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("MOVE status = %d, want 201/204 (body=%s)", rec.Code, rec.Body.String())
	}
	// Old path gone, new path present.
	if got := f.req(t, http.MethodGet, "/old.txt", "", true); got.Code != http.StatusNotFound {
		t.Fatalf("GET moved-from status = %d, want 404", got.Code)
	}
	got := f.req(t, http.MethodGet, "/new.txt", "", true)
	if got.Code != http.StatusOK {
		t.Fatalf("GET moved-to status = %d, want 200", got.Code)
	}
	if body, _ := io.ReadAll(got.Body); string(body) != "content" {
		t.Fatalf("moved body = %q, want content", body)
	}
}

func TestWebDAVDelete(t *testing.T) {
	f := newDavFixture(t, 0)
	if rec := f.req(t, http.MethodPut, "/gone.txt", "bye", true); rec.Code >= 300 {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}
	rec := f.req(t, http.MethodDelete, "/gone.txt", "", true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", rec.Code)
	}
	if got := f.req(t, http.MethodGet, "/gone.txt", "", true); got.Code != http.StatusNotFound {
		t.Fatalf("GET deleted status = %d, want 404", got.Code)
	}
}

func TestWebDAVGetRange(t *testing.T) {
	f := newDavFixture(t, 0)
	const content = "0123456789ABCDEF"
	if rec := f.req(t, http.MethodPut, "/data.bin", content, true); rec.Code >= 300 {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/data.bin", nil)
	req.Header.Set("Range", "bytes=4-9")
	req.SetBasicAuth(f.login, f.pass)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("ranged GET status = %d, want 206 (body=%s)", rec.Code, rec.Body.String())
	}
	if body, _ := io.ReadAll(rec.Body); string(body) != content[4:10] {
		t.Fatalf("range body = %q, want %q", body, content[4:10])
	}
}

func TestWebDAVRateLimited429(t *testing.T) {
	// rate 1/min: the first request passes, the immediate second is throttled.
	f := newDavFixtureRate(t, 0, 1)

	if rec := f.req(t, "OPTIONS", "/", "", true); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	rec := f.req(t, "OPTIONS", "/", "", true)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 response missing Retry-After header")
	}
}

// TestWebDAVImpersonationForbidden verifies a non-admin attempting Basic
// "admin/target" impersonation is rejected with 403 by the auth middleware.
func TestWebDAVImpersonationForbidden(t *testing.T) {
	f := newDavFixture(t, 0)
	// "dav" is not an admin; "dav/victim" must be forbidden.
	req := httptest.NewRequest("OPTIONS", "/", nil)
	req.SetBasicAuth(f.login+"/victim", f.pass)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin impersonation status = %d, want 403", rec.Code)
	}
}
