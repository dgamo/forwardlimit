// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/httpapi"
	"github.com/dgamo/forwardlimit/internal/limiter"
	"github.com/dgamo/forwardlimit/internal/limiter/keyer"
	"github.com/dgamo/forwardlimit/internal/observability"
	"github.com/dgamo/forwardlimit/internal/store"
	"github.com/dgamo/forwardlimit/internal/store/storetest"
)

const testEmail = "user@example.com"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type harness struct {
	handler *httpapi.Handler
	metrics *observability.Metrics
	fake    *storetest.Fake
}

func newHarness(t *testing.T, limit int64, limiters ...limiter.Limiter) *harness {
	t.Helper()
	return newResponseHarness(t, httpapi.Response{}, nil, defaultLimiters(limit, limiters)...)
}

// newResponseHarness is newHarness with the rejection response configured, which
// is what a real deployment does through the configuration file.
func newResponseHarness(
	t *testing.T,
	def httpapi.Response,
	per map[string]httpapi.Response,
	limiters ...limiter.Limiter,
) *harness {
	t.Helper()

	fake := storetest.New()
	m := observability.NewMetrics("")

	return &harness{
		handler: httpapi.New(httpapi.Options{
			Engine:       limiter.NewEngine(fake, discardLogger(), limiters...),
			Metrics:      m,
			Logger:       discardLogger(),
			MaxBodyBytes: 1024,
			Response:     def,
			Responses:    per,
		}),
		metrics: m,
		fake:    fake,
	}
}

// defaultLimiters supplies a single body-keyed limiter unless the test brought its
// own.
func defaultLimiters(limit int64, limiters []limiter.Limiter) []limiter.Limiter {
	if len(limiters) > 0 {
		return limiters
	}
	return []limiter.Limiter{{
		Name: "login",
		Rule: store.Rule{Limit: limit, Window: time.Minute, Block: 2 * time.Minute},
		Keyer: keyer.BodyField{
			Field:     "email",
			Normalise: keyer.Lower,
			Hasher:    keyer.NewHMAC("test-secret"),
		},
		Paths: []string{"/v1/login"},
	}}
}

func (h *harness) post(t *testing.T, path, body string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/check", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-Uri", path)
	r.Header.Set("X-Forwarded-Method", http.MethodPost)
	for k, vs := range hdr {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	return rec
}

func (h *harness) scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func loginBody(email string) string { return `{"email":"` + email + `"}` }

// ---------- decisions ----------

func TestAllowsUnderLimitThenBlocks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 3)

	for i := 1; i <= 3; i++ {
		rec := h.post(t, "/v1/login", loginBody(testEmail), nil)
		require.Equal(t, http.StatusOK, rec.Code, "request %d", i)
		require.Empty(t, rec.Body.String(), "an allow carries no body")
	}

	rec := h.post(t, "/v1/login", loginBody(testEmail), nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// The built-in default is what an operator gets before configuring anything, so
// it is locked with a golden file: a well-meaning refactor must not quietly change
// a client-facing contract.
func TestDefaultBlockedResponseMatchesGoldenContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)
	rec := h.post(t, "/v1/login", loginBody(testEmail), nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	golden := filepath.Join("testdata", "blocked_response.json")
	want, err := os.ReadFile(golden)
	require.NoError(t, err, "golden file missing: %s", golden)

	require.Equal(t, string(bytes.TrimSpace(want)), rec.Body.String())
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

// The response is a client-facing contract the operator controls, so a configured
// one must be returned exactly as given - the proxy passes it to the client
// verbatim, and a caller may be matching on its shape.
func TestConfiguredResponseIsReturnedVerbatim(t *testing.T) {
	t.Parallel()

	h := newResponseHarness(t, httpapi.Response{
		Status:      http.StatusServiceUnavailable,
		ContentType: "text/html; charset=utf-8",
		Body:        []byte("<html><body>slow down</body></html>"),
	}, nil, defaultLimiters(1, nil)...)

	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)
	rec := h.post(t, "/v1/login", loginBody(testEmail), nil)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, "<html><body>slow down</body></html>", rec.Body.String())
}

