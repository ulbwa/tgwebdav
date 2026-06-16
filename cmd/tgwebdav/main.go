// Command tgwebdav runs the WebDAV-over-Telegram server: a single binary that
// serves a per-user WebDAV namespace and an admin Management API over one
// PostgreSQL database, buffering writes in a WAL and packing them into Telegram
// channel blobs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ulbwa/tgwebdav/db/migrations"
	"github.com/ulbwa/tgwebdav/internal/auth"
	"github.com/ulbwa/tgwebdav/internal/blob"
	"github.com/ulbwa/tgwebdav/internal/cache"
	"github.com/ulbwa/tgwebdav/internal/config"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"github.com/ulbwa/tgwebdav/internal/limits"
	"github.com/ulbwa/tgwebdav/internal/management"
	"github.com/ulbwa/tgwebdav/internal/server"
	"github.com/ulbwa/tgwebdav/internal/services"
	"github.com/ulbwa/tgwebdav/internal/stats"
	"github.com/ulbwa/tgwebdav/internal/storage/postgres"
	"github.com/ulbwa/tgwebdav/internal/telegram"
	"github.com/ulbwa/tgwebdav/internal/wal"
	"github.com/ulbwa/tgwebdav/internal/webdavfs"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCommand builds the cobra command. Flags are registered via
// config.AddFlags; on run it loads the optional .env file, resolves the
// configuration (flags + env via viper) and starts the servers.
func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "tgwebdav",
		Short:         "WebDAV server backed by Telegram channels",
		Long:          "A single binary serving a per-user WebDAV namespace and an admin Management API over one PostgreSQL database, packing file content into Telegram channel blobs.",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			envFile, _ := cmd.Flags().GetString("env-file")
			config.LoadDotenv(envFile)
			cfg, err := config.Resolve(cmd.Flags())
			if err != nil {
				return err
			}
			return run(cfg)
		},
	}
	config.AddFlags(cmd.Flags())
	return cmd
}

func run(cfg *config.Config) error {
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	logger.Info("starting tgwebdav",
		"webdav_addr", cfg.WebDAVAddr, "mgmt_addr", cfg.MgmtAddr,
		"cache_dir", cfg.CacheDir, "cache_size", cfg.CacheSize)

	// 1. Migrations (fail fast).
	if err := migrations.Run(cfg.DSN, logger); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	// 2. Data layer.
	db, err := postgres.Open(cfg.DSN, logger)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	repos, tx, err := postgres.New(db, cfg.SecretKey)
	if err != nil {
		return fmt.Errorf("build repositories: %w", err)
	}

	// 3. Long-lived worker context (cancelled after the servers drain). The
	// deferred cancel covers early-return error paths; the happy path cancels
	// explicitly before waiting on the workers (CancelFunc is idempotent).
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	var wg sync.WaitGroup

	// 4. Infrastructure services.
	blobCache, err := cache.New(cfg.CacheDir, cfg.CacheSize, time.Hour, logger)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	startWorker(&wg, func() { blobCache.Start(workerCtx) })

	tg := telegram.New(logger)
	statsRec := stats.NewRecorder(repos.Stats, 10*time.Second, logger)
	statsRec.RegisterGauge(domain.MetricCacheBytes, "", func() float64 {
		b, _ := blobCache.Stats()
		return float64(b)
	})
	statsRec.RegisterGauge("blobs_total", "", func() float64 {
		n, _ := repos.Blobs.Count(workerCtx)
		return float64(n)
	})
	startWorker(&wg, func() { statsRec.Start(workerCtx) })

	limiter := limits.New()
	startWorker(&wg, func() { limiter.Start(workerCtx) })

	// 5. Domain services.
	settingsSvc := services.NewSettingsService(repos)
	botSvc := services.NewBotService(repos, tx, tg, logger)
	channelSvc := services.NewChannelService(repos, tx, tg, settingsSvc, logger)
	authSvc := auth.NewService(repos.Users, repos.Tokens)

	blobReader := blob.NewReader(repos, tx, tg, blobCache, statsRec, logger)
	fs := webdavfs.NewFileSystem(repos, tx, blobReader, limiter, settingsSvc, statsRec, logger)

	// 6. Seed bots/channels and bootstrap the first admin.
	seed(workerCtx, cfg, repos, channelSvc, botSvc, authSvc, logger)

	// 7. Background workers.
	packer := wal.NewPacker(repos, tx, tg, channelSvc, botSvc, settingsSvc, statsRec, logger)
	startWorker(&wg, func() { packer.Run(workerCtx) })
	startWorker(&wg, func() { runMaintenance(workerCtx, repos, channelSvc, botSvc, logger) })

	// 8. HTTP servers.
	mgmtHandlers := management.NewHandlers(management.Deps{
		Repos: repos, Tx: tx, Auth: authSvc,
		Bots: botSvc, Channels: channelSvc, Settings: settingsSvc, Logger: logger,
	})
	mgmtServer := management.NewServer(cfg.MgmtAddr, mgmtHandlers, authSvc, logger)
	davServer := server.NewWebDAV(cfg.WebDAVAddr, fs, authSvc, limiter, logger)

	serveErr := make(chan error, 2)
	go func() {
		logger.Info("webdav server listening", "addr", cfg.WebDAVAddr)
		if err := davServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("webdav server: %w", err)
		}
	}()
	go func() {
		logger.Info("management server listening", "addr", cfg.MgmtAddr)
		if err := mgmtServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("management server: %w", err)
		}
	}()

	// 9. Wait for a signal or a fatal server error.
	sigCtx, stopSig := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSig()

	select {
	case err := <-serveErr:
		cancelWorkers()
		return err
	case <-sigCtx.Done():
		logger.Info("shutdown signal received, draining")
	}

	// 10. Graceful shutdown: stop accepting requests, then stop workers (the
	// packer performs a final flush on context cancellation).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = davServer.Shutdown(shutdownCtx)
	_ = mgmtServer.Shutdown(shutdownCtx)

	cancelWorkers()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		logger.Warn("workers did not stop within timeout")
	}
	logger.Info("stopped")
	return nil
}

func startWorker(wg *sync.WaitGroup, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn()
	}()
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
