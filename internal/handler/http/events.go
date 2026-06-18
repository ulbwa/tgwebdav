package http

import (
	"net/http"

	"github.com/samber/lo"

	"github.com/ulbwa/tgwebdav/internal/model"
	management "github.com/ulbwa/tgwebdav/pkg/openapi/management"
)

// ListEvents handles GET /api/v1/events. The limit defaults to (and clamps to)
// 100 and a negative offset is treated as 0, matching the old handler exactly.
func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request, params management.ListEventsParams) {
	var kind string
	if params.Kind != nil {
		kind = *params.Kind
	}
	limit := 100
	if params.Limit != nil {
		limit = int(*params.Limit)
	}
	if limit <= 0 {
		limit = 100
	}
	offset := 0
	if params.Offset != nil {
		offset = int(*params.Offset)
	}
	if offset < 0 {
		offset = 0
	}

	events, total, err := h.events.List(r.Context(), kind, limit, offset)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := lo.Map(events, func(e model.Event, _ int) management.Event { return toAPIEvent(e) })
	h.writeJSON(w, http.StatusOK, management.EventPage{Events: out, Total: total})
}
