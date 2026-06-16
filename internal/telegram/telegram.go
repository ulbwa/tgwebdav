// Package telegram implements domain.TelegramAPI over the official Telegram
// Bot API using only net/http. Every call is scoped to a specific *domain.Bot
// so the client can enforce per-bot request pacing and surface the typed
// errors the rest of tgwebdav expects (*domain.RateLimitError,
// domain.ErrTelegramNotFound, domain.ErrTelegramForbidden).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

// defaultBaseURL is the public Telegram Bot API endpoint.
const defaultBaseURL = "https://api.telegram.org"

// minInterval is the minimum spacing enforced between two requests issued for
// the same bot. Telegram tolerates roughly 30 requests/second per bot; 34ms
// keeps us comfortably under that ceiling.
const minInterval = 34 * time.Millisecond

// Client is a net/http-backed implementation of domain.TelegramAPI.
//
// The zero value is not ready for use; construct it with New. BaseURL and HTTP
// are exported so tests (and callers) can point the client at an httptest
// server or supply a customized transport before any request is made.
type Client struct {
	// BaseURL is the API root, e.g. https://api.telegram.org. No trailing slash.
	BaseURL string
	// HTTP is the underlying HTTP client. It carries no overall timeout; request
	// deadlines are governed by the per-call context.
	HTTP *http.Client

	logger *slog.Logger

	mu   sync.Mutex
	bots map[uuid.UUID]*botState
}

// botState tracks the timestamp of the last request issued for a single bot so
// the client can space subsequent requests by at least minInterval.
type botState struct {
	mu       sync.Mutex
	lastSent time.Time
}

// New constructs a Client with the default Bot API base URL and an HTTP client
// that relies on the request context for cancellation (no global timeout).
func New(logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		BaseURL: defaultBaseURL,
		HTTP: &http.Client{
			Transport: http.DefaultTransport,
		},
		logger: logger,
		bots:   make(map[uuid.UUID]*botState),
	}
}

// stateFor returns (creating if needed) the pacing state for a bot.
func (c *Client) stateFor(id uuid.UUID) *botState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.bots[id]
	if !ok {
		st = &botState{}
		c.bots[id] = st
	}
	return st
}

// pace blocks until at least minInterval has elapsed since the last request for
// this bot, honoring ctx cancellation. It then records the new send time.
func (c *Client) pace(ctx context.Context, bot *domain.Bot) error {
	st := c.stateFor(bot.ID)
	st.mu.Lock()
	defer st.mu.Unlock()

	now := time.Now()
	if !st.lastSent.IsZero() {
		wait := minInterval - now.Sub(st.lastSent)
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	st.lastSent = time.Now()
	return nil
}

// baseURL returns the configured base URL or the default if unset.
func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultBaseURL
}

// httpClient returns the configured HTTP client or a context-driven default.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Transport: http.DefaultTransport}
}

// apiResponse is the standard Telegram envelope wrapping every method result.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *responseParams `json:"parameters"`
}

// responseParams carries the optional retry/migration hints on an error.
type responseParams struct {
	RetryAfter      int   `json:"retry_after"`
	MigrateToChatID int64 `json:"migrate_to_chat_id"`
}

// documentResult is the subset of a Message we care about: the message id and,
// for documents, the file identifiers needed to re-fetch the bytes later.
type documentResult struct {
	MessageID int64 `json:"message_id"`
	Document  *struct {
		FileID       string `json:"file_id"`
		FileUniqueID string `json:"file_unique_id"`
	} `json:"document"`
}

// getMeResult is the subset of a User returned by getMe.
type getMeResult struct {
	Username string `json:"username"`
}

// getChatResult is the subset of a Chat returned by getChat.
type getChatResult struct {
	Title string `json:"title"`
}

// getFileResult is the subset of a File returned by getFile.
type getFileResult struct {
	FilePath string `json:"file_path"`
}

// methodURL builds the full URL for a Bot API method call.
func (c *Client) methodURL(bot *domain.Bot, method string) string {
	return fmt.Sprintf("%s/bot%s/%s", c.baseURL(), bot.Token, method)
}

// do paces the bot, performs the HTTP request, decodes the standard envelope
// and returns the raw result on success or a typed error on failure. The
// caller supplies a ready-to-send *http.Request (body and headers already set);
// do attaches ctx and issues it.
func (c *Client) do(ctx context.Context, bot *domain.Bot, req *http.Request) (json.RawMessage, error) {
	if err := c.pace(ctx, bot); err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("telegram: read response body: %w", err)
	}

	var env apiResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("telegram: decode response (status %d): %w", resp.StatusCode, err)
	}

	if !env.OK {
		return nil, c.mapError(env)
	}
	return env.Result, nil
}

