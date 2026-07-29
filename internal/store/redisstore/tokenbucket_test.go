// SPDX-License-Identifier: Apache-2.0

package redisstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/store"
)

// TestMain silences go-redis's internal dial logging: several tests close the
// backend deliberately, and the retry noise obscures real failures.
func TestMain(m *testing.M) {
	redis.SetLogger(nopLogger{})
	os.Exit(m.Run())
}

type nopLogger struct{}

func (nopLogger) Printf(context.Context, string, ...any) {}

func bucket(rate, burst int64) store.Bucket {
	return store.Bucket{Rate: rate, Burst: burst}
}

var tenantKey = store.Key{Limiter: "tenant", Bucket: "apikeyhash"}

// A fresh bucket starts full, so a caller may spend its whole burst allowance at
// once - that is the point of the burst.
func TestTakeAllowsFullBurstThenRejects(t *testing.T) {
	s, mr := newTestStore(t)
	mr.SetTime(time.Now())
	ctx := context.Background()
	b := bucket(200, 5)

	for i := 0; i < 5; i++ {
		res, err := s.Take(ctx, tenantKey, b)
		require.NoError(t, err)
		require.True(t, res.Allowed, "burst request %d must be allowed", i+1)
	}

	res, err := s.Take(ctx, tenantKey, b)
	require.NoError(t, err)
	require.False(t, res.Allowed, "the bucket is empty")
	require.Zero(t, res.Remaining)
	require.Positive(t, res.RetryAfter, "a rejected caller must be told when to retry")
}

func TestTakeReportsRemainingTokens(t *testing.T) {
	s, mr := newTestStore(t)
	mr.SetTime(time.Now())
	ctx := context.Background()
	b := bucket(200, 4)

	// A fresh bucket holds Burst tokens; taking one leaves Burst-1.
	for want := int64(3); want >= 0; want-- {
		res, err := s.Take(ctx, tenantKey, b)
		require.NoError(t, err)
		require.True(t, res.Allowed)
		require.Equal(t, want, res.Remaining)
	}
}

// Refill is continuous, which is what removes the fixed-window boundary problem:
// there is no instant at which a fresh allowance appears.
func TestTakeRefillsOverTime(t *testing.T) {
	s, mr := newTestStore(t)
	now := time.Now()
	mr.SetTime(now)
	ctx := context.Background()
	b := bucket(10, 5) // 10/s, burst 5

	// Drain it.
	for i := 0; i < 5; i++ {
		res, err := s.Take(ctx, tenantKey, b)
		require.NoError(t, err)
		require.True(t, res.Allowed)
	}
	res, err := s.Take(ctx, tenantKey, b)
	require.NoError(t, err)
	require.False(t, res.Allowed)

	// Half a second at 10/s is 5 tokens, but the burst caps it at 5.
	now = now.Add(500 * time.Millisecond)
	mr.SetTime(now)

	for i := 0; i < 5; i++ {
		res, err = s.Take(ctx, tenantKey, b)
		require.NoError(t, err)
		require.True(t, res.Allowed, "token %d should have refilled", i+1)
	}
	res, err = s.Take(ctx, tenantKey, b)
	require.NoError(t, err)
	require.False(t, res.Allowed, "refill must be capped at the burst")
}

func TestTakeRefillIsProportionalToElapsedTime(t *testing.T) {
	s, mr := newTestStore(t)
	now := time.Now()
	mr.SetTime(now)
	ctx := context.Background()
	b := bucket(10, 10)

	for i := 0; i < 10; i++ {
		_, err := s.Take(ctx, tenantKey, b)
		require.NoError(t, err)
	}

	// 300ms at 10/s is 3 tokens.
	now = now.Add(300 * time.Millisecond)
	mr.SetTime(now)

	for i := 0; i < 3; i++ {
		res, err := s.Take(ctx, tenantKey, b)
		require.NoError(t, err)
		require.True(t, res.Allowed, "token %d of 3 should be available", i+1)
	}
	res, err := s.Take(ctx, tenantKey, b)
	require.NoError(t, err)
	require.False(t, res.Allowed, "only 3 tokens should have accrued")
}

