package telegram

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// newTestClient builds a Client pointed at srv with pacing effectively disabled
// (the default 34ms interval would only matter across same-bot calls; tests use
// a fresh bot each time anyway).
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := New(nil)
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()
	return c
}

func testBot() *model.Bot {
	return &model.Bot{ID: uuid.New(), Username: "test_bot", Token: "TOKEN", Enabled: true}
}

func TestSendDocumentSuccess(t *testing.T) {
	var gotFilename string
	var gotChatID string
	var gotBytes []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendDocument") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Fatalf("expected multipart content type, got %q (%v)", r.Header.Get("Content-Type"), err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read part: %v", err)
			}
			data, _ := io.ReadAll(part)
			switch part.FormName() {
			case "chat_id":
				gotChatID = string(data)
			case "document":
				gotFilename = part.FileName()
				gotBytes = data
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":{"message_id":4242,"document":{"file_id":"FID","file_unique_id":"FUID"}}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.SendDocument(context.Background(), testBot(), -1001234567890, "blob.bin", []byte("hello bytes"))
	if err != nil {
		t.Fatalf("SendDocument: %v", err)
	}
	if res.MessageID != 4242 {
		t.Errorf("MessageID = %d, want 4242", res.MessageID)
	}
	if res.FileID != "FID" || res.FileUniqueID != "FUID" {
		t.Errorf("file ids = %q/%q, want FID/FUID", res.FileID, res.FileUniqueID)
	}
	if gotChatID != "-1001234567890" {
		t.Errorf("server saw chat_id %q", gotChatID)
	}
	if gotFilename != "blob.bin" {
		t.Errorf("server saw filename %q", gotFilename)
	}
	if string(gotBytes) != "hello bytes" {
		t.Errorf("server saw body %q", string(gotBytes))
	}
}

func TestSendDocumentRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 17","parameters":{"retry_after":17}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.SendDocument(context.Background(), testBot(), -100123, "x.bin", []byte("x"))
	if err == nil {
		t.Fatal("expected error")
	}
	var rl *model.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error = %v (%T), want *model.RateLimitError", err, err)
	}
	if rl.RetryAfter != 17*time.Second {
		t.Errorf("RetryAfter = %s, want 17s", rl.RetryAfter)
	}
}

func TestDownloadFileNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getFile") {
			t.Errorf("expected getFile call, got %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: file not found"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.DownloadFile(context.Background(), testBot(), "STALE_FID")
	if !errors.Is(err, model.ErrTelegramNotFound) {
		t.Fatalf("error = %v, want ErrTelegramNotFound", err)
	}
}

func TestGetChatMember(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":{"id":-100999,"title":"Blob Store","type":"channel"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	title, member, err := c.GetChat(context.Background(), testBot(), -100999)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if !member {
		t.Error("member = false, want true")
	}
	if title != "Blob Store" {
		t.Errorf("title = %q, want Blob Store", title)
	}
}

func TestGetChatNotMember(t *testing.T) {
	// 403 forbidden must be reported as (member=false, err=nil).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"ok":false,"error_code":403,"description":"Forbidden: bot is not a member of the channel chat"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	title, member, err := c.GetChat(context.Background(), testBot(), -100999)
	if err != nil {
		t.Fatalf("GetChat returned err %v, want nil", err)
	}
	if member {
		t.Error("member = true, want false")
	}
	if title != "" {
		t.Errorf("title = %q, want empty", title)
	}
}

func TestGetChatNotFound(t *testing.T) {
	// not-found also collapses to (member=false, err=nil).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, member, err := c.GetChat(context.Background(), testBot(), -100999)
	if err != nil {
		t.Fatalf("GetChat returned err %v, want nil", err)
	}
	if member {
		t.Error("member = true, want false")
	}
}

func TestGetChatTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, member, err := c.GetChat(context.Background(), testBot(), -100999)
	if err == nil {
		t.Fatal("expected error for non-membership server fault")
	}
	if member {
		t.Error("member = true, want false")
	}
}

