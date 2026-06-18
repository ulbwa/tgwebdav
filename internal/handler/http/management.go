// Package http hosts the canonical HTTP handlers for tgwebdav: the OpenAPI-first
// Management REST API and the WebDAV endpoint. Both are exposed as plain
// http.Handler values (NewManagementHandler / NewWebDAVHandler) so cmd can mount
// them on an http.Server. Following the canon layering, the handlers depend only
// on the application services, the model, the generated Management server
// interface and the embedded OpenAPI spec — never on repositories or clients.
package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	chimiddleware "github.com/go-chi/chi/v5/middleware"

	openapi "github.com/ulbwa/tgwebdav/api/openapi"
	"github.com/ulbwa/tgwebdav/internal/handler/http/middleware"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
	"github.com/ulbwa/tgwebdav/internal/service"
	"github.com/ulbwa/tgwebdav/internal/service/webdavfs"
	management "github.com/ulbwa/tgwebdav/pkg/openapi/management"
)

// Handler implements the generated management.ServerInterface by delegating to
// the application services. It is the canonical replacement for the old
// internal/management.Handlers: identical status codes, JSON shapes, validation
// and error→status mapping, but wired to services instead of repositories.
type Handler struct {
	users    *service.UserService
	bots     *service.BotService
	channels *service.ChannelService
	settings *service.SettingsService
	events   *service.EventService
	stats    *service.StatRecorder
	logger   *slog.Logger
}

// compile-time assertion that *Handler satisfies the generated interface.
var _ management.ServerInterface = (*Handler)(nil)

// ManagementDeps bundles the services the Management handler delegates to, plus
// the auth service used by the admin middleware. cmd constructs this.
type ManagementDeps struct {
	Auth     *service.AuthService
	Users    *service.UserService
	Bots     *service.BotService
	Channels *service.ChannelService
	Settings *service.SettingsService
	Events   *service.EventService
	Stats    *service.StatRecorder
	Logger   *slog.Logger
}

// specPath is where the embedded OpenAPI document is served (public).
const specPath = "/openapi.yaml"

// NewManagementHandler assembles the Management API into an http.Handler.
//
// Routing matches the old internal/management.NewServer exactly: a std
// net/http.ServeMux is the base router; the generated oapi handler registers
// every /api/v1/... route plus /healthz on it; GET /openapi.yaml serves the
// embedded spec; /healthz and /openapi.yaml are public while every /api/v1/...
// request requires an administrator.
//
// The mux is wrapped, outermost-first, with:
//
//	chimiddleware.RequestID → middleware.Logging → chimiddleware.Recoverer → admin auth (/api/v1 only)
//
// The admin auth is mounted only on the /api/v1/ subtree so /healthz and
// /openapi.yaml stay public, preserving the old isPublic behavior.
func NewManagementHandler(d ManagementDeps) http.Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	h := &Handler{
		users:    d.Users,
		bots:     d.Bots,
		channels: d.Channels,
		settings: d.Settings,
		events:   d.Events,
		stats:    d.Stats,
		logger:   logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+specPath, serveSpec)

	// The generated handler registers every /api/v1/... route plus /healthz on
	// the mux. Auth is enforced by the admin middleware below, scoped to /api/v1
	// so /healthz (and /openapi.yaml, registered above) stay public — mirroring
	// the old muxAdapter wiring and isPublic logic.
	management.HandlerWithOptions(h, management.StdHTTPServerOptions{
		BaseRouter: muxAdapter{mux},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSONError(w, http.StatusBadRequest, err.Error())
		},
	})

	admin := middleware.AdminAuth(d.Auth)

	// Apply auth only to the /api/v1 subtree: a thin dispatcher gates those paths
	// while leaving /healthz and /openapi.yaml untouched (old isPublic).
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isManagementPublic(r.URL.Path) {
			mux.ServeHTTP(w, r)
			return
		}
		admin(mux).ServeHTTP(w, r)
	})

	// Cross-cutting middleware, outermost first.
	return chimiddleware.RequestID(
		middleware.Logging(logger)(
			chimiddleware.Recoverer(gated),
		),
	)
}

// isManagementPublic reports whether path bypasses admin authentication. It
// mirrors the old isPublic: /healthz and /openapi.yaml are reachable without
// credentials; everything else (the /api/v1 surface) requires an admin.
func isManagementPublic(path string) bool {
	return path == "/healthz" || path == specPath
}

// serveSpec serves the embedded OpenAPI document.
func serveSpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapi.Spec)
}

