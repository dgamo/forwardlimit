// SPDX-License-Identifier: Apache-2.0

package breaker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/store"
	"github.com/dgamo/forwardlimit/internal/store/breaker"
)

type stubStore struct {
	mu     sync.Mutex
	err    error
	calls  int
	closed bool
}

func (s *stubStore) Consume(context.Context, store.Key, store.Rule) (store.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return store.Result{}, s.err
	}
	return store.Result{Count: 1}, nil
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

func (s *stubStore) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *stubStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

var (
	testKey    = store.Key{Limiter: "login", Bucket: "abc"}
	testRule   = store.Rule{Limit: 3, Window: time.Minute, Block: time.Minute}
	errBackend = errors.New("backend down")
)

func TestPassesThroughWhenHealthy(t *testing.T) {
	inner := &stubStore{}
	b := breaker.New(inner, breaker.Options{Threshold: 3, Cooldown: time.Second})

	for i := 0; i < 5; i++ {
		res, err := b.Consume(context.Background(), testKey, testRule)
		require.NoError(t, err)
		require.Equal(t, int64(1), res.Count)
	}
	require.Equal(t, 5, inner.callCount())
}

// The point of the breaker: once the backend is known bad, stop dialling it. The
// PoC measured go-redis's dial timeout adding 3s to *every* request during an
// outage, which is worse than the outage itself.
func TestOpensAfterConsecutiveFailuresAndStopsCallingInner(t *testing.T) {
	now := time.Now()
	inner := &stubStore{err: errBackend}
	b := breaker.New(inner, breaker.Options{
		Threshold: 3,
		Cooldown:  time.Minute,
		Now:       func() time.Time { return now },
	})

	for i := 0; i < 3; i++ {
		_, err := b.Consume(context.Background(), testKey, testRule)
		require.ErrorIs(t, err, errBackend)
	}
	require.Equal(t, 3, inner.callCount())
	require.True(t, b.Open())

	for i := 0; i < 10; i++ {
		_, err := b.Consume(context.Background(), testKey, testRule)
		require.ErrorIs(t, err, breaker.ErrOpen)
	}
	require.Equal(t, 3, inner.callCount(), "an open breaker must not touch the inner store")
}

func TestSuccessResetsFailureCount(t *testing.T) {
	inner := &stubStore{err: errBackend}
	b := breaker.New(inner, breaker.Options{Threshold: 3, Cooldown: time.Minute})

	for i := 0; i < 2; i++ {
		_, err := b.Consume(context.Background(), testKey, testRule)
		require.ErrorIs(t, err, errBackend)
	}

	inner.setErr(nil)
	_, err := b.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)

	inner.setErr(errBackend)
	for i := 0; i < 2; i++ {
		_, err = b.Consume(context.Background(), testKey, testRule)
		require.ErrorIs(t, err, errBackend, "counter must have reset, so 2 failures is not enough to open")
	}
	require.False(t, b.Open())
}

func TestProbeAfterCooldownClosesBreakerOnSuccess(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	inner := &stubStore{err: errBackend}
	b := breaker.New(inner, breaker.Options{Threshold: 1, Cooldown: 30 * time.Second, Now: clock})

	_, err := b.Consume(context.Background(), testKey, testRule)
	require.ErrorIs(t, err, errBackend)
	require.True(t, b.Open())

	// Still cooling down.
	now = now.Add(29 * time.Second)
	_, err = b.Consume(context.Background(), testKey, testRule)
	require.ErrorIs(t, err, breaker.ErrOpen)

	// Cooldown elapsed and the backend recovered: the probe should succeed and
	// close the breaker.
	now = now.Add(2 * time.Second)
	inner.setErr(nil)
	before := inner.callCount()
	_, err = b.Consume(context.Background(), testKey, testRule)
	require.NoError(t, err)
	require.Equal(t, before+1, inner.callCount(), "one probe must reach the inner store")
	require.False(t, b.Open())
}

func TestFailedProbeReopensBreaker(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	inner := &stubStore{err: errBackend}
	b := breaker.New(inner, breaker.Options{Threshold: 1, Cooldown: 10 * time.Second, Now: clock})

	_, err := b.Consume(context.Background(), testKey, testRule)
	require.ErrorIs(t, err, errBackend)

	now = now.Add(11 * time.Second)
	_, err = b.Consume(context.Background(), testKey, testRule)
	require.ErrorIs(t, err, errBackend, "the probe itself surfaces the real error")
	require.True(t, b.Open())

	_, err = b.Consume(context.Background(), testKey, testRule)
	require.ErrorIs(t, err, breaker.ErrOpen, "must be open again without waiting for another cooldown")
}

func TestDisabledBreakerAlwaysPassesThrough(t *testing.T) {
	inner := &stubStore{err: errBackend}
	b := breaker.New(inner, breaker.Options{Threshold: 0})

	for i := 0; i < 5; i++ {
		_, err := b.Consume(context.Background(), testKey, testRule)
		require.ErrorIs(t, err, errBackend)
	}
	require.Equal(t, 5, inner.callCount())
	require.False(t, b.Open())
}

func TestCloseClosesInner(t *testing.T) {
	inner := &stubStore{}
	b := breaker.New(inner, breaker.Options{Threshold: 1, Cooldown: time.Second})
	require.NoError(t, b.Close())
	require.True(t, inner.closed)
}

func TestConcurrentUseIsSafe(t *testing.T) {
	inner := &stubStore{err: errBackend}
	b := breaker.New(inner, breaker.Options{Threshold: 5, Cooldown: time.Millisecond})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = b.Consume(context.Background(), testKey, testRule)
			}
		}()
	}
	wg.Wait()
}
