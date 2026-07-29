// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// -healthcheck is what the container healthcheck runs, so its address handling has
// to cope with every form LISTEN_ADDR takes - including the wildcards, which are not
// dialable and must be rewritten to loopback.
func TestProbeHealthAddressForms(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)

	for _, addr := range []string{
		":" + port,          // the default form
		"0.0.0.0:" + port,   // IPv4 wildcard, not dialable as-is
		"[::]:" + port,      // IPv6 wildcard, likewise
		"127.0.0.1:" + port, // the sidecar form
	} {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, probeHealth(addr), "should have reached the server")
		})
	}
}

func TestProbeHealthFailures(t *testing.T) {
	t.Parallel()

	t.Run("unparseable address", func(t *testing.T) {
		t.Parallel()
		err := probeHealth("8080")
		require.Error(t, err)
		require.Contains(t, err.Error(), "LISTEN_ADDR")
	})

	t.Run("nothing listening", func(t *testing.T) {
		t.Parallel()
		// Port 1 is privileged and unbound in any sane environment.
		require.Error(t, probeHealth("127.0.0.1:1"))
	})

	t.Run("unhealthy status", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)

		err := probeHealth(strings.TrimPrefix(srv.URL, "http://"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "503")
	})
}

// The flags that exit early must not require a reachable store or a config file, or
// they are useless in the situations they exist for.
func TestVersionFlagExitsCleanly(t *testing.T) {
	require.NoError(t, run([]string{"-version"}))
}

func TestValidateFlagReportsAndExits(t *testing.T) {
	t.Setenv("CONFIG_PATH", "../../examples/config/simple.yaml")
	t.Setenv("REDIS_ENABLED", "false")

	require.NoError(t, run([]string{"-validate"}))
}

func TestValidateFlagRejectsABadConfig(t *testing.T) {
	path := t.TempDir() + "/bad.yaml"
	require.NoError(t, writeFile(path, "limiters:\n  - name: bad\n    key: {}\n"))

	t.Setenv("CONFIG_PATH", path)

	err := run([]string{"-validate"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "one of window or bucket")
}

// A limiter that needs hashing with no secret must be reported, not silently
// dropped: the operator has to know that limiter is inert.
func TestValidateWarnsWhenHashingIsDisabled(t *testing.T) {
	path := t.TempDir() + "/hash.yaml"
	require.NoError(t, writeFile(path,
		"limiters:\n  - name: signup\n    key: {body: email, hash: true}\n"+
			"    window: {limit: 5, window: 1m, block: 1m}\n"))

	t.Setenv("CONFIG_PATH", path)
	t.Setenv("HASH_SECRET", "")
	t.Setenv("REDIS_ENABLED", "false")

	// Still exits zero: a disabled limiter is the operator's call, not a syntax
	// error. The warning goes to stderr.
	require.NoError(t, run([]string{"-validate"}))
}
