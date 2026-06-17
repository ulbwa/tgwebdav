package database

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/tracelog"
)

// slogTracer adapts pgx's tracelog.Logger onto an *slog.Logger. Every SQL trace
// event pgx emits is forwarded at DEBUG with the trace data attached as
// structured attributes, so query logging rides the project's standard logger
// (Rule 7). Wire it onto a pool with
//
//	cfg.ConnConfig.Tracer = &tracelog.TraceLog{
//		Logger:   NewSlogTracer(logger),
//		LogLevel: tracelog.LogLevelDebug,
//	}
type slogTracer struct{ logger *slog.Logger }

// NewSlogTracer returns a tracelog.Logger that forwards pgx trace events to
// logger at DEBUG. A nil logger falls back to slog.Default.
func NewSlogTracer(logger *slog.Logger) tracelog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return slogTracer{logger: logger.With("component", "pgx")}
}

// Log implements tracelog.Logger. The pgx LogLevel is ignored for severity (the
// TraceLog wrapper already gates which events reach us via its own LogLevel);
// everything that arrives is logged at DEBUG with the event data as attrs.
func (t slogTracer) Log(ctx context.Context, _ tracelog.LogLevel, msg string, data map[string]any) {
	attrs := make([]any, 0, len(data)*2)
	for k, v := range data {
		attrs = append(attrs, k, v)
	}
	t.logger.DebugContext(ctx, msg, attrs...)
}