// A per-limiter override lets one limiter answer differently - a capacity limit
// may reasonably say "retry shortly" where an abuse control says nothing at all.
func TestPerLimiterResponseOverrideWins(t *testing.T) {
	t.Parallel()

	login := limiter.Limiter{
		Name: "login",
		Rule: store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute},
		Keyer: keyer.BodyField{
			Field:     "email",
			Normalise: keyer.Lower,
			Hasher:    keyer.NewHMAC("test-secret"),
		},
	}
	tenant := func(rate, burst int64) limiter.Limiter {
		return limiter.Limiter{
			Name:   "tenant",
			Bucket: store.Bucket{Rate: rate, Burst: burst},
			Keyer:  keyer.Header{Names: []string{"x-api-key"}},
		}
	}

	base := httpapi.Response{
		Status:      http.StatusTooManyRequests,
		ContentType: "application/json",
		Body:        []byte(`{"error":"rate_limited"}`),
	}
	overrides := map[string]httpapi.Response{
		"tenant": {Status: 503, ContentType: "application/json", Body: []byte(`{"error":"busy"}`)},
	}

	hdr := http.Header{"X-Api-Key": {"tenant-a"}}

	// A single token, so the second request exhausts the tenant bucket and the
	// override decides the response.
	h := newResponseHarness(t, base, overrides, tenant(1, 1), login)

	_ = h.post(t, "/v1/login", loginBody(testEmail), hdr)
	rec := h.post(t, "/v1/login", loginBody(testEmail), hdr)
	require.Equal(t, 503, rec.Code)
	require.Equal(t, `{"error":"busy"}`, rec.Body.String())

	// With ample tokens the tenant limiter allows and the login limiter is what
	// blocks - so the default applies, proving the override is scoped to one
	// limiter rather than to every rejection.
	other := newResponseHarness(t, base, overrides, tenant(1000, 1000), login)

	_ = other.post(t, "/v1/login", loginBody(testEmail), hdr)
	rec = other.post(t, "/v1/login", loginBody(testEmail), hdr)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, `{"error":"rate_limited"}`, rec.Body.String())
}

// No timing hints may be exposed to the client.
func TestBlockedResponseExposesNothingExtra(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)
	rec := h.post(t, "/v1/login", loginBody(testEmail), nil)

	require.Empty(t, rec.Header().Get("Retry-After"))
	for k := range rec.Header() {
		lower := strings.ToLower(k)
		require.NotContains(t, lower, "ratelimit", "header %q leaks limiter state", k)
		require.False(t, strings.HasPrefix(lower, "x-rl-"), "header %q leaks limiter state", k)
	}
}

// A blocked response is returned to the client verbatim by ForwardAuth, so it
// must not disclose the keyed value, its hash, or which limiter fired.
func TestBlockedResponseLeaksNoSensitiveData(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)
	rec := h.post(t, "/v1/login", loginBody(testEmail), nil)

	dump := rec.Body.String()
	for k, vs := range rec.Header() {
		dump += k + strings.Join(vs, "")
	}

	require.NotContains(t, dump, testEmail)
	require.NotContains(t, dump, testEmail[:6], "not even a prefix must appear")
	require.NotContains(t, dump, testEmail[len(testEmail)-4:], "the last four must not appear")
	require.NotContains(t, strings.ToLower(dump), "email")
	require.NotContains(t, strings.ToLower(dump), "login")
	require.NotContains(t, strings.ToLower(dump), "hash")
}

func TestRequestWithoutTargetFieldIsAllowedIndefinitely(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	for i := 0; i < 5; i++ {
		rec := h.post(t, "/v1/login", `{"token":"abc"}`, nil)
		require.Equal(t, http.StatusOK, rec.Code)
	}
	require.Zero(t, h.fake.CallCount(), "no bucket means no store call")
}

// The path comes from X-Forwarded-Uri, not from the URL of the /check call.
func TestPathScopingUsesForwardedURI(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	for i := 0; i < 5; i++ {
		rec := h.post(t, "/v1/search", loginBody(testEmail), nil)
		require.Equal(t, http.StatusOK, rec.Code)
	}
	require.Zero(t, h.fake.CallCount(), "an out-of-scope path must not be limited")

	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)
	require.Equal(t, 1, h.fake.CallCount())
}

func TestForwardedURIQueryStringIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)

	rec := h.post(t, "/v1/login?foo=bar", loginBody(testEmail), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, h.fake.CallCount(), "the query string must not defeat path matching")
}

func TestFallsBackToRequestURLWhenNotBehindForwardAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)

	// No X-Forwarded-* headers, calling the real path directly.
	r := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(loginBody(testEmail)))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)

	// The mux only routes /check, so a direct call to the business path 404s;
	// what matters is that /check without forwarded headers still works.
	require.Equal(t, http.StatusNotFound, rec.Code)

	r = httptest.NewRequest(http.MethodPost, "/check", strings.NewReader(loginBody(testEmail)))
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestGETIsAcceptedBecauseForwardAuthMayNotPreserveMethod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)

	r := httptest.NewRequest(http.MethodGet, "/check", strings.NewReader(loginBody(testEmail)))
	r.Header.Set("X-Forwarded-Uri", "/v1/login")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, h.fake.CallCount(), "the body must still be inspected on a GET")
}

