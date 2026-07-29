// SPDX-License-Identifier: Apache-2.0

// Package config loads the service configuration: infrastructure and secrets from
// the environment, limiters from a YAML file (see file.go for why).
//
// Both are deliberately fail-fast. A limiter quietly running with the wrong
// threshold is worse than one that refuses to start, because the mistake stays
// invisible until it matters.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dgamo/forwardlimit/internal/store/redisstore"
)

// DefaultKeyPrefix namespaces every key written to the store, which is often shared
// with other services.
const DefaultKeyPrefix = "forwardlimit:"

// DefaultConfigPath is used when neither the flag nor the environment names one.
const DefaultConfigPath = "/etc/forwardlimit/config.yaml"

// Bounds applied to fixed-window limiters. They exist to catch mistakes - an
// accidental extra digit on a limit or window - rather than to express policy.
const (
	MinLimit  = 1
	MaxLimit  = 10000
	MinWindow = time.Second
	MaxWindow = time.Hour
	MinBlock  = time.Second
	MaxBlock  = 24 * time.Hour
)

// Bounds applied to token-bucket limiters.
const (
	MinRate  = 1
	MaxRate  = 1_000_000
	MinBurst = 1
	MaxBurst = 1_000_000
)

// Config is the resolved environment configuration.
type Config struct {
	// ConfigPath is where the limiter definitions are read from.
	ConfigPath string

	ListenAddr      string
	LogLevel        string
	LogFormat       string
	MaxBodyBytes    int64
	ShutdownTimeout time.Duration

	// MetricsNamespace prefixes every exported metric.
	MetricsNamespace string

	// HashSecret keys the HMAC used wherever a key spec sets hash: true. Empty
	// disables any limiter that needs it, which fails open.
	HashSecret string

	Redis      Redis
	BlockCache BlockCache
	Breaker    Breaker
}

// Redis describes the store backend.
type Redis struct {
	Enabled       bool
	Addrs         []string
	Mode          redisstore.Mode
	TLSEnabled    bool
	TLSSkipVerify bool
	TLSCAFile     string
	TLSCertFile   string
	TLSKeyFile    string
	PoolSize      int
	MaxRetries    int
	DialTimeout   time.Duration
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	KeyPrefix     string
}

// BlockCache configures the in-memory cache of known-blocked buckets.
type BlockCache struct {
	Enabled    bool
	MaxEntries int
	MaxTTL     time.Duration
}

// Breaker configures the store circuit breaker.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration
}

// LoadFromEnv loads configuration from the process environment.
func LoadFromEnv() (*Config, error) { return Load(os.Getenv) }

