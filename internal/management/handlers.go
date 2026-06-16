// Package management implements the OpenAPI-first administrative REST API for
// tgwebdav. All endpoints live under /api/v1 and require an administrator
// (HTTP Basic with an is_admin user, or a Bearer token belonging to one);
// GET /healthz is public. The HTTP routing and request/response models are
// generated from openapi.yaml into the sibling `api` package; this file wires
// those generated handlers onto the domain services and repositories.
package management

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
	"github.com/ulbwa/tgwebdav/internal/management/api"
)

// Deps bundles everything the Management handlers need. It mirrors the wiring
// contract so cmd can construct it without edits.
type Deps struct {
	Repos    *domain.Repositories
	Tx       domain.TxManager
	Auth     domain.AuthService
	Bots     domain.BotService
	Channels domain.ChannelService
	Settings domain.SettingsService
	Logger   *slog.Logger
}

// Handlers implements the generated api.ServerInterface backed by the domain
// services and repositories.
type Handlers struct {
	repos    *domain.Repositories
	tx       domain.TxManager
	auth     domain.AuthService
	bots     domain.BotService
	channels domain.ChannelService
	settings domain.SettingsService
	logger   *slog.Logger
}

// compile-time assertion that *Handlers satisfies the generated interface.
var _ api.ServerInterface = (*Handlers)(nil)

// NewHandlers builds the Management API handlers from the provided dependencies.
func NewHandlers(d Deps) *Handlers {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{
		repos:    d.Repos,
		tx:       d.Tx,
		auth:     d.Auth,
		bots:     d.Bots,
		channels: d.Channels,
		settings: d.Settings,
		logger:   logger,
	}
}

// ---- error / response helpers ---------------------------------------------

// statusForError maps a domain sentinel error onto an HTTP status code.
func statusForError(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrAlreadyExists):
		return http.StatusConflict
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, domain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrInvalidPath),
		errors.Is(err, domain.ErrNotDir),
		errors.Is(err, domain.ErrIsDir),
		errors.Is(err, domain.ErrNotEmpty):
		return http.StatusBadRequest
	case errors.Is(err, domain.ErrQuotaExceeded):
		return http.StatusInsufficientStorage
	case errors.Is(err, domain.ErrFileTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, domain.ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, domain.ErrNoBot),
		errors.Is(err, domain.ErrBlobUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writeJSON serializes v as JSON with the given status code.
func (h *Handlers) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("management: encode response", "error", err)
	}
}

// writeError emits a JSON error body with the status derived from err.
func (h *Handlers) writeError(w http.ResponseWriter, err error) {
	status := statusForError(err)
	if status >= http.StatusInternalServerError {
		h.logger.Error("management: request failed", "error", err, "status", status)
	}
	h.writeJSON(w, status, api.Error{Error: err.Error()})
}

// badRequest emits a 400 with a custom message.
func (h *Handlers) badRequest(w http.ResponseWriter, msg string) {
	h.writeJSON(w, http.StatusBadRequest, api.Error{Error: msg})
}

// decodeBody decodes the JSON request body into dst, rejecting unknown fields.
func decodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// ---- mappers ---------------------------------------------------------------

func toAPIUser(u *domain.User) api.User {
	return api.User{
		Id:           u.ID,
		Login:        u.Login,
		IsAdmin:      u.IsAdmin,
		QuotaBytes:   u.QuotaBytes,
		BandwidthBps: u.BandwidthBPS,
		RatePerMin:   int32(u.RatePerMin),
		CreatedAt:    u.CreatedAt,
	}
}

func toAPIToken(t *domain.APIToken) api.APIToken {
	return api.APIToken{
		Id:         t.ID,
		UserId:     t.UserID,
		Name:       t.Name,
		CreatedAt:  t.CreatedAt,
		LastUsedAt: t.LastUsedAt,
	}
}

func toAPIBot(b *domain.Bot) api.Bot {
	return api.Bot{
		Id:               b.ID,
		Username:         b.Username,
		Enabled:          b.Enabled,
		UnavailableUntil: b.UnavailableUntil,
		CreatedAt:        b.CreatedAt,
	}
}

func toAPIChannel(c *domain.Channel) api.Channel {
	return api.Channel{
		Id:                c.ID,
		TgChatId:          c.TGChatID,
		Title:             c.Title,
		MessageCounter:    c.MessageCounter,
		EvictionThreshold: c.EvictionThreshold,
		Available:         c.Available,
		CreatedAt:         c.CreatedAt,
	}
}

