// SPDX-License-Identifier: Apache-2.0

package limiter_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/limiter"
	"github.com/dgamo/forwardlimit/internal/store"
	"github.com/dgamo/forwardlimit/internal/store/storetest"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func validRule(limit int64) store.Rule {
	return store.Rule{Limit: limit, Window: time.Minute, Block: time.Minute}
}

// constKeyer always yields the same bucket.
func constKeyer(v string) limiter.Keyer {
	return limiter.KeyerFunc(func(*limiter.Request) (string, bool) { return v, true })
}

func req(path string) *limiter.Request {
	return &limiter.Request{Method: http.MethodPost, Path: path, Header: http.Header{}}
}

func TestEvaluateWithNoLimitersAllows(t *testing.T) {
	t.Parallel()
	e := limiter.NewEngine(storetest.New(), discardLogger())

	d := e.Evaluate(context.Background(), req("/anything"))
	require.False(t, d.Blocked)
	require.Empty(t, d.Evaluations)
}

func TestEvaluateAllowsUnderLimitThenBlocks(t *testing.T) {
	t.Parallel()
	e := limiter.NewEngine(storetest.New(), discardLogger(), limiter.Limiter{
		Name:  "login",
		Rule:  validRule(2),
		Keyer: constKeyer("bucket"),
	})

	for i := 0; i < 2; i++ {
		d := e.Evaluate(context.Background(), req("/x"))
		require.False(t, d.Blocked)
		require.Equal(t, []limiter.Evaluation{{Limiter: "login", Verdict: limiter.VerdictAllowed}}, d.Evaluations)
	}

	d := e.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Blocked)
	require.Equal(t, "login", d.Limiter)
	require.Positive(t, d.RetryAfter)
}

func TestPathScopingSkipsLimiterAndStore(t *testing.T) {
	t.Parallel()
	fake := storetest.New()
	e := limiter.NewEngine(fake, discardLogger(), limiter.Limiter{
		Name:  "login",
		Rule:  validRule(1),
		Keyer: constKeyer("bucket"),
		Paths: []string{"/v1/login"},
	})

	d := e.Evaluate(context.Background(), req("/v1/search"))
	require.False(t, d.Blocked)
	require.Empty(t, d.Evaluations)
	require.Zero(t, fake.CallCount(), "an out-of-scope limiter must not touch the store")
}

func TestPathScopingMatchesExactAndSubPaths(t *testing.T) {
	t.Parallel()
	l := limiter.Limiter{Paths: []string{"/v1/login"}}

	require.True(t, l.Applies("/v1/login"))
	require.True(t, l.Applies("/v1/login/extra"))
	require.False(t, l.Applies("/v1/loginfoo"), "prefix match must respect the path separator")
	require.False(t, l.Applies("/v1"))

	require.True(t, limiter.Limiter{}.Applies("/anything"), "no paths means all paths")
}

// A trailing slash in configuration must not quietly produce a limiter that never
// fires on the bare path - a silent loss of protection.
func TestPathScopingIgnoresATrailingSlash(t *testing.T) {
	t.Parallel()
	l := limiter.Limiter{Paths: []string{"/v1/login/"}}

	require.True(t, l.Applies("/v1/login"), "the bare path must still match")
	require.True(t, l.Applies("/v1/login/"))
	require.True(t, l.Applies("/v1/login/extra"))
	require.False(t, l.Applies("/v1/loginfoo"))

	// And "/" means every path, rather than trimming to something that matches none.
	require.True(t, limiter.Limiter{Paths: []string{"/"}}.Applies("/anything"))
}

func TestKeyerNotApplicableSkipsLimiter(t *testing.T) {
	t.Parallel()
	fake := storetest.New()
	e := limiter.NewEngine(fake, discardLogger(), limiter.Limiter{
		Name: "login",
		Rule: validRule(1),
		Keyer: limiter.KeyerFunc(func(*limiter.Request) (string, bool) {
			return "", false
		}),
	})

	d := e.Evaluate(context.Background(), req("/x"))
	require.False(t, d.Blocked)
	require.Empty(t, d.Evaluations)
	require.Zero(t, fake.CallCount())
}

