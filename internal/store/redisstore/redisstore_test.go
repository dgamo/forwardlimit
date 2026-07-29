// SPDX-License-Identifier: Apache-2.0

package redisstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/store"
	"github.com/dgamo/forwardlimit/internal/store/redisstore"
)

const prefix = "forwardlimit:"

func newTestStore(t *testing.T) (*redisstore.Store, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	return redisstore.NewWithClient(client, prefix), mr
}

func rule(limit int64, window, block time.Duration) store.Rule {
	return store.Rule{Limit: limit, Window: window, Block: block}
}

func TestConsumeAllowsUpToLimitThenBlocks(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	k := store.Key{Limiter: "login", Bucket: "abc"}
	r := rule(3, time.Minute, 2*time.Minute)

	for i := int64(1); i <= 3; i++ {
		res, err := s.Consume(ctx, k, r)
		require.NoError(t, err)
		require.False(t, res.Blocked, "request %d must be allowed", i)
		require.Equal(t, i, res.Count)
		require.Zero(t, res.RetryAfter)
	}

	res, err := s.Consume(ctx, k, r)
	require.NoError(t, err)
	require.True(t, res.Blocked, "request 4 must be blocked")
	require.Equal(t, int64(4), res.Count)
	require.Equal(t, 2*time.Minute, res.RetryAfter)
}

// The distinguishing behaviour of the algorithm: exceeding the limit starts a
// hard block that outlives the counting window, so the caller is NOT released
// when the window rolls over.
func TestBlockOutlivesCountingWindow(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	k := store.Key{Limiter: "login", Bucket: "abc"}
	r := rule(1, 10*time.Second, 5*time.Minute)

	_, err := s.Consume(ctx, k, r)
	require.NoError(t, err)

	res, err := s.Consume(ctx, k, r)
	require.NoError(t, err)
	require.True(t, res.Blocked)

	// Advance well past the counting window but not past the block.
	mr.FastForward(30 * time.Second)

	res, err = s.Consume(ctx, k, r)
	require.NoError(t, err)
	require.True(t, res.Blocked, "must still be blocked after the window expired")
	require.Equal(t, int64(-1), res.Count, "decision came from the block, not a fresh count")
	require.Positive(t, res.RetryAfter)
}

func TestBucketRecoversAfterBlockExpires(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	k := store.Key{Limiter: "login", Bucket: "abc"}
	r := rule(1, 10*time.Second, 30*time.Second)

	_, err := s.Consume(ctx, k, r)
	require.NoError(t, err)
	res, err := s.Consume(ctx, k, r)
	require.NoError(t, err)
	require.True(t, res.Blocked)

	mr.FastForward(31 * time.Second)

	res, err = s.Consume(ctx, k, r)
	require.NoError(t, err)
	require.False(t, res.Blocked, "must be allowed once the block expires")
	require.Equal(t, int64(1), res.Count, "counter must have reset")
}

func TestCounterKeyCarriesWindowTTL(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	k := store.Key{Limiter: "login", Bucket: "abc"}

	_, err := s.Consume(ctx, k, rule(5, 90*time.Second, time.Minute))
	require.NoError(t, err)

	require.Equal(t, 90*time.Second, mr.TTL(prefix+"login:{abc}:c"))
}

// Both keys must share a hash tag or the Lua script fails on a sharded cluster
// with CROSSSLOT.
func TestKeysAreHashTaggedAndNamespaced(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()

	k := store.Key{Limiter: "login", Bucket: "abc"}

	// The counter exists while under the limit.
	_, err := s.Consume(ctx, k, rule(1, time.Minute, time.Minute))
	require.NoError(t, err)
	require.Contains(t, mr.Keys(), prefix+"login:{abc}:c")

	// Exceeding it replaces the counter with the block marker. Both names carry the
	// same hash tag, which is what a multi-key script needs on a sharded cluster.
	_, err = s.Consume(ctx, k, rule(1, time.Minute, time.Minute))
	require.NoError(t, err)
	require.Contains(t, mr.Keys(), prefix+"login:{abc}:b")
}

// Blocking clears the counter, so `block` is the whole story: without this, a
// counter that outlives the block re-blocks on the next request and the effective
// duration silently becomes the rest of the window.
func TestBlockingClearsTheCounterSoBlockMeansBlock(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	k := store.Key{Limiter: "login", Bucket: "abc"}
	// Window deliberately much longer than the block.
	r := rule(2, time.Hour, 10*time.Second)

	for i := 1; i <= 2; i++ {
		res, err := s.Consume(ctx, k, r)
		require.NoError(t, err)
		require.False(t, res.Blocked, "request %d", i)
	}

	res, err := s.Consume(ctx, k, r)
	require.NoError(t, err)
	require.True(t, res.Blocked)
	require.NotContains(t, mr.Keys(), prefix+"login:{abc}:c",
		"the counter must be dropped along with the block")

	// Once the block lapses the caller gets a fresh window, even though the original
	// counting window has an hour left to run.
	mr.FastForward(11 * time.Second)

	res, err = s.Consume(ctx, k, r)
	require.NoError(t, err)
	require.False(t, res.Blocked, "block was 10s, not the rest of the hour")
	require.Equal(t, int64(1), res.Count)
}

func TestDifferentLimitersDoNotShareCounters(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	r := rule(1, time.Minute, time.Minute)

	res, err := s.Consume(ctx, store.Key{Limiter: "login", Bucket: "same"}, r)
	require.NoError(t, err)
	require.False(t, res.Blocked)

	res, err = s.Consume(ctx, store.Key{Limiter: "ip", Bucket: "same"}, r)
	require.NoError(t, err)
	require.False(t, res.Blocked, "a different limiter must have its own bucket")
	require.Equal(t, int64(1), res.Count)
}

func TestDifferentBucketsAreIndependent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	r := rule(1, time.Minute, time.Minute)

	for _, bucket := range []string{"a", "b", "c"} {
		res, err := s.Consume(ctx, store.Key{Limiter: "login", Bucket: bucket}, r)
		require.NoError(t, err)
		require.False(t, res.Blocked)
	}
}

func TestInvalidRuleIsRejected(t *testing.T) {
	s, _ := newTestStore(t)

	_, err := s.Consume(context.Background(), store.Key{Limiter: "login", Bucket: "abc"}, store.Rule{})
	require.Error(t, err)
}

func TestBucketWithBracesIsSanitized(t *testing.T) {
	s, mr := newTestStore(t)

	_, err := s.Consume(context.Background(),
		store.Key{Limiter: "login", Bucket: "ab{cd}"}, rule(5, time.Minute, time.Minute))
	require.NoError(t, err)

	require.Contains(t, mr.Keys(), prefix+"login:{abcd}:c")
}

func TestConsumeSurfacesBackendErrors(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Close() // make the backend unreachable

	_, err := s.Consume(context.Background(),
		store.Key{Limiter: "login", Bucket: "abc"}, rule(5, time.Minute, time.Minute))
	require.Error(t, err, "backend failure must be reported so the caller can fail open")
}

// Sub-second windows must not round down to a zero TTL.
func TestSubSecondDurationsRoundUp(t *testing.T) {
	s, mr := newTestStore(t)

	_, err := s.Consume(context.Background(),
		store.Key{Limiter: "login", Bucket: "abc"}, rule(5, 100*time.Millisecond, 200*time.Millisecond))
	require.NoError(t, err)

	require.Equal(t, time.Second, mr.TTL(prefix+"login:{abc}:c"))
}
