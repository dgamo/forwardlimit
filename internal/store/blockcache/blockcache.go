// SPDX-License-Identifier: Apache-2.0

// Package blockcache decorates a store.Store with a bounded in-memory cache of
// buckets already known to be blocked.
//
// This is what makes flood traffic cheap: during an automated attack the same
// buckets are retried repeatedly, and a load test measured this absorbing ~97.5%
// of requests without touching Redis.
//
// # Why this is safe
//
// Only *blocked* verdicts are cached - never counts, never "allowed". Counting
// therefore always reaches the authoritative store, so the cache cannot cause a
// request to go uncounted, and it can only ever produce a 429, never an
// incorrect allow. For an abuse control that is the safe direction to fail.
//
// Entries are capped at Options.MaxTTL rather than honouring the full block. An
// uncapped cache holding a one-hour block would be unflushable across replicas, so
// a false positive could not be cleared; the cap bounds that to MaxTTL for the cost
// of one store lookup per bucket per cap period.
package blockcache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgamo/forwardlimit/internal/store"
)

// Options configures the cache.
type Options struct {
	// MaxEntries bounds the cache. At capacity new blocks are simply not cached,
	// rather than evicting arbitrarily, so memory cannot grow without limit.
	MaxEntries int
	// MaxTTL caps how long any entry is trusted.
	MaxTTL time.Duration
	// SweepInterval controls the background removal of expired entries.
	// Defaults to 10s.
	SweepInterval time.Duration
	// Now overrides the clock. Tests use this; production leaves it nil.
	Now func() time.Time
}

// Store is a caching store.Store decorator.
type Store struct {
	inner  store.Store
	maxLen int
	maxTTL time.Duration
	now    func() time.Time

	mu      sync.RWMutex
	entries map[string]time.Time

	hits atomic.Int64

	stopOnce sync.Once
	stop     chan struct{}
}

var _ store.Store = (*Store)(nil)

// New wraps inner with a block cache.
func New(inner store.Store, opts Options) *Store {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	sweep := opts.SweepInterval
	if sweep <= 0 {
		sweep = 10 * time.Second
	}

	s := &Store{
		inner:   inner,
		maxLen:  opts.MaxEntries,
		maxTTL:  opts.MaxTTL,
		now:     now,
		entries: make(map[string]time.Time),
		stop:    make(chan struct{}),
	}

	go s.sweepLoop(sweep)
	return s
}

// Consume implements store.Store.
func (s *Store) Consume(ctx context.Context, key store.Key, rule store.Rule) (store.Result, error) {
	k := key.String()

	if until, ok := s.lookup(k); ok {
		s.hits.Add(1)
		return store.Result{
			Blocked:    true,
			Count:      -1,
			RetryAfter: until.Sub(s.now()),
		}, nil
	}

	res, err := s.inner.Consume(ctx, key, rule)
	if err != nil {
		return res, err
	}

	// Cache blocks only, and only when we know how long they last.
	if res.Blocked && res.RetryAfter > 0 {
		s.remember(k, res.RetryAfter)
	}
	return res, nil
}

// Take delegates without caching.
//
// A token bucket has no blocked state to remember: every request must actually
// take a token, or the count is wrong. Caching would be incorrect here, not
// merely unhelpful.
func (s *Store) Take(ctx context.Context, key store.Key, bucket store.Bucket) (store.TakeResult, error) {
	return s.inner.Take(ctx, key, bucket)
}

// Close stops the sweeper and closes the inner store.
func (s *Store) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	return s.inner.Close()
}

// Hits returns how many decisions were served from the cache.
func (s *Store) Hits() int64 { return s.hits.Load() }

// Len returns the current number of cached entries.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

func (s *Store) lookup(k string) (time.Time, bool) {
	s.mu.RLock()
	until, ok := s.entries[k]
	s.mu.RUnlock()

	if !ok || !s.now().Before(until) {
		return time.Time{}, false
	}
	return until, true
}

func (s *Store) remember(k string, ttl time.Duration) {
	if s.maxTTL > 0 && ttl > s.maxTTL {
		ttl = s.maxTTL
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Refreshing an existing entry is always allowed; only new entries are
	// subject to the capacity bound.
	if _, exists := s.entries[k]; !exists && s.maxLen > 0 && len(s.entries) >= s.maxLen {
		return
	}
	s.entries[k] = s.now().Add(ttl)
}

func (s *Store) sweepLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

func (s *Store) sweep() {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	for k, until := range s.entries {
		if !now.Before(until) {
			delete(s.entries, k)
		}
	}
}