func TestInvalidRuleDisablesLimiter(t *testing.T) {
	t.Parallel()
	fake := storetest.New()
	e := limiter.NewEngine(fake, discardLogger(), limiter.Limiter{
		Name:  "login",
		Rule:  store.Rule{}, // zero value: disabled
		Keyer: constKeyer("bucket"),
	})

	d := e.Evaluate(context.Background(), req("/x"))
	require.False(t, d.Blocked)
	require.Zero(t, fake.CallCount())
}

func TestFirstBlockWinsAndStopsEvaluation(t *testing.T) {
	t.Parallel()
	fake := storetest.New()
	e := limiter.NewEngine(fake, discardLogger(),
		limiter.Limiter{Name: "first", Rule: validRule(0xFFFF), Keyer: constKeyer("a")},
		limiter.Limiter{Name: "blocker", Rule: store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute}, Keyer: constKeyer("b")},
		limiter.Limiter{Name: "never", Rule: validRule(1), Keyer: constKeyer("c")},
	)

	// Exhaust "blocker".
	_ = e.Evaluate(context.Background(), req("/x"))
	before := fake.CallCount()

	d := e.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Blocked)
	require.Equal(t, "blocker", d.Limiter)
	require.Len(t, d.Evaluations, 2, "evaluation must stop at the blocking limiter")
	require.Equal(t, limiter.VerdictBlocked, d.Evaluations[1].Verdict)
	require.Equal(t, before+2, fake.CallCount(), "the third limiter must not be consulted")
}

func TestStoreErrorFailsOpenAndContinuesToNextLimiter(t *testing.T) {
	t.Parallel()
	fake := storetest.New()
	fake.Err = errors.New("redis down")

	e := limiter.NewEngine(fake, discardLogger(),
		limiter.Limiter{Name: "login", Rule: validRule(1), Keyer: constKeyer("a")},
		limiter.Limiter{Name: "ip", Rule: validRule(1), Keyer: constKeyer("b")},
	)

	d := e.Evaluate(context.Background(), req("/x"))
	require.False(t, d.Blocked, "a store error must never block")
	require.True(t, d.FailedOpen())
	require.Len(t, d.Evaluations, 2, "a failing limiter must not stop the others being tried")
	for _, ev := range d.Evaluations {
		require.Equal(t, limiter.VerdictFailedOpen, ev.Verdict)
	}
}

func TestPanicInKeyerFailsOpen(t *testing.T) {
	t.Parallel()
	e := limiter.NewEngine(storetest.New(), discardLogger(), limiter.Limiter{
		Name: "boom",
		Rule: validRule(1),
		Keyer: limiter.KeyerFunc(func(*limiter.Request) (string, bool) {
			panic("keyer bug")
		}),
	})

	require.NotPanics(t, func() {
		d := e.Evaluate(context.Background(), req("/x"))
		require.False(t, d.Blocked)
	})
}

func TestPanicInStoreFailsOpen(t *testing.T) {
	t.Parallel()
	e := limiter.NewEngine(panicStore{}, discardLogger(), limiter.Limiter{
		Name:  "login",
		Rule:  validRule(1),
		Keyer: constKeyer("a"),
	})

	require.NotPanics(t, func() {
		d := e.Evaluate(context.Background(), req("/x"))
		require.False(t, d.Blocked)
		require.True(t, d.FailedOpen())
	})
}

type panicStore struct{}

func (panicStore) Consume(context.Context, store.Key, store.Rule) (store.Result, error) {
	panic("store bug")
}
func (panicStore) Take(context.Context, store.Key, store.Bucket) (store.TakeResult, error) {
	panic("store bug")
}
func (panicStore) Close() error { return nil }

