// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/httpapi"
	"github.com/dgamo/forwardlimit/internal/limiter"
	"github.com/dgamo/forwardlimit/internal/limiter/keyer"
	"github.com/dgamo/forwardlimit/internal/observability"
	"github.com/dgamo/forwardlimit/internal/store"
	"github.com/dgamo/forwardlimit/internal/store/blockcache"
	"github.com/dgamo/forwardlimit/internal/store/breaker"
	"github.com/dgamo/forwardlimit/internal/store/redisstore"
)

// stack wires the real components in the same order as cmd/forwardlimit:
// blockcache -> breaker -> redisstore.
type stack struct {
	handler *httpapi.Handler
	metrics *observability.Metrics
	mr      *miniredis.Miniredis
	breaker *breaker.Store
	cache   *blockcache.Store
}

type stackOpts struct {
	limit       int64
	window      time.Duration
	block       time.Duration
	cacheMaxTTL time.Duration
	breakerN    int
}

func newStack(t *testing.T, o stackOpts) *stack {
	t.Helper()

	if o.cacheMaxTTL == 0 {
		o.cacheMaxTTL = time.Minute
	}
	if o.breakerN == 0 {
		o.breakerN = 5
	}

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	rs := redisstore.NewWithClient(client, "forwardlimit:")
	br := breaker.New(rs, breaker.Options{Threshold: o.breakerN, Cooldown: 50 * time.Millisecond})
	bc := blockcache.New(br, blockcache.Options{MaxEntries: 1000, MaxTTL: o.cacheMaxTTL})
	t.Cleanup(func() { _ = bc.Close() })

	m := observability.NewMetrics("")
	handler := httpapi.New(httpapi.Options{
		Engine: limiter.NewEngine(bc, discardLogger(), limiter.Limiter{
			Name: "login",
			Rule: store.Rule{Limit: o.limit, Window: o.window, Block: o.block},
			Keyer: keyer.BodyField{
				Field:     "email",
				Normalise: keyer.Lower,
				Hasher:    keyer.NewHMAC("integration-secret"),
			},
			Paths: []string{"/v1/login"},
		}),
		Metrics:      m,
		Logger:       discardLogger(),
		MaxBodyBytes: 262144,
	})

	return &stack{handler: handler, metrics: m, mr: mr, breaker: br, cache: bc}
}

func (s *stack) check(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/check", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-Uri", "/v1/login")
	r.Header.Set("X-Forwarded-Method", http.MethodPost)

	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	return rec
}

func (s *stack) scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// Reproduces the sequence documented in the README quickstart, through the real
// Redis-backed store rather than a fake.
func TestStackAllowsThenBlocks(t *testing.T) {
	t.Parallel()
	s := newStack(t, stackOpts{limit: 3, window: time.Minute, block: 2 * time.Minute})

	for i := 1; i <= 3; i++ {
		require.Equal(t, http.StatusOK, s.check(t, loginBody(testEmail)).Code, "request %d", i)
	}

	rec := s.check(t, loginBody(testEmail))
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.JSONEq(t, `{"error":"rate_limited"}`, rec.Body.String())

	require.Contains(t, s.scrape(t), `forwardlimit_decisions_total{decision="blocked",limiter="login"} 1`)
}

func TestStackNormalisesCase(t *testing.T) {
	t.Parallel()
	s := newStack(t, stackOpts{limit: 2, window: time.Minute, block: time.Minute})

	require.Equal(t, http.StatusOK, s.check(t, loginBody("USER@Example.com")).Code)
	require.Equal(t, http.StatusOK, s.check(t, loginBody("user@example.com")).Code)

	// Third request on the same logical value must trip the limit.
	require.Equal(t, http.StatusTooManyRequests, s.check(t, loginBody("User@EXAMPLE.COM")).Code)
}

// The block must outlive the counting window - the property that distinguishes
// this algorithm from a plain fixed window.
func TestStackBlockOutlivesWindow(t *testing.T) {
	t.Parallel()
	s := newStack(t, stackOpts{
		limit:       1,
		window:      10 * time.Second,
		block:       10 * time.Minute,
		cacheMaxTTL: time.Nanosecond, // force every decision back to Redis
	})

	require.Equal(t, http.StatusOK, s.check(t, loginBody(testEmail)).Code)
	require.Equal(t, http.StatusTooManyRequests, s.check(t, loginBody(testEmail)).Code)

	s.mr.FastForward(30 * time.Second) // past the window, inside the block

	require.Equal(t, http.StatusTooManyRequests, s.check(t, loginBody(testEmail)).Code,
		"must not be released when the counting window rolls over")
}

