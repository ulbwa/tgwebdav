package http

import (
	"net/http"

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
	out := make([]management.Event, 0, len(events))
	for i := range events {
		out = append(out, toAPIEvent(events[i]))
	}
	h.writeJSON(w, http.StatusOK, management.EventPage{Events: out, Total: total})
}