func TestDownloadFileTwoStep(t *testing.T) {
	const wantPath = "documents/file_42.bin"
	const wantBytes = "BINARY-PAYLOAD"

	var sawGetFile, sawDownload bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			sawGetFile = true
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse form: %v", err)
			}
			if r.FormValue("file_id") != "FID42" {
				t.Errorf("getFile file_id = %q", r.FormValue("file_id"))
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true,"result":{"file_id":"FID42","file_path":"`+wantPath+`"}}`)
		case strings.Contains(r.URL.Path, "/file/bot"):
			sawDownload = true
			if !strings.HasSuffix(r.URL.Path, wantPath) {
				t.Errorf("download path = %q, want suffix %q", r.URL.Path, wantPath)
			}
			io.WriteString(w, wantBytes)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	data, err := c.DownloadFile(context.Background(), testBot(), "FID42")
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if !sawGetFile || !sawDownload {
		t.Fatalf("two-step not observed: getFile=%v download=%v", sawGetFile, sawDownload)
	}
	if string(data) != wantBytes {
		t.Errorf("data = %q, want %q", string(data), wantBytes)
	}
}

func TestForwardMessageRecoversFileID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/forwardMessage") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.FormValue("message_id") != "555" {
			t.Errorf("message_id = %q, want 555", r.FormValue("message_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":{"message_id":777,"document":{"file_id":"FRESH","file_unique_id":"U"}}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.ForwardMessage(context.Background(), testBot(), -100222, -100111, 555)
	if err != nil {
		t.Fatalf("ForwardMessage: %v", err)
	}
	if res.MessageID != 777 || res.FileID != "FRESH" {
		t.Errorf("result = %+v, want message_id=777 file_id=FRESH", res)
	}
}

func TestDeleteMessageAlreadyGoneIsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to delete not found"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := c.DeleteMessage(context.Background(), testBot(), -100222, 9); err != nil {
		t.Fatalf("DeleteMessage of a gone message = %v, want nil", err)
	}
}

func TestGetMeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getMe") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"my_blob_bot"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	name, err := c.GetMe(context.Background(), testBot())
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if name != "my_blob_bot" {
		t.Errorf("username = %q, want my_blob_bot", name)
	}
}

func TestForbiddenMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.SendByFileID(context.Background(), testBot(), -100222, "FID")
	if !errors.Is(err, model.ErrTelegramForbidden) {
		t.Fatalf("error = %v, want ErrTelegramForbidden", err)
	}
}

func TestPerBotPacing(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"b"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	bot := testBot()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.GetMe(context.Background(), bot); err != nil {
			t.Fatalf("GetMe #%d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	// Three same-bot calls must be spaced by >= 2*minInterval total.
	if elapsed < 2*minInterval {
		t.Errorf("3 same-bot calls took %s, expected at least %s", elapsed, 2*minInterval)
	}
	if hits != 3 {
		t.Errorf("server hits = %d, want 3", hits)
	}
}

func TestPaceHonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"b"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	bot := testBot()

	// Prime lastSent so the next call must wait out minInterval.
	if _, err := c.GetMe(context.Background(), bot); err != nil {
		t.Fatalf("priming GetMe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetMe(ctx, bot); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestRedactTokenScrubsURLError(t *testing.T) {
	const token = "123456:AAsecretTOKENvalue"
	ue := &url.Error{Op: "Get", URL: "https://api.telegram.org/bot" + token + "/getMe", Err: context.Canceled}
	got := redactToken(ue, token)
	if strings.Contains(got.Error(), token) {
		t.Fatalf("token leaked in error: %q", got.Error())
	}
	if !strings.Contains(got.Error(), "<redacted>") {
		t.Errorf("expected <redacted> placeholder, got %q", got.Error())
	}
	// The wrapped error identity must be preserved for errors.Is checks.
	if !errors.Is(got, context.Canceled) {
		t.Errorf("redactToken broke the error chain (errors.Is context.Canceled failed)")
	}
}

func TestRedactTokenPlainError(t *testing.T) {
	const token = "999:ZZtok"
	got := redactToken(errors.New("dial tcp via bot"+token+" failed"), token)
	if strings.Contains(got.Error(), token) {
		t.Fatalf("token leaked: %q", got.Error())
	}
}
