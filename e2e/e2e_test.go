// SPDX-License-Identifier: Apache-2.0

// Package e2e_test exercises a running stack through the proxy - the assertions that
// need Traefik actually in front, so they cannot be unit tests.
//
//	FORWARDLIMIT_E2E          set to 1 to run at all; otherwise every test skips
//	FORWARDLIMIT_E2E_URL      proxy base URL     (default http://localhost:8000)
//	FORWARDLIMIT_E2E_METRICS  metrics base URL   (default http://localhost:8081)
//	FORWARDLIMIT_E2E_COMPOSE  compose directory; enables the disruption tests
//
// Every test derives its own keys, so nothing depends on what ran first. That
// matters: these were shell once, and the dry-run assertion only passed because of
// how many requests happened to precede it.
package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These mirror examples/docker-compose/forwardlimit.yaml. A change there that is
// not reflected here should fail loudly rather than silently weaken a test.
const (
	loginPath = "/v1/login" // limiter "login": window 3 per 1m, block 2m
	quickPath = "/v1/quick" // limiter "quick": window 2 per 5s, block 20s
	writePath = "/v1/write" // limiter "write": window 2 per 1m, block 1m, methods [POST]
	openPath  = "/open"     // no middleware attached
)

func TestMain(m *testing.M) {
	if os.Getenv("FORWARDLIMIT_E2E") == "" {
		fmt.Println("skipping: set FORWARDLIMIT_E2E=1 to run the end-to-end suite")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func baseURL() string { return env("FORWARDLIMIT_E2E_URL", "http://localhost:8000") }

func metricsURL() string { return env("FORWARDLIMIT_E2E_METRICS", "http://localhost:8081") }

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return def
}

// uniqueEmail derives a key nobody else will use - necessary because a block
// outlives its counting window.
func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d@example.com",
		strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")), time.Now().UnixNano())
}

// post sends a JSON body to path and returns the response, body already read.
func post(t *testing.T, path, body string, header http.Header) (int, string, http.Header) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, baseURL()+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "is the stack up? see the package doc for the env vars")
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, strings.TrimSpace(string(raw)), resp.Header
}

func login(t *testing.T, email string) int {
	t.Helper()
	code, _, _ := post(t, loginPath, `{"email":"`+email+`"}`, nil)
	return code
}

// uniqueClientID derives a header key nobody else will use, for the same reason
// uniqueEmail does.
func uniqueClientID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d",
		strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")), time.Now().UnixNano())
}

// send issues a bodyless request with an arbitrary method and returns the status.
func send(t *testing.T, method, path string, header http.Header) int {
	t.Helper()

	req, err := http.NewRequest(method, baseURL()+path, nil)
	require.NoError(t, err)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "is the stack up? see the package doc for the env vars")
	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// ---------- the documented contract ----------

// The sequence the README promises, and the first thing a stranger runs.
func TestQuickstartSequence(t *testing.T) {
	email := uniqueEmail(t)

	for i := 1; i <= 3; i++ {
		require.Equal(t, http.StatusOK, login(t, email), "request %d should be allowed", i)
	}
	require.Equal(t, http.StatusTooManyRequests, login(t, email),
		"the 4th request exceeds limit: 3 and must be rejected")
}

// Traefik returns this to the client verbatim, so it is a public contract.
func TestRejectionIsTheConfiguredResponse(t *testing.T) {
	email := uniqueEmail(t)
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, login(t, email))
	}

	code, body, hdr := post(t, loginPath, `{"email":"`+email+`"}`, nil)
	require.Equal(t, http.StatusTooManyRequests, code)
	require.JSONEq(t, `{"error":"rate_limited"}`, body)
	require.Equal(t, "application/json", hdr.Get("Content-Type"))

	// An abuse control must not tell an adversary where the threshold sits.
	require.Empty(t, hdr.Get("Retry-After"))
	for k := range hdr {
		require.NotContains(t, strings.ToLower(k), "ratelimit", "header %q leaks limiter state", k)
	}
}

