package database

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/tracelog"
)

// TestSlogTracerForwardsEventAtDebug verifies the slog adapter forwards a pgx
// trace event to the underlying logger at DEBUG, carrying the message and the
// event data as structured attributes, scoped to the pgx component.
func TestSlogTracerForwardsEventAtDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	tr := NewSlogTracer(logger)
	tr.Log(context.Background(), tracelog.LogLevelInfo, "Query", map[string]any{
		"sql":  "SELECT 1",
		"args": []any{42},
	})

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("trace line is not valid JSON: %v\n%s", err, buf.String())
	}
	if line["level"] != "DEBUG" {
		t.Errorf("level = %v, want DEBUG", line["level"])
	}
	if line["msg"] != "Query" {
		t.Errorf("msg = %v, want Query", line["msg"])
	}
	if line["sql"] != "SELECT 1" {
		t.Errorf("sql attr = %v, want SELECT 1", line["sql"])
	}
	if line["component"] != "pgx" {
		t.Errorf("component = %v, want pgx", line["component"])
	}
}

// TestSlogTracerNilLoggerDoesNotPanic verifies a nil logger falls back to
// slog.Default rather than panicking.
func TestSlogTracerNilLoggerDoesNotPanic(t *testing.T) {
	tr := NewSlogTracer(nil)
	// Must not panic even with no attributes.
	tr.Log(context.Background(), tracelog.LogLevelDebug, "Connect", nil)
}
