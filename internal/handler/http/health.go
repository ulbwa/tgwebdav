package http

import (
	"net/http"

	management "github.com/ulbwa/tgwebdav/pkg/openapi/management"
)

// Healthz handles GET /healthz (public). It mirrors the old handler: a 200 with
// {"status":"ok"}.
func (h *Handler) Healthz(w http.ResponseWriter, _ *http.Request) {
	h.writeJSON(w, http.StatusOK, management.Health{Status: "ok"})
}
