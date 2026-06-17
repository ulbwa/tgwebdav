package main

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/ulbwa/tgwebdav/internal/config"
)

// logLevelVar controls the global slog level at runtime. It defaults to INFO
// and is lowered/raised once the configuration is resolved (setLogLevel), so the
// handler installed in init() can be reconfigured without rebuilding it.
var logLevelVar = new(slog.LevelVar)

// rootCmd is the base command. Running the binary with no subcommand defaults to
// the server (its RunE delegates to serverCmd). The persistent flags registered
// here (config.AddFlags) are visible to every subcommand, so both `server` and
// `migrate` resolve DSN and the .env file the same way.
var rootCmd = &cobra.Command{
	Use:           "tgwebdav",
	Short:         "WebDAV server backed by Telegram channels",
	Long:          "A single binary serving a per-user WebDAV namespace and an admin Management API over one PostgreSQL database, packing file content into Telegram channel blobs.",
	SilenceUsage:  true,
	SilenceErrors: false,
	// With no subcommand, behave like `server`.
	RunE: func(cmd *cobra.Command, args []string) error {
		return serverCmd.RunE(cmd, args)
	},
}

// Execute runs the root command. main exits non-zero on a returned error.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	// Rule 3: a JSON slog handler to stderr, level-controlled by logLevelVar,
	// installed as the process default before any command runs.
	logLevelVar.Set(slog.LevelInfo)
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevelVar})
	slog.SetDefault(slog.New(handler))

	config.AddFlags(rootCmd.PersistentFlags())

	rootCmd.AddCommand(serverCmd, migrateCmd)
}

// setLogLevel applies the resolved configuration's log level to the live handler
// installed in init(). Unknown values fall back to INFO.
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

// loadConfig handles the shared bootstrap both subcommands perform: load the
// optional .env file, resolve the configuration from flags + env, then apply the
// resolved log level to the global logger.
func loadConfig(cmd *cobra.Command) (*config.Config, error) {
	envFile, _ := cmd.Flags().GetString("env-file")
	config.LoadDotenv(envFile)
	cfg, err := config.Resolve(cmd.Flags())
	if err != nil {
		return nil, err
	}
	setLogLevel(cfg.LogLevel)
	return cfg, nil
}