// The sustained rate is what the limit actually enforces: over a long window the
// caller gets rate x seconds, regardless of how they bunch their requests.
func TestTakeEnforcesSustainedRate(t *testing.T) {
	s, mr := newTestStore(t)
	now := time.Now()
	mr.SetTime(now)
	ctx := context.Background()
	b := bucket(100, 10)

	allowed := 0
	// Ten batches, advancing 100ms after each. The first batch spends the burst
	// (10); the following nine each get one 100ms refill at 100/s (10 apiece).
	// So 10 + 9x10 = 100 of the 500 attempted.
	for step := 0; step < 10; step++ {
		for i := 0; i < 50; i++ {
			res, err := s.Take(ctx, tenantKey, b)
			require.NoError(t, err)
			if res.Allowed {
				allowed++
			}
		}
		now = now.Add(100 * time.Millisecond)
		mr.SetTime(now)
	}

	require.InDelta(t, 100, allowed, 2,
		"only the burst plus accrued refill should pass, not the 500 attempted")
}

func TestTakeRetryAfterShrinksAsTokensAccrue(t *testing.T) {
	s, mr := newTestStore(t)
	now := time.Now()
	mr.SetTime(now)
	ctx := context.Background()
	b := bucket(2, 1) // slow refill so the wait is measurable

	_, err := s.Take(ctx, tenantKey, b)
	require.NoError(t, err)

	first, err := s.Take(ctx, tenantKey, b)
	require.NoError(t, err)
	require.False(t, first.Allowed)

	now = now.Add(250 * time.Millisecond)
	mr.SetTime(now)

	second, err := s.Take(ctx, tenantKey, b)
	require.NoError(t, err)
	require.False(t, second.Allowed)
	require.Less(t, second.RetryAfter, first.RetryAfter,
		"the wait must shorten as the bucket refills")
}

func TestTakeBucketsAreIndependent(t *testing.T) {
	s, mr := newTestStore(t)
	mr.SetTime(time.Now())
	ctx := context.Background()
	b := bucket(200, 1)

	res, err := s.Take(ctx, store.Key{Limiter: "tenant", Bucket: "key-a"}, b)
	require.NoError(t, err)
	require.True(t, res.Allowed)

	res, err = s.Take(ctx, store.Key{Limiter: "tenant", Bucket: "key-a"}, b)
	require.NoError(t, err)
	require.False(t, res.Allowed)

	// A different tenant is unaffected - the whole point of per-key quotas.
	res, err = s.Take(ctx, store.Key{Limiter: "tenant", Bucket: "key-b"}, b)
	require.NoError(t, err)
	require.True(t, res.Allowed)
}

func TestTakeUsesASeparateKeyspaceFromConsume(t *testing.T) {
	s, mr := newTestStore(t)
	mr.SetTime(time.Now())
	ctx := context.Background()

	_, err := s.Take(ctx, tenantKey, bucket(200, 5))
	require.NoError(t, err)

	require.Contains(t, mr.Keys(), prefix+"tenant:{apikeyhash}:tb")
	require.NotContains(t, mr.Keys(), prefix+"tenant:{apikeyhash}:c",
		"a token bucket must not collide with the fixed-window counter")
}

func TestTakeKeyIsHashTagged(t *testing.T) {
	s, mr := newTestStore(t)
	mr.SetTime(time.Now())

	_, err := s.Take(context.Background(),
		store.Key{Limiter: "tenant", Bucket: "ab{cd}"}, bucket(200, 5))
	require.NoError(t, err)

	require.Contains(t, mr.Keys(), prefix+"tenant:{abcd}:tb")
}

// An idle bucket must not linger: with thousands of API keys the keyspace would
// otherwise grow without bound.
func TestTakeSetsExpiryOnTheBucket(t *testing.T) {
	s, mr := newTestStore(t)
	mr.SetTime(time.Now())

	_, err := s.Take(context.Background(), tenantKey, bucket(200, 400))
	require.NoError(t, err)

	require.Positive(t, mr.TTL(prefix+"tenant:{apikeyhash}:tb"))
}

func TestTakeRejectsInvalidBucket(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	for _, b := range []store.Bucket{{}, {Rate: 0, Burst: 5}, {Rate: 5, Burst: 0}, {Rate: -1, Burst: 5}} {
		_, err := s.Take(ctx, tenantKey, b)
		require.Error(t, err, "bucket %+v must be rejected", b)
	}
}

func TestTakeSurfacesBackendErrors(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Close()

	_, err := s.Take(context.Background(), tenantKey, bucket(200, 5))
	require.Error(t, err, "a backend failure must be reported so the caller can fail open")
}