func TestLimitersAreNamespacedInTheStore(t *testing.T) {
	t.Parallel()
	// Same bucket value under two limiters must not share a counter.
	e := limiter.NewEngine(storetest.New(), discardLogger(),
		limiter.Limiter{Name: "login", Rule: store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute}, Keyer: constKeyer("same")},
		limiter.Limiter{Name: "ip", Rule: store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute}, Keyer: constKeyer("same")},
	)

	d := e.Evaluate(context.Background(), req("/x"))
	require.False(t, d.Blocked)
	require.Len(t, d.Evaluations, 2)
	for _, ev := range d.Evaluations {
		require.Equal(t, limiter.VerdictAllowed, ev.Verdict)
	}
}

// ---------- token bucket limiters ----------

func TestBucketLimiterAllowsBurstThenBlocks(t *testing.T) {
	t.Parallel()
	e := limiter.NewEngine(storetest.New(), discardLogger(), limiter.Limiter{
		Name:             "tenant",
		Bucket:           store.Bucket{Rate: 1, Burst: 3},
		Keyer:            constKeyer("apikey"),
		AdviseRetryAfter: true,
	})

	for i := 0; i < 3; i++ {
		d := e.Evaluate(context.Background(), req("/x"))
		require.False(t, d.Blocked, "burst request %d", i+1)
	}

	d := e.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Blocked)
	require.Equal(t, "tenant", d.Limiter)
	require.Positive(t, d.RetryAfter)
}

// A capacity limiter must advise the caller when to retry; an abuse control must
// not. The engine carries that per-limiter choice through to the decision.
func TestAdviseRetryAfterIsCarriedFromTheBlockingLimiter(t *testing.T) {
	t.Parallel()

	advising := limiter.NewEngine(storetest.New(), discardLogger(), limiter.Limiter{
		Name:             "tenant",
		Bucket:           store.Bucket{Rate: 1, Burst: 1},
		Keyer:            constKeyer("k"),
		AdviseRetryAfter: true,
	})
	_ = advising.Evaluate(context.Background(), req("/x"))
	d := advising.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Blocked)
	require.True(t, d.AdviseRetryAfter)

	silent := limiter.NewEngine(storetest.New(), discardLogger(), limiter.Limiter{
		Name:  "login",
		Rule:  store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute},
		Keyer: constKeyer("k"),
	})
	_ = silent.Evaluate(context.Background(), req("/x"))
	d = silent.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Blocked)
	require.False(t, d.AdviseRetryAfter)
}

func TestInvalidBucketDisablesLimiter(t *testing.T) {
	t.Parallel()
	fake := storetest.New()
	e := limiter.NewEngine(fake, discardLogger(), limiter.Limiter{
		Name:   "tenant",
		Bucket: store.Bucket{}, // zero value: disabled
		Keyer:  constKeyer("k"),
	})

	d := e.Evaluate(context.Background(), req("/x"))
	require.False(t, d.Blocked)
	require.Zero(t, fake.CallCount())
}

// Mixing both is a configuration mistake; the bucket wins so two limits are never
// silently applied to one limiter.
func TestBucketTakesPrecedenceOverRule(t *testing.T) {
	t.Parallel()
	e := limiter.NewEngine(storetest.New(), discardLogger(), limiter.Limiter{
		Name:   "both",
		Rule:   store.Rule{Limit: 100, Window: time.Minute, Block: time.Minute},
		Bucket: store.Bucket{Rate: 1, Burst: 1},
		Keyer:  constKeyer("k"),
	})

	require.False(t, e.Evaluate(context.Background(), req("/x")).Blocked)
	require.True(t, e.Evaluate(context.Background(), req("/x")).Blocked,
		"the bucket's burst of 1 should apply, not the rule's limit of 100")
}

func TestActiveReportsWhetherALimiterWillRun(t *testing.T) {
	t.Parallel()

	require.False(t, limiter.Limiter{}.Active())
	require.True(t, limiter.Limiter{Rule: validRule(1)}.Active())
	require.True(t, limiter.Limiter{Bucket: store.Bucket{Rate: 1, Burst: 1}}.Active())
}