// muxAdapter adapts *http.ServeMux to the generated management.ServeMux
// interface (HandleFunc + ServeHTTP), mirroring the old muxAdapter.
type muxAdapter struct{ mux *http.ServeMux }

func (m muxAdapter) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.mux.HandleFunc(pattern, handler)
}

func (m muxAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mux.ServeHTTP(w, r)
}

// ---- error / response helpers ---------------------------------------------

// statusForError maps a sentinel error (from the repository, service, webdavfs
// or this handler) onto an HTTP status code via errors.Is. It is the canonical
// port of the old management.statusForError, preserving every mapping.
func statusForError(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, repository.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, repository.ErrAlreadyExists):
		return http.StatusConflict
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, service.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, service.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrInvalidPath),
		errors.Is(err, ErrNotDir),
		errors.Is(err, ErrIsDir),
		errors.Is(err, ErrNotEmpty):
		return http.StatusBadRequest
	case errors.Is(err, webdavfs.ErrQuotaExceeded):
		return http.StatusInsufficientStorage
	case errors.Is(err, ErrFileTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, service.ErrNoBot),
		errors.Is(err, service.ErrBlobUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writeJSON serializes v as JSON with the given status code.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("management: encode response", "error", err)
	}
}

// writeError emits a JSON error body with the status derived from err.
func (h *Handler) writeError(w http.ResponseWriter, err error) {
	status := statusForError(err)
	if status >= http.StatusInternalServerError {
		h.logger.Error("management: request failed", "error", err, "status", status)
	}
	h.writeJSON(w, status, management.Error{Error: err.Error()})
}

// badRequest emits a 400 with a custom message.
func (h *Handler) badRequest(w http.ResponseWriter, msg string) {
	h.writeJSON(w, http.StatusBadRequest, management.Error{Error: msg})
}

// writeJSONError writes a JSON {"error": msg} body with the given status (used by
// the oapi ErrorHandlerFunc), matching the old writeJSONError.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(management.Error{Error: msg})
}

// decodeBody decodes the JSON request body into dst, rejecting unknown fields.
func decodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// ---- mappers ---------------------------------------------------------------

func toAPIUser(u *model.User) management.User {
	return management.User{
		Id:           u.ID,
		Login:        u.Login,
		IsAdmin:      u.IsAdmin,
		QuotaBytes:   u.QuotaBytes,
		BandwidthBps: u.BandwidthBPS,
		RatePerMin:   int32(u.RatePerMin),
		CreatedAt:    u.CreatedAt,
	}
}

func toAPIToken(t *model.APIToken) management.APIToken {
	return management.APIToken{
		Id:         t.ID,
		UserId:     t.UserID,
		Name:       t.Name,
		CreatedAt:  t.CreatedAt,
		LastUsedAt: t.LastUsedAt,
	}
}

func toAPIBot(b *model.Bot) management.Bot {
	return management.Bot{
		Id:               b.ID,
		Username:         b.Username,
		Enabled:          b.Enabled,
		UnavailableUntil: b.UnavailableUntil,
		CreatedAt:        b.CreatedAt,
	}
}

func toAPIChannel(c *model.Channel) management.Channel {
	return management.Channel{
		Id:                c.ID,
		TgChatId:          c.TGChatID,
		Title:             c.Title,
		MessageCounter:    c.MessageCounter,
		EvictionThreshold: c.EvictionThreshold,
		Available:         c.Available,
		CreatedAt:         c.CreatedAt,
	}
}

func toAPISettings(s model.Settings) management.Settings {
	return management.Settings{
		BlobMaxSize:              s.BlobMaxSize,
		WalIdleTimeoutMs:         s.WALIdleTimeout.Milliseconds(),
		MaxFileSize:              s.MaxFileSize,
		DefaultEvictionThreshold: s.DefaultEvictionThreshold,
		UpdatedAt:                s.UpdatedAt,
	}
}

func toAPIStatPoint(s model.StatSample) management.StatPoint {
	return management.StatPoint{
		Ts:     s.TS,
		Metric: s.Metric,
		Label:  s.Label,
		Value:  s.Value,
	}
}

func toAPIEvent(e model.Event) management.Event {
	return management.Event{
		Id:      e.ID,
		Ts:      e.TS,
		Kind:    e.Kind,
		Message: e.Message,
		Ref:     e.Ref,
	}
}

// ---- small helpers ---------------------------------------------------------

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}
