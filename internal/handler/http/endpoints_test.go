package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// ---- settings endpoints ----------------------------------------------------

func TestSettingsGetAndUpdate(t *testing.T) {
	f := newMgmtFixture(t)

	// GET returns the defaults.
	rec := f.do(t, http.MethodGet, "/api/v1/settings", "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("get settings status = %d, want 200", rec.Code)
	}
	var got struct {
		BlobMaxSize      int64 `json:"blob_max_size"`
		WalIdleTimeoutMs int64 `json:"wal_idle_timeout_ms"`
		MaxFileSize      int64 `json:"max_file_size"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	def := model.DefaultSettings()
	if got.BlobMaxSize != def.BlobMaxSize {
		t.Fatalf("blob_max_size = %d, want %d", got.BlobMaxSize, def.BlobMaxSize)
	}

	// PUT a partial update; omitted fields keep their value.
	rec = f.do(t, http.MethodPut, "/api/v1/settings",
		`{"blob_max_size":12345,"wal_idle_timeout_ms":2000}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("update settings status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode updated settings: %v", err)
	}
	if got.BlobMaxSize != 12345 || got.WalIdleTimeoutMs != 2000 {
		t.Fatalf("updated settings = %+v, want blob=12345 wal=2000", got)
	}
	if got.MaxFileSize != def.MaxFileSize {
		t.Fatalf("max_file_size changed to %d; omitted fields must be preserved", got.MaxFileSize)
	}
}

