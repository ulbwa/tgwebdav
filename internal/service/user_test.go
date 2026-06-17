package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// fakeUserRepo is an in-memory userRepo keyed by id, with a login uniqueness
// check mirroring the real repository's unique constraint.
type fakeUserRepo struct {
	byID map[uuid.UUID]*model.User
}

func newFakeUserRepo() *fakeUserRepo {
	return &fakeUserRepo{byID: make(map[uuid.UUID]*model.User)}
}

func (f *fakeUserRepo) Create(_ context.Context, u *model.User) error {
	for _, existing := range f.byID {
		if existing.Login == u.Login {
			return model.ErrAlreadyExists
		}
	}
	cp := *u
	f.byID[u.ID] = &cp
	return nil
}

func (f *fakeUserRepo) Update(_ context.Context, u *model.User) error {
	if _, ok := f.byID[u.ID]; !ok {
		return model.ErrNotFound
	}
	cp := *u
	f.byID[u.ID] = &cp
	return nil
}

func (f *fakeUserRepo) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := f.byID[id]; !ok {
		return model.ErrNotFound
	}
	delete(f.byID, id)
	return nil
}

func (f *fakeUserRepo) GetByID(_ context.Context, id uuid.UUID) (*model.User, error) {
	u, ok := f.byID[id]
	if !ok {
		return nil, model.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (f *fakeUserRepo) List(_ context.Context) ([]model.User, error) {
	out := make([]model.User, 0, len(f.byID))
	for _, u := range f.byID {
		out = append(out, *u)
	}
	return out, nil
}

var _ userRepo = (*fakeUserRepo)(nil)

// fakeTokenRepo is an in-memory tokenRepo keyed by id.
type fakeTokenRepo struct {
	byID map[uuid.UUID]*model.APIToken
}

func newFakeTokenRepo() *fakeTokenRepo {
	return &fakeTokenRepo{byID: make(map[uuid.UUID]*model.APIToken)}
}

func (f *fakeTokenRepo) Create(_ context.Context, t *model.APIToken) error {
	cp := *t
	f.byID[t.ID] = &cp
	return nil
}

func (f *fakeTokenRepo) ListByUser(_ context.Context, userID uuid.UUID) ([]model.APIToken, error) {
	out := make([]model.APIToken, 0)
	for _, t := range f.byID {
		if t.UserID == userID {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (f *fakeTokenRepo) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := f.byID[id]; !ok {
		return model.ErrNotFound
	}
	delete(f.byID, id)
	return nil
}

var _ tokenRepo = (*fakeTokenRepo)(nil)

func newTestUserService() (*UserService, *fakeUserRepo, *fakeTokenRepo) {
	users := newFakeUserRepo()
	tokens := newFakeTokenRepo()
	return NewUserService(users, tokens), users, tokens
}

func TestUserCreateHashesPasswordAndPersistsFields(t *testing.T) {
	svc, users, _ := newTestUserService()

	u, err := svc.Create(context.Background(), CreateUserParams{
		Login:        "alice",
		Password:     "s3cr3t",
		IsAdmin:      true,
		QuotaBytes:   1000,
		BandwidthBPS: 200,
		RatePerMin:   30,
	})
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}

	if u.ID == uuid.Nil {
		t.Error("Create: expected a generated id")
	}
	if u.CreatedAt.IsZero() {
		t.Error("Create: expected CreatedAt to be set")
	}
	if u.Login != "alice" || !u.IsAdmin || u.QuotaBytes != 1000 || u.BandwidthBPS != 200 || u.RatePerMin != 30 {
		t.Errorf("Create: fields not persisted: %+v", u)
	}

	// Password must be stored as a verifiable argon2id hash, never plaintext.
	if u.PasswordHash == "" || u.PasswordHash == "s3cr3t" {
		t.Fatalf("Create: password hash looks wrong: %q", u.PasswordHash)
	}
	ok, err := VerifyPassword(u.PasswordHash, "s3cr3t")
	if err != nil {
		t.Fatalf("VerifyPassword: unexpected error: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword: stored hash does not verify the original password")
	}
	if ok, _ := VerifyPassword(u.PasswordHash, "wrong"); ok {
		t.Error("VerifyPassword: wrong password unexpectedly verified")
	}

	// And it must actually be persisted in the repo.
	if _, ok := users.byID[u.ID]; !ok {
		t.Error("Create: user not persisted in repo")
	}
}

func TestUserCreateDuplicateLoginIsAlreadyExists(t *testing.T) {
	svc, _, _ := newTestUserService()
	ctx := context.Background()

	if _, err := svc.Create(ctx, CreateUserParams{Login: "bob", Password: "pw"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := svc.Create(ctx, CreateUserParams{Login: "bob", Password: "pw2"})
	if !errors.Is(err, model.ErrAlreadyExists) {
		t.Fatalf("duplicate Create: expected ErrAlreadyExists, got %v", err)
	}
}

func TestUserGetListDelete(t *testing.T) {
	svc, _, _ := newTestUserService()
	ctx := context.Background()

	u, err := svc.Create(ctx, CreateUserParams{Login: "carol", Password: "pw"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Login != "carol" {
		t.Errorf("Get: got login %q, want carol", got.Login)
	}

	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List: got %d users, want 1", len(list))
	}

	if err := svc.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, u.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Get after delete: expected ErrNotFound, got %v", err)
	}
}

func TestUserGetMissingIsNotFound(t *testing.T) {
	svc, _, _ := newTestUserService()
	if _, err := svc.Get(context.Background(), uuid.New()); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Get missing: expected ErrNotFound, got %v", err)
	}
}

func TestUserSetPasswordRehashesAndUpdates(t *testing.T) {
	svc, _, _ := newTestUserService()
	ctx := context.Background()

	u, err := svc.Create(ctx, CreateUserParams{Login: "dave", Password: "old"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldHash := u.PasswordHash

	if err := svc.SetPassword(ctx, u.ID, "new"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	updated, err := svc.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.PasswordHash == oldHash {
		t.Error("SetPassword: hash unchanged")
	}
	if ok, _ := VerifyPassword(updated.PasswordHash, "new"); !ok {
		t.Error("SetPassword: new password does not verify")
	}
	if ok, _ := VerifyPassword(updated.PasswordHash, "old"); ok {
		t.Error("SetPassword: old password still verifies")
	}
}

func TestUserSetPasswordMissingIsNotFound(t *testing.T) {
	svc, _, _ := newTestUserService()
	if err := svc.SetPassword(context.Background(), uuid.New(), "x"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("SetPassword missing: expected ErrNotFound, got %v", err)
	}
}

func TestUserCreateTokenReturnsPlaintextAndStoresHash(t *testing.T) {
	svc, _, tokens := newTestUserService()
	ctx := context.Background()

	u, err := svc.Create(ctx, CreateUserParams{Login: "erin", Password: "pw"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	plaintext, tok, err := svc.CreateToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if plaintext == "" {
		t.Fatal("CreateToken: empty plaintext")
	}
	if tok.Name != "laptop" || tok.UserID != u.ID {
		t.Errorf("CreateToken: token fields wrong: %+v", tok)
	}
	// Stored hash must match the AuthenticateBearer scheme (sha256 hex of the
	// plaintext) and must not equal the plaintext.
	if tok.TokenHash == plaintext {
		t.Error("CreateToken: stored the plaintext instead of a hash")
	}
	if tok.TokenHash != hashAPIToken(plaintext) {
		t.Errorf("CreateToken: stored hash %q does not match sha256(plaintext)", tok.TokenHash)
	}
	stored, ok := tokens.byID[tok.ID]
	if !ok {
		t.Fatal("CreateToken: token not persisted")
	}
	if stored.TokenHash != hashAPIToken(plaintext) {
		t.Error("CreateToken: persisted hash mismatch")
	}
}

func TestUserCreateTokenMissingUserIsNotFound(t *testing.T) {
	svc, _, _ := newTestUserService()
	if _, _, err := svc.CreateToken(context.Background(), uuid.New(), "x"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("CreateToken missing user: expected ErrNotFound, got %v", err)
	}
}

func TestUserListAndDeleteTokens(t *testing.T) {
	svc, _, _ := newTestUserService()
	ctx := context.Background()

	u, err := svc.Create(ctx, CreateUserParams{Login: "frank", Password: "pw"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, t1, err := svc.CreateToken(ctx, u.ID, "a")
	if err != nil {
		t.Fatalf("CreateToken a: %v", err)
	}
	if _, _, err := svc.CreateToken(ctx, u.ID, "b"); err != nil {
		t.Fatalf("CreateToken b: %v", err)
	}

	list, err := svc.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListTokens: got %d, want 2", len(list))
	}

	if err := svc.DeleteToken(ctx, u.ID, t1.ID); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	list, err = svc.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListTokens after delete: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListTokens after delete: got %d, want 1", len(list))
	}
}

func TestUserListTokensMissingUserIsNotFound(t *testing.T) {
	svc, _, _ := newTestUserService()
	if _, err := svc.ListTokens(context.Background(), uuid.New()); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("ListTokens missing user: expected ErrNotFound, got %v", err)
	}
}

func TestUserDeleteTokenWrongOwnerIsNotFound(t *testing.T) {
	svc, _, _ := newTestUserService()
	ctx := context.Background()

	owner, err := svc.Create(ctx, CreateUserParams{Login: "grace", Password: "pw"})
	if err != nil {
		t.Fatalf("Create owner: %v", err)
	}
	other, err := svc.Create(ctx, CreateUserParams{Login: "heidi", Password: "pw"})
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}

	_, tok, err := svc.CreateToken(ctx, owner.ID, "k")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// Deleting owner's token under the wrong user must 404 and leave it intact.
	if err := svc.DeleteToken(ctx, other.ID, tok.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("DeleteToken wrong owner: expected ErrNotFound, got %v", err)
	}
	list, err := svc.ListTokens(ctx, owner.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("token should still exist for owner, got %d", len(list))
	}
}
