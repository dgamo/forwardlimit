// SPDX-License-Identifier: Apache-2.0

package observability_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/observability"
)

func TestNewLoggerEmitsJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := observability.NewLogger(&buf, "info", "json")
	require.NoError(t, err)

	log.Info("hello", "limiter", "login")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	require.Equal(t, "hello", entry["msg"])
	require.Equal(t, "login", entry["limiter"])
	require.Equal(t, "INFO", entry["level"])
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := observability.NewLogger(&buf, "warn", "json")
	require.NoError(t, err)

	log.Info("suppressed")
	require.Empty(t, buf.String())

	log.Warn("emitted")
	require.Contains(t, buf.String(), "emitted")
}

func TestNewLoggerRejectsUnknownSettings(t *testing.T) {
	t.Parallel()

	_, err := observability.NewLogger(io.Discard, "verbose", "json")
	require.Error(t, err)

	_, err = observability.NewLogger(io.Discard, "info", "xml")
	require.Error(t, err)
}

// Every exposed metric must carry the namespace, including the Go runtime and
// process collectors: an operator should be able to find everything this service
// exposes with a single name selector.
func TestAllMetricsArePrefixed(t *testing.T) {
	t.Parallel()

	for _, ns := range []string{"", "custom_ns"} {
		t.Run("namespace="+ns, func(t *testing.T) {
			t.Parallel()

			m := observability.NewMetrics(ns)
			m.RecordDecision("login", "blocked")
			m.RecordStoreError()
			m.ObserveCheck(2 * time.Millisecond)
			m.SetBlockCacheHits(7)
			m.SetBreakerOpen(true)

			want := m.Namespace() + "_"

			var checked int
			for _, line := range strings.Split(scrape(t, m), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				name := line
				if i := strings.IndexAny(name, "{ "); i >= 0 {
					name = name[:i]
				}
				require.True(t, strings.HasPrefix(name, want),
					"metric %q is missing the %q prefix", name, want)
				checked++
			}
			require.Positive(t, checked, "expected some metrics to be exposed")
		})
	}
}

// An empty namespace must resolve to the default rather than producing metrics
// named "_decisions_total".
func TestNamespaceDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	require.Equal(t, observability.DefaultNamespace, observability.NewMetrics("").Namespace())
	require.Equal(t, observability.DefaultNamespace, observability.NewMetrics("  ").Namespace())

	m := observability.NewMetrics("acme")
	require.Equal(t, "acme", m.Namespace())

	m.RecordDecision("login", "blocked")
	require.Contains(t, scrape(t, m), `acme_decisions_total{decision="blocked",limiter="login"} 1`)
}

func TestDecisionCounterIsLabelled(t *testing.T) {
	t.Parallel()

	m := observability.NewMetrics("")
	m.RecordDecision("login", "blocked")
	m.RecordDecision("login", "blocked")
	m.RecordDecision("ip", "allowed")

	body := scrape(t, m)
	require.Contains(t, body, `forwardlimit_decisions_total{decision="blocked",limiter="login"} 2`)
	require.Contains(t, body, `forwardlimit_decisions_total{decision="allowed",limiter="ip"} 1`)
}

// An allowed request has no blocking limiter; the series must still exist so the
// blocked count has a denominator.
func TestEmptyLimiterNameBecomesNone(t *testing.T) {
	t.Parallel()

	m := observability.NewMetrics("")
	m.RecordDecision("", "allowed")

	require.Contains(t, scrape(t, m),
		`forwardlimit_decisions_total{decision="allowed",limiter="none"} 1`)
}

func TestBreakerGaugeTracksState(t *testing.T) {
	t.Parallel()

	m := observability.NewMetrics("")

	m.SetBreakerOpen(true)
	require.Contains(t, scrape(t, m), "forwardlimit_store_breaker_open 1")

	m.SetBreakerOpen(false)
	require.Contains(t, scrape(t, m), "forwardlimit_store_breaker_open 0")
}

func scrape(t *testing.T, m *observability.Metrics) string {
	t.Helper()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}
