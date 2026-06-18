package http

import (
	"net/http"

	"github.com/samber/lo"

	"github.com/ulbwa/tgwebdav/internal/model"
	management "github.com/ulbwa/tgwebdav/pkg/openapi/management"
)

// ListBots handles GET /api/v1/bots.
func (h *Handler) ListBots(w http.ResponseWriter, r *http.Request) {
	bots, err := h.bots.List(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := lo.Map(bots, func(b model.Bot, _ int) management.Bot { return toAPIBot(&b) })
	h.writeJSON(w, http.StatusOK, out)
}

// AddBot handles POST /api/v1/bots.
func (h *Handler) AddBot(w http.ResponseWriter, r *http.Request) {
	var body management.AddBotJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if body.Token == "" {
		h.badRequest(w, "token is required")
		return
	}
	bot, err := h.bots.Add(r.Context(), body.Token)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, toAPIBot(bot))
}

// DeleteBot handles DELETE /api/v1/bots/{botId}.
func (h *Handler) DeleteBot(w http.ResponseWriter, r *http.Request, botId management.BotId) {
	if err := h.bots.Remove(r.Context(), botId); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetBotEnabled handles PUT /api/v1/bots/{botId}/enabled.
func (h *Handler) SetBotEnabled(w http.ResponseWriter, r *http.Request, botId management.BotId) {
	var body management.SetBotEnabledJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if err := h.bots.SetEnabled(r.Context(), botId, body.Enabled); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