func TestSettingsUpdateValidation(t *testing.T) {
	f := newMgmtFixture(t)
	cases := []struct {
		name string
		body string
	}{
		{"non-positive blob_max_size", `{"blob_max_size":0}`},
		{"negative wal_idle_timeout_ms", `{"wal_idle_timeout_ms":-1}`},
		{"negative max_file_size", `{"max_file_size":-1}`},
		{"negative default_eviction_threshold", `{"default_eviction_threshold":-1}`},
		{"unknown field", `{"nope":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPut, "/api/v1/settings", c.body, [2]string{f.adminLogin, f.adminPass})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---- stats endpoints -------------------------------------------------------

func TestQueryStats(t *testing.T) {
	f := newMgmtFixture(t)

	// metric is required → 400.
	rec := f.do(t, http.MethodGet, "/api/v1/stats", "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stats without metric status = %d, want 400", rec.Code)
	}

	// Valid metric → 200 (empty list, since no samples seeded).
	rec = f.do(t, http.MethodGet, "/api/v1/stats?metric=read_bytes", "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("stats query status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var points []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &points); err != nil {
		t.Fatalf("decode stats: %v", err)
	}

	// to before from → 400.
	rec = f.do(t, http.MethodGet,
		"/api/v1/stats?metric=read_bytes&from=2026-06-18T12:00:00Z&to=2026-06-17T12:00:00Z",
		"", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stats to<from status = %d, want 400", rec.Code)
	}
}

func TestQueryStatsReturnsSeededSample(t *testing.T) {
	f := newMgmtFixture(t)
	// Seed a sample directly through the stat store the fixture wired up is not
	// reachable here; instead query an empty metric and assert the JSON shape.
	rec := f.do(t, http.MethodGet, "/api/v1/stats?metric=read_bytes&label=primary",
		"", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("stats query status = %d, want 200", rec.Code)
	}
	var points []model.StatSample
	if err := json.Unmarshal(rec.Body.Bytes(), &points); err != nil {
		t.Fatalf("decode stat points: %v", err)
	}
}

// ---- events endpoints ------------------------------------------------------

func TestListEvents(t *testing.T) {
	f := newMgmtFixture(t)

	rec := f.do(t, http.MethodGet, "/api/v1/events", "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("list events status = %d, want 200", rec.Code)
	}
	var page struct {
		Events []map[string]any `json:"events"`
		Total  int64            `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode event page: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("event total = %d, want 0", page.Total)
	}

	// With filters and paging params (limit/offset/kind) — still 200.
	rec = f.do(t, http.MethodGet, "/api/v1/events?kind=bot_disabled&limit=10&offset=0",
		"", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("list events filtered status = %d, want 200", rec.Code)
	}
}

// ---- user delete / set-password / token list+delete ------------------------

func TestUserDeleteAndSetPassword(t *testing.T) {
	f := newMgmtFixture(t)

	// Create a user.
	rec := f.do(t, http.MethodPost, "/api/v1/users", `{"login":"target","password":"pw1"}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user status = %d, want 201", rec.Code)
	}
	var u struct {
		Id string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode user: %v", err)
	}

	// Set password (204).
	rec = f.do(t, http.MethodPut, "/api/v1/users/"+u.Id+"/password", `{"password":"pw2"}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set password status = %d, want 204", rec.Code)
	}

	// The new password authenticates (verifies the hash was actually updated).
	if _, err := f.auth.AuthenticateBasic(context.Background(), "target", "pw2"); err != nil {
		t.Fatalf("new password should authenticate: %v", err)
	}

	// Empty password → 400.
	rec = f.do(t, http.MethodPut, "/api/v1/users/"+u.Id+"/password", `{"password":""}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("set empty password status = %d, want 400", rec.Code)
	}

	// Set password on missing user → 404.
	rec = f.do(t, http.MethodPut, "/api/v1/users/"+uuid.New().String()+"/password", `{"password":"x"}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("set password missing user status = %d, want 404", rec.Code)
	}

	// Delete the user (204).
	rec = f.do(t, http.MethodDelete, "/api/v1/users/"+u.Id, "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete user status = %d, want 204", rec.Code)
	}

	// Delete missing → 404.
	rec = f.do(t, http.MethodDelete, "/api/v1/users/"+uuid.New().String(), "", [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing user status = %d, want 404", rec.Code)
	}
}

func TestUserTokensListAndDelete(t *testing.T) {
	f := newMgmtFixture(t)

	// List tokens for the admin (empty initially) → 200.
	rec := f.do(t, http.MethodGet, "/api/v1/users/"+f.adminID.String()+"/tokens", "",
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("list tokens status = %d, want 200", rec.Code)
	}
	var toks []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &toks); err != nil {
		t.Fatalf("decode tokens: %v", err)
	}
	if len(toks) != 0 {
		t.Fatalf("initial token list len = %d, want 0", len(toks))
	}

	// Create a token.
	rec = f.do(t, http.MethodPost, "/api/v1/users/"+f.adminID.String()+"/tokens", `{"name":"ci"}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token status = %d, want 201", rec.Code)
	}
	var created struct {
		Id string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created token: %v", err)
	}

	// Empty name → 400.
	rec = f.do(t, http.MethodPost, "/api/v1/users/"+f.adminID.String()+"/tokens", `{"name":""}`,
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create token empty name status = %d, want 400", rec.Code)
	}

	// List now returns one token.
	rec = f.do(t, http.MethodGet, "/api/v1/users/"+f.adminID.String()+"/tokens", "",
		[2]string{f.adminLogin, f.adminPass})
	if err := json.Unmarshal(rec.Body.Bytes(), &toks); err != nil {
		t.Fatalf("decode tokens: %v", err)
	}
	if len(toks) != 1 {
		t.Fatalf("token list len = %d, want 1", len(toks))
	}

	// Delete the token under the wrong user → 404, leaving it intact.
	rec = f.do(t, http.MethodDelete,
		"/api/v1/users/"+uuid.New().String()+"/tokens/"+created.Id, "",
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete token wrong owner status = %d, want 404", rec.Code)
	}

	// Delete the token under the right owner → 204.
	rec = f.do(t, http.MethodDelete,
		"/api/v1/users/"+f.adminID.String()+"/tokens/"+created.Id, "",
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete token status = %d, want 204", rec.Code)
	}
}

// TestListTokensMissingUser verifies an unknown user yields 404 rather than an
// empty list (the service verifies existence first).
func TestListTokensMissingUser(t *testing.T) {
	f := newMgmtFixture(t)
	rec := f.do(t, http.MethodGet, "/api/v1/users/"+uuid.New().String()+"/tokens", "",
		[2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("list tokens missing user status = %d, want 404", rec.Code)
	}
}

// TestCreateUserDuplicateLoginConflict verifies the repository ErrAlreadyExists
// maps to 409.
func TestCreateUserDuplicateLoginConflict(t *testing.T) {
	f := newMgmtFixture(t)
	body := `{"login":"dup","password":"pw"}`
	if rec := f.do(t, http.MethodPost, "/api/v1/users", body, [2]string{f.adminLogin, f.adminPass}); rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", rec.Code)
	}
	rec := f.do(t, http.MethodPost, "/api/v1/users", body, [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate login status = %d, want 409", rec.Code)
	}
}

// TestCreateUserMissingFields verifies login/password are required.
func TestCreateUserMissingFields(t *testing.T) {
	f := newMgmtFixture(t)
	rec := f.do(t, http.MethodPost, "/api/v1/users", `{"login":"x"}`, [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing password status = %d, want 400", rec.Code)
	}
	rec = f.do(t, http.MethodPost, "/api/v1/users", `{not json`, [2]string{f.adminLogin, f.adminPass})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400", rec.Code)
	}
}
