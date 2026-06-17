package http

import (
	"net/http"

	management "github.com/ulbwa/tgwebdav/pkg/openapi/management"

	"github.com/ulbwa/tgwebdav/internal/service"
)

// ListUsers handles GET /api/v1/users.
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.users.List(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]management.User, 0, len(users))
	for i := range users {
		out = append(out, toAPIUser(&users[i]))
	}
	h.writeJSON(w, http.StatusOK, out)
}

// CreateUser handles POST /api/v1/users.
func (h *Handler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var body management.CreateUserJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if body.Login == "" || body.Password == "" {
		h.badRequest(w, "login and password are required")
		return
	}

	u, err := h.users.Create(r.Context(), service.CreateUserParams{
		Login:        body.Login,
		Password:     body.Password,
		IsAdmin:      derefBool(body.IsAdmin),
		QuotaBytes:   derefInt64(body.QuotaBytes),
		BandwidthBPS: derefInt64(body.BandwidthBps),
		RatePerMin:   int(derefInt32(body.RatePerMin)),
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, toAPIUser(u))
}

// GetUser handles GET /api/v1/users/{userId}.
func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request, userId management.UserId) {
	u, err := h.users.Get(r.Context(), userId)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toAPIUser(u))
}

// DeleteUser handles DELETE /api/v1/users/{userId}.
func (h *Handler) DeleteUser(w http.ResponseWriter, r *http.Request, userId management.UserId) {
	if err := h.users.Delete(r.Context(), userId); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetUserPassword handles PUT /api/v1/users/{userId}/password.
func (h *Handler) SetUserPassword(w http.ResponseWriter, r *http.Request, userId management.UserId) {
	var body management.SetUserPasswordJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if body.Password == "" {
		h.badRequest(w, "password is required")
		return
	}
	if err := h.users.SetPassword(r.Context(), userId, body.Password); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
