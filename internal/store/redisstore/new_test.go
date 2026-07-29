// SPDX-License-Identifier: Apache-2.0

package redisstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/store"
	"github.com/dgamo/forwardlimit/internal/store/redisstore"
)

func baseConfig(addr string) redisstore.Config {
	return redisstore.Config{
		Addrs:        []string{addr},
		Mode:         redisstore.ModeSingle,
		PoolSize:     4,
		MaxRetries:   1,
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		KeyPrefix:    prefix,
	}
}

func TestNewSingleMode(t *testing.T) {
	mr := miniredis.RunT(t)

	s, err := redisstore.New(baseConfig(mr.Addr()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.NoError(t, s.Ping(context.Background()))

	res, err := s.Consume(context.Background(),
		store.Key{Limiter: "login", Bucket: "abc"},
		store.Rule{Limit: 2, Window: time.Minute, Block: time.Minute})
	require.NoError(t, err)
	require.False(t, res.Blocked)
}

func TestNewClusterModeConstructs(t *testing.T) {
	// A single miniredis cannot serve cluster commands, so this covers
	// construction and option wiring only - real cluster behaviour is covered by
	// TestIntegrationRealRedis.
	cfg := baseConfig("127.0.0.1:6379")
	cfg.Mode = redisstore.ModeCluster

	s, err := redisstore.New(cfg)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func TestNewWithTLSConstructs(t *testing.T) {
	cfg := baseConfig("127.0.0.1:6379")
	cfg.TLSEnabled = true
	cfg.TLSSkipVerify = true

	s, err := redisstore.New(cfg)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func TestNewRejectsBadConfig(t *testing.T) {
	t.Run("no addresses", func(t *testing.T) {
		cfg := baseConfig("")
		cfg.Addrs = nil
		_, err := redisstore.New(cfg)
		require.Error(t, err)
	})

	t.Run("unknown mode", func(t *testing.T) {
		cfg := baseConfig("127.0.0.1:6379")
		cfg.Mode = "sentinel"
		_, err := redisstore.New(cfg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "sentinel")
	})
}

// Construction must succeed even when the backend is unreachable: the service has
// to start and fail open during a Redis outage rather than crash-loop.
func TestNewSucceedsWhenBackendUnreachable(t *testing.T) {
	// RFC 5737 address, guaranteed not routable.
	s, err := redisstore.New(baseConfig("192.0.2.1:6379"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.Error(t, s.Ping(context.Background()), "ping should fail")

	_, err = s.Consume(context.Background(),
		store.Key{Limiter: "login", Bucket: "abc"},
		store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute})
	require.Error(t, err, "the error must surface so the caller can fail open")
}
