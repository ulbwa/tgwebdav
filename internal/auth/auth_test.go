package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
)

// ---- in-memory fakes -------------------------------------------------------

type fakeUserRepo struct {
	byID    map[uuid.UUID]*domain.User
	byLogin map[string]*domain.User
}

func newFakeUserRepo() *fakeUserRepo {
	return &fakeUserRepo{
		byID:    map[uuid.UUID]*domain.User{},
		byLogin: map[string]*domain.User{},
	}
}

func (f *fakeUserRepo) add(u *domain.User) {
	f.byID[u.ID] = u
	f.byLogin[u.Login] = u
}

func (f *fakeUserRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeUserRepo) GetByLogin(_ context.Context, login string) (*domain.User, error) {
	if u, ok := f.byLogin[login]; ok {
		return u, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeUserRepo) Create(context.Context, *domain.User) error  { return errors.New("unused") }
func (f *fakeUserRepo) Update(context.Context, *domain.User) error  { return errors.New("unused") }
func (f *fakeUserRepo) Delete(context.Context, uuid.UUID) error     { return errors.New("unused") }
func (f *fakeUserRepo) List(context.Context) ([]domain.User, error) { return nil, errors.New("unused") }
func (f *fakeUserRepo) Count(context.Context) (int64, error)        { return 0, errors.New("unused") }

var _ domain.UserRepository = (*fakeUserRepo)(nil)

type fakeTokenRepo struct {
	byHash    map[string]*domain.APIToken
	touchedID uuid.UUID
	touchedAt time.Time
	touches   int
}

func newFakeTokenRepo() *fakeTokenRepo {
	return &fakeTokenRepo{byHash: map[string]*domain.APIToken{}}
}

func (f *fakeTokenRepo) GetByHash(_ context.Context, hash string) (*domain.APIToken, error) {
	if t, ok := f.byHash[hash]; ok {
		return t, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeTokenRepo) TouchLastUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	f.touchedID = id
	f.touchedAt = at
	f.touches++
	return nil
}

func (f *fakeTokenRepo) Create(context.Context, *domain.APIToken) error { return errors.New("unused") }
func (f *fakeTokenRepo) Delete(context.Context, uuid.UUID) error        { return errors.New("unused") }
func (f *fakeTokenRepo) ListByUser(context.Context, uuid.UUID) ([]domain.APIToken, error) {
	return nil, errors.New("unused")
}

var _ domain.APITokenRepository = (*fakeTokenRepo)(nil)

// mustUser builds a user with a freshly hashed password.
func mustUser(t *testing.T, login, password string, admin bool) *domain.User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return &domain.User{
		ID:           uuid.New(),
		Login:        login,
		PasswordHash: hash,
		IsAdmin:      admin,
		CreatedAt:    time.Now(),
	}
}

// ---- password hashing ------------------------------------------------------

func TestHashVerifyRoundtrip(t *testing.T) {
	const pw = "correct horse battery staple"
	encoded, err := HashPassword(pw)
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

	ok, err = VerifyPassword(encoded, "wrong password")
	if err != nil {
		t.Fatalf("VerifyPassword(wrong): %v", err)
	}
	if ok {
		t.Fatal("VerifyPassword returned true for the wrong password")
	}
}

func TestHashSaltsAreUnique(t *testing.T) {
	a, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password collided (salt not random)")
	}
}

func TestVerifyMalformedHash(t *testing.T) {
	for _, bad := range []string{
		"",
		"not-a-phc-string",
		"$argon2id$v=19$m=65536,t=1,p=4$onlyfourfields",
		"$bcrypt$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=99$m=65536,t=1,p=4$c2FsdA$aGFzaA",
	} {
		if _, err := VerifyPassword(bad, "x"); err == nil {
			t.Errorf("expected error for malformed hash %q", bad)
		}
	}
}

// ---- AuthenticateBasic: plain user ----------------------------------------

