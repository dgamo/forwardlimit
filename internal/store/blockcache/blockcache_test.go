// SPDX-License-Identifier: Apache-2.0

package blockcache_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/store"
	"github.com/dgamo/forwardlimit/internal/store/blockcache"
)

// stubStore returns a scripted sequence of results and counts calls.
type stubStore struct {
	mu      sync.Mutex
	results []store.Result
	err     error
	calls   int
	closed  bool
}

func (s *stubStore) Consume(context.Context, store.Key, store.Rule) (store.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return store.Result{}, s.err
	}
	if len(s.results) == 0 {
		return store.Result{}, nil
	}
	r := s.results[0]
	if len(s.results) > 1 {
		s.results = s.results[1:]
	}
	return r, nil
}

func (s *stubStore) Take(context.Context, store.Key, store.Bucket) (store.TakeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return store.TakeResult{}, s.err
	}
	return store.TakeResult{Allowed: true}, nil
}

func (s *stubStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *stubStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

var (
	testKey  = store.Key{Limiter: "login", Bucket: "abc"}
	testRule = store.Rule{Limit: 3, Window: time.Minute, Block: 2 * time.Minute}
)

func newCache(t *testing.T, inner store.Store, opts blockcache.Options) *blockcache.Store {
	t.Helper()
	c := blockcache.New(inner, opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The central safety property: allowed verdicts are never cached, so counting
// always reaches the authoritative store and the cache can never turn a request
// that should be counted into an uncounted one.
func TestAllowedResultsAreNotCached(t *testing.T) {
	inner := &stubStore{results: []store.Result{{Blocked: false, Count: 1}}}
	c := newCache(t, inner, blockcache.Options{MaxEntries: 10, MaxTTL: time.Minute})

	for i := 0; i < 3; i++ {
		res, err := c.Consume(context.Background(), testKey, testRule)
		require.NoError(t, err)
		require.False(t, res.Blocked)
	}
	require.Equal(t, 3, inner.callCount(), "every allowed request must reach the inner store")
}

func TestBlockedResultShortCircuitsSubsequentCalls(t *testing.T) {
	inner := &stubStore{results: []store.Result{
		{Blocked: true, Count: 4, RetryAfter: 2 * time.Minute},
	}}
	c := newCache(t, inner, blockcache.Options{MaxEntries: 10, MaxTTL: time.Hour})

	res, err := c.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)
	require.True(t, res.Blocked)
	require.Equal(t, 1, inner.callCount())

	for i := 0; i < 5; i++ {
		res, err = c.Consume(context.Background(), testKey, testRule)
		require.NoError(t, err)
		require.True(t, res.Blocked)
	}
	require.Equal(t, 1, inner.callCount(), "cached blocks must not reach the inner store")
	require.Equal(t, int64(5), c.Hits())
}

// A 1-hour block with an uncapped cache would be unflushable across replicas, so
// entries are capped to allow a manual unblock to propagate.
func TestCachedTTLIsCappedByMaxTTL(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	inner := &stubStore{results: []store.Result{
		{Blocked: true, Count: 4, RetryAfter: time.Hour},
	}}
	c := newCache(t, inner, blockcache.Options{
		MaxEntries: 10,
		MaxTTL:     30 * time.Second,
		Now:        clock,
	})

	_, err := c.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)
	require.Equal(t, 1, inner.callCount())

	// Still inside the cap: served locally.
	now = now.Add(29 * time.Second)
	_, err = c.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)
	require.Equal(t, 1, inner.callCount())

	// Past the cap, but well inside the real 1h block: must re-check the store.
	now = now.Add(2 * time.Second)
	_, err = c.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)
	require.Equal(t, 2, inner.callCount(), "entry must expire at MaxTTL, not at RetryAfter")
}

func TestExpiredEntriesAreNotServed(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	inner := &stubStore{results: []store.Result{
		{Blocked: true, Count: 4, RetryAfter: 10 * time.Second},
	}}
	c := newCache(t, inner, blockcache.Options{MaxEntries: 10, MaxTTL: time.Hour, Now: clock})

	_, err := c.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)

	now = now.Add(11 * time.Second)
	_, err = c.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)
	require.Equal(t, 2, inner.callCount())
}

