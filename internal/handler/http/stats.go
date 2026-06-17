package http

import (
	"net/http"
	"time"

	management "github.com/ulbwa/tgwebdav/pkg/openapi/management"
)

// QueryStats handles GET /api/v1/stats.
func (h *Handler) QueryStats(w http.ResponseWriter, r *http.Request, params management.QueryStatsParams) {
	if params.Metric == "" {
		h.badRequest(w, "metric is required")
		return
	}
	var label string
	if params.Label != nil {
		label = *params.Label
	}
	from := time.Time{}
	if params.From != nil {
		from = *params.From
	}
	to := time.Now().UTC()
	if params.To != nil {
		to = *params.To
	}
	if !from.IsZero() && to.Before(from) {
		h.badRequest(w, "to must not be before from")
		return
	}

	samples, err := h.stats.Query(r.Context(), params.Metric, label, from, to)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]management.StatPoint, 0, len(samples))
	for i := range samples {
		out = append(out, toAPIStatPoint(samples[i]))
	}
	h.writeJSON(w, http.StatusOK, out)
}
