// Package auth implements password hashing and the domain.AuthService that
// authenticates WebDAV (HTTP Basic) and Management API (Bearer) principals.
//
// Passwords are hashed with argon2id and stored as standard PHC strings (see
// HashPassword / VerifyPassword). Bearer tokens are presented in clear and
// matched against their sha-256 hex digest, which is what the database stores.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// verifyCacheTTL bounds how long a successful argon2id verification is trusted
// without recomputation. WebDAV clients resend Basic credentials on every
// request, so without this cache the (deliberately slow) argon2id hash would run
// on every call. The cache key includes the stored password hash, so a password
// change invalidates it immediately.
const verifyCacheTTL = 30 * time.Second

// decoyPasswordHash is a valid argon2id PHC string for a throwaway password. It
// exists solely to neutralise a username-enumeration timing side-channel: when a
// login is unknown there is no stored hash to verify against, so we run a dummy
// argon2id verification against this constant instead. That makes the work done
// on the not-found path comparable to the found-but-wrong-password path, so the
// response time no longer reveals whether a username exists. The result is
// always discarded. It is never used to authenticate anyone.
const decoyPasswordHash = "$argon2id$v=19$m=65536,t=1,p=4$a+F+q6BJpooDXaEZpElung$/DPMn8Z+G5x553RSdYsslDlkU169ThOtw6vCo3T96s8"

// Service authenticates principals against the user and token repositories. It
// is safe for concurrent use.
type Service struct {
	users  domain.UserRepository
	tokens domain.APITokenRepository
	now    func() time.Time

	vcMu sync.Mutex
	vc   map[string]time.Time // verification cache: key → expiry
}

// compile-time assertion that *Service satisfies the domain contract.
var _ domain.AuthService = (*Service)(nil)

// NewService constructs a Service backed by the given repositories.
func NewService(users domain.UserRepository, tokens domain.APITokenRepository) *Service {
	return &Service{
		users:  users,
		tokens: tokens,
		now:    time.Now,
		vc:     make(map[string]time.Time),
	}
}

// verifiedRecently reports whether key was verified within the TTL.
func (s *Service) verifiedRecently(key string) bool {
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
func (s *Service) markVerified(key string) {
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

// AuthenticateBasic resolves an HTTP Basic credential into a Principal.
//
// A username containing a '/' selects admin impersonation: the part before the
// slash is an administrator's login whose password must match and who must have
// the admin flag, and the part after the slash names the target user whose
// namespace is served. The returned principal then has Acting == target and
// Auth == admin.
//
// A plain username authenticates that user directly, yielding Acting == Auth.
//
// Unknown users and wrong passwords are reported as domain.ErrUnauthorized so
// callers cannot distinguish the two. A valid non-admin attempting
// impersonation is reported as domain.ErrForbidden.
func (s *Service) AuthenticateBasic(ctx context.Context, username, password string) (*domain.Principal, error) {
	if adminLogin, targetLogin, ok := strings.Cut(username, "/"); ok {
		return s.authenticateImpersonation(ctx, adminLogin, targetLogin, password)
	}

	user, err := s.lookupAndVerify(ctx, username, password)
	if err != nil {
		return nil, err
	}
	return &domain.Principal{Acting: user, Auth: user}, nil
}

// authenticateImpersonation handles the "admin/target" form of Basic auth.
func (s *Service) authenticateImpersonation(ctx context.Context, adminLogin, targetLogin, password string) (*domain.Principal, error) {
	admin, err := s.lookupAndVerify(ctx, adminLogin, password)
	if err != nil {
		return nil, err
	}
	if !admin.IsAdmin {
		return nil, fmt.Errorf("auth: %q is not an administrator: %w", adminLogin, domain.ErrForbidden)
	}

	target, err := s.users.GetByLogin(ctx, targetLogin)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("auth: impersonation target %q: %w", targetLogin, domain.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: load impersonation target: %w", err)
	}
	return &domain.Principal{Acting: target, Auth: admin}, nil
}

// lookupAndVerify loads a user by login and checks the password. An unknown
// login, a malformed stored hash, or a wrong password all collapse to
// domain.ErrUnauthorized. Other repository errors are propagated wrapped.
func (s *Service) lookupAndVerify(ctx context.Context, login, password string) (*domain.User, error) {
	user, err := s.users.GetByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Run a dummy argon2id verification against a fixed valid hash so the
			// not-found path spends the same (deliberately slow) work as the
			// found-but-wrong-password path. Without this, an unknown login would
			// return immediately while a known one pays the argon2id cost, and the
			// response-time difference would let an attacker enumerate usernames.
			// The result is intentionally discarded.
			_, _ = VerifyPassword(decoyPasswordHash, password)
			return nil, fmt.Errorf("auth: unknown user %q: %w", login, domain.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: load user: %w", err)
	}

	// Fast path: skip the (deliberately slow) argon2id verification if this exact
	// (login, stored hash, password) tuple was verified recently.
	key := verifyCacheKey(login, user.PasswordHash, password)
	if s.verifiedRecently(key) {
		return user, nil
	}

	ok, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		// A stored hash we cannot parse is a credential failure, not a 500: we
		// can never authenticate against it.
		return nil, fmt.Errorf("auth: verify password for %q: %w", login, domain.ErrUnauthorized)
	}
	if !ok {
		return nil, fmt.Errorf("auth: bad password for %q: %w", login, domain.ErrUnauthorized)
	}
	s.markVerified(key)
	return user, nil
}

// AuthenticateBearer verifies a Management API bearer token. The presented
// token is hashed (sha-256, hex) and looked up; on a hit the owning user is
// loaded and the token's last-used timestamp is refreshed (best-effort — a
// touch failure does not fail the request). A missing token or user yields
// domain.ErrUnauthorized.
func (s *Service) AuthenticateBearer(ctx context.Context, token string) (*domain.User, error) {
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])

	tok, err := s.tokens.GetByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("auth: unknown bearer token: %w", domain.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: load token: %w", err)
	}

	user, err := s.users.GetByID(ctx, tok.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("auth: token owner missing: %w", domain.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: load token owner: %w", err)
	}

	if err := s.tokens.TouchLastUsed(ctx, tok.ID, s.now()); err != nil {
		// Recording usage is non-critical; authentication still succeeds.
		return user, nil //nolint:nilerr // touch is best-effort
	}
	return user, nil
}

// HashPassword produces an argon2id PHC hash for storage. It is a thin method
// wrapper around the package-level HashPassword so callers holding only the
// domain.AuthService interface can hash passwords too.
func (s *Service) HashPassword(password string) (string, error) {
	return HashPassword(password)
}
