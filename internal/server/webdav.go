// Package server wires the WebDAV HTTP endpoint: HTTP Basic authentication and
// per-user rate limiting in front of x/net/webdav, with a quota pre-check for
// PUT (correct 507) and a COPY interceptor that shares immutable blobs instead
// of re-uploading bytes.
package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"github.com/ulbwa/tgwebdav/internal/domain"
	"github.com/ulbwa/tgwebdav/internal/webdavfs"
)

// NewWebDAV builds the WebDAV HTTP server.
func NewWebDAV(addr string, fs *webdavfs.FileSystem, auth domain.AuthService, limiter domain.Limiter, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "webdav-server")

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
				if err := fs.CheckQuota(r.Context(), r.ContentLength); err != nil {
					if errors.Is(err, domain.ErrQuotaExceeded) {
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

	handler := withAuth(auth, limiter, log, router)

	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// withAuth enforces HTTP Basic authentication and the per-user request rate
// limit, then injects the resolved principal into the request context.
func withAuth(auth domain.AuthService, limiter domain.Limiter, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			unauthorized(w)
			return
		}
		principal, err := auth.AuthenticateBasic(r.Context(), username, password)
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrForbidden):
				http.Error(w, "forbidden", http.StatusForbidden)
			default:
				unauthorized(w)
			}
			return
		}
		if !limiter.Allow(principal.Acting.ID, principal.Acting.RatePerMin) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		ctx := domain.ContextWithPrincipal(r.Context(), principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="tgwebdav"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// handleCopy implements WebDAV COPY by sharing blobs (see webdavfs.Copy).
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
