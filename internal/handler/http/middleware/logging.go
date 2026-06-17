package middleware

import (
	"log/slog"
	"net/http"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

// Logging logs one INFO line per request via slog, including the method, path,
// final status code, duration in milliseconds and the chi request id. It wraps
// the response writer with chi's WrapResponseWriter so the status code is
// observable after next has run, and logs through slog.InfoContext so any
// context-bound attributes (and the request id) are carried. It is meant to run
// after chimiddleware.RequestID so GetReqID resolves.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			next.ServeHTTP(ww, r)

			logger.InfoContext(r.Context(), "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.String("request_id", chimiddleware.GetReqID(r.Context())),
			)
		})
	}
}