// ---------- fail open ----------

func TestStoreErrorAllowsRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.fake.Err = errors.New("redis down")

	for i := 0; i < 5; i++ {
		rec := h.post(t, "/v1/login", loginBody(testEmail), nil)
		require.Equal(t, http.StatusOK, rec.Code, "a store failure must never block")
	}

	body := h.scrape(t)
	require.Contains(t, body, `forwardlimit_decisions_total{decision="failed_open",limiter="login"} 5`)
	require.Contains(t, body, "forwardlimit_store_errors_total 5")
}

func TestMalformedBodyIsAllowed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	rec := h.post(t, "/v1/login", `{not json`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestEmptyBodyIsAllowed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	rec := h.post(t, "/v1/login", ``, nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

// Truncation disables body-keyed limits, so it must be reported rather than
// silently allowed - otherwise padding the body would be an invisible bypass.
func TestOversizedBodyIsAllowedButRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	padding := strings.Repeat("x", 2048)
	body := `{"email":"` + testEmail + `","pad":"` + padding + `"}`

	rec := h.post(t, "/v1/login", body, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Contains(t, h.scrape(t),
		`forwardlimit_decisions_total{decision="failed_open",limiter="body"} 1`)
}

// ---------- multiple limiters ----------

func TestIPLimiterBlocksOnCloudflareHeader(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 0, limiter.Limiter{
		Name:  "ip",
		Rule:  store.Rule{Limit: 2, Window: time.Minute, Block: time.Minute},
		Keyer: keyer.Header{Names: []string{"CF-Connecting-IP", "X-Forwarded-For"}},
	})

	hdr := http.Header{"Cf-Connecting-Ip": {"203.0.113.7"}}
	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusOK, h.post(t, "/anything", `{}`, hdr).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, h.post(t, "/anything", `{}`, hdr).Code)

	// A different client address is unaffected.
	other := http.Header{"Cf-Connecting-Ip": {"198.51.100.9"}}
	require.Equal(t, http.StatusOK, h.post(t, "/anything", `{}`, other).Code)
}

func TestFirstBlockingLimiterDecides(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 0,
		limiter.Limiter{
			Name:  "ip",
			Rule:  store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute},
			Keyer: keyer.Header{Names: []string{"CF-Connecting-IP"}},
		},
		limiter.Limiter{
			Name:  "login",
			Rule:  store.Rule{Limit: 100, Window: time.Minute, Block: time.Minute},
			Keyer: keyer.BodyField{Field: "email", Normalise: keyer.Lower},
		},
	)

	hdr := http.Header{"Cf-Connecting-Ip": {"203.0.113.7"}}
	require.Equal(t, http.StatusOK, h.post(t, "/x", loginBody(testEmail), hdr).Code)
	require.Equal(t, http.StatusTooManyRequests, h.post(t, "/x", loginBody(testEmail), hdr).Code)

	require.Contains(t, h.scrape(t), `forwardlimit_decisions_total{decision="blocked",limiter="ip"} 1`)
}

// ---------- metrics and health ----------

func TestMetricsRecordAllowedAndBlocked(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)
	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)

	body := h.scrape(t)
	require.Contains(t, body, `forwardlimit_decisions_total{decision="allowed",limiter="login"} 1`)
	require.Contains(t, body, `forwardlimit_decisions_total{decision="blocked",limiter="login"} 1`)
	require.Contains(t, body, "forwardlimit_check_duration_seconds_bucket")
}

// An allowed request that no limiter matched still needs a series, so the blocked
// count has a denominator.
func TestRequestWithNoApplicableLimiterIsCountedAsNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)

	_ = h.post(t, "/v1/search", `{}`, nil)

	require.Contains(t, h.scrape(t),
		`forwardlimit_decisions_total{decision="allowed",limiter="none"} 1`)
}

func TestHealthzIsIndependentOfTheStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.fake.Err = errors.New("redis down")

	// Drive traffic so the store is definitely failing.
	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusOK, rec.Code,
		"readiness must not depend on Redis: as a Traefik sidecar, failing here would remove the pod from the load balancer")
	require.Equal(t, "ok", rec.Body.String())
}

