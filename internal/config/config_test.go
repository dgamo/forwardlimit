// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/config"
	"github.com/dgamo/forwardlimit/internal/store/redisstore"
)

// env builds a getenv function from a map, so tests never mutate the process
// environment and can run in parallel.
func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestDefaults(t *testing.T) {
	t.Parallel()

	c, err := config.Load(env(nil))
	require.NoError(t, err)

	require.Equal(t, config.DefaultConfigPath, c.ConfigPath)
	require.Equal(t, ":8080", c.ListenAddr)
	require.Equal(t, "info", c.LogLevel)
	require.Equal(t, "json", c.LogFormat)
	require.Equal(t, int64(262144), c.MaxBodyBytes)
	require.Equal(t, 10*time.Second, c.ShutdownTimeout)
	require.Equal(t, "forwardlimit", c.MetricsNamespace)
	require.Empty(t, c.HashSecret)

	require.True(t, c.Redis.Enabled)
	require.Equal(t, []string{"127.0.0.1:6379"}, c.Redis.Addrs)
	require.Equal(t, redisstore.ModeSingle, c.Redis.Mode)
	require.Equal(t, config.DefaultKeyPrefix, c.Redis.KeyPrefix)
	require.Equal(t, 32, c.Redis.PoolSize)

	// Not the go-redis default of 3: retries multiply fail-open latency.
	require.Equal(t, 1, c.Redis.MaxRetries)
	// Not the go-redis default either, for the same reason.
	require.Equal(t, 200*time.Millisecond, c.Redis.DialTimeout)

	require.True(t, c.BlockCache.Enabled)
	require.Equal(t, 200000, c.BlockCache.MaxEntries)
	require.Equal(t, 60*time.Second, c.BlockCache.MaxTTL)

	require.Equal(t, 3, c.Breaker.Threshold)
	require.Equal(t, 5*time.Second, c.Breaker.Cooldown)
}

func TestOverrides(t *testing.T) {
	t.Parallel()

	c, err := config.Load(env(map[string]string{
		"CONFIG_PATH":             "/etc/custom/limits.yaml",
		"LISTEN_ADDR":             "127.0.0.1:9099",
		"LOG_LEVEL":               "debug",
		"LOG_FORMAT":              "text",
		"MAX_BODY_BYTES":          "1024",
		"SHUTDOWN_TIMEOUT":        "3s",
		"METRICS_NAMESPACE":       "acme",
		"HASH_SECRET":             "secret",
		"REDIS_ADDRS":             "a:6379,b:6379",
		"REDIS_MODE":              "cluster",
		"REDIS_TLS_ENABLED":       "true",
		"REDIS_POOL_SIZE":         "64",
		"REDIS_MAX_RETRIES":       "0",
		"REDIS_DIAL_TIMEOUT":      "1s",
		"REDIS_READ_TIMEOUT":      "2s",
		"REDIS_WRITE_TIMEOUT":     "3s",
		"BLOCK_CACHE_ENABLED":     "false",
		"STORE_BREAKER_THRESHOLD": "9",
		"STORE_BREAKER_COOLDOWN":  "30s",
	}))
	require.NoError(t, err)

	require.Equal(t, "/etc/custom/limits.yaml", c.ConfigPath)
	require.Equal(t, "127.0.0.1:9099", c.ListenAddr)
	require.Equal(t, "debug", c.LogLevel)
	require.Equal(t, "text", c.LogFormat)
	require.Equal(t, int64(1024), c.MaxBodyBytes)
	require.Equal(t, 3*time.Second, c.ShutdownTimeout)
	require.Equal(t, "acme", c.MetricsNamespace)
	require.Equal(t, "secret", c.HashSecret)

	require.Equal(t, []string{"a:6379", "b:6379"}, c.Redis.Addrs)
	require.Equal(t, redisstore.ModeCluster, c.Redis.Mode)
	require.True(t, c.Redis.TLSEnabled)
	require.Equal(t, 64, c.Redis.PoolSize)
	require.Equal(t, 0, c.Redis.MaxRetries, "zero retries must be honoured, not treated as unset")
	require.Equal(t, time.Second, c.Redis.DialTimeout)
	require.Equal(t, 2*time.Second, c.Redis.ReadTimeout)
	require.Equal(t, 3*time.Second, c.Redis.WriteTimeout)

	require.False(t, c.BlockCache.Enabled)
	require.Equal(t, 9, c.Breaker.Threshold)
	require.Equal(t, 30*time.Second, c.Breaker.Cooldown)
}

func TestMalformedValuesAreRejected(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]string{
		"not a boolean":      {"REDIS_ENABLED": "yes-please"},
		"not an integer":     {"REDIS_POOL_SIZE": "many"},
		"not a duration":     {"SHUTDOWN_TIMEOUT": "30"},
		"unknown log level":  {"LOG_LEVEL": "verbose"},
		"unknown log format": {"LOG_FORMAT": "xml"},
		"unknown redis mode": {"REDIS_MODE": "sentinel"},
		"negative body size": {"MAX_BODY_BYTES": "-1"},
		"negative retries":   {"REDIS_MAX_RETRIES": "-1"},
		"zero pool":          {"REDIS_POOL_SIZE": "0"},
		"zero timeout":       {"REDIS_DIAL_TIMEOUT": "0s"},
		"no redis addrs":     {"REDIS_ADDRS": ", ,"},
	}

	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(env(kv))
			require.Error(t, err)
		})
	}
}