func toAPISettings(s domain.Settings) api.Settings {
	return api.Settings{
		BlobMaxSize:              s.BlobMaxSize,
		WalIdleTimeoutMs:         s.WALIdleTimeout.Milliseconds(),
		MaxFileSize:              s.MaxFileSize,
		DefaultEvictionThreshold: s.DefaultEvictionThreshold,
		UpdatedAt:                s.UpdatedAt,
	}
}

func toAPIStatPoint(s domain.StatSample) api.StatPoint {
	return api.StatPoint{
		Ts:     s.TS,
		Metric: s.Metric,
		Label:  s.Label,
		Value:  s.Value,
	}
}

func toAPIEvent(e domain.Event) api.Event {
	return api.Event{
		Id:      e.ID,
		Ts:      e.TS,
		Kind:    e.Kind,
		Message: e.Message,
		Ref:     e.Ref,
	}
}

// ---- users -----------------------------------------------------------------

// ListUsers handles GET /api/v1/users.
func (h *Handlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.repos.Users.List(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]api.User, 0, len(users))
	for i := range users {
		out = append(out, toAPIUser(&users[i]))
	}
	h.writeJSON(w, http.StatusOK, out)
}

// CreateUser handles POST /api/v1/users.
func (h *Handlers) CreateUser(w http.ResponseWriter, r *http.Request) {
	var body api.CreateUserJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if body.Login == "" || body.Password == "" {
		h.badRequest(w, "login and password are required")
		return
	}

	hash, err := h.auth.HashPassword(body.Password)
	if err != nil {
		h.writeError(w, err)
		return
	}

	u := &domain.User{
		ID:           uuid.New(),
		Login:        body.Login,
		PasswordHash: hash,
		IsAdmin:      derefBool(body.IsAdmin),
		QuotaBytes:   derefInt64(body.QuotaBytes),
		BandwidthBPS: derefInt64(body.BandwidthBps),
		RatePerMin:   int(derefInt32(body.RatePerMin)),
		CreatedAt:    time.Now().UTC(),
	}
	if err := h.repos.Users.Create(r.Context(), u); err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, toAPIUser(u))
}

// GetUser handles GET /api/v1/users/{userId}.
func (h *Handlers) GetUser(w http.ResponseWriter, r *http.Request, userId api.UserId) {
	u, err := h.repos.Users.GetByID(r.Context(), userId)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toAPIUser(u))
}

// DeleteUser handles DELETE /api/v1/users/{userId}.
func (h *Handlers) DeleteUser(w http.ResponseWriter, r *http.Request, userId api.UserId) {
	if err := h.repos.Users.Delete(r.Context(), userId); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetUserPassword handles PUT /api/v1/users/{userId}/password.
func (h *Handlers) SetUserPassword(w http.ResponseWriter, r *http.Request, userId api.UserId) {
	var body api.SetUserPasswordJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if body.Password == "" {
		h.badRequest(w, "password is required")
		return
	}

	ctx := r.Context()
	u, err := h.repos.Users.GetByID(ctx, userId)
	if err != nil {
		h.writeError(w, err)
		return
	}
	hash, err := h.auth.HashPassword(body.Password)
	if err != nil {
		h.writeError(w, err)
		return
	}
	u.PasswordHash = hash
	if err := h.repos.Users.Update(ctx, u); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListUserTokens handles GET /api/v1/users/{userId}/tokens.
func (h *Handlers) ListUserTokens(w http.ResponseWriter, r *http.Request, userId api.UserId) {
	ctx := r.Context()
	if _, err := h.repos.Users.GetByID(ctx, userId); err != nil {
		h.writeError(w, err)
		return
	}
	tokens, err := h.repos.Tokens.ListByUser(ctx, userId)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]api.APIToken, 0, len(tokens))
	for i := range tokens {
		out = append(out, toAPIToken(&tokens[i]))
	}
	h.writeJSON(w, http.StatusOK, out)
}

// CreateUserToken handles POST /api/v1/users/{userId}/tokens. The plaintext
// token is returned exactly once; only its sha-256 hash is persisted.
func (h *Handlers) CreateUserToken(w http.ResponseWriter, r *http.Request, userId api.UserId) {
	var body api.CreateUserTokenJSONRequestBody
	if err := decodeBody(r, &body); err != nil {
		h.badRequest(w, "invalid request body: "+err.Error())
		return
	}
	if body.Name == "" {
		h.badRequest(w, "name is required")
		return
	}

	ctx := r.Context()
	if _, err := h.repos.Users.GetByID(ctx, userId); err != nil {
		h.writeError(w, err)
		return
	}

	plaintext, err := generateToken()
	if err != nil {
		h.writeError(w, err)
		return
	}
	now := time.Now().UTC()
	tok := &domain.APIToken{
		ID:        uuid.New(),
		UserID:    userId,
		TokenHash: hashToken(plaintext),
		Name:      body.Name,
		CreatedAt: now,
	}
	if err := h.repos.Tokens.Create(ctx, tok); err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, api.CreatedToken{
		Id:        tok.ID,
		UserId:    tok.UserID,
		Name:      tok.Name,
		Token:     plaintext,
		CreatedAt: tok.CreatedAt,
	})
}

