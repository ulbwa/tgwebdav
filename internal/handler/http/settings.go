package http

import (
	"net/http"
	"time"

	management "github.com/ulbwa/tgwebdav/pkg/openapi/management"
)

// GetSettings handles GET /api/v1/settings.
func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	s, err := h.settings.Get(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toAPISettings(s))
}

// UpdateSettings handles PUT /api/v1/settings. Only the supplied fields are
// changed; omitted fields keep their current value.
func (h *Handler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body management.UpdateSettingsJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}

	ctx := r.Context()
	s, err := h.settings.Get(ctx)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if body.BlobMaxSize != nil {
		if *body.BlobMaxSize <= 0 {
			h.badRequest(w, "blob_max_size must be positive")
			return
		}
		s.BlobMaxSize = *body.BlobMaxSize
	}
	if body.WalIdleTimeoutMs != nil {
		if *body.WalIdleTimeoutMs < 0 {
			h.badRequest(w, "wal_idle_timeout_ms must be non-negative")
			return
		}
		s.WALIdleTimeout = time.Duration(*body.WalIdleTimeoutMs) * time.Millisecond
	}
	if body.MaxFileSize != nil {
		if *body.MaxFileSize < 0 {
			h.badRequest(w, "max_file_size must be non-negative")
			return
		}
		s.MaxFileSize = *body.MaxFileSize
	}
	if body.DefaultEvictionThreshold != nil {
		if *body.DefaultEvictionThreshold < 0 {
			h.badRequest(w, "default_eviction_threshold must be non-negative")
			return
		}
		s.DefaultEvictionThreshold = *body.DefaultEvictionThreshold
	}

	if err := h.settings.Update(ctx, s); err != nil {
		h.writeError(w, err)
		return
	}
	updated, err := h.settings.Get(ctx)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toAPISettings(updated))
}
