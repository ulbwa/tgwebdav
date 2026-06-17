// Package middleware holds the HTTP middleware shared by the canonical handlers:
// business authentication (Basic for WebDAV, Basic-or-Bearer admin for the
// Management API) and slog request logging. The mature cross-cutting middleware
// (request id, panic recovery, response-writer wrapping) is taken from
// github.com/go-chi/chi/v5/middleware; only the application-specific logic lives
// here.
package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/service"
)

// basicAuthenticator is the slice of *service.AuthService the WebDAV Basic auth
// needs. Declaring it lets tests substitute a fake without the real service.
type basicAuthenticator interface {
	AuthenticateBasic(ctx context.Context, username, password string) (*model.Principal, error)
}

// adminAuthenticator is the slice of *service.AuthService the Management
// admin-only auth needs: Basic credentials plus bearer-token resolution.
type adminAuthenticator interface {
	AuthenticateBasic(ctx context.Context, username, password string) (*model.Principal, error)
	AuthenticateBearer(ctx context.Context, token string) (*model.User, error)
}

// compile-time check: the real *service.AuthService satisfies both surfaces.
var (
	_ basicAuthenticator = (*service.AuthService)(nil)
	_ adminAuthenticator = (*service.AuthService)(nil)
)

// BasicAuth authenticates WebDAV requests with HTTP Basic credentials and stores
// the resolved *model.Principal on the request context. A username of the form
// "admin/target" selects admin impersonation (handled by the auth service). A
// missing or unparseable credential yields 401 with a WWW-Authenticate header;
// a valid non-admin attempting impersonation yields 403. This mirrors the old
// internal/server/webdav.go withAuth exactly (it does NOT enforce the rate limit;
// that is a separate middleware).
func BasicAuth(auth basicAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			username, password, ok := r.BasicAuth()
			if !ok {
				webdavUnauthorized(w)
				return
			}
			principal, err := auth.AuthenticateBasic(r.Context(), username, password)
			if err != nil {
				switch {
				case errors.Is(err, model.ErrForbidden):
					http.Error(w, "forbidden", http.StatusForbidden)
				default:
					webdavUnauthorized(w)
				}
				return
			}
			ctx := model.ContextWithPrincipal(r.Context(), principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// webdavUnauthorized writes the WebDAV 401 with its Basic challenge, matching the
// old server's unauthorized helper byte-for-byte.
func webdavUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="tgwebdav"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// AdminAuth authenticates Management API requests as an administrator and stores
// the resulting *model.Principal (Acting == Auth == the admin) on the context.
// It accepts either a Bearer token whose owner is an admin, or HTTP Basic
// credentials that resolve to an admin user with no impersonation. A principal
// that authenticated but is not an admin yields 403; anything else yields 401
// with a combined Basic+Bearer WWW-Authenticate challenge. This mirrors the old
// internal/management/server.go adminAuthMiddleware/authenticateAdmin exactly.
func AdminAuth(auth adminAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, err := authenticateAdmin(r, auth)
			if err != nil {
				if errors.Is(err, model.ErrForbidden) {
					writeJSONError(w, http.StatusForbidden, "administrator privileges required")
					return
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="tgwebdav management", Bearer`)
				writeJSONError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			p := &model.Principal{Acting: user, Auth: user}
			next.ServeHTTP(w, r.WithContext(model.ContextWithPrincipal(r.Context(), p)))
		})
	}
}

// authenticateAdmin resolves the request to an administrator user. It accepts a
// Bearer token whose owner is an is_admin user, or HTTP Basic credentials that
// resolve to an is_admin user (with no impersonation). It returns
// model.ErrForbidden when the principal authenticated but is not an admin, and
// model.ErrUnauthorized otherwise.
func authenticateAdmin(r *http.Request, auth adminAuthenticator) (*model.User, error) {
	authz := r.Header.Get("Authorization")

	if token, ok := bearerToken(authz); ok {
		user, err := auth.AuthenticateBearer(r.Context(), token)
		if err != nil {
			return nil, model.ErrUnauthorized
		}
		if !user.IsAdmin {
			return nil, model.ErrForbidden
		}
		return user, nil
	}

	if username, password, ok := r.BasicAuth(); ok {
		principal, err := auth.AuthenticateBasic(r.Context(), username, password)
		if err != nil {
			// AuthenticateBasic returns ErrForbidden for non-admin impersonation;
			// any other failure is treated as unauthorized.
			if errors.Is(err, model.ErrForbidden) {
				return nil, model.ErrForbidden
			}
			return nil, model.ErrUnauthorized
		}
		if !principal.IsAdmin() {
			return nil, model.ErrForbidden
		}
		return principal.Auth, nil
	}

	return nil, model.ErrUnauthorized
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// writeJSONError writes a JSON {"error": msg} body with the given status,
// matching the Management API error envelope.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
