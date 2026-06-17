package http_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	canonhttp "github.com/ulbwa/tgwebdav/internal/handler/http"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/service"
	"github.com/ulbwa/tgwebdav/internal/service/webdavfs"
)

// ---- in-memory webdavfs store ----------------------------------------------

// memFSStore is a single in-memory store satisfying every dependency interface
// the webdavfs.FileSystem requires: nodes, extents, WAL and blob refcounts. It
// is enough to round-trip PUT/GET/PROPFIND and to exercise the quota path.
type memFSStore struct {
	mu       sync.Mutex
	nodes    map[uuid.UUID]*model.Node
	extents  map[uuid.UUID][]model.Extent
	wal      map[uuid.UUID][]model.WALChunk
	refcount map[uuid.UUID]int64
}

func newMemFSStore() *memFSStore {
	return &memFSStore{
		nodes:    map[uuid.UUID]*model.Node{},
		extents:  map[uuid.UUID][]model.Extent{},
		wal:      map[uuid.UUID][]model.WALChunk{},
		refcount: map[uuid.UUID]int64{},
	}
}

// nodeStore.

func (s *memFSStore) Create(_ context.Context, n *model.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ex := range s.nodes {
		if ex.UserID == n.UserID && ex.Path == n.Path {
			return model.ErrAlreadyExists
		}
	}
	cp := *n
	s.nodes[n.ID] = &cp
	return nil
}

func (s *memFSStore) Update(_ context.Context, n *model.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[n.ID]; !ok {
		return model.ErrNotFound
	}
	cp := *n
	s.nodes[n.ID] = &cp
	return nil
}

func (s *memFSStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.nodes[id]
	if !ok {
		return model.ErrNotFound
	}
	prefix := node.Path
	for nid, n := range s.nodes {
		if n.UserID == node.UserID && (n.Path == prefix || strings.HasPrefix(n.Path, prefix+"/")) {
			delete(s.nodes, nid)
			delete(s.extents, nid)
			delete(s.wal, nid)
		}
	}
	return nil
}

func (s *memFSStore) GetByID(_ context.Context, id uuid.UUID) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return nil, model.ErrNotFound
	}
	cp := *n
	return &cp, nil
}

func (s *memFSStore) GetByPath(_ context.Context, userID uuid.UUID, p string) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.nodes {
		if n.UserID == userID && n.Path == p {
			cp := *n
			return &cp, nil
		}
	}
	return nil, model.ErrNotFound
}

func (s *memFSStore) ListChildren(_ context.Context, userID, parentID uuid.UUID) ([]model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Node
	for _, n := range s.nodes {
		if n.UserID == userID && n.ParentID != nil && *n.ParentID == parentID {
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *memFSStore) ListSubtree(_ context.Context, userID uuid.UUID, prefix string) ([]model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Node
	for _, n := range s.nodes {
		if n.UserID == userID && (n.Path == prefix || strings.HasPrefix(n.Path, prefix+"/")) {
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *memFSStore) SumSizeByUser(_ context.Context, userID uuid.UUID) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, n := range s.nodes {
		if n.UserID == userID && !n.IsDir {
			sum += n.Size
		}
	}
	return sum, nil
}

// extentStore.

func (s *memFSStore) ListByNode(_ context.Context, nodeID uuid.UUID) ([]model.Extent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Extent(nil), s.extents[nodeID]...), nil
}

func (s *memFSStore) DeleteByNode(_ context.Context, nodeID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.extents, nodeID)
	delete(s.wal, nodeID)
	return nil
}

func (s *memFSStore) CopyForNode(_ context.Context, srcNodeID, dstNodeID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.extents[srcNodeID]
	dst := make([]model.Extent, len(src))
	for i, e := range src {
		e.ID = uuid.New()
		e.NodeID = dstNodeID
		dst[i] = e
	}
	s.extents[dstNodeID] = dst
	return nil
}

// walStore.

func (s *memFSStore) AppendChunk(_ context.Context, c *model.WALChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wal[c.NodeID] = append(s.wal[c.NodeID], *c)
	return nil
}

func (s *memFSStore) ReadRange(_ context.Context, nodeID uuid.UUID, offset, length int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chunks := append([]model.WALChunk(nil), s.wal[nodeID]...)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Seq < chunks[j].Seq })
	var all []byte
	for _, c := range chunks {
		all = append(all, c.Data...)
	}
	if offset >= int64(len(all)) {
		return []byte{}, nil
	}
	end := offset + length
	if end > int64(len(all)) {
		end = int64(len(all))
	}
	return append([]byte(nil), all[offset:end]...), nil
}

