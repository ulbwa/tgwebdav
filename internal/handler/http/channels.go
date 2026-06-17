package http

import (
	"net/http"

	management "github.com/ulbwa/tgwebdav/pkg/openapi/management"
)

// ListChannels handles GET /api/v1/channels.
func (h *Handler) ListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := h.channels.List(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]management.Channel, 0, len(channels))
	for i := range channels {
		out = append(out, toAPIChannel(&channels[i]))
	}
	h.writeJSON(w, http.StatusOK, out)
}

// AddChannel handles POST /api/v1/channels.
func (h *Handler) AddChannel(w http.ResponseWriter, r *http.Request) {
	var body management.AddChannelJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if body.BareId == 0 {
		h.badRequest(w, "bare_id is required")
		return
	}
	ch, err := h.channels.Add(r.Context(), body.BareId)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, toAPIChannel(ch))
}

// DeleteChannel handles DELETE /api/v1/channels/{channelId}.
func (h *Handler) DeleteChannel(w http.ResponseWriter, r *http.Request, channelId management.ChannelId) {
	if err := h.channels.Remove(r.Context(), channelId); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetChannelEviction handles PUT /api/v1/channels/{channelId}/eviction.
func (h *Handler) SetChannelEviction(w http.ResponseWriter, r *http.Request, channelId management.ChannelId) {
	var body management.SetChannelEvictionJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if body.Threshold < 0 {
		h.badRequest(w, "threshold must be non-negative")
		return
	}
	if err := h.channels.SetEvictionThreshold(r.Context(), channelId, body.Threshold); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