// A store failure on a capacity limiter must allow the request, exactly as for an
// abuse control: the limiter is never the reason a legitimate request fails.
func TestBucketLimiterFailsOpenOnStoreError(t *testing.T) {
	t.Parallel()
	fake := storetest.New()
	fake.Err = errors.New("redis down")

	e := limiter.NewEngine(fake, discardLogger(), limiter.Limiter{
		Name:   "tenant",
		Bucket: store.Bucket{Rate: 1, Burst: 1},
		Keyer:  constKeyer("k"),
	})

	d := e.Evaluate(context.Background(), req("/x"))
	require.False(t, d.Blocked)
	require.True(t, d.FailedOpen())
}

// ---------- dry run ----------

// The point of dry run: measure the projected impact without refusing anything.
func TestDryRunBlocksTheVerdictButNotTheRequest(t *testing.T) {
	t.Parallel()
	e := limiter.NewEngine(storetest.New(), discardLogger(), limiter.Limiter{
		Name:   "login",
		Rule:   store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute},
		Keyer:  constKeyer("k"),
		DryRun: true,
	})

	require.False(t, e.Evaluate(context.Background(), req("/x")).Blocked)

	d := e.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Blocked, "the verdict is still a block")
	require.True(t, d.DryRun)
	require.False(t, d.Rejected(), "but the request must not be refused")
	require.Equal(t, limiter.VerdictWouldBlock, d.Evaluations[0].Verdict)
}

// A suppressed decision must not be reported as a real block, or metrics cannot
// distinguish "protected" from "measuring".
func TestDryRunUsesADistinctVerdict(t *testing.T) {
	t.Parallel()

	newEngine := func(dry bool) *limiter.Engine {
		return limiter.NewEngine(storetest.New(), discardLogger(), limiter.Limiter{
			Name:   "login",
			Rule:   store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute},
			Keyer:  constKeyer("k"),
			DryRun: dry,
		})
	}

	dry := newEngine(true)
	_ = dry.Evaluate(context.Background(), req("/x"))
	require.Equal(t, limiter.VerdictWouldBlock,
		dry.Evaluate(context.Background(), req("/x")).Evaluations[0].Verdict)

	live := newEngine(false)
	_ = live.Evaluate(context.Background(), req("/x"))
	require.Equal(t, limiter.VerdictBlocked,
		live.Evaluate(context.Background(), req("/x")).Evaluations[0].Verdict)
}

// Counters must still accumulate, or the measurement would be meaningless and
// switching enforcement on would behave differently from what was observed.
func TestDryRunStillCountsInTheStore(t *testing.T) {
	t.Parallel()
	fake := storetest.New()
	e := limiter.NewEngine(fake, discardLogger(), limiter.Limiter{
		Name:   "login",
		Rule:   store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute},
		Keyer:  constKeyer("k"),
		DryRun: true,
	})

	for i := 0; i < 4; i++ {
		_ = e.Evaluate(context.Background(), req("/x"))
	}
	require.Equal(t, 4, fake.CallCount(), "every request must still reach the store")

	// The block persisted across requests, so requests 2..4 all reported a
	// would-block rather than only the one that crossed the threshold.
	d := e.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Blocked)
	require.True(t, d.DryRun)
}

