package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
)

// userRepo is the repository surface UserService needs for users. It is wider
// than auth.go's read-only userStore (which only loads users for
// authentication); UserService also creates, updates, deletes and lists them.
// The real *repository.UserRepository satisfies this structurally.
type userRepo interface {
	Create(ctx context.Context, u *model.User) error
	Update(ctx context.Context, u *model.User) error
	Delete(ctx context.Context, id uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*model.User, error)
	List(ctx context.Context) ([]model.User, error)
}

// tokenRepo is the repository surface UserService needs for API tokens. It is
// wider than auth.go's read-only tokenStore (which only resolves a presented
// bearer token); UserService also creates, lists and deletes tokens on behalf
// of a user. The real *repository.TokenRepository satisfies this structurally.
type tokenRepo interface {
	Create(ctx context.Context, t *model.APIToken) error
	ListByUser(ctx context.Context, userID uuid.UUID) ([]model.APIToken, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// CreateUserParams carries the mutable fields of a new user. The login and
// password are required; the remaining fields default to their zero values,
// which mean "admin: no" and "unlimited" for the limit fields.
type CreateUserParams struct {
	Login        string
	Password     string
	IsAdmin      bool
	QuotaBytes   int64 // 0 means unlimited
	BandwidthBPS int64 // 0 means unlimited
	RatePerMin   int   // 0 means unlimited
}

// UserService manages users and the API tokens that belong to them. Tokens are
// always scoped to a user: every token operation takes a userID and is
// validated against that user.
type UserService struct {
	users  userRepo
	tokens tokenRepo
}

// NewUserService wires a UserService from the user and token repositories.
func NewUserService(users userRepo, tokens tokenRepo) *UserService {
	return &UserService{users: users, tokens: tokens}
}

// Create hashes the supplied password, populates a new user from p and
// persists it. A duplicate login surfaces as repository.ErrAlreadyExists (the
// repository maps the unique-constraint violation). The created user, including
// its generated id and timestamp, is returned.
func (s *UserService) Create(ctx context.Context, p CreateUserParams) (*model.User, error) {
	hash, err := HashPassword(p.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	u := &model.User{
		ID:           uuid.New(),
		Login:        p.Login,
		PasswordHash: hash,
		IsAdmin:      p.IsAdmin,
		QuotaBytes:   p.QuotaBytes,
		BandwidthBPS: p.BandwidthBPS,
		RatePerMin:   p.RatePerMin,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, fmt.Errorf("create user %q: %w", p.Login, err)
	}
	return u, nil
}

// List returns every user.
func (s *UserService) List(ctx context.Context) ([]model.User, error) {
	users, err := s.users.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

// Get returns the user with the given id, or repository.ErrNotFound.
func (s *UserService) Get(ctx context.Context, id uuid.UUID) (*model.User, error) {
	u, err := s.users.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get user %s: %w", id, err)
	}
	return u, nil
}

// Delete removes the user with the given id, or repository.ErrNotFound.
func (s *UserService) Delete(ctx context.Context, id uuid.UUID) error {
	if err := s.users.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	}
	return nil
}

// SetPassword replaces a user's password with a fresh argon2id hash of
// newPassword. A missing user surfaces as repository.ErrNotFound.
func (s *UserService) SetPassword(ctx context.Context, id uuid.UUID, newPassword string) error {
	u, err := s.users.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get user %s: %w", id, err)
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	u.PasswordHash = hash
	if err := s.users.Update(ctx, u); err != nil {
		return fmt.Errorf("update user %s: %w", id, err)
	}
	return nil
}

// ListTokens returns every API token belonging to userID. The user is verified
// to exist first so an unknown user yields repository.ErrNotFound rather than an
// empty list.
func (s *UserService) ListTokens(ctx context.Context, userID uuid.UUID) ([]model.APIToken, error) {
	if _, err := s.users.GetByID(ctx, userID); err != nil {
		return nil, fmt.Errorf("get user %s: %w", userID, err)
	}
	tokens, err := s.tokens.ListByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list tokens for user %s: %w", userID, err)
	}
	return tokens, nil
}

// CreateToken mints a new bearer token for userID. A cryptographically random
// opaque token is generated; only its sha-256 hash is persisted, and the
// plaintext is returned exactly once for the caller to relay to the user. The
// owning user is verified to exist first. The hashing scheme matches
// AuthService.AuthenticateBearer so the token authenticates.
func (s *UserService) CreateToken(ctx context.Context, userID uuid.UUID, name string) (string, *model.APIToken, error) {
	if _, err := s.users.GetByID(ctx, userID); err != nil {
		return "", nil, fmt.Errorf("get user %s: %w", userID, err)
	}

	plaintext, err := generateAPIToken()
	if err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	tok := &model.APIToken{
		ID:        uuid.New(),
		UserID:    userID,
		TokenHash: hashAPIToken(plaintext),
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.tokens.Create(ctx, tok); err != nil {
		return "", nil, fmt.Errorf("create token for user %s: %w", userID, err)
	}
	return plaintext, tok, nil
}

// DeleteToken removes tokenID, but only if it actually belongs to userID. A
// token id that does not belong to the user (or does not exist) yields
// repository.ErrNotFound rather than deleting an unrelated token.
func (s *UserService) DeleteToken(ctx context.Context, userID, tokenID uuid.UUID) error {
	tokens, err := s.tokens.ListByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("list tokens for user %s: %w", userID, err)
	}
	found := false
	for i := range tokens {
		if tokens[i].ID == tokenID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("token %s not owned by user %s: %w", tokenID, userID, repository.ErrNotFound)
	}
	if err := s.tokens.Delete(ctx, tokenID); err != nil {
		return fmt.Errorf("delete token %s: %w", tokenID, err)
	}
	return nil
}

// generateAPIToken returns a cryptographically-random opaque bearer token as a
// 64-char hex string (32 random bytes), matching the old management handler.
func generateAPIToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// hashAPIToken returns the sha-256 hex digest used to store and look up a bearer
// token. It must match AuthService.AuthenticateBearer's scheme (sha256 of the
// presented token, hex-encoded) and the old management handler's hashToken.
func hashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