// mapError translates a failed Telegram envelope into a typed domain error.
//
//   - HTTP 429 (or a retry_after hint) → *domain.RateLimitError.
//   - "not found" style descriptions   → domain.ErrTelegramNotFound.
//   - HTTP 403                         → domain.ErrTelegramForbidden.
//   - anything else                    → a wrapped error carrying the code and
//     description for diagnostics.
func (c *Client) mapError(env apiResponse) error {
	if env.ErrorCode == 429 || (env.Parameters != nil && env.Parameters.RetryAfter > 0) {
		retry := time.Duration(0)
		if env.Parameters != nil {
			retry = time.Duration(env.Parameters.RetryAfter) * time.Second
		}
		return &domain.RateLimitError{RetryAfter: retry}
	}

	desc := strings.ToLower(env.Description)
	if strings.Contains(desc, "not found") ||
		strings.Contains(desc, "message to forward not found") ||
		strings.Contains(desc, "message_id_invalid") ||
		strings.Contains(desc, "wrong file_id") ||
		strings.Contains(desc, "wrong remote file identifier") {
		return fmt.Errorf("%w: %s", domain.ErrTelegramNotFound, env.Description)
	}

	if env.ErrorCode == 403 {
		return fmt.Errorf("%w: %s", domain.ErrTelegramForbidden, env.Description)
	}

	return fmt.Errorf("telegram: api error %d: %s", env.ErrorCode, env.Description)
}

// GetMe validates the bot token and returns the bot's username.
func (c *Client) GetMe(ctx context.Context, bot *domain.Bot) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.methodURL(bot, "getMe"), nil)
	if err != nil {
		return "", fmt.Errorf("telegram: build getMe request: %w", err)
	}
	raw, err := c.do(ctx, bot, req)
	if err != nil {
		return "", err
	}
	var res getMeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("telegram: decode getMe result: %w", err)
	}
	return res.Username, nil
}

// GetChat reports whether the bot can access chatID and, if so, the chat title.
//
// A definitive not-found or forbidden response is reported as (title="",
// member=false, err=nil) so callers can record the membership matrix without
// treating an expected "not a member" as a fault. Transport-level failures are
// returned as ("", false, err).
func (c *Client) GetChat(ctx context.Context, bot *domain.Bot, chatID int64) (string, bool, error) {
	form := url.Values{}
	form.Set("chat_id", strconv.FormatInt(chatID, 10))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(bot, "getChat"),
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", false, fmt.Errorf("telegram: build getChat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	raw, err := c.do(ctx, bot, req)
	if err != nil {
		if errors.Is(err, domain.ErrTelegramNotFound) ||
			errors.Is(err, domain.ErrTelegramForbidden) {
			return "", false, nil
		}
		return "", false, err
	}
	var res getChatResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, fmt.Errorf("telegram: decode getChat result: %w", err)
	}
	return res.Title, true, nil
}

