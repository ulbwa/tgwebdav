package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/model"
)

// ---- in-memory fakes -------------------------------------------------------

type fakeUserStore struct {
	byID    map[uuid.UUID]*model.User
	byLogin map[string]*model.User
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		byID:    map[uuid.UUID]*model.User{},
		byLogin: map[string]*model.User{},
	}
}

func (f *fakeUserStore) add(u *model.User) {
	f.byID[u.ID] = u
	f.byLogin[u.Login] = u
}

func (f *fakeUserStore) GetByID(_ context.Context, id uuid.UUID) (*model.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, model.ErrNotFound
}

func (f *fakeUserStore) GetByLogin(_ context.Context, login string) (*model.User, error) {
	if u, ok := f.byLogin[login]; ok {
		return u, nil
	}
	return nil, model.ErrNotFound
}

// compile-time assertion the fake satisfies the interface.
var _ userStore = (*fakeUserStore)(nil)

type fakeTokenStore struct {
	byHash    map[string]*model.APIToken
	touchedID uuid.UUID
	touchedAt time.Time
	touches   int
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{byHash: map[string]*model.APIToken{}}
}

func (f *fakeTokenStore) GetByHash(_ context.Context, hash string) (*model.APIToken, error) {
	if t, ok := f.byHash[hash]; ok {
		return t, nil
	}
	return nil, model.ErrNotFound
}

func (f *fakeTokenStore) TouchLastUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	f.touchedID = id
	f.touchedAt = at
	f.touches++
	return nil
}

var _ tokenStore = (*fakeTokenStore)(nil)

// mustUser builds a user with a freshly hashed password.
func mustUser(t *testing.T, login, password string, admin bool) *model.User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return &model.User{
		ID:           uuid.New(),
		Login:        login,
		PasswordHash: hash,
		IsAdmin:      admin,
		CreatedAt:    time.Now(),
	}
}

// sha256Hex mirrors the digest the service computes, for test fixtures.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---- HashPassword round-trip -----------------------------------------------

func TestAuthHashVerifyRoundtrip(t *testing.T) {
	svc := NewAuthService(newFakeUserStore(), newFakeTokenStore())
	const pw = "correct horse battery staple"

	encoded, err := svc.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=1,p=4$") {
		t.Fatalf("unexpected PHC prefix: %q", encoded)
	}

	ok, err := VerifyPassword(encoded, pw)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("VerifyPassword returned false for the correct password")
	}

	ok, err = VerifyPassword(encoded, "wrong")
	if err != nil {
		t.Fatalf("VerifyPassword(wrong): %v", err)
	}
	if ok {
		t.Fatal("VerifyPassword returned true for the wrong password")
	}
}

// ---- AuthenticateBasic: plain user -----------------------------------------

func TestAuthBasicPlainSuccess(t *testing.T) {
	users := newFakeUserStore()
	alice := mustUser(t, "alice", "s3cret", false)
	users.add(alice)

	svc := NewAuthService(users, newFakeTokenStore())

	p, err := svc.AuthenticateBasic(context.Background(), "alice", "s3cret")
	if err != nil {
		t.Fatalf("AuthenticateBasic: %v", err)
	}
	if p.Acting.ID != alice.ID || p.Auth.ID != alice.ID {
		t.Fatal("plain auth should set Acting == Auth == the user")
	}
	if p.Impersonating() {
		t.Fatal("plain auth must not be impersonating")
	}
}

