// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/observability"
)

// These tests exercise the recovery wrapper directly. It is the last line of
// defence for the service's central promise - rate limiting must never be the
// reason a request fails - and ForwardAuth turns any non-2xx into a refusal, so a
// panic that escaped would reject the request.

func TestRecoverAndAllowTurnsAPanicIntoAnAllow(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	m := observability.NewMetrics("")

	h := recoverAndAllow(log, m, func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	rec := httptest.NewRecorder()
	require.NotPanics(t, func() {
		h(rec, httptest.NewRequest(http.MethodPost, "/check", nil))
	})

	require.Equal(t, http.StatusOK, rec.Code, "a panic must allow the request, not refuse it")

	// Loud, not silent: both a log line and a metric.
	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	require.Equal(t, "ERROR", entry["level"])
	require.Contains(t, entry["msg"], "allowing it through")
	require.Contains(t, entry, "stack")

	body := scrapeMetrics(t, m)
	require.Contains(t, body, `forwardlimit_decisions_total{decision="failed_open",limiter="panic"} 1`)
	require.Contains(t, body, "forwardlimit_store_errors_total 1")
}

func TestRecoverAndAllowLeavesNormalResponsesAlone(t *testing.T) {
	t.Parallel()

	h := recoverAndAllow(discardLog(), nil, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status":429}`))
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/check", nil))

	require.Equal(t, http.StatusTooManyRequests, rec.Code, "an enforced block must still reject")
	require.Equal(t, `{"status":429}`, rec.Body.String())
}

// A panic after the status line has gone out cannot be corrected. The wrapper must
// not attempt a second WriteHeader, which would corrupt the response and log a
// superfluous-write error.
func TestRecoverAndAllowDoesNotRewriteAnAlreadySentStatus(t *testing.T) {
	t.Parallel()

	h := recoverAndAllow(discardLog(), nil, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		panic("panic after the header was sent")
	})

	rec := httptest.NewRecorder()
	require.NotPanics(t, func() {
		h(rec, httptest.NewRequest(http.MethodPost, "/check", nil))
	})

	require.Equal(t, http.StatusTooManyRequests, rec.Code,
		"the status already sent must be preserved")
}

func TestRecoverAndAllowToleratesNilMetrics(t *testing.T) {
	t.Parallel()

	h := recoverAndAllow(discardLog(), nil, func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	rec := httptest.NewRecorder()
	require.NotPanics(t, func() {
		h(rec, httptest.NewRequest(http.MethodPost, "/check", nil))
	})
	require.Equal(t, http.StatusOK, rec.Code)
}

// The property must hold through the real mux, not only in isolation. A nil Engine
// makes handleCheck dereference a nil pointer, which is a genuine panic on the real
// request path rather than a synthetic one.
func TestPanicOnTheRealCheckPathStillAllows(t *testing.T) {
	t.Parallel()

	m := observability.NewMetrics("")
	h := New(Options{
		Engine:       nil, // deliberately broken
		Metrics:      m,
		Logger:       discardLog(),
		MaxBodyBytes: 1024,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/check", strings.NewReader(`{"email":"user@example.com"}`))
	req.Header.Set("X-Forwarded-Uri", "/v1/login")

	require.NotPanics(t, func() { h.ServeHTTP(rec, req) })
	require.Equal(t, http.StatusOK, rec.Code,
		"a defect in the limiter must never refuse a request")
	require.Contains(t, scrapeMetrics(t, m), `decision="failed_open",limiter="panic"`)
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func scrapeMetrics(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return strings.TrimSpace(rec.Body.String())
}