// Load builds a Config using getenv as the source.
//
// Taking the lookup as a parameter keeps tests free of global environment
// mutation, so they can run in parallel.
func Load(getenv func(string) string) (*Config, error) {
	l := &loader{getenv: getenv}

	cfg := &Config{
		ConfigPath:       l.str("CONFIG_PATH", DefaultConfigPath),
		ListenAddr:       l.str("LISTEN_ADDR", ":8080"),
		LogLevel:         l.str("LOG_LEVEL", "info"),
		LogFormat:        l.str("LOG_FORMAT", "json"),
		MaxBodyBytes:     l.int64("MAX_BODY_BYTES", 262144),
		ShutdownTimeout:  l.dur("SHUTDOWN_TIMEOUT", 10*time.Second),
		MetricsNamespace: l.str("METRICS_NAMESPACE", "forwardlimit"),
		HashSecret:       l.getenv("HASH_SECRET"),

		Redis: Redis{
			Enabled:       l.boolean("REDIS_ENABLED", true),
			Addrs:         l.list("REDIS_ADDRS", []string{"127.0.0.1:6379"}),
			Mode:          redisstore.Mode(l.str("REDIS_MODE", string(redisstore.ModeSingle))),
			TLSEnabled:    l.boolean("REDIS_TLS_ENABLED", false),
			TLSSkipVerify: l.boolean("REDIS_TLS_SKIP_VERIFY", false),
			TLSCAFile:     l.str("REDIS_TLS_CA_FILE", ""),
			TLSCertFile:   l.str("REDIS_TLS_CERT_FILE", ""),
			TLSKeyFile:    l.str("REDIS_TLS_KEY_FILE", ""),
			PoolSize:      l.integer("REDIS_POOL_SIZE", 32),
			// 1, not the library default of 3: retries multiply the fail-open
			// latency when the backend is down.
			MaxRetries: l.integer("REDIS_MAX_RETRIES", 1),
			// 200ms, not the library default. A load test measured the default
			// dial timeout adding seconds to every request during an outage; the
			// service still failed open, but that latency cascades into client
			// timeouts.
			DialTimeout:  l.dur("REDIS_DIAL_TIMEOUT", 200*time.Millisecond),
			ReadTimeout:  l.dur("REDIS_READ_TIMEOUT", 500*time.Millisecond),
			WriteTimeout: l.dur("REDIS_WRITE_TIMEOUT", 500*time.Millisecond),
			KeyPrefix:    normalisePrefix(l.str("REDIS_KEY_PREFIX", DefaultKeyPrefix)),
		},

		BlockCache: BlockCache{
			Enabled:    l.boolean("BLOCK_CACHE_ENABLED", true),
			MaxEntries: l.integer("BLOCK_CACHE_MAX_ENTRIES", 200000),
			// Capped well below a typical block duration so a manual unblock
			// propagates promptly instead of being pinned in every replica's
			// memory for the full block.
			MaxTTL: l.dur("BLOCK_CACHE_MAX_TTL", 60*time.Second),
		},

		Breaker: Breaker{
			Threshold: l.integer("STORE_BREAKER_THRESHOLD", 3),
			Cooldown:  l.dur("STORE_BREAKER_COOLDOWN", 5*time.Second),
		},
	}

	l.validate(cfg)

	if err := l.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Warnings reports configuration that is valid but will not do what the operator
// probably intends. These are surfaced at startup rather than failing, because
// each corresponds to a deliberate fail-open path.
func (c *Config) Warnings() []string {
	var w []string

	if !c.Redis.Enabled {
		w = append(w, "REDIS_ENABLED=false: all rate limiting is disabled and every request will be allowed")
	}
	if c.BlockCache.Enabled && c.BlockCache.MaxTTL > 5*time.Minute {
		w = append(w, "BLOCK_CACHE_MAX_TTL is large: a manual unblock may take that long to take effect on every replica")
	}
	if c.Redis.TLSSkipVerify {
		msg := "REDIS_TLS_SKIP_VERIFY=true: the store's certificate is not verified"
		if c.Redis.TLSCAFile != "" {
			msg += " - REDIS_TLS_CA_FILE is set and is being ignored, so remove one of them"
		}
		w = append(w, msg)
	}

	return w
}

// ---------- loading helpers ----------

type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) raw(key string) (string, bool) {
	v := strings.TrimSpace(l.getenv(key))
	return v, v != ""
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

func (l *loader) err() error {
	if len(l.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration: %w", errors.Join(l.errs...))
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

func (l *loader) boolean(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail("%s: %q is not a boolean (use true or false)", key, v)
		return def
	}
	return b
}

func (l *loader) integer(key string, def int) int {
	return int(l.int64(key, int64(def)))
}

func (l *loader) int64(key string, def int64) int64 {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		l.fail("%s: %q is not an integer", key, v)
		return def
	}
	return n
}

func (l *loader) dur(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail("%s: %q is not a duration (use forms like 500ms, 30s, 1h)", key, v)
		return def
	}
	return d
}

func (l *loader) list(key string, def []string) []string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalisePrefix guarantees the prefix ends with a separator, so
// REDIS_KEY_PREFIX=forwardlimit and forwardlimit: behave identically rather than
// producing keys like "forwardlimitlogin:...".
func normalisePrefix(p string) string {
	if p == "" || strings.HasSuffix(p, ":") {
		return p
	}
	return p + ":"
}

// ---------- validation ----------