func TestAuthBasicWrongPassword(t *testing.T) {
	users := newFakeUserStore()
	users.add(mustUser(t, "alice", "s3cret", false))
	svc := NewAuthService(users, newFakeTokenStore())

	_, err := svc.AuthenticateBasic(context.Background(), "alice", "nope")
	if !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestAuthBasicUnknownUser(t *testing.T) {
	svc := NewAuthService(newFakeUserStore(), newFakeTokenStore())

	_, err := svc.AuthenticateBasic(context.Background(), "ghost", "whatever")
	if !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

// ---- AuthenticateBasic: impersonation --------------------------------------

func TestAuthBasicImpersonationSuccess(t *testing.T) {
	users := newFakeUserStore()
	admin := mustUser(t, "root", "adminpw", true)
	target := mustUser(t, "bob", "bobpw", false)
	users.add(admin)
	users.add(target)

	svc := NewAuthService(users, newFakeTokenStore())

	p, err := svc.AuthenticateBasic(context.Background(), "root/bob", "adminpw")
	if err != nil {
		t.Fatalf("AuthenticateBasic(impersonation): %v", err)
	}
	if p.Acting.ID != target.ID {
		t.Fatalf("Acting should be target bob, got %s", p.Acting.Login)
	}
	if p.Auth.ID != admin.ID {
		t.Fatalf("Auth should be admin root, got %s", p.Auth.Login)
	}
	if !p.IsAdmin() {
		t.Fatal("principal should report IsAdmin from the admin auth identity")
	}
	if !p.Impersonating() {
		t.Fatal("principal should report Impersonating")
	}
}

func TestAuthBasicImpersonationNonAdminForbidden(t *testing.T) {
	users := newFakeUserStore()
	users.add(mustUser(t, "carol", "carolpw", false))
	users.add(mustUser(t, "bob", "bobpw", false))

	svc := NewAuthService(users, newFakeTokenStore())

	_, err := svc.AuthenticateBasic(context.Background(), "carol/bob", "carolpw")
	if !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestAuthBasicImpersonationBadAdminPassword(t *testing.T) {
	users := newFakeUserStore()
	users.add(mustUser(t, "root", "adminpw", true))
	users.add(mustUser(t, "bob", "bobpw", false))

	svc := NewAuthService(users, newFakeTokenStore())

	_, err := svc.AuthenticateBasic(context.Background(), "root/bob", "wrong")
	if !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized for bad admin password, got %v", err)
	}
}

func TestAuthBasicImpersonationUnknownTarget(t *testing.T) {
	users := newFakeUserStore()
	users.add(mustUser(t, "root", "adminpw", true))

	svc := NewAuthService(users, newFakeTokenStore())

	_, err := svc.AuthenticateBasic(context.Background(), "root/ghost", "adminpw")
	if !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized for unknown target, got %v", err)
	}
}

// ---- AuthenticateBearer ----------------------------------------------------

func TestAuthBearerSuccessAndTouch(t *testing.T) {
	users := newFakeUserStore()
	owner := mustUser(t, "svcuser", "x", false)
	users.add(owner)

	tokens := newFakeTokenStore()
	const raw = "tok_abc123"
	hash := sha256Hex(raw)
	tok := &model.APIToken{
		ID:        uuid.New(),
		UserID:    owner.ID,
		TokenHash: hash,
		Name:      "ci",
		CreatedAt: time.Now(),
	}
	tokens.byHash[hash] = tok

	fixedNow := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	svc := NewAuthService(users, tokens)
	svc.now = func() time.Time { return fixedNow }

	got, err := svc.AuthenticateBearer(context.Background(), raw)
	if err != nil {
		t.Fatalf("AuthenticateBearer: %v", err)
	}
	if got.ID != owner.ID {
		t.Fatalf("returned wrong user: %s", got.Login)
	}
	if tokens.touches != 1 {
		t.Fatalf("expected exactly one TouchLastUsed, got %d", tokens.touches)
	}
	if tokens.touchedID != tok.ID {
		t.Fatal("touched the wrong token id")
	}
	if !tokens.touchedAt.Equal(fixedNow) {
		t.Fatalf("touched with wrong time: %v", tokens.touchedAt)
	}
}

func TestAuthBearerUnknownToken(t *testing.T) {
	svc := NewAuthService(newFakeUserStore(), newFakeTokenStore())

	_, err := svc.AuthenticateBearer(context.Background(), "does-not-exist")
	if !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestAuthBearerMissingOwner(t *testing.T) {
	tokens := newFakeTokenStore()
	const raw = "orphan"
	hash := sha256Hex(raw)
	tokens.byHash[hash] = &model.APIToken{
		ID:     uuid.New(),
		UserID: uuid.New(), // no such user
	}

	svc := NewAuthService(newFakeUserStore(), tokens)

	_, err := svc.AuthenticateBearer(context.Background(), raw)
	if !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized for missing owner, got %v", err)
	}
}
