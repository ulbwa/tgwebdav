package main

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/samber/lo"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// envPrefix is the prefix viper expects on environment variables, e.g. the
// "dsn" key maps to TGWEBDAV_DSN.
const envPrefix = "TGWEBDAV"

// serverConfig is the fully-resolved bootstrap configuration for the server
// command. Only bootstrap-level settings live here; everything tunable at
// runtime is stored in the database.
type serverConfig struct {
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

// newViper builds a viper instance bound to a cobra command's flags and the
// TGWEBDAV-prefixed environment (with "-" → "_" in keys). It is the single place
// viper/os.Getenv touch the environment for this command tree.
func newViper(cmd *cobra.Command) (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	// Bind both persistent (root) and local (command) flags so each command
	// resolves precedence flag > env > default uniformly.
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return nil, fmt.Errorf("bind flags: %w", err)
	}
	return v, nil
}

// loadDotenv loads KEY=VALUE pairs from path into the process environment
// without overriding variables that are already set, so viper's AutomaticEnv
// picks them up. Lines that are blank or start with '#' are ignored. Values may
// be optionally single- or double-quoted. A missing file is not an error.
func loadDotenv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return nil // optional file
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
	return sc.Err()
}

// loadServerConfig resolves the server command's full configuration from flags,
// environment variables (bound via viper) and defaults, then validates it. The
// optional .env file named by --env-file is loaded into the environment first.
func loadServerConfig(cmd *cobra.Command) (*serverConfig, error) {
	envFile, _ := cmd.Flags().GetString("env-file")
	_ = loadDotenv(envFile)

	v, err := newViper(cmd)
	if err != nil {
		return nil, err
	}
	// Secret material has no flag (env-only); bind it explicitly.
	for _, k := range []string{"secret-key", "bot-tokens", "channel-ids"} {
		_ = v.BindEnv(k)
	}

	cfg := &serverConfig{
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

	size, err := parseSize(v.GetString("cache-size"))
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

// loadMigrateConfig resolves only what the migrate command needs: the DSN. It
// loads the optional .env file named by --env-file first, then reads --dsn /
// TGWEBDAV_DSN. It deliberately does not touch webdav/cache/bot configuration.
func loadMigrateConfig(cmd *cobra.Command) (dsn string, err error) {
	envFile, _ := cmd.Flags().GetString("env-file")
	_ = loadDotenv(envFile)

	v, err := newViper(cmd)
	if err != nil {
		return "", err
	}
	dsn = v.GetString("dsn")
	if dsn == "" {
		return "", fmt.Errorf("DSN is required (--dsn or TGWEBDAV_DSN)")
	}
	return dsn, nil
}

// firstUserParts splits the configured FirstUser into login and password
// (returning ok=false when unset or malformed).
func (c *serverConfig) firstUserParts() (login, password string, ok bool) {
	if c.FirstUser == "" {
		return "", "", false
	}
	login, password, _ = strings.Cut(c.FirstUser, ":")
	return login, password, login != "" && password != ""
}

func splitNonEmpty(s string) []string {
	trimmed := lo.Map(strings.Split(s, ","), func(p string, _ int) string {
		return strings.TrimSpace(p)
	})
	return lo.Filter(trimmed, func(p string, _ int) bool { return p != "" })
}

// parseSize parses a human byte size such as "512", "512MiB", "2GiB", "1gb".
// Binary (Ki/Mi/Gi) and decimal (k/m/g) suffixes are accepted.
func parseSize(s string) (int64, error) {
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
