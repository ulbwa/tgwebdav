// Package config parses runtime configuration from command-line flags and
// environment variables (with an optional .env file). Only bootstrap-level
// settings live here; everything tunable at runtime is stored in the database.
package config

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the fully-resolved bootstrap configuration.
type Config struct {
	DSN        string // PostgreSQL connection string
	WebDAVAddr string // WebDAV listen address
	MgmtAddr   string // Management API listen address

	CacheDir  string // disk cache directory
	CacheSize int64  // disk cache size in bytes

	FirstUser string // "login:password" used to bootstrap the first admin
	SecretKey []byte // 32-byte AES key derived from TGWEBDAV_SECRET_KEY

	BotTokens  []string // bot tokens to seed
	ChannelIDs []int64  // bare channel ids to seed

	LogLevel string // debug|info|warn|error
}

// Load resolves configuration from args (flag set), the process environment and
// an optional .env file. Precedence: explicit flag > environment > .env > default.
func Load(args []string) (*Config, error) {
	// Best-effort .env load (does not override already-set env vars).
	envFile := ".env"
	for i, a := range args {
		if a == "--env-file" && i+1 < len(args) {
			envFile = args[i+1]
		} else if strings.HasPrefix(a, "--env-file=") {
			envFile = strings.TrimPrefix(a, "--env-file=")
		}
	}
	_ = loadDotenv(envFile)

	fs := flag.NewFlagSet("tgwebdav", flag.ContinueOnError)
	var (
		dsn        = fs.String("dsn", env("TGWEBDAV_DSN", ""), "PostgreSQL DSN (or TGWEBDAV_DSN)")
		webdavAddr = fs.String("webdav-addr", env("TGWEBDAV_WEBDAV_ADDR", ":8080"), "WebDAV listen address")
		mgmtAddr   = fs.String("mgmt-addr", env("TGWEBDAV_MGMT_ADDR", ":8081"), "Management API listen address")
		cacheDir   = fs.String("cache-dir", env("TGWEBDAV_CACHE_DIR", ""), "blob cache directory (default: user cache dir)")
		cacheSize  = fs.String("cache-size", env("TGWEBDAV_CACHE_SIZE", "1GiB"), "blob cache size (e.g. 512MiB, 2GiB)")
		firstUser  = fs.String("first-user", env("TGWEBDAV_FIRST_USER", ""), "bootstrap admin as login:password")
		logLevel   = fs.String("log-level", env("TGWEBDAV_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
		_          = fs.String("env-file", envFile, ".env file to load")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	cfg := &Config{
		DSN:        *dsn,
		WebDAVAddr: *webdavAddr,
		MgmtAddr:   *mgmtAddr,
		CacheDir:   *cacheDir,
		FirstUser:  *firstUser,
		LogLevel:   *logLevel,
		BotTokens:  splitNonEmpty(env("TGWEBDAV_BOT_TOKENS", "")),
	}

	if cfg.DSN == "" {
		return nil, fmt.Errorf("DSN is required (--dsn or TGWEBDAV_DSN)")
	}

	size, err := ParseSize(*cacheSize)
	if err != nil {
		return nil, fmt.Errorf("invalid --cache-size: %w", err)
	}
	cfg.CacheSize = size

	if cfg.CacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil || base == "" {
			base = os.TempDir()
		}
		cfg.CacheDir = filepath.Join(base, "tgwebdav")
	}

	for _, raw := range splitNonEmpty(env("TGWEBDAV_CHANNEL_IDS", "")) {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid channel id %q: %w", raw, err)
		}
		cfg.ChannelIDs = append(cfg.ChannelIDs, id)
	}

	if sk := env("TGWEBDAV_SECRET_KEY", ""); sk != "" {
		// Derive a stable 32-byte key from the provided secret.
		sum := sha256.Sum256([]byte(sk))
		cfg.SecretKey = sum[:]
	} else if len(cfg.BotTokens) > 0 {
		return nil, fmt.Errorf("TGWEBDAV_SECRET_KEY is required when bot tokens are configured")
	}

	if cfg.FirstUser != "" && !strings.Contains(cfg.FirstUser, ":") {
		return nil, fmt.Errorf("--first-user must be login:password")
	}

	return cfg, nil
}

// FirstUserParts splits FirstUser into login and password (empty if unset).
func (c *Config) FirstUserParts() (login, password string, ok bool) {
	if c.FirstUser == "" {
		return "", "", false
	}
	login, password, _ = strings.Cut(c.FirstUser, ":")
	return login, password, login != "" && password != ""
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseSize parses a human byte size such as "512", "512MiB", "2GiB", "1gb".
// Binary (Ki/Mi/Gi) and decimal (k/m/g) suffixes are accepted.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	lower := strings.ToLower(s)
	mult := int64(1)
	switch {
	case strings.HasSuffix(lower, "kib"):
		mult, lower = 1<<10, strings.TrimSuffix(lower, "kib")
	case strings.HasSuffix(lower, "mib"):
		mult, lower = 1<<20, strings.TrimSuffix(lower, "mib")
	case strings.HasSuffix(lower, "gib"):
		mult, lower = 1<<30, strings.TrimSuffix(lower, "gib")
	case strings.HasSuffix(lower, "tib"):
		mult, lower = 1<<40, strings.TrimSuffix(lower, "tib")
	case strings.HasSuffix(lower, "kb"), strings.HasSuffix(lower, "k"):
		mult, lower = 1000, strings.TrimRight(strings.TrimSuffix(lower, "kb"), "k")
	case strings.HasSuffix(lower, "mb"), strings.HasSuffix(lower, "m"):
		mult, lower = 1000*1000, strings.TrimRight(strings.TrimSuffix(lower, "mb"), "m")
	case strings.HasSuffix(lower, "gb"), strings.HasSuffix(lower, "g"):
		mult, lower = 1000*1000*1000, strings.TrimRight(strings.TrimSuffix(lower, "gb"), "g")
	case strings.HasSuffix(lower, "tb"), strings.HasSuffix(lower, "t"):
		mult, lower = 1000*1000*1000*1000, strings.TrimRight(strings.TrimSuffix(lower, "tb"), "t")
	case strings.HasSuffix(lower, "b"):
		lower = strings.TrimSuffix(lower, "b")
	}
	lower = strings.TrimSpace(lower)
	n, err := strconv.ParseFloat(lower, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative size %q", s)
	}
	return int64(n * float64(mult)), nil
}
