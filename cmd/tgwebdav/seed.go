package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/config"
	"github.com/ulbwa/tgwebdav/internal/domain"
)

// seed idempotently registers the configured channels and bots and bootstraps
// the first administrator when the users table is empty. Failures to reach
// Telegram are logged but never abort startup.
func seed(
	ctx context.Context,
	cfg *config.Config,
	repos *domain.Repositories,
	channels domain.ChannelService,
	bots domain.BotService,
	authSvc domain.AuthService,
	logger *slog.Logger,
) {
	for _, id := range cfg.ChannelIDs {
		ch, err := channels.Add(ctx, id)
		if err != nil {
			logger.Warn("seed channel failed", "bare_id", id, "err", err)
			continue
		}
		logger.Info("seeded channel", "bare_id", id, "chat_id", ch.TGChatID, "available", ch.Available)
	}

	for _, token := range cfg.BotTokens {
		b, err := bots.Add(ctx, token)
		if err != nil {
			logger.Warn("seed bot failed", "err", err)
			continue
		}
		logger.Info("seeded bot", "username", b.Username, "id", b.ID)
	}

	if err := channels.ReevaluateAvailability(ctx); err != nil {
		logger.Warn("reevaluate channel availability", "err", err)
	}

	count, err := repos.Users.Count(ctx)
	if err != nil {
		logger.Error("count users", "err", err)
		return
	}
	if count > 0 {
		return
	}

	login, password, ok := cfg.FirstUserParts()
	if !ok {
		logger.Warn("no users exist; set --first-user or TGWEBDAV_FIRST_USER (login:password) to bootstrap an admin")
		return
	}
	hash, err := authSvc.HashPassword(password)
	if err != nil {
		logger.Error("hash first-user password", "err", err)
		return
	}
	user := &domain.User{
		ID:           uuid.New(),
		Login:        login,
		PasswordHash: hash,
		IsAdmin:      true,
		CreatedAt:    time.Now(),
	}
	if err := repos.Users.Create(ctx, user); err != nil {
		logger.Error("create first admin", "err", err)
		return
	}
	logger.Info("created first admin user", "login", login)
}