func (l *loader) validate(c *Config) {
	l.validateService(c)

	// Nothing about a disabled store can matter, and requiring valid settings for
	// something that will never be contacted would make disabling it awkward.
	if !c.Redis.Enabled {
		return
	}
	l.validateStore(c)
	l.validateDecorators(c)
}

func (l *loader) validateService(c *Config) {
	if c.ListenAddr == "" {
		l.fail("LISTEN_ADDR must not be empty")
	}
	if c.ConfigPath == "" {
		l.fail("CONFIG_PATH must not be empty")
	}
	if c.MaxBodyBytes <= 0 {
		l.fail("MAX_BODY_BYTES must be positive, got %d", c.MaxBodyBytes)
	}
	if c.ShutdownTimeout <= 0 {
		l.fail("SHUTDOWN_TIMEOUT must be positive, got %s", c.ShutdownTimeout)
	}
	if c.MetricsNamespace == "" {
		l.fail("METRICS_NAMESPACE must not be empty")
	}

	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		l.fail("LOG_LEVEL: %q is not one of debug, info, warn, error", c.LogLevel)
	}
	switch strings.ToLower(c.LogFormat) {
	case "json", "text":
	default:
		l.fail("LOG_FORMAT: %q is not one of json, text", c.LogFormat)
	}
}

func (l *loader) validateStore(c *Config) {
	switch c.Redis.Mode {
	case redisstore.ModeSingle, redisstore.ModeCluster:
	default:
		l.fail("REDIS_MODE: %q is not one of single, cluster", c.Redis.Mode)
	}
	if len(c.Redis.Addrs) == 0 {
		l.fail("REDIS_ADDRS must list at least one address")
	}
	for _, f := range []struct{ key, path string }{
		{"REDIS_TLS_CA_FILE", c.Redis.TLSCAFile},
		{"REDIS_TLS_CERT_FILE", c.Redis.TLSCertFile},
		{"REDIS_TLS_KEY_FILE", c.Redis.TLSKeyFile},
	} {
		if f.path == "" {
			continue
		}
		if !c.Redis.TLSEnabled {
			l.fail("%s is set but REDIS_TLS_ENABLED is false", f.key)
		}
		if _, err := os.Stat(f.path); err != nil {
			l.fail("%s: %v", f.key, err)
		}
	}
	if (c.Redis.TLSCertFile == "") != (c.Redis.TLSKeyFile == "") {
		l.fail("REDIS_TLS_CERT_FILE and REDIS_TLS_KEY_FILE must be set together")
	}

	if c.Redis.MaxRetries < 0 {
		l.fail("REDIS_MAX_RETRIES must not be negative, got %d", c.Redis.MaxRetries)
	}
	if c.Redis.PoolSize <= 0 {
		l.fail("REDIS_POOL_SIZE must be positive, got %d", c.Redis.PoolSize)
	}

	for _, tv := range []struct {
		key string
		d   time.Duration
	}{
		{"REDIS_DIAL_TIMEOUT", c.Redis.DialTimeout},
		{"REDIS_READ_TIMEOUT", c.Redis.ReadTimeout},
		{"REDIS_WRITE_TIMEOUT", c.Redis.WriteTimeout},
	} {
		if tv.d <= 0 {
			l.fail("%s must be positive, got %s", tv.key, tv.d)
		}
	}
}

// validateDecorators covers the block cache and the circuit breaker, both of which
// only exist in front of a live store.
func (l *loader) validateDecorators(c *Config) {
	if c.BlockCache.Enabled {
		if c.BlockCache.MaxEntries <= 0 {
			l.fail("BLOCK_CACHE_MAX_ENTRIES must be positive, got %d", c.BlockCache.MaxEntries)
		}
		if c.BlockCache.MaxTTL <= 0 {
			l.fail("BLOCK_CACHE_MAX_TTL must be positive, got %s", c.BlockCache.MaxTTL)
		}
	}

	if c.Breaker.Threshold > 0 && c.Breaker.Cooldown <= 0 {
		l.fail("STORE_BREAKER_COOLDOWN must be positive when STORE_BREAKER_THRESHOLD is set")
	}
}