// The default is deliberately minimal: it goes to the client verbatim, so it must
// carry no diagnostic detail, and it must still be well-formed JSON under the
// content type it declares.
func TestGoldenFileIsValidJSONWithTheExpectedShape(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "blocked_response.json"))
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))

	require.Equal(t, map[string]any{"error": "rate_limited"}, got,
		"the default response must say nothing beyond that the request was limited")
}

// ---------- capacity limiter ----------

func tenantHarness(t *testing.T, rate, burst int64) *harness {
	t.Helper()
	return newHarness(t, 0, limiter.Limiter{
		Name:             "tenant",
		Bucket:           store.Bucket{Rate: rate, Burst: burst},
		Keyer:            keyer.Header{Names: []string{"X-Api-Key"}, Hasher: keyer.NewHMAC("s")},
		AdviseRetryAfter: true,
	})
}

func TestTenantLimiterBlocksOverBurst(t *testing.T) {
	t.Parallel()
	h := tenantHarness(t, 1, 3)
	hdr := http.Header{"X-Api-Key": {"tenant-a-key"}}

	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, h.post(t, "/v1/search", `{}`, hdr).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, h.post(t, "/v1/search", `{}`, hdr).Code)

	// A different credential has its own quota - one noisy tenant must not
	// consume another's allowance.
	other := http.Header{"X-Api-Key": {"tenant-b-key"}}
	require.Equal(t, http.StatusOK, h.post(t, "/v1/search", `{}`, other).Code)
}

// The capacity limiter tells a cooperating caller when to retry. Without it they
// retry immediately and amplify the overload.
func TestTenantLimiterSetsRetryAfter(t *testing.T) {
	t.Parallel()
	h := tenantHarness(t, 1, 1)
	hdr := http.Header{"X-Api-Key": {"tenant-a-key"}}

	require.Equal(t, http.StatusOK, h.post(t, "/v1/search", `{}`, hdr).Code)

	rec := h.post(t, "/v1/search", `{}`, hdr)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	ra := rec.Header().Get("Retry-After")
	require.NotEmpty(t, ra, "a capacity limit must advise when to retry")
	secs, err := strconv.Atoi(ra)
	require.NoError(t, err, "Retry-After must be an integer number of seconds")
	require.GreaterOrEqual(t, secs, 1, "never advise an immediate retry")
}

// The abuse controls must keep withholding it, so the two behaviours cannot drift
// into one another.
func TestAbuseLimiterStillWithholdsRetryAfter(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)

	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)
	rec := h.post(t, "/v1/login", loginBody(testEmail), nil)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Empty(t, rec.Header().Get("Retry-After"))
}

// The credential must never appear in a storage key, a log line or the response.
func TestTenantLimiterDoesNotLeakTheCredential(t *testing.T) {
	t.Parallel()
	const secret = "sk_live_supersecretvalue"
	h := tenantHarness(t, 1, 1)
	hdr := http.Header{"X-Api-Key": {secret}}

	_ = h.post(t, "/v1/search", `{}`, hdr)
	rec := h.post(t, "/v1/search", `{}`, hdr)

	dump := rec.Body.String()
	for k, vs := range rec.Header() {
		dump += k + strings.Join(vs, "")
	}
	require.NotContains(t, dump, secret)
	require.NotContains(t, h.scrape(t), secret, "metrics labels must not carry the credential")
}

// Capacity is checked before the body is parsed, so an over-quota caller costs
// nothing beyond a header lookup.
func TestTenantLimiterIsEvaluatedBeforeBodyKeyedLimiters(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 0,
		limiter.Limiter{
			Name:             "tenant",
			Bucket:           store.Bucket{Rate: 1, Burst: 1},
			Keyer:            keyer.Header{Names: []string{"X-Api-Key"}},
			AdviseRetryAfter: true,
		},
		limiter.Limiter{
			Name:  "login",
			Rule:  store.Rule{Limit: 100, Window: time.Minute, Block: time.Minute},
			Keyer: keyer.BodyField{Field: "email", Normalise: keyer.Lower},
		},
	)
	hdr := http.Header{"X-Api-Key": {"k"}}

	require.Equal(t, http.StatusOK, h.post(t, "/v1/login", loginBody(testEmail), hdr).Code)
	before := h.fake.CallCount()

	rec := h.post(t, "/v1/login", loginBody(testEmail), hdr)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, before+1, h.fake.CallCount(),
		"only the tenant limiter should have been consulted")

	require.Contains(t, h.scrape(t), `forwardlimit_decisions_total{decision="blocked",limiter="tenant"} 1`)
}

// ---------- dry run ----------

