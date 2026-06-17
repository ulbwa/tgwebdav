// Package service contains canonical service implementations for tgwebdav.
// Each service declares tiny interfaces for its dependencies (Rule 5) so the
// real repository types satisfy them structurally without importing this
// package.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/model"
)

// verifyCacheTTL bounds how long a successful argon2id verification is
// trusted without recomputation. WebDAV clients resend Basic credentials
// on every request, so without this cache the deliberately slow argon2id
// hash would run on every call. The cache key includes the stored password
// hash, so a password change invalidates it immediately.
const verifyCacheTTL = 30 * time.Second

// decoyPasswordHash is a valid argon2id PHC string for a throwaway password.
// It exists solely to neutralise a username-enumeration timing side-channel:
// when a login is unknown there is no stored hash to verify against, so we
// run a dummy argon2id verification against this constant instead.
const decoyPasswordHash = "$argon2id$v=19$m=65536,t=1,p=4$a+F+q6BJpooDXaEZpElung$/DPMn8Z+G5x553RSdYsslDlkU169ThOtw6vCo3T96s8"

// userStore is the narrow repository interface AuthService needs for users.
// The real *repository.UserRepository satisfies this structurally.
type userStore interface {
	GetByLogin(ctx context.Context, login string) (*model.User, error)
	GetByID(ctx context.Context, id uuid.UUID) (*model.User, error)
}

// tokenStore is the narrow repository interface AuthService needs for API tokens.
// The real *repository.TokenRepository satisfies this structurally.
type tokenStore interface {
	GetByHash(ctx context.Context, hash string) (*model.APIToken, error)
	TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error
}

// AuthService authenticates principals against the user and token repositories.
// It is safe for concurrent use.
type AuthService struct {
	users  userStore
	tokens tokenStore
	now    func() time.Time

	vcMu sync.Mutex
	vc   map[string]time.Time // verification cache: key → expiry
}

// NewAuthService constructs an AuthService backed by the given stores.
func NewAuthService(users userStore, tokens tokenStore) *AuthService {
	return &AuthService{
		users:  users,
		tokens: tokens,
		now:    time.Now,
		vc:     make(map[string]time.Time),
	}
}

// AuthenticateBasic resolves an HTTP Basic credential into a Principal.
//
// A username containing a '/' selects admin impersonation: the part before
// the slash is an administrator's login whose password must match and who
// must have the admin flag, and the part after the slash names the target
// user whose namespace is served. The returned principal then has
// Acting == target and Auth == admin.
//
// A plain username authenticates that user directly, yielding
// Acting == Auth.
//
// Unknown users and wrong passwords are reported as model.ErrUnauthorized
// so callers cannot distinguish the two. A valid non-admin attempting
// impersonation is reported as model.ErrForbidden.
func (s *AuthService) AuthenticateBasic(ctx context.Context, username, password string) (*model.Principal, error) {
	if adminLogin, targetLogin, ok := strings.Cut(username, "/"); ok {
		return s.authenticateImpersonation(ctx, adminLogin, targetLogin, password)
	}

	user, err := s.lookupAndVerify(ctx, username, password)
	if err != nil {
		return nil, err
	}
	return &model.Principal{Acting: user, Auth: user}, nil
}

// authenticateImpersonation handles the "admin/target" form of Basic auth.
func (s *AuthService) authenticateImpersonation(ctx context.Context, adminLogin, targetLogin, password string) (*model.Principal, error) {
	admin, err := s.lookupAndVerify(ctx, adminLogin, password)
	if err != nil {
		return nil, err
	}
	if !admin.IsAdmin {
		return nil, fmt.Errorf("auth: %q is not an administrator: %w", adminLogin, model.ErrForbidden)
	}

	target, err := s.users.GetByLogin(ctx, targetLogin)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, fmt.Errorf("auth: impersonation target %q: %w", targetLogin, model.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: load impersonation target: %w", err)
	}
	return &model.Principal{Acting: target, Auth: admin}, nil
}

// lookupAndVerify loads a user by login and checks the password. An unknown
// login, a malformed stored hash, or a wrong password all collapse to
// model.ErrUnauthorized. Other repository errors are propagated wrapped.
func (s *AuthService) lookupAndVerify(ctx context.Context, login, password string) (*model.User, error) {
	user, err := s.users.GetByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			// Run a dummy argon2id verification against a fixed valid hash so the
			// not-found path spends the same (deliberately slow) work as the
			// found-but-wrong-password path.
			_, _ = VerifyPassword(decoyPasswordHash, password)
			return nil, fmt.Errorf("auth: unknown user %q: %w", login, model.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: load user: %w", err)
	}

	// Fast path: skip argon2id if this (login, hash, password) tuple was
	// verified recently.
	key := verifyCacheKey(login, user.PasswordHash, password)
	if s.verifiedRecently(key) {
		return user, nil
	}

	ok, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		return nil, fmt.Errorf("auth: verify password for %q: %w", login, model.ErrUnauthorized)
	}
	if !ok {
		return nil, fmt.Errorf("auth: bad password for %q: %w", login, model.ErrUnauthorized)
	}
	s.markVerified(key)
	return user, nil
}

// AuthenticateBearer verifies a Management API bearer token. The presented
// token is hashed (sha-256, hex) and looked up; on a hit the owning user is
// loaded and the token's last-used timestamp is refreshed (best-effort).
func (s *AuthService) AuthenticateBearer(ctx context.Context, token string) (*model.User, error) {
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])

	tok, err := s.tokens.GetByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, fmt.Errorf("auth: unknown bearer token: %w", model.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: load token: %w", err)
	}

	user, err := s.users.GetByID(ctx, tok.UserID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, fmt.Errorf("auth: token owner missing: %w", model.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: load token owner: %w", err)
	}

	if err := s.tokens.TouchLastUsed(ctx, tok.ID, s.now()); err != nil {
		// Recording usage is non-critical; authentication still succeeds.
		return user, nil //nolint:nilerr // touch is best-effort
	}
	return user, nil
}

// HashPassword produces an argon2id PHC hash for storage.
func (s *AuthService) HashPassword(password string) (string, error) {
	return HashPassword(password)
}

// verifiedRecently reports whether key was verified within the TTL.
func (s *AuthService) verifiedRecently(key string) bool {
	s.vcMu.Lock()
	defer s.vcMu.Unlock()
	exp, ok := s.vc[key]
	if !ok {
		return false
	}
	if s.now().After(exp) {
		delete(s.vc, key)
		return false
	}
	return true
}

// markVerified records a successful verification, opportunistically pruning
// expired entries to bound memory.
func (s *AuthService) markVerified(key string) {
	s.vcMu.Lock()
	defer s.vcMu.Unlock()
	now := s.now()
	if len(s.vc) > 4096 {
		for k, exp := range s.vc {
			if now.After(exp) {
				delete(s.vc, k)
			}
		}
	}
	s.vc[key] = now.Add(verifyCacheTTL)
}

func verifyCacheKey(login, hash, password string) string {
	sum := sha256.Sum256([]byte(login + "\x00" + hash + "\x00" + password))
	return hex.EncodeToString(sum[:])
}
