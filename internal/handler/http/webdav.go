package http

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"golang.org/x/net/webdav"

	"github.com/ulbwa/tgwebdav/internal/handler/http/middleware"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/service"
	"github.com/ulbwa/tgwebdav/internal/service/webdavfs"
)

// WebDAVDeps bundles what the WebDAV handler needs. cmd constructs this.
type WebDAVDeps struct {
	FS      *webdavfs.FileSystem
	Auth    *service.AuthService
	Limiter *service.Limiter
	Logger  *slog.Logger
}

// NewWebDAVHandler builds the WebDAV endpoint as an http.Handler.
//
// The core is x/net/webdav.Handler over the webdavfs.FileSystem with an
// in-memory lock system, exactly as the old internal/server.NewWebDAV. Two
// methods are intercepted before reaching the webdav handler: COPY shares
// immutable blobs via FileSystem.Copy (instead of re-uploading), and PUT runs a
// quota pre-check so an over-quota write answers 507 before any bytes stream.
//
// The handler is wrapped, outermost-first, with:
//
//	chimiddleware.RequestID → middleware.Logging → chimiddleware.Recoverer → basic auth → rate limiter
//
// Basic auth resolves the principal (with admin impersonation) and the limiter
// enforces the per-user request rate, both preserving the old status codes and
// headers (401 + WWW-Authenticate, 403 forbidden, 429 + Retry-After).
func NewWebDAVHandler(d WebDAVDeps) http.Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "webdav-server")

	fs := d.FS
	dav := &webdav.Handler{
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Debug("webdav", "method", r.Method, "path", r.URL.Path, "err", err)
			}
		},
	}

	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "COPY":
			handleCopy(w, r, fs, log)
			return
		case http.MethodPut:
			if r.ContentLength > 0 {
				if err := fs.CheckQuota(r.Context(), r.URL.Path, r.ContentLength); err != nil {
					if errors.Is(err, model.ErrQuotaExceeded) {
						http.Error(w, "insufficient storage", http.StatusInsufficientStorage)
						return
					}
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
			}
		}
		dav.ServeHTTP(w, r)
	})

	// Inner business middleware: basic auth then the per-user rate limiter.
	authed := middleware.BasicAuth(d.Auth)(rateLimit(d.Limiter, router))

	// Cross-cutting middleware, outermost first.
	return chimiddleware.RequestID(
		middleware.Logging(logger)(
			chimiddleware.Recoverer(authed),
		),
	)
}

// rateLimit enforces the per-user request rate using the principal placed on the
// context by the basic-auth middleware. An allowed request passes through; an
// exceeded rate answers 429 with Retry-After, matching the old withAuth. A
// request without a principal (should not happen behind auth) is allowed through.
func rateLimit(limiter *service.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := model.PrincipalFromContext(r.Context())
		if ok && p.Acting != nil {
			if !limiter.Allow(p.Acting.ID, p.Acting.RatePerMin) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleCopy implements WebDAV COPY by sharing blobs (see webdavfs.Copy). It is
// a verbatim port of the old internal/server.handleCopy, preserving every
// status: 201 created / 204 no-content, 507 quota, 412 precondition (dest
// exists), 409 conflict, 403 forbidden, 500 otherwise.
func handleCopy(w http.ResponseWriter, r *http.Request, fs *webdavfs.FileSystem, log *slog.Logger) {
	dest := r.Header.Get("Destination")
	if dest == "" {
		http.Error(w, "missing Destination", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(dest)
	if err != nil {
		http.Error(w, "bad Destination", http.StatusBadRequest)
		return
	}
	dstPath := u.Path

	overwrite := !strings.EqualFold(r.Header.Get("Overwrite"), "F")
	recursive := r.Header.Get("Depth") != "0"

	ctx := r.Context()

	// Source must exist.
	if _, err := fs.Stat(ctx, r.URL.Path); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, destErr := fs.Stat(ctx, dstPath)
	destExisted := destErr == nil

	err = fs.Copy(ctx, r.URL.Path, dstPath, recursive, overwrite)
	switch {
	case err == nil:
		if destExisted {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
	case errors.Is(err, model.ErrQuotaExceeded):
		http.Error(w, "insufficient storage", http.StatusInsufficientStorage)
	case errors.Is(err, os.ErrExist):
		http.Error(w, "destination exists", http.StatusPreconditionFailed)
	case errors.Is(err, os.ErrNotExist):
		http.Error(w, "conflict", http.StatusConflict)
	case errors.Is(err, os.ErrPermission):
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		log.Warn("copy failed", "src", r.URL.Path, "dst", dstPath, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