// DeleteUserToken handles DELETE /api/v1/users/{userId}/tokens/{tokenId}.
func (h *Handlers) DeleteUserToken(w http.ResponseWriter, r *http.Request, userId api.UserId, tokenId api.TokenId) {
	if err := h.repos.Tokens.Delete(r.Context(), tokenId); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- bots ------------------------------------------------------------------

// ListBots handles GET /api/v1/bots.
func (h *Handlers) ListBots(w http.ResponseWriter, r *http.Request) {
	bots, err := h.bots.List(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]api.Bot, 0, len(bots))
	for i := range bots {
		out = append(out, toAPIBot(&bots[i]))
	}
	h.writeJSON(w, http.StatusOK, out)
}

// AddBot handles POST /api/v1/bots.
func (h *Handlers) AddBot(w http.ResponseWriter, r *http.Request) {
	var body api.AddBotJSONRequestBody
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
func (h *Handlers) DeleteBot(w http.ResponseWriter, r *http.Request, botId api.BotId) {
	if err := h.bots.Remove(r.Context(), botId); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetBotEnabled handles PUT /api/v1/bots/{botId}/enabled.
func (h *Handlers) SetBotEnabled(w http.ResponseWriter, r *http.Request, botId api.BotId) {
	var body api.SetBotEnabledJSONRequestBody
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

// ---- channels --------------------------------------------------------------

// ListChannels handles GET /api/v1/channels.
func (h *Handlers) ListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := h.channels.List(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]api.Channel, 0, len(channels))
	for i := range channels {
		out = append(out, toAPIChannel(&channels[i]))
	}
	h.writeJSON(w, http.StatusOK, out)
}

// AddChannel handles POST /api/v1/channels.
func (h *Handlers) AddChannel(w http.ResponseWriter, r *http.Request) {
	var body api.AddChannelJSONRequestBody
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
func (h *Handlers) DeleteChannel(w http.ResponseWriter, r *http.Request, channelId api.ChannelId) {
	if err := h.channels.Remove(r.Context(), channelId); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetChannelEviction handles PUT /api/v1/channels/{channelId}/eviction.
func (h *Handlers) SetChannelEviction(w http.ResponseWriter, r *http.Request, channelId api.ChannelId) {
	var body api.SetChannelEvictionJSONRequestBody
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

// ---- settings --------------------------------------------------------------

// GetSettings handles GET /api/v1/settings.
func (h *Handlers) GetSettings(w http.ResponseWriter, r *http.Request) {
	s, err := h.settings.Get(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toAPISettings(s))
}

// UpdateSettings handles PUT /api/v1/settings. Only the supplied fields are
// changed; omitted fields keep their current value.
func (h *Handlers) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body api.UpdateSettingsJSONRequestBody
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

// ---- stats -----------------------------------------------------------------

// QueryStats handles GET /api/v1/stats.
func (h *Handlers) QueryStats(w http.ResponseWriter, r *http.Request, params api.QueryStatsParams) {
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

	samples, err := h.repos.Stats.Query(r.Context(), params.Metric, label, from, to)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]api.StatPoint, 0, len(samples))
	for i := range samples {
		out = append(out, toAPIStatPoint(samples[i]))
	}
	h.writeJSON(w, http.StatusOK, out)
}

// ---- events ----------------------------------------------------------------

// ListEvents handles GET /api/v1/events.
func (h *Handlers) ListEvents(w http.ResponseWriter, r *http.Request, params api.ListEventsParams) {
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

	events, total, err := h.repos.Events.List(r.Context(), kind, limit, offset)
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]api.Event, 0, len(events))
	for i := range events {
		out = append(out, toAPIEvent(events[i]))
	}
	h.writeJSON(w, http.StatusOK, api.EventPage{Events: out, Total: total})
}

// ---- system ----------------------------------------------------------------

// Healthz handles GET /healthz (public).
func (h *Handlers) Healthz(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, http.StatusOK, api.Health{Status: "ok"})
}

// ---- small helpers ---------------------------------------------------------

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// generateToken returns a cryptographically-random opaque bearer token.
func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// hashToken returns the sha-256 hex digest used to look up a bearer token. It
// must match the scheme used by domain.AuthService.AuthenticateBearer.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
