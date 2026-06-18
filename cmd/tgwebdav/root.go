package main

import (
	"github.com/spf13/cobra"
)

// rootCmd is the base command. Running the binary with no subcommand defaults to
// the server (its RunE delegates to serverCmd). The persistent flags registered
// here are visible to every subcommand, so both `server` and `migrate` resolve
// the DSN, the .env file and the log level the same way.
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
	initLogger()

	// Persistent flags shared by every command.
	pf := rootCmd.PersistentFlags()
	pf.String("env-file", ".env", ".env file to load before resolving configuration")
	pf.String("log-level", "info", "log level: debug|info|warn|error (env TGWEBDAV_LOG_LEVEL)")
	pf.String("dsn", "", "PostgreSQL DSN (env TGWEBDAV_DSN)")

	// Local flags for the server command only.
	sf := serverCmd.Flags()
	sf.String("webdav-addr", ":8080", "WebDAV listen address (env TGWEBDAV_WEBDAV_ADDR)")
	sf.String("mgmt-addr", ":8081", "Management API listen address (env TGWEBDAV_MGMT_ADDR)")
	sf.String("cache-dir", "", "blob cache directory; default user cache dir (env TGWEBDAV_CACHE_DIR)")
	sf.String("cache-size", "1GiB", "blob cache size, e.g. 512MiB, 2GiB (env TGWEBDAV_CACHE_SIZE)")
	sf.String("first-user", "", "bootstrap admin as login:password (env TGWEBDAV_FIRST_USER)")
	sf.String("secret-key", "", "secret used to derive the key encrypting bot tokens (env TGWEBDAV_SECRET_KEY)")

	rootCmd.AddCommand(serverCmd, migrateCmd)
}

// resolveLogLevel reads the --log-level flag / TGWEBDAV_LOG_LEVEL env (after the
// .env file has been loaded) and applies it to the global logger. Both commands
// call it so the shared log level behaves identically regardless of subcommand.
func resolveLogLevel(cmd *cobra.Command) {
	v, err := newViper(cmd)
	if err != nil {
		return
	}
	setLogLevel(v.GetString("log-level"))
}
