// Package config resolves runtime configuration from command-line flags
// (registered on a cobra command via AddFlags) and environment variables
// (bound with viper, prefixed TGWEBDAV_), with an optional .env file loaded into
// the process environment beforehand. Only bootstrap-level settings live here;
// everything tunable at runtime is stored in the database.
package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// envPrefix is the prefix viper expects on environment variables, e.g. the
// "dsn" key maps to TGWEBDAV_DSN.
const envPrefix = "TGWEBDAV"

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

// AddFlags registers the command-line flags on fs (a cobra command's flag set).
// Defaults are static; the precedence flag > env > default is applied by Resolve
// through viper.
func AddFlags(fs *pflag.FlagSet) {
	fs.String("dsn", "", "PostgreSQL DSN (env TGWEBDAV_DSN)")
	fs.String("webdav-addr", ":8080", "WebDAV listen address (env TGWEBDAV_WEBDAV_ADDR)")
	fs.String("mgmt-addr", ":8081", "Management API listen address (env TGWEBDAV_MGMT_ADDR)")
	fs.String("cache-dir", "", "blob cache directory; default user cache dir (env TGWEBDAV_CACHE_DIR)")
	fs.String("cache-size", "1GiB", "blob cache size, e.g. 512MiB, 2GiB (env TGWEBDAV_CACHE_SIZE)")
	fs.String("first-user", "", "bootstrap admin as login:password (env TGWEBDAV_FIRST_USER)")
	fs.String("log-level", "info", "log level: debug|info|warn|error (env TGWEBDAV_LOG_LEVEL)")
	fs.String("env-file", ".env", ".env file to load before resolving configuration")
}

// LoadDotenv loads KEY=VALUE pairs from path into the process environment
// (without overriding already-set variables) so viper's AutomaticEnv picks them
// up. A missing file is not an error.
func LoadDotenv(path string) {
	_ = loadDotenv(path)
}

// Resolve builds the Config from flags, environment variables (bound via viper)
// and defaults, then validates it. Call LoadDotenv first if a .env file is used.
func Resolve(fs *pflag.FlagSet) (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	if err := v.BindPFlags(fs); err != nil {
		return nil, fmt.Errorf("bind flags: %w", err)
	}
	// Secret material has no flag (env-only); bind it explicitly.
	for _, k := range []string{"secret-key", "bot-tokens", "channel-ids"} {
		_ = v.BindEnv(k)
	}

	cfg := &Config{
		DSN:        v.GetString("dsn"),
		WebDAVAddr: v.GetString("webdav-addr"),
		MgmtAddr:   v.GetString("mgmt-addr"),
		CacheDir:   v.GetString("cache-dir"),
		FirstUser:  v.GetString("first-user"),
		LogLevel:   v.GetString("log-level"),
		BotTokens:  splitNonEmpty(v.GetString("bot-tokens")),
	}

	if cfg.DSN == "" {
		return nil, fmt.Errorf("DSN is required (--dsn or TGWEBDAV_DSN)")
	}

	size, err := ParseSize(v.GetString("cache-size"))
	if err != nil {
		return nil, fmt.Errorf("invalid cache-size: %w", err)
	}
	cfg.CacheSize = size

	if cfg.CacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil || base == "" {
			base = os.TempDir()
		}
		cfg.CacheDir = filepath.Join(base, "tgwebdav")
	}

	for _, raw := range splitNonEmpty(v.GetString("channel-ids")) {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid channel id %q: %w", raw, err)
		}
		cfg.ChannelIDs = append(cfg.ChannelIDs, id)
	}

	if sk := v.GetString("secret-key"); sk != "" {
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