// blobStore.

func (s *memFSStore) AddRefcount(_ context.Context, id uuid.UUID, delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refcount[id] += delta
	return nil
}

// ---- other webdavfs dependency fakes ---------------------------------------

type fsTx struct{}

func (fsTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

type fsBlobReader struct{}

func (fsBlobReader) ReadBlob(context.Context, uuid.UUID) ([]byte, error) { return nil, nil }
func (fsBlobReader) Prefetch(context.Context, []uuid.UUID)               {}

type fsSettings struct{}

func (fsSettings) Get(context.Context) (model.Settings, error) { return model.DefaultSettings(), nil }

type fsStats struct{}

func (fsStats) AddReadBytes(int64) {}
func (fsStats) IncReadOps()        {}
func (fsStats) IncWriteOps()       {}

// ---- harness ---------------------------------------------------------------

type davFixture struct {
	handler http.Handler
	login   string
	pass    string
}

// newDavFixture builds a real webdavfs.FileSystem over the in-memory store and a
// real WebDAV handler, seeding a single user with the given quota (0 = unlimited).
func newDavFixture(t *testing.T, quotaBytes int64) *davFixture {
	t.Helper()

	store := newMemFSStore()
	users := newMemUserStore()
	tokens := newMemTokenStore()

	auth := service.NewAuthService(users, tokens)
	limiter := service.NewLimiter()

	// Seed a user directly with a known password hash.
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
		CreatedAt:    time.Now().UTC(),
	}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	fs := webdavfs.NewFileSystem(
		store, store, store, store, // nodes, extents, wal, blobMeta
		fsTx{},
		fsBlobReader{},
		limiter,
		fsSettings{},
		fsStats{},
		nil,
	)

	h := canonhttp.NewWebDAVHandler(canonhttp.WebDAVDeps{
		FS:      fs,
		Auth:    auth,
		Limiter: limiter,
		Logger:  nil,
	})

	return &davFixture{handler: h, login: "dav", pass: pass}
}

func (f *davFixture) req(t *testing.T, method, target, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if auth {
		req.SetBasicAuth(f.login, f.pass)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// ---- tests -----------------------------------------------------------------

func TestWebDAVRequiresAuth(t *testing.T) {
	f := newDavFixture(t, 0)
	rec := f.req(t, "OPTIONS", "/", "", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated OPTIONS status = %d, want 401", rec.Code)
	}
	if wa := rec.Header().Get("WWW-Authenticate"); !strings.Contains(wa, "Basic") {
		t.Fatalf("WWW-Authenticate = %q, want Basic challenge", wa)
	}
}

func TestWebDAVOptionsAdvertisesDAV(t *testing.T) {
	f := newDavFixture(t, 0)
	rec := f.req(t, "OPTIONS", "/", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS status = %d, want 200", rec.Code)
	}
	if dav := rec.Header().Get("DAV"); dav == "" {
		t.Fatal("OPTIONS response missing DAV header")
	}
}

func TestWebDAVPutGetRoundTrip(t *testing.T) {
	f := newDavFixture(t, 0)
	const content = "hello webdav world"

	rec := f.req(t, http.MethodPut, "/file.txt", content, true)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 201 or 204", rec.Code)
	}

	rec = f.req(t, http.MethodGet, "/file.txt", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	got, _ := io.ReadAll(rec.Body)
	if string(got) != content {
		t.Fatalf("GET body = %q, want %q", got, content)
	}
}

func TestWebDAVPropfind(t *testing.T) {
	f := newDavFixture(t, 0)
	// Seed a file so the listing is non-trivial.
	if rec := f.req(t, http.MethodPut, "/a.txt", "abc", true); rec.Code >= 300 {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}

	req := httptest.NewRequest("PROPFIND", "/", strings.NewReader(""))
	req.Header.Set("Depth", "1")
	req.SetBasicAuth(f.login, f.pass)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND status = %d, want 207", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "a.txt") {
		t.Fatalf("PROPFIND body does not list a.txt: %s", rec.Body.String())
	}
}

func TestWebDAVQuotaExceeded507(t *testing.T) {
	// Quota of 4 bytes; a 10-byte PUT must be rejected with 507 by the pre-check.
	f := newDavFixture(t, 4)
	rec := f.req(t, http.MethodPut, "/big.txt", "0123456789", true)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-quota PUT status = %d, want 507", rec.Code)
	}
}