// The normaliser, end to end: one address in two cases is one bucket.
func TestNormaliserCollapsesEquivalentValues(t *testing.T) {
	email := uniqueEmail(t)
	upper := strings.ToUpper(email)

	require.Equal(t, http.StatusOK, login(t, email))
	require.Equal(t, http.StatusOK, login(t, upper))
	require.Equal(t, http.StatusOK, login(t, email))

	require.Equal(t, http.StatusTooManyRequests, login(t, upper),
		"case variants must share one bucket, so this is the 4th against limit: 3")
}

func TestDistinctValuesGetTheirOwnBucket(t *testing.T) {
	blocked := uniqueEmail(t)
	for i := 0; i < 4; i++ {
		login(t, blocked)
	}
	require.Equal(t, http.StatusTooManyRequests, login(t, blocked))

	require.Equal(t, http.StatusOK, login(t, uniqueEmail(t)),
		"a different value must be unaffected")
}

// The property no proxy configuration can express: the block has its own TTL, so
// it persists after the counting window has rolled over.
func TestBlockOutlivesTheCountingWindow(t *testing.T) {
	email := uniqueEmail(t)
	body := `{"email":"` + email + `"}`

	for i := 1; i <= 2; i++ {
		code, _, _ := post(t, quickPath, body, nil)
		require.Equal(t, http.StatusOK, code, "request %d", i)
	}
	code, _, _ := post(t, quickPath, body, nil)
	require.Equal(t, http.StatusTooManyRequests, code, "limit: 2 exceeded")

	// window is 5s, block is 20s. Waiting past the window proves the block is not
	// released by the window rolling over.
	time.Sleep(7 * time.Second)

	code, _, _ = post(t, quickPath, body, nil)
	require.Equal(t, http.StatusTooManyRequests, code,
		"the block must outlive its counting window")
}

// A capacity limiter must say when to return, unlike the abuse control above.
func TestCapacityLimiterAdvisesRetryAfter(t *testing.T) {
	hdr := http.Header{"X-Api-Key": {uniqueEmail(t)}}

	// bucket: rate 5, burst 10. A burst larger than that must exhaust it, using
	// distinct emails so the login limiter cannot be what rejects.
	var sawRetryAfter bool
	for i := 0; i < 25; i++ {
		code, _, h := post(t, loginPath, `{"email":"`+uniqueEmail(t)+`"}`, hdr)
		if code == http.StatusTooManyRequests && h.Get("Retry-After") != "" {
			secs, err := strconv.Atoi(h.Get("Retry-After"))
			require.NoError(t, err, "Retry-After must be an integer number of seconds")
			require.Positive(t, secs)
			sawRetryAfter = true
			break
		}
	}
	require.True(t, sawRetryAfter, "the token bucket should have shed with Retry-After")
}

// A route without the middleware is not limited at all - the most common reason
// someone thinks limiting is broken.
func TestUnlimitedRouteIsNeverRejected(t *testing.T) {
	for i := 0; i < 30; i++ {
		resp, err := http.Get(baseURL() + openPath)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "request %d", i)
	}
}

// A limiter scoped to POST must ignore every other method on the same route, and
// the methods it ignores must not consume the allowance. That is the whole point:
// a browser sends a CORS preflight before the request it actually cares about, and
// counting the preflight would halve every limit for no reason.
//
// The router matches every method, so forwardlimit is what distinguishes them.
func TestMethodScopedLimiterIgnoresOtherMethods(t *testing.T) {
	h := http.Header{"X-Client-Id": []string{uniqueClientID(t)}}

	// Well past the limit of 2, and none of it counts.
	for i := 1; i <= 6; i++ {
		require.Equal(t, http.StatusOK, send(t, http.MethodOptions, writePath, h),
			"preflight %d must never be limited", i)
		require.Equal(t, http.StatusOK, send(t, http.MethodGet, writePath, h),
			"GET %d must never be limited", i)
	}

	// The allowance is untouched, so the limiter still fires on the method it covers.
	require.Equal(t, http.StatusOK, send(t, http.MethodPost, writePath, h))
	require.Equal(t, http.StatusOK, send(t, http.MethodPost, writePath, h))
	require.Equal(t, http.StatusTooManyRequests, send(t, http.MethodPost, writePath, h),
		"the 3rd POST exceeds limit: 2")
}

