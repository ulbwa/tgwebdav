package main

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/ulbwa/tgwebdav/db/migrations"
)

// migrateCmd applies all pending database migrations and exits. It uses the same
// embedded migration runner the server runs on boot, so `tgwebdav migrate` and
// the auto-migrate-on-start path are identical.
var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database migrations",
	Long:  "Apply all pending embedded database migrations against the configured DSN and exit.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig(cmd)
		if err != nil {
			return err
		}
		return migrations.Run(cfg.DSN, slog.Default())
	},
}
