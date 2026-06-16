package management

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ulbwa/tgwebdav/internal/domain"
	"github.com/ulbwa/tgwebdav/internal/management/api"
)

// healthzPath is the single public route that bypasses admin authentication.
const healthzPath = "/healthz"

// NewServer assembles the Management API into an *http.Server. The generated
// router is wrapped in an admin-auth middleware: every request except
// GET /healthz must present either HTTP Basic credentials for an is_admin user
// or a Bearer token belonging to an is_admin user. The spec is served (without
// auth) at GET /openapi.yaml.
func NewServer(addr string, h *Handlers, auth domain.AuthService, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+specPath, serveSpec)

	// The generated handler registers every /api/v1/... route plus /healthz on
	// the same mux. Auth is enforced by the middleware below, which lets
	// /healthz (and /openapi.yaml, registered above) through untouched.
	handler := api.HandlerWithOptions(h, api.StdHTTPServerOptions{
		BaseRouter: muxAdapter{mux},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSONError(w, http.StatusBadRequest, err.Error())
		},
	})

	return &http.Server{
		Addr:    addr,
		Handler: adminAuthMiddleware(auth, logger)(handler),
	}
}

// specPath serves the embedded OpenAPI document.
const specPath = "/openapi.yaml"

func serveSpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rawSpec)
}

// muxAdapter adapts *http.ServeMux to the generated api.ServeMux interface.
type muxAdapter struct{ mux *http.ServeMux }

func (m muxAdapter) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.mux.HandleFunc(pattern, handler)
}

func (m muxAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mux.ServeHTTP(w, r)
}

// adminAuthMiddleware authenticates every request (except the public probe and
// the spec document) as an administrator before passing it to next.
func adminAuthMiddleware(auth domain.AuthService, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isPublic(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			user, err := authenticateAdmin(r, auth)
			if err != nil {
				if errors.Is(err, domain.ErrForbidden) {
					writeJSONError(w, http.StatusForbidden, "administrator privileges required")
					return
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="tgwebdav management", Bearer`)
				writeJSONError(w, http.StatusUnauthorized, "authentication required")
				return
			}

			// Surface the authenticated admin as a principal for downstream code.
			p := &domain.Principal{Acting: user, Auth: user}
			next.ServeHTTP(w, r.WithContext(domain.ContextWithPrincipal(r.Context(), p)))
		})
	}
}

// isPublic reports whether path is reachable without authentication.
func isPublic(path string) bool {
	return path == healthzPath || path == specPath
}

// authenticateAdmin resolves the request to an administrator user. It accepts
// HTTP Basic credentials (which must resolve to an is_admin user, with no
// impersonation) or a Bearer token whose owner is an is_admin user. It returns
// domain.ErrForbidden when the principal authenticated but is not an admin, and
// domain.ErrUnauthorized otherwise.
func authenticateAdmin(r *http.Request, auth domain.AuthService) (*domain.User, error) {
	authz := r.Header.Get("Authorization")

	if token, ok := bearerToken(authz); ok {
		user, err := auth.AuthenticateBearer(r.Context(), token)
		if err != nil {
			return nil, domain.ErrUnauthorized
		}
		if !user.IsAdmin {
			return nil, domain.ErrForbidden
		}
		return user, nil
	}

	if username, password, ok := r.BasicAuth(); ok {
		principal, err := auth.AuthenticateBasic(r.Context(), username, password)
		if err != nil {
			// AuthenticateBasic returns ErrForbidden for non-admin impersonation;
			// any other failure is treated as unauthorized.
			if errors.Is(err, domain.ErrForbidden) {
				return nil, domain.ErrForbidden
			}
			return nil, domain.ErrUnauthorized
		}
		if !principal.IsAdmin() {
			return nil, domain.ErrForbidden
		}
		return principal.Auth, nil
	}

	return nil, domain.ErrUnauthorized
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

// writeJSONError writes a JSON error body with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Error{Error: msg})
}