// A limiter whose key is absent does not apply, and the request is allowed. This
// is also the check that forwardBody is doing its job: if the body never arrived,
// every request would look like this one and nothing above would ever be limited.
func TestRequestWithoutTheKeyedFieldIsAllowed(t *testing.T) {
	for i := 0; i < 6; i++ {
		code, _, _ := post(t, loginPath, `{"unrelated":"value"}`, nil)
		require.Equal(t, http.StatusOK, code, "request %d", i)
	}
}

// Dry run must report without rejecting, and must not mask the limiters in front of
// it.
//
// A delta rather than an absolute, so it does not depend on what other tests did.
// The `blocked` invariant holds regardless: a dry-run limiter may never record an
// enforced block.
func TestDryRunProjectsWithoutRejecting(t *testing.T) {
	const (
		wouldBlock = `forwardlimit_decisions_total{decision="would_block",limiter="ip"}`
		blocked    = `forwardlimit_decisions_total{decision="blocked",limiter="ip"}`
	)

	before := counter(t, wouldBlock)
	require.Zero(t, counter(t, blocked),
		"a dry-run limiter must never record an enforced block")

	// The ip limiter is limit: 20 per minute, keyed on the caller's /24 - which is
	// this host. Exceed it, with distinct emails so the login limiter allows.
	for i := 0; i < 25; i++ {
		require.Equal(t, http.StatusOK, login(t, uniqueEmail(t)),
			"a dry-run limiter must not reject, even over its limit")
	}

	require.Greater(t, counter(t, wouldBlock), before,
		"exceeding a dry-run limit must be projected as would_block")
	require.Zero(t, counter(t, blocked),
		"a dry-run limiter must still never record an enforced block")
}

// ---------- disruption ----------
//
// These stop containers, so they need the compose directory and run last. Skipped
// without FORWARDLIMIT_E2E_COMPOSE, which is what lets the suite above run against a
// deployment it does not control.

// A store outage must allow traffic, and quickly once the breaker opens.
func TestZZStoreOutageFailsOpen(t *testing.T) {
	dir := composeDir(t)

	compose(t, dir, "stop", "redis")
	t.Cleanup(func() {
		compose(t, dir, "start", "redis")
		// Let the breaker's cooldown elapse and one probe succeed, so a later run
		// against this stack starts from a working store.
		time.Sleep(8 * time.Second)
	})

	// Fresh keys, so the block cache cannot be what answers.
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, login(t, uniqueEmail(t)),
			"a store outage must fail open, not reject")
	}

	// Three consecutive failures is the default breaker threshold, so by now it is
	// open and a request should cost roughly nothing.
	start := time.Now()
	require.Equal(t, http.StatusOK, login(t, uniqueEmail(t)))
	require.Less(t, time.Since(start), 500*time.Millisecond,
		"with the breaker open, failing open should be fast")
}

// The asymmetry that shapes every deployment decision: the service fails open, the
// integration fails closed.
func TestZZServiceOutageFailsClosed(t *testing.T) {
	dir := composeDir(t)

	compose(t, dir, "stop", "forwardlimit")
	t.Cleanup(func() {
		compose(t, dir, "start", "forwardlimit")
		waitReady(t)
	})

	code, _, _ := post(t, loginPath, `{"email":"`+uniqueEmail(t)+`"}`, nil)
	require.Equal(t, http.StatusInternalServerError, code,
		"ForwardAuth fails closed: an unreachable service means 5xx to the client")

	resp, err := http.Get(baseURL() + openPath)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"a route without the middleware must be unaffected")
}

// ---------- helpers ----------

func composeDir(t *testing.T) string {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv("FORWARDLIMIT_E2E_COMPOSE"))
	if dir == "" {
		t.Skip("set FORWARDLIMIT_E2E_COMPOSE to the compose directory to run disruption tests")
	}
	return dir
}

func compose(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "docker compose %s: %s", strings.Join(args, " "), out)
}

// waitReady blocks until the service answers again after a restart.
func waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(metricsURL() + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("service did not become ready again within 30s")
}

// counter reads one Prometheus counter, 0 when the series does not exist yet.
func counter(t *testing.T, series string) float64 {
	t.Helper()

	resp, err := http.Get(metricsURL() + "/metrics")
	require.NoError(t, err, "cannot reach %s/metrics", metricsURL())
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, series) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		require.NoError(t, err, "unparseable metric line: %q", line)
		return v
	}
	return 0
}