// The safety property that matters most: a limiter in dry run must not change what
// any other limiter does. Otherwise enabling one for measurement would silently
// disable enforcement behind it.
func TestDryRunDoesNotMaskEnforcingLimiters(t *testing.T) {
	t.Parallel()
	e := limiter.NewEngine(storetest.New(), discardLogger(),
		limiter.Limiter{
			Name:   "measuring",
			Rule:   store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute},
			Keyer:  constKeyer("a"),
			DryRun: true,
		},
		limiter.Limiter{
			Name:  "enforcing",
			Rule:  store.Rule{Limit: 2, Window: time.Minute, Block: time.Minute},
			Keyer: constKeyer("b"),
		},
	)

	// 1st and 2nd: both allowed by "enforcing"; "measuring" trips on the 2nd.
	require.False(t, e.Evaluate(context.Background(), req("/x")).Rejected())
	d := e.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Blocked)
	require.False(t, d.Rejected(), "the dry-run verdict must not reject")

	// 3rd: "enforcing" is now over its limit and must still fire, even though a
	// dry-run limiter reached its limit earlier in the chain.
	d = e.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Rejected(), "an enforcing limiter behind a dry-run one must still reject")
	require.Equal(t, "enforcing", d.Limiter)
	require.False(t, d.DryRun)

	// Both verdicts are recorded, so each limiter's impact is visible separately.
	verdicts := map[string]limiter.Verdict{}
	for _, ev := range d.Evaluations {
		verdicts[ev.Limiter] = ev.Verdict
	}
	require.Equal(t, limiter.VerdictWouldBlock, verdicts["measuring"])
	require.Equal(t, limiter.VerdictBlocked, verdicts["enforcing"])
}

// An enforced block still short-circuits: there is no point consulting further
// limiters once the request is definitely refused.
func TestEnforcedBlockStopsEvaluation(t *testing.T) {
	t.Parallel()
	fake := storetest.New()
	e := limiter.NewEngine(fake, discardLogger(),
		limiter.Limiter{Name: "first", Rule: store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute}, Keyer: constKeyer("a")},
		limiter.Limiter{Name: "never", Rule: validRule(100), Keyer: constKeyer("b")},
	)

	_ = e.Evaluate(context.Background(), req("/x"))
	before := fake.CallCount()

	d := e.Evaluate(context.Background(), req("/x"))
	require.True(t, d.Rejected())
	require.Len(t, d.Evaluations, 1)
	require.Equal(t, before+1, fake.CallCount(), "the later limiter must not be consulted")
}

// Any combination must be expressible, in either order.
func TestDryRunIsPerLimiterInEitherOrder(t *testing.T) {
	t.Parallel()

	block := store.Rule{Limit: 1, Window: time.Minute, Block: time.Minute}

	t.Run("enforcing first", func(t *testing.T) {
		t.Parallel()
		e := limiter.NewEngine(storetest.New(), discardLogger(),
			limiter.Limiter{Name: "enforcing", Rule: block, Keyer: constKeyer("a")},
			limiter.Limiter{Name: "measuring", Rule: block, Keyer: constKeyer("b"), DryRun: true},
		)
		require.False(t, e.Evaluate(context.Background(), req("/x")).Rejected())
		require.True(t, e.Evaluate(context.Background(), req("/x")).Rejected())
	})

	t.Run("measuring first", func(t *testing.T) {
		t.Parallel()
		e := limiter.NewEngine(storetest.New(), discardLogger(),
			limiter.Limiter{Name: "measuring", Rule: block, Keyer: constKeyer("a"), DryRun: true},
			limiter.Limiter{Name: "enforcing", Rule: block, Keyer: constKeyer("b")},
		)
		require.False(t, e.Evaluate(context.Background(), req("/x")).Rejected())
		require.True(t, e.Evaluate(context.Background(), req("/x")).Rejected(),
			"order must not matter: enforcement is reached either way")
	})

	t.Run("all measuring rejects nothing", func(t *testing.T) {
		t.Parallel()
		e := limiter.NewEngine(storetest.New(), discardLogger(),
			limiter.Limiter{Name: "a", Rule: block, Keyer: constKeyer("a"), DryRun: true},
			limiter.Limiter{Name: "b", Rule: block, Keyer: constKeyer("b"), DryRun: true},
		)
		for i := 0; i < 4; i++ {
			require.False(t, e.Evaluate(context.Background(), req("/x")).Rejected())
		}
	})
}

func TestRejectedDistinguishesVerdictFromEnforcement(t *testing.T) {
	t.Parallel()

	require.False(t, limiter.Decision{}.Rejected())
	require.True(t, limiter.Decision{Blocked: true}.Rejected())
	require.False(t, limiter.Decision{Blocked: true, DryRun: true}.Rejected())
}
