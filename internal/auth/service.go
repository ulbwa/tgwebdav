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
	"time"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// Service authenticates principals against the user and token repositories. It
// holds no mutable state and is safe for concurrent use.
type Service struct {
	users  domain.UserRepository
	tokens domain.APITokenRepository
	now    func() time.Time
}

// compile-time assertion that *Service satisfies the domain contract.
var _ domain.AuthService = (*Service)(nil)

// NewService constructs a Service backed by the given repositories.
func NewService(users domain.UserRepository, tokens domain.APITokenRepository) *Service {
	return &Service{
		users:  users,
		tokens: tokens,
		now:    time.Now,
	}
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
			return nil, fmt.Errorf("auth: unknown user %q: %w", login, domain.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: load user: %w", err)
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
