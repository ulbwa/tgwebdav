package main

import (
	"log/slog"
	"os"
)

// logLevelVar controls the global slog level at runtime. It defaults to INFO and
// is lowered/raised once the configuration is resolved (setLogLevel), so the
// handler installed by initLogger can be reconfigured without rebuilding it.
var logLevelVar = new(slog.LevelVar)

// initLogger installs a JSON slog handler to stderr as the process default,
// level-controlled by logLevelVar, before any command runs.
func initLogger() {
	logLevelVar.Set(slog.LevelInfo)
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevelVar})
	slog.SetDefault(slog.New(handler))
}

// setLogLevel applies the resolved configuration's log level to the live handler
// installed by initLogger. Unknown values fall back to INFO.
func setLogLevel(level string) {
	switch level {
	case "debug":
		logLevelVar.Set(slog.LevelDebug)
	case "warn":
		logLevelVar.Set(slog.LevelWarn)
	case "error":
		logLevelVar.Set(slog.LevelError)
	default:
		logLevelVar.Set(slog.LevelInfo)
	}
}