func TestCacheIsBounded(t *testing.T) {
	inner := &stubStore{results: []store.Result{
		{Blocked: true, Count: 4, RetryAfter: time.Hour},
	}}
	c := newCache(t, inner, blockcache.Options{MaxEntries: 2, MaxTTL: time.Hour})

	// Three distinct buckets, capacity two.
	for _, b := range []string{"a", "b", "c"} {
		_, err := c.Consume(context.Background(), store.Key{Limiter: "login", Bucket: b}, testRule)
		require.NoError(t, err)
	}
	require.LessOrEqual(t, c.Len(), 2, "cache must not grow beyond MaxEntries")
}

func TestDifferentLimitersAreCachedSeparately(t *testing.T) {
	inner := &stubStore{results: []store.Result{
		{Blocked: true, Count: 4, RetryAfter: time.Hour},
	}}
	c := newCache(t, inner, blockcache.Options{MaxEntries: 10, MaxTTL: time.Hour})

	_, err := c.Consume(context.Background(), store.Key{Limiter: "login", Bucket: "same"}, testRule)
	require.NoError(t, err)
	_, err = c.Consume(context.Background(), store.Key{Limiter: "ip", Bucket: "same"}, testRule)
	require.NoError(t, err)

	require.Equal(t, 2, inner.callCount(), "same bucket under a different limiter is a different entry")
}

func TestErrorsArePassedThroughAndNotCached(t *testing.T) {
	wantErr := errors.New("backend down")
	inner := &stubStore{err: wantErr}
	c := newCache(t, inner, blockcache.Options{MaxEntries: 10, MaxTTL: time.Hour})

	for i := 0; i < 3; i++ {
		_, err := c.Consume(context.Background(), testKey, testRule)
		require.ErrorIs(t, err, wantErr)
	}
	require.Equal(t, 3, inner.callCount())
}

func TestBlockedWithoutRetryAfterIsNotCached(t *testing.T) {
	inner := &stubStore{results: []store.Result{{Blocked: true, Count: 4, RetryAfter: 0}}}
	c := newCache(t, inner, blockcache.Options{MaxEntries: 10, MaxTTL: time.Hour})

	_, err := c.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)
	_, err = c.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)

	require.Equal(t, 2, inner.callCount(), "a block with no TTL cannot be cached safely")
}

func TestCloseClosesInner(t *testing.T) {
	inner := &stubStore{}
	c := blockcache.New(inner, blockcache.Options{MaxEntries: 1, MaxTTL: time.Second})
	require.NoError(t, c.Close())
	require.True(t, inner.closed)
}

func TestConcurrentUseIsSafe(t *testing.T) {
	inner := &stubStore{results: []store.Result{
		{Blocked: true, Count: 4, RetryAfter: time.Minute},
	}}
	c := newCache(t, inner, blockcache.Options{MaxEntries: 1000, MaxTTL: time.Minute})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = c.Consume(context.Background(),
					store.Key{Limiter: "login", Bucket: string(rune('a' + n%26))}, testRule)
			}
		}(i)
	}
	wg.Wait()
}

// The background sweeper must reclaim expired entries, or a long-running process
// accumulates them until the capacity bound stops caching altogether.
func TestSweeperEvictsExpiredEntries(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}

	inner := &stubStore{results: []store.Result{
		{Blocked: true, Count: 4, RetryAfter: 20 * time.Millisecond},
	}}
	c := blockcache.New(inner, blockcache.Options{
		MaxEntries:    10,
		MaxTTL:        time.Hour,
		SweepInterval: 5 * time.Millisecond,
		Now:           clock,
	})
	t.Cleanup(func() { _ = c.Close() })

	_, err := c.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)
	require.Equal(t, 1, c.Len())

	// Advance past the entry's expiry and let the sweeper run.
	mu.Lock()
	now = now.Add(time.Second)
	mu.Unlock()

	require.Eventually(t, func() bool { return c.Len() == 0 },
		2*time.Second, 5*time.Millisecond, "sweeper should have evicted the expired entry")
}
