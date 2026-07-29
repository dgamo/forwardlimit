// SPDX-License-Identifier: Apache-2.0

// Package redisstore implements store.Store on top of Redis.
//
// Each decision is one round trip, via Lua. The alternative - TTL, INCR, EXPIRE, SET
// - costs three to four, which makes Redis latency the dominant cost at the rates
// this is sized for.
//
// A bucket's keys share a Redis Cluster hash tag, without which a multi-key script
// is rejected with CROSSSLOT on a sharded cluster.
package redisstore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/dgamo/forwardlimit/internal/store"
)

// Mode selects the Redis topology.
type Mode string

// Supported topologies.
const (
	ModeSingle  Mode = "single"
	ModeCluster Mode = "cluster"
)

// Config describes how to reach Redis.
type Config struct {
	Addrs         []string
	Mode          Mode
	TLSEnabled    bool
	TLSSkipVerify bool
	// TLSCAFile is a PEM bundle to verify the server against, for a store whose
	// certificate is signed by a private CA. Without it only the system roots are
	// trusted, which for most managed and self-hosted Redis means the only way to
	// use TLS at all is to stop verifying it.
	TLSCAFile string
	// TLSCertFile and TLSKeyFile present a client certificate, for a store that
	// requires mutual TLS.
	TLSCertFile string
	TLSKeyFile  string
	PoolSize    int
	// MaxRetries bounds go-redis's internal retries. It matters on the
	// fail-open path: with the default of 3, a dead backend costs
	// (dial timeout x retries + backoff) per request, measured at ~1.7s even
	// with a 200ms dial timeout.
	MaxRetries   int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	KeyPrefix    string
}

// decide performs the whole rate limit decision atomically.
//
// KEYS[1] counter, KEYS[2] block marker.
// ARGV[1] limit, ARGV[2] window seconds, ARGV[3] block seconds.
// Returns {blocked, count, retryAfterSeconds}; count is -1 when the decision
// came from an existing block rather than a fresh increment.
var decide = redis.NewScript(`
local blockTTL = redis.call('TTL', KEYS[2])
if blockTTL > 0 then
  return {1, -1, blockTTL}
end

local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
end

if count > tonumber(ARGV[1]) then
  redis.call('SET', KEYS[2], '1', 'EX', ARGV[3])
  -- Drop the counter with it. Otherwise a counter that outlives the block (any
  -- config where window > block) is still over the limit when the block lifts, so
  -- the next request re-blocks immediately and the effective duration is the rest
  -- of the window, not ARGV[3].
  redis.call('DEL', KEYS[1])
  return {1, count, tonumber(ARGV[3])}
end

return {0, count, 0}
`)

// take implements a token bucket in one round trip.
//
// KEYS[1] bucket state (a hash of tokens + last-update timestamp).
// ARGV[1] rate per second, ARGV[2] burst capacity.
// Returns {allowed, whole tokens remaining, milliseconds until the next token}.
//
// The clock is read from Redis rather than passed in by the caller, so all
// replicas share one time source and pod clock skew cannot distort the refill.
var take = redis.NewScript(`
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])

local t   = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local st     = redis.call('HMGET', KEYS[1], 'n', 't')
local tokens = tonumber(st[1])
local ts     = tonumber(st[2])

if tokens == nil or ts == nil then
  tokens = burst
  ts = now
end

local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = math.min(burst, tokens + (elapsed * rate / 1000.0))

local allowed  = 0
local retry_ms = 0
if tokens >= 1 then
  tokens  = tokens - 1
  allowed = 1
else
  retry_ms = math.ceil((1 - tokens) * 1000.0 / rate)
end

redis.call('HSET', KEYS[1], 'n', tokens, 't', now)
-- Idle buckets disappear once they would have refilled completely.
redis.call('PEXPIRE', KEYS[1], math.ceil(burst * 1000.0 / rate) + 10000)

return {allowed, math.floor(tokens), retry_ms}
`)

// Store is a Redis-backed store.Store.
type Store struct {
	client redis.UniversalClient
	prefix string
}

// New dials Redis according to cfg.
//
// It does not verify connectivity: a store that cannot reach Redis must still
// construct successfully so the service starts and fails open, rather than
// crash-looping during a Redis outage.
func New(cfg Config) (*Store, error) {
	if len(cfg.Addrs) == 0 {
		return nil, fmt.Errorf("redisstore: no addresses configured")
	}

	tlsCfg, err := buildTLS(cfg)
	if err != nil {
		return nil, err
	}

	var client redis.UniversalClient
	switch cfg.Mode {
	case ModeCluster:
		client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:        cfg.Addrs,
			TLSConfig:    tlsCfg,
			PoolSize:     cfg.PoolSize,
			MaxRetries:   cfg.MaxRetries,
			DialTimeout:  cfg.DialTimeout,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
		})
	case ModeSingle:
		client = redis.NewClient(&redis.Options{
			Addr:         cfg.Addrs[0],
			TLSConfig:    tlsCfg,
			PoolSize:     cfg.PoolSize,
			MaxRetries:   cfg.MaxRetries,
			DialTimeout:  cfg.DialTimeout,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
		})
	default:
		return nil, fmt.Errorf("redisstore: unknown mode %q", cfg.Mode)
	}

	return NewWithClient(client, cfg.KeyPrefix), nil
}