func TestAuthenticateBasicPlainUser(t *testing.T) {
	users := newFakeUserRepo()
	alice := mustUser(t, "alice", "s3cret", false)
	users.add(alice)

	svc := NewService(users, newFakeTokenRepo())

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

func TestAuthenticateBasicWrongPassword(t *testing.T) {
	users := newFakeUserRepo()
	users.add(mustUser(t, "alice", "s3cret", false))
	svc := NewService(users, newFakeTokenRepo())

	_, err := svc.AuthenticateBasic(context.Background(), "alice", "nope")
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestAuthenticateBasicUnknownUser(t *testing.T) {
	svc := NewService(newFakeUserRepo(), newFakeTokenRepo())

	_, err := svc.AuthenticateBasic(context.Background(), "ghost", "whatever")
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

// ---- AuthenticateBasic: impersonation -------------------------------------

func TestAuthenticateBasicImpersonationSuccess(t *testing.T) {
	users := newFakeUserRepo()
	admin := mustUser(t, "root", "adminpw", true)
	target := mustUser(t, "bob", "bobpw", false)
	users.add(admin)
	users.add(target)

	svc := NewService(users, newFakeTokenRepo())

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

func TestAuthenticateBasicImpersonationNonAdminForbidden(t *testing.T) {
	users := newFakeUserRepo()
	// Valid credentials but the actor is NOT an admin.
	users.add(mustUser(t, "carol", "carolpw", false))
	users.add(mustUser(t, "bob", "bobpw", false))

	svc := NewService(users, newFakeTokenRepo())

	_, err := svc.AuthenticateBasic(context.Background(), "carol/bob", "carolpw")
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestAuthenticateBasicImpersonationBadAdminPassword(t *testing.T) {
	users := newFakeUserRepo()
	users.add(mustUser(t, "root", "adminpw", true))
	users.add(mustUser(t, "bob", "bobpw", false))

	svc := NewService(users, newFakeTokenRepo())

	_, err := svc.AuthenticateBasic(context.Background(), "root/bob", "wrong")
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized for bad admin password, got %v", err)
	}
}

func TestAuthenticateBasicImpersonationUnknownTarget(t *testing.T) {
	users := newFakeUserRepo()
	users.add(mustUser(t, "root", "adminpw", true))

	svc := NewService(users, newFakeTokenRepo())

	_, err := svc.AuthenticateBasic(context.Background(), "root/ghost", "adminpw")
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized for unknown target, got %v", err)
	}
}

// ---- AuthenticateBearer ----------------------------------------------------

func TestAuthenticateBearerSuccessAndTouch(t *testing.T) {
	users := newFakeUserRepo()
	owner := mustUser(t, "svcuser", "x", false)
	users.add(owner)

	tokens := newFakeTokenRepo()
	const raw = "tok_abc123"
	// sha-256 hex of the raw token, as the repo would store it.
	hash := sha256Hex(raw)
	tok := &domain.APIToken{
		ID:        uuid.New(),
		UserID:    owner.ID,
		TokenHash: hash,
		Name:      "ci",
		CreatedAt: time.Now(),
	}
	tokens.byHash[hash] = tok

	fixedNow := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	svc := NewService(users, tokens)
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

func TestAuthenticateBearerUnknownToken(t *testing.T) {
	svc := NewService(newFakeUserRepo(), newFakeTokenRepo())

	_, err := svc.AuthenticateBearer(context.Background(), "does-not-exist")
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestAuthenticateBearerMissingOwner(t *testing.T) {
	tokens := newFakeTokenRepo()
	const raw = "orphan"
	hash := sha256Hex(raw)
	tokens.byHash[hash] = &domain.APIToken{
		ID:     uuid.New(),
		UserID: uuid.New(), // no such user
	}

	svc := NewService(newFakeUserRepo(), tokens)

	_, err := svc.AuthenticateBearer(context.Background(), raw)
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized for missing owner, got %v", err)
	}
}

// sha256Hex mirrors the digest the service computes, for test fixtures.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