// SendDocument uploads raw bytes as a document (multipart/form-data) and
// returns the new message id together with the document's file identifiers.
func (c *Client) SendDocument(ctx context.Context, bot *domain.Bot, chatID int64, filename string, data []byte) (domain.TGSendResult, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	if err := mw.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return domain.TGSendResult{}, fmt.Errorf("telegram: write chat_id field: %w", err)
	}
	part, err := mw.CreateFormFile("document", filename)
	if err != nil {
		return domain.TGSendResult{}, fmt.Errorf("telegram: create document part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return domain.TGSendResult{}, fmt.Errorf("telegram: write document bytes: %w", err)
	}
	if err := mw.Close(); err != nil {
		return domain.TGSendResult{}, fmt.Errorf("telegram: finalize multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(bot, "sendDocument"), &body)
	if err != nil {
		return domain.TGSendResult{}, fmt.Errorf("telegram: build sendDocument request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	raw, err := c.do(ctx, bot, req)
	if err != nil {
		return domain.TGSendResult{}, err
	}
	return decodeSendResult(raw)
}

// SendByFileID re-posts an already-uploaded document by its file_id, avoiding a
// fresh byte upload. Returns the new message and (echoed) file identifiers.
func (c *Client) SendByFileID(ctx context.Context, bot *domain.Bot, chatID int64, fileID string) (domain.TGSendResult, error) {
	form := url.Values{}
	form.Set("chat_id", strconv.FormatInt(chatID, 10))
	form.Set("document", fileID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(bot, "sendDocument"),
		strings.NewReader(form.Encode()))
	if err != nil {
		return domain.TGSendResult{}, fmt.Errorf("telegram: build sendDocument(file_id) request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	raw, err := c.do(ctx, bot, req)
	if err != nil {
		return domain.TGSendResult{}, err
	}
	return decodeSendResult(raw)
}

// ForwardMessage forwards an existing message into toChatID and returns the
// forwarded copy's message id plus a fresh, bot-scoped file_id for its
// document. This is the cross-bot recovery path for stale file_ids.
func (c *Client) ForwardMessage(ctx context.Context, bot *domain.Bot, toChatID, fromChatID, messageID int64) (domain.TGSendResult, error) {
	form := url.Values{}
	form.Set("chat_id", strconv.FormatInt(toChatID, 10))
	form.Set("from_chat_id", strconv.FormatInt(fromChatID, 10))
	form.Set("message_id", strconv.FormatInt(messageID, 10))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(bot, "forwardMessage"),
		strings.NewReader(form.Encode()))
	if err != nil {
		return domain.TGSendResult{}, fmt.Errorf("telegram: build forwardMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	raw, err := c.do(ctx, bot, req)
	if err != nil {
		return domain.TGSendResult{}, err
	}
	return decodeSendResult(raw)
}

// DeleteMessage removes a message best-effort. A "message to delete not found"
// style response is treated as success (the message is already gone).
func (c *Client) DeleteMessage(ctx context.Context, bot *domain.Bot, chatID, messageID int64) error {
	form := url.Values{}
	form.Set("chat_id", strconv.FormatInt(chatID, 10))
	form.Set("message_id", strconv.FormatInt(messageID, 10))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(bot, "deleteMessage"),
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("telegram: build deleteMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	_, err = c.do(ctx, bot, req)
	if err != nil {
		// Already gone is success for a best-effort delete.
		if errors.Is(err, domain.ErrTelegramNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// DownloadFile resolves fileID via getFile and downloads the underlying bytes
// from the file CDN endpoint (<BaseURL>/file/bot<token>/<file_path>).
func (c *Client) DownloadFile(ctx context.Context, bot *domain.Bot, fileID string) ([]byte, error) {
	form := url.Values{}
	form.Set("file_id", fileID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(bot, "getFile"),
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("telegram: build getFile request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	raw, err := c.do(ctx, bot, req)
	if err != nil {
		return nil, err
	}
	var fr getFileResult
	if err := json.Unmarshal(raw, &fr); err != nil {
		return nil, fmt.Errorf("telegram: decode getFile result: %w", err)
	}
	if fr.FilePath == "" {
		return nil, fmt.Errorf("%w: empty file_path", domain.ErrTelegramNotFound)
	}

	return c.downloadPath(ctx, bot, fr.FilePath)
}

// downloadPath fetches the raw bytes for a resolved file_path. A 404 from the
// CDN is mapped to domain.ErrTelegramNotFound; other non-200 responses become a
// wrapped error.
func (c *Client) downloadPath(ctx context.Context, bot *domain.Bot, filePath string) ([]byte, error) {
	dlURL := fmt.Sprintf("%s/file/bot%s/%s", c.baseURL(), bot.Token, filePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram: build download request: %w", err)
	}

	if err := c.pace(ctx, bot); err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: file path %q", domain.ErrTelegramNotFound, filePath)
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("telegram: download status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("telegram: read downloaded bytes: %w", err)
	}
	return data, nil
}

// decodeSendResult parses a Message result (sendDocument/forwardMessage) into a
// TGSendResult. A missing document object is tolerated (file ids left empty).
func decodeSendResult(raw json.RawMessage) (domain.TGSendResult, error) {
	var msg documentResult
	if err := json.Unmarshal(raw, &msg); err != nil {
		return domain.TGSendResult{}, fmt.Errorf("telegram: decode message result: %w", err)
	}
	res := domain.TGSendResult{MessageID: msg.MessageID}
	if msg.Document != nil {
		res.FileID = msg.Document.FileID
		res.FileUniqueID = msg.Document.FileUniqueID
	}
	return res, nil
}

// Ensure *Client satisfies the domain port at compile time.
var _ domain.TelegramAPI = (*Client)(nil)