// A blank value must behave as if the variable were unset. Otherwise an empty
// value injected by a templating layer silently overrides a sound default.
func TestBlankValueIsTreatedAsUnset(t *testing.T) {
	t.Parallel()

	c, err := config.Load(env(map[string]string{
		"LOG_LEVEL":        "  ",
		"MAX_BODY_BYTES":   "",
		"SHUTDOWN_TIMEOUT": "   ",
	}))
	require.NoError(t, err)

	require.Equal(t, "info", c.LogLevel)
	require.Equal(t, int64(262144), c.MaxBodyBytes)
	require.Equal(t, 10*time.Second, c.ShutdownTimeout)
}

// Every problem should be reported at once. Fixing configuration one restart at a
// time is the failure mode this avoids.
func TestAllErrorsAreReportedTogether(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{
		"LOG_LEVEL":       "verbose",
		"REDIS_POOL_SIZE": "lots",
		"REDIS_MODE":      "sentinel",
	}))
	require.Error(t, err)

	require.Contains(t, err.Error(), "LOG_LEVEL")
	require.Contains(t, err.Error(), "REDIS_POOL_SIZE")
	require.Contains(t, err.Error(), "REDIS_MODE")
}

// With the store disabled nothing about it can matter, so its settings must not
// be validated - that would make disabling it require a valid configuration for
// something that will never be contacted.
func TestRedisChecksSkippedWhenDisabled(t *testing.T) {
	t.Parallel()

	c, err := config.Load(env(map[string]string{
		"REDIS_ENABLED":   "false",
		"REDIS_MODE":      "nonsense",
		"REDIS_POOL_SIZE": "0",
		"REDIS_ADDRS":     "",
	}))
	require.NoError(t, err)
	require.False(t, c.Redis.Enabled)
}

func TestListParsingTrimsAndDropsEmpties(t *testing.T) {
	t.Parallel()

	c, err := config.Load(env(map[string]string{
		"REDIS_ADDRS": " a:6379 , ,b:6379,",
	}))
	require.NoError(t, err)
	require.Equal(t, []string{"a:6379", "b:6379"}, c.Redis.Addrs)
}

// The prefix separates this service's keys from everything else sharing the
// store, so it must be present and unambiguous however it is written.
func TestKeyPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"default", "", config.DefaultKeyPrefix},
		{"kept as given", "custom:", "custom:"},
		{"separator appended", "custom", "custom:"},
		{"nested kept", "env:svc:", "env:svc:"},
		{"nested completed", "env:svc", "env:svc:"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			kv := map[string]string{}
			if tc.in != "" {
				kv["REDIS_KEY_PREFIX"] = tc.in
			}
			c, err := config.Load(env(kv))
			require.NoError(t, err)
			require.Equal(t, tc.want, c.Redis.KeyPrefix)
		})
	}
}

func TestBreakerCooldownRequiredWhenEnabled(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{
		"STORE_BREAKER_THRESHOLD": "3",
		"STORE_BREAKER_COOLDOWN":  "0s",
	}))
	require.Error(t, err)

	// Threshold 0 disables the breaker, so the cooldown is irrelevant.
	c, err := config.Load(env(map[string]string{
		"STORE_BREAKER_THRESHOLD": "0",
		"STORE_BREAKER_COOLDOWN":  "0s",
	}))
	require.NoError(t, err)
	require.Zero(t, c.Breaker.Threshold)
}

// Warnings cover configurations that are valid but fail open, so they must be
// visible at startup rather than inferred from behaviour.
func TestWarnings(t *testing.T) {
	t.Parallel()

	t.Run("store disabled", func(t *testing.T) {
		t.Parallel()

		c, err := config.Load(env(map[string]string{"REDIS_ENABLED": "false"}))
		require.NoError(t, err)
		require.Contains(t, joined(c.Warnings()), "REDIS_ENABLED=false")
	})

	t.Run("long block cache ttl", func(t *testing.T) {
		t.Parallel()

		c, err := config.Load(env(map[string]string{"BLOCK_CACHE_MAX_TTL": "10m"}))
		require.NoError(t, err)
		require.Contains(t, joined(c.Warnings()), "BLOCK_CACHE_MAX_TTL")
	})

	t.Run("tls verification disabled", func(t *testing.T) {
		t.Parallel()

		c, err := config.Load(env(map[string]string{
			"REDIS_TLS_ENABLED":     "true",
			"REDIS_TLS_SKIP_VERIFY": "true",
		}))
		require.NoError(t, err)
		require.Contains(t, joined(c.Warnings()), "REDIS_TLS_SKIP_VERIFY")
	})

	t.Run("none for a sound configuration", func(t *testing.T) {
		t.Parallel()

		c, err := config.Load(env(map[string]string{"HASH_SECRET": "secret"}))
		require.NoError(t, err)
		require.Empty(t, c.Warnings())
	})
}

func joined(ws []string) string {
	out := ""
	for _, w := range ws {
		out += w + "\n"
	}
	return out
}
