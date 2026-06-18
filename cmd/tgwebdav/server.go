package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
	"github.com/spf13/cobra"

	telegram "github.com/ulbwa/tgwebdav/internal/client/telegram"
	"github.com/ulbwa/tgwebdav/internal/database"
	httphandler "github.com/ulbwa/tgwebdav/internal/handler/http"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository"
	"github.com/ulbwa/tgwebdav/internal/service"
	"github.com/ulbwa/tgwebdav/internal/service/cache"
	"github.com/ulbwa/tgwebdav/internal/service/webdavfs"
)

// serverCmd runs the full server: migrations, the two HTTP listeners (WebDAV and
// Management) and every background worker. It is the default action of the root
// command. RunE ports the old run() wiring verbatim, swapped onto the canon
// packages (config → db → repositories → clients → services → handlers → server).
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Run the WebDAV and Management API servers",
	Long:  "Run the WebDAV and Management API servers, applying pending migrations on boot and starting the background workers.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadServerConfig(cmd)
		if err != nil {
			return err
		}
		setLogLevel(cfg.LogLevel)
		return runServer(cmd.Context(), cfg)
	},
}

// runServer performs the full wire-up and serves until a signal or a fatal serve
// error. It is the canon port of the old run(): config → db → repositories →
// clients → services → handlers → server, then graceful shutdown.
func runServer(rootCtx context.Context, cfg *serverConfig) error {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	logger := slog.Default()

	logger.Info("starting tgwebdav",
		"webdav_addr", cfg.WebDAVAddr, "mgmt_addr", cfg.MgmtAddr,
		"cache_dir", cfg.CacheDir, "cache_size", cfg.CacheSize)

	// The secret key encrypts bot tokens at rest. Without it, bots cannot be
	// added via the Management API (the only way to add them now).
	if len(cfg.SecretKey) == 0 {
		logger.Warn("no secret key set; adding bots via the Management API will fail until --secret-key / TGWEBDAV_SECRET_KEY is provided")
	}

	// 1. Migrations (fail fast). Keep auto-migrate-on-boot.
	if err := runMigrations(cfg.DSN, logger); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	// 2. Database pool. SQL is traced to the standard logger at DEBUG (Rule 7).
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	poolCfg.ConnConfig.Tracer = &tracelog.TraceLog{
		Logger:   database.NewSlogTracer(logger),
		LogLevel: tracelog.LogLevelDebug,
	}
	// Mirror the old GORM pool tuning.
	poolCfg.MaxConns = 50
	poolCfg.MinConns = 10
	poolCfg.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(rootCtx, poolCfg)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	tx := database.NewTxManager(pool)

	// 3. Repositories (13). The bot repo encrypts tokens at rest with the secret
	// key (may be nil when no bots are configured).
	var (
		userRepo        = repository.NewUserRepository(pool)
		tokenRepo       = repository.NewTokenRepository(pool)
		botRepo         = repository.NewBotRepository(pool, cfg.SecretKey)
		channelRepo     = repository.NewChannelRepository(pool)
		botChannelRepo  = repository.NewBotChannelRepository(pool)
		nodeRepo        = repository.NewNodeRepository(pool)
		extentRepo      = repository.NewExtentRepository(pool)
		blobRepo        = repository.NewBlobRepository(pool)
		blobBotFileRepo = repository.NewBlobBotFileRepository(pool)
		walRepo         = repository.NewWALRepository(pool)
		settingsRepo    = repository.NewSettingsRepository(pool)
		statRepo        = repository.NewStatRepository(pool)
		eventRepo       = repository.NewEventRepository(pool)
	)

	// 4. Long-lived worker context (cancelled after the servers drain). The
	// deferred cancel covers early-return error paths; the happy path cancels
	// explicitly before waiting on the workers (CancelFunc is idempotent).
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	var wg sync.WaitGroup

	// 5. Clients and infrastructure services.
	blobCache, err := cache.New(cfg.CacheDir, cfg.CacheSize, time.Hour, logger)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	startWorker(&wg, func() { blobCache.Start(workerCtx) })

	tg := telegram.New(logger)

	statRecorder := service.NewStatRecorder(statRepo, 10*time.Second, logger)
	statRecorder.RegisterGauge(model.MetricCacheBytes, "", func() float64 {
		b, _ := blobCache.Stats()
		return float64(b)
	})
	statRecorder.RegisterGauge("blobs_total", "", func() float64 {
		n, _ := blobRepo.Count(workerCtx)
		return float64(n)
	})
	startWorker(&wg, func() { statRecorder.Start(workerCtx) })

	limiter := service.NewLimiter()
	startWorker(&wg, func() { limiter.Start(workerCtx) })

	// 6. Domain services.
	eventSvc := service.NewEventService(eventRepo)
	authSvc := service.NewAuthService(userRepo, tokenRepo)
	userSvc := service.NewUserService(userRepo, tokenRepo)
	settingsSvc := service.NewSettingsService(settingsRepo)

	botSvc := service.NewBotService(botRepo, channelRepo, botChannelRepo, blobRepo, eventRepo, tx, tg, logger)
	channelSvc := service.NewChannelService(channelRepo, botRepo, botChannelRepo, blobRepo, eventRepo, settingsSvc, tx, tg, logger)

	blobReader := service.NewBlobReader(
		blobRepo, channelRepo, botRepo, botChannelRepo, blobBotFileRepo,
		nodeRepo, extentRepo, tx, tg, blobCache, statRecorder, eventRepo, logger,
	)

	fs := webdavfs.NewFileSystem(
		nodeRepo, extentRepo, walRepo, blobRepo, tx, blobReader, limiter, settingsSvc, statRecorder, logger,
	)

	// 7. Recompute channel availability from current DB membership (correct after
	// a restart; harmless with zero bots/channels) and bootstrap the first admin.
	// Bots and channels are added exclusively via the Management API.
	if err := channelSvc.ReevaluateAvailability(workerCtx); err != nil {
		logger.Warn("reevaluate channel availability", "err", err)
	}
	bootstrapAdmin(workerCtx, cfg, userSvc, logger)

	// 8. Background workers.
	packer := service.NewPacker(
		nodeRepo, walRepo, blobRepo, extentRepo, channelRepo, botRepo, botChannelRepo, blobBotFileRepo,
		tx, tg, channelSvc, botSvc, settingsSvc, statRecorder, eventRepo, logger,
	)
	startWorker(&wg, func() { packer.Run(workerCtx) })

	maintenance := service.NewMaintenanceService(blobRepo, channelRepo, botRepo, botSvc, botSvc, tg, eventRepo, logger)
	startWorker(&wg, func() { maintenance.Run(workerCtx) })

	// 9. HTTP handlers and servers.
	mgmtHandler := httphandler.NewManagementHandler(httphandler.ManagementDeps{
		Auth:     authSvc,
		Users:    userSvc,
		Bots:     botSvc,
		Channels: channelSvc,
		Settings: settingsSvc,
		Events:   eventSvc,
		Stats:    statRecorder,
		Logger:   logger,
	})
	davHandler := httphandler.NewWebDAVHandler(httphandler.WebDAVDeps{
		FS:      fs,
		Auth:    authSvc,
		Limiter: limiter,
		Logger:  logger,
	})

	mgmtServer := &http.Server{Addr: cfg.MgmtAddr, Handler: mgmtHandler}
	// Match the old WebDAV server's timeouts (internal/server/webdav.go): a read-
	// header timeout to shed slow-loris connections and an idle timeout to reap
	// kept-alive connections. No Read/Write timeout, so large uploads/downloads
	// are not cut off mid-transfer.
	davServer := &http.Server{
		Addr:              cfg.WebDAVAddr,
		Handler:           davHandler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

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

	// 10. Wait for a signal or a fatal server error.
	sigCtx, stopSig := signal.NotifyContext(rootCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSig()

	select {
	case err := <-serveErr:
		cancelWorkers()
		return err
	case <-sigCtx.Done():
		logger.Info("shutdown signal received, draining")
	}

	// 11. Graceful shutdown: stop accepting requests, then stop workers (the
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

// startWorker runs fn in a tracked goroutine so graceful shutdown can wait on it.
func startWorker(wg *sync.WaitGroup, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn()
	}()
}

// bootstrapAdmin creates the first administrator from --first-user /
// TGWEBDAV_FIRST_USER, but only when no users exist yet. Bots and channels are
// no longer seeded here; they are added exclusively via the Management API. As
// in the old seed.go, failures are logged but never abort startup.
func bootstrapAdmin(
	ctx context.Context,
	cfg *serverConfig,
	userSvc *service.UserService,
	logger *slog.Logger,
) {
	users, err := userSvc.List(ctx)
	if err != nil {
		logger.Error("list users", "err", err)
		return
	}
	if len(users) > 0 {
		return
	}

	login, password, ok := cfg.firstUserParts()
	if !ok {
		logger.Warn("no users exist; set --first-user or TGWEBDAV_FIRST_USER (login:password) to bootstrap an admin")
		return
	}
	if _, err := userSvc.Create(ctx, service.CreateUserParams{
		Login:    login,
		Password: password,
		IsAdmin:  true,
	}); err != nil {
		logger.Error("create first admin", "err", err)
		return
	}
	logger.Info("created first admin user", "login", login)
}