func TestDryRunAllowsTheRequestButRecordsWouldBlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 0, limiter.Limiter{
		Name:   "login",
		Rule:   store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute},
		Keyer:  keyer.BodyField{Field: "email", Normalise: keyer.Lower},
		DryRun: true,
	})

	require.Equal(t, http.StatusOK, h.post(t, "/v1/login", loginBody(testEmail), nil).Code)

	// Over the limit, yet still allowed through.
	for i := 0; i < 3; i++ {
		rec := h.post(t, "/v1/login", loginBody(testEmail), nil)
		require.Equal(t, http.StatusOK, rec.Code, "dry run must never reject")
		require.Empty(t, rec.Body.String())
		require.Empty(t, rec.Header().Get("Retry-After"))
	}

	body := h.scrape(t)
	require.Contains(t, body, `forwardlimit_decisions_total{decision="would_block",limiter="login"} 3`)
	require.NotContains(t, body, `decision="blocked",limiter="login"`,
		"a suppressed decision must not be counted as a real block")
}

func TestEnforcingLimiterStillRejects(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1) // default harness enforces

	_ = h.post(t, "/v1/login", loginBody(testEmail), nil)
	rec := h.post(t, "/v1/login", loginBody(testEmail), nil)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Contains(t, h.scrape(t), `forwardlimit_decisions_total{decision="blocked",limiter="login"} 1`)
}

// One limiter measuring while another enforces, which is the intended rollout
// shape for introducing a new limiter to production.
func TestDryRunAndEnforcingLimitersCoexist(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 0,
		limiter.Limiter{
			Name:   "tenant",
			Bucket: store.Bucket{Rate: 1, Burst: 1},
			Keyer:  keyer.Header{Names: []string{"X-Api-Key"}},
			DryRun: true,
		},
		limiter.Limiter{
			Name:  "login",
			Rule:  store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute},
			Keyer: keyer.BodyField{Field: "email", Normalise: keyer.Lower},
		},
	)
	hdr := http.Header{"X-Api-Key": {"k"}}

	require.Equal(t, http.StatusOK, h.post(t, "/v1/login", loginBody(testEmail), hdr).Code)

	// tenant would block from here on but is suppressed. The enforcing login limiter
	// must still be reached, so the request is refused by login rather than sailing
	// through - a dry-run limiter must never mask an enforcing one.
	rec := h.post(t, "/v1/login", loginBody(testEmail), hdr)
	require.Equal(t, http.StatusTooManyRequests, rec.Code,
		"login must still enforce despite tenant being in dry run")

	body := h.scrape(t)
	require.Contains(t, body, `decision="would_block",limiter="tenant"`)
	require.NotContains(t, body, `decision="blocked",limiter="tenant"`)
	require.Contains(t, body, `decision="blocked",limiter="login"`)
}

// A credential supplied as a query parameter must be limited too, or callers
// authenticating that way are exempt from the quota entirely.
func TestTenantLimiterKeysOnQueryParamCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 0, limiter.Limiter{
		Name:   "tenant",
		Bucket: store.Bucket{Rate: 1, Burst: 1},
		Keyer: keyer.First{Parts: []limiter.Keyer{
			keyer.Header{Names: []string{"X-Api-Key"}},
			keyer.QueryParam{Names: []string{"token"}},
		}},
		AdviseRetryAfter: true,
	})

	// No credential header at all - only a query parameter.
	require.Equal(t, http.StatusOK, h.post(t, "/v1/search?token=at_123", `{}`, nil).Code)
	require.Equal(t, http.StatusTooManyRequests,
		h.post(t, "/v1/search?token=at_123", `{}`, nil).Code,
		"a query-parameter credential must still be rate limited")

	// A different token has its own bucket.
	require.Equal(t, http.StatusOK, h.post(t, "/v1/search?token=at_999", `{}`, nil).Code)
}

func TestHeaderCredentialTakesPrecedenceOverQueryParam(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 0, limiter.Limiter{
		Name:   "tenant",
		Bucket: store.Bucket{Rate: 1, Burst: 1},
		Keyer: keyer.First{Parts: []limiter.Keyer{
			keyer.Header{Names: []string{"X-Api-Key"}},
			keyer.QueryParam{Names: []string{"token"}},
		}},
	})

	hdr := http.Header{"X-Api-Key": {"sk_same"}}
	require.Equal(t, http.StatusOK, h.post(t, "/v1/search?token=at_a", `{}`, hdr).Code)

	// Same header, different query token: the header decides the bucket, so this
	// is the same caller and must be limited.
	require.Equal(t, http.StatusTooManyRequests,
		h.post(t, "/v1/search?token=at_b", `{}`, hdr).Code)
}
