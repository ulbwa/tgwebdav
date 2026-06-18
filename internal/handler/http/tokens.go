package http

import (
	"net/http"

	"github.com/samber/lo"

	"github.com/ulbwa/tgwebdav/internal/model"
	management "github.com/ulbwa/tgwebdav/pkg/openapi/management"
)

// ListUserTokens handles GET /api/v1/users/{userId}/tokens. The service verifies
// the user exists first, so an unknown user yields 404 rather than an empty list.
func (h *Handler) ListUserTokens(w http.ResponseWriter, r *http.Request, userId management.UserId) {
	tokens, err := h.users.ListTokens(r.Context(), userId)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := lo.Map(tokens, func(t model.APIToken, _ int) management.APIToken { return toAPIToken(&t) })
	h.writeJSON(w, http.StatusOK, out)
}

// CreateUserToken handles POST /api/v1/users/{userId}/tokens. The plaintext
// token is returned exactly once; only its sha-256 hash is persisted.
func (h *Handler) CreateUserToken(w http.ResponseWriter, r *http.Request, userId management.UserId) {
	var body management.CreateUserTokenJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if body.Name == "" {
		h.badRequest(w, "name is required")
		return
	}

	plaintext, tok, err := h.users.CreateToken(r.Context(), userId, body.Name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, management.CreatedToken{
		Id:        tok.ID,
		UserId:    tok.UserID,
		Name:      tok.Name,
		Token:     plaintext,
		CreatedAt: tok.CreatedAt,
	})
}

// DeleteUserToken handles DELETE /api/v1/users/{userId}/tokens/{tokenId}. The
// token is deleted only if it belongs to userId; a token id under the wrong user
// path yields 404 (enforced by the service) rather than deleting it.
func (h *Handler) DeleteUserToken(w http.ResponseWriter, r *http.Request, userId management.UserId, tokenId management.TokenId) {
	if err := h.users.DeleteToken(r.Context(), userId, tokenId); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