// With the store gone, a bucket already known to be blocked is still blocked from
// local memory - and everything else fails open.
func TestStackBlockCacheSurvivesStoreOutage(t *testing.T) {
	t.Parallel()
	s := newStack(t, stackOpts{limit: 1, window: time.Minute, block: time.Minute})

	require.Equal(t, http.StatusOK, s.check(t, loginBody(testEmail)).Code)
	require.Equal(t, http.StatusTooManyRequests, s.check(t, loginBody(testEmail)).Code)

	s.mr.Close() // store is now unreachable

	require.Equal(t, http.StatusTooManyRequests, s.check(t, loginBody(testEmail)).Code,
		"the cached block must still be enforced")
	require.Positive(t, s.cache.Hits())

	// A different value has no cached verdict, so it fails open.
	require.Equal(t, http.StatusOK, s.check(t, loginBody("other@example.com")).Code)
}

// A store outage must fail open, and the breaker must stop the service dialling a
// backend it already knows is down.
func TestStackFailsOpenAndOpensBreakerOnOutage(t *testing.T) {
	t.Parallel()
	s := newStack(t, stackOpts{limit: 1, window: time.Minute, block: time.Minute, breakerN: 2})

	s.mr.Close()

	for i := 0; i < 10; i++ {
		require.Equal(t, http.StatusOK, s.check(t, loginBody(testEmail)).Code,
			"an outage must never block a legitimate request")
	}
	require.True(t, s.breaker.Open(), "the breaker should have opened rather than dialling every request")

	body := s.scrape(t)
	require.Contains(t, body, `decision="failed_open",limiter="login"`)
	require.Contains(t, body, "forwardlimit_store_errors_total")
}

// TestIntegrationRealRedis exercises the store against an actual Redis, which is
// the only way to cover cluster slot routing: miniredis does not emulate it, so
// the hash-tagged keys used by the Lua script cannot be verified in-process.
//
// Enable with:
//
//	FORWARDLIMIT_INTEGRATION=1 REDIS_ADDRS=host:6379 REDIS_MODE=cluster \
//	  go test -run Integration ./internal/httpapi/
func TestIntegrationRealRedis(t *testing.T) {
	if os.Getenv("FORWARDLIMIT_INTEGRATION") == "" {
		t.Skip("set FORWARDLIMIT_INTEGRATION=1 and REDIS_ADDRS to run")
	}
	addrs := os.Getenv("REDIS_ADDRS")
	require.NotEmpty(t, addrs, "REDIS_ADDRS is required")

	mode := redisstore.Mode(os.Getenv("REDIS_MODE"))
	if mode == "" {
		mode = redisstore.ModeSingle
	}

	rs, err := redisstore.New(redisstore.Config{
		Addrs:        strings.Split(addrs, ","),
		Mode:         mode,
		TLSEnabled:   os.Getenv("REDIS_TLS_ENABLED") == "true",
		PoolSize:     8,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		// A dedicated prefix keeps the test clear of any real counters.
		KeyPrefix: "forwardlimit:itest:",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rs.Close() })

	ctx := t.Context()
	require.NoError(t, rs.Ping(ctx), "Redis must be reachable")

	// A unique bucket per run so repeated runs do not interfere.
	bucket := "itest-" + time.Now().UTC().Format("20060102150405.000000000")
	key := store.Key{Limiter: "login", Bucket: bucket}
	rule := store.Rule{Limit: 2, Window: 30 * time.Second, Block: 30 * time.Second}

	for i := int64(1); i <= 2; i++ {
		res, cerr := rs.Consume(ctx, key, rule)
		require.NoError(t, cerr)
		require.False(t, res.Blocked)
		require.Equal(t, i, res.Count)
	}

	res, cerr := rs.Consume(ctx, key, rule)
	require.NoError(t, cerr)
	require.True(t, res.Blocked, "the multi-key Lua script must run without CROSSSLOT")
	require.Positive(t, res.RetryAfter)
}