// buildTLS assembles the client TLS configuration, or nil when TLS is off.
func buildTLS(cfg Config) (*tls.Config, error) {
	if !cfg.TLSEnabled {
		return nil, nil
	}

	out := &tls.Config{
		MinVersion: tls.VersionTLS12,
		//nolint:gosec // Opt-in only and off by default. Prefer TLSCAFile; this
		// exists for a store whose certificate does not match its endpoint name,
		// where there is no other way to use TLS at all.
		InsecureSkipVerify: cfg.TLSSkipVerify,
	}

	if cfg.TLSCAFile != "" {
		pem, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("redisstore: reading REDIS_TLS_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("redisstore: %s contains no PEM certificate", cfg.TLSCAFile)
		}
		out.RootCAs = pool
	}

	// Both or neither: a key without its certificate cannot present anything, and
	// silently continuing without mutual TLS would fail later and less clearly.
	switch {
	case cfg.TLSCertFile != "" && cfg.TLSKeyFile != "":
		pair, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("redisstore: loading client certificate: %w", err)
		}
		out.Certificates = []tls.Certificate{pair}
	case cfg.TLSCertFile != "" || cfg.TLSKeyFile != "":
		return nil, fmt.Errorf(
			"redisstore: REDIS_TLS_CERT_FILE and REDIS_TLS_KEY_FILE must be set together")
	}

	return out, nil
}

// NewWithClient wraps an existing client. Used by tests and by callers that
// manage the connection themselves.
func NewWithClient(client redis.UniversalClient, prefix string) *Store {
	return &Store{client: client, prefix: prefix}
}

// Ping checks connectivity. It is used for a one-off startup log line, never for
// readiness - see internal/httpapi for why readiness must not depend on Redis.
func (s *Store) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// Consume implements store.Store.
func (s *Store) Consume(ctx context.Context, key store.Key, rule store.Rule) (store.Result, error) {
	if !rule.Valid() {
		return store.Result{}, fmt.Errorf("redisstore: invalid rule %+v", rule)
	}

	base := s.prefix + key.Limiter + ":{" + store.SanitizeBucket(key.Bucket) + "}"
	keys := []string{base + ":c", base + ":b"}

	raw, err := decide.Run(ctx, s.client, keys,
		rule.Limit, seconds(rule.Window), seconds(rule.Block)).Int64Slice()
	if err != nil {
		return store.Result{}, fmt.Errorf("redisstore: consume %s: %w", key, err)
	}
	if len(raw) != 3 {
		return store.Result{}, fmt.Errorf("redisstore: unexpected script result length %d", len(raw))
	}

	return store.Result{
		Blocked:    raw[0] == 1,
		Count:      raw[1],
		RetryAfter: time.Duration(raw[2]) * time.Second,
	}, nil
}

// Take implements store.Store.
func (s *Store) Take(ctx context.Context, key store.Key, bucket store.Bucket) (store.TakeResult, error) {
	if !bucket.Valid() {
		return store.TakeResult{}, fmt.Errorf("redisstore: invalid bucket %+v", bucket)
	}

	k := s.prefix + key.Limiter + ":{" + store.SanitizeBucket(key.Bucket) + "}:tb"

	raw, err := take.Run(ctx, s.client, []string{k}, bucket.Rate, bucket.Burst).Int64Slice()
	if err != nil {
		return store.TakeResult{}, fmt.Errorf("redisstore: take %s: %w", key, err)
	}
	if len(raw) != 3 {
		return store.TakeResult{}, fmt.Errorf("redisstore: unexpected script result length %d", len(raw))
	}

	return store.TakeResult{
		Allowed:    raw[0] == 1,
		Remaining:  raw[1],
		RetryAfter: time.Duration(raw[2]) * time.Millisecond,
	}, nil
}

// Close releases the underlying connections.
func (s *Store) Close() error { return s.client.Close() }

// seconds converts a duration to whole seconds, rounding up so that a
// sub-second configuration never becomes a zero TTL (which in Redis means "no
// expiry" for EXPIRE and would leak keys forever).
func seconds(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}
	return int64(math.Ceil(d.Seconds()))
}
