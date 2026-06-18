package http

import (
	"net/http"
	"time"

	"github.com/samber/lo"

	"github.com/ulbwa/tgwebdav/internal/model"
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
	out := lo.Map(samples, func(s model.StatSample, _ int) management.StatPoint { return toAPIStatPoint(s) })
	h.writeJSON(w, http.StatusOK, out)
}
