package http

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
	"github.com/ulbwa/tgwebdav/internal/service"
	"github.com/ulbwa/tgwebdav/internal/service/webdavfs"
)

// TestStatusForError covers every sentinel→HTTP-status mapping in
// statusForError, including the wrapped-error case (errors.Is must unwrap) and
// the nil / default fallbacks.
func TestStatusForError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, http.StatusOK},
		{"not found", repository.ErrNotFound, http.StatusNotFound},
		{"already exists", repository.ErrAlreadyExists, http.StatusConflict},
		{"handler conflict", ErrConflict, http.StatusConflict},
		{"unauthorized", service.ErrUnauthorized, http.StatusUnauthorized},
		{"forbidden", service.ErrForbidden, http.StatusForbidden},
		{"invalid path", ErrInvalidPath, http.StatusBadRequest},
		{"not dir", ErrNotDir, http.StatusBadRequest},
		{"is dir", ErrIsDir, http.StatusBadRequest},
		{"not empty", ErrNotEmpty, http.StatusBadRequest},
		{"quota exceeded", webdavfs.ErrQuotaExceeded, http.StatusInsufficientStorage},
		{"file too large", ErrFileTooLarge, http.StatusRequestEntityTooLarge},
		{"rate limited", ErrRateLimited, http.StatusTooManyRequests},
		{"no bot", service.ErrNoBot, http.StatusServiceUnavailable},
		{"blob unavailable", service.ErrBlobUnavailable, http.StatusServiceUnavailable},
		{"unknown", errors.New("something else"), http.StatusInternalServerError},
		// Wrapped error: errors.Is must still resolve the mapping.
		{"wrapped not found", fmt.Errorf("get user: %w", repository.ErrNotFound), http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := statusForError(c.err); got != c.want {
				t.Errorf("statusForError(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// TestToAPIStatPointAndEvent covers the StatPoint and Event mappers directly,
// since the in-memory fixtures cannot easily seed pre-existing rows.
func TestToAPIStatPointAndEvent(t *testing.T) {
	ts := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	sp := toAPIStatPoint(model.StatSample{TS: ts, Metric: "read_bytes", Label: "primary", Value: 1024})
	if sp.Metric != "read_bytes" || sp.Label != "primary" || sp.Value != 1024 || !sp.Ts.Equal(ts) {
		t.Fatalf("toAPIStatPoint mapped wrong: %+v", sp)
	}

	id := uuid.New()
	ev := toAPIEvent(model.Event{ID: id, TS: ts, Kind: "bot_disabled", Message: "m", Ref: "r"})
	if ev.Id != id || ev.Kind != "bot_disabled" || ev.Message != "m" || ev.Ref != "r" || !ev.Ts.Equal(ts) {
		t.Fatalf("toAPIEvent mapped wrong: %+v", ev)
	}
}

// TestDerefHelpers covers both branches of the pointer-deref helpers.
func TestDerefHelpers(t *testing.T) {
	if derefBool(nil) != false {
		t.Error("derefBool(nil) should be false")
	}
	b := true
	if derefBool(&b) != true {
		t.Error("derefBool(&true) should be true")
	}
	if derefInt64(nil) != 0 {
		t.Error("derefInt64(nil) should be 0")
	}
	v := int64(7)
	if derefInt64(&v) != 7 {
		t.Error("derefInt64(&7) should be 7")
	}
	if derefInt32(nil) != 0 {
		t.Error("derefInt32(nil) should be 0")
	}
	w := int32(9)
	if derefInt32(&w) != 9 {
		t.Error("derefInt32(&9) should be 9")
	}
}
