// SPDX-License-Identifier: Apache-2.0

// Package storetest provides an in-memory store.Store for tests.
package storetest

import (
	"context"
	"sync"
	"time"

	"github.com/dgamo/forwardlimit/internal/store"
)

// Fake is an in-memory Store that reproduces the real semantics - a counting window
// that rolls over, a block that outlives it, and a token bucket that refills - so
// engine tests are meaningful rather than trivially satisfied.
//
// Use SetClock to advance time.
//
// Fake is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	counts  map[string]int64
	expires map[string]time.Time
	blocked map[string]time.Time
	buckets map[string]*bucketState
	now     func() time.Time

	// Err, when non-nil, is returned from every Consume call. Used to exercise
	// fail-open behaviour.
	Err error

	// Calls counts Consume invocations.
	Calls int

	// Closed records whether Close was called.
	Closed bool
}

type bucketState struct {
	tokens float64
	ts     time.Time
}

// New returns a ready-to-use Fake.
func New() *Fake {
	return &Fake{
		counts:  make(map[string]int64),
		expires: make(map[string]time.Time),
		blocked: make(map[string]time.Time),
		buckets: make(map[string]*bucketState),
		now:     time.Now,
	}
}

// SetClock replaces the time source, allowing tests to advance time.
func (f *Fake) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// Consume implements store.Store.
func (f *Fake) Consume(_ context.Context, key store.Key, rule store.Rule) (store.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Calls++
	if f.Err != nil {
		return store.Result{}, f.Err
	}

	k := key.String()
	now := f.now()

	if until, ok := f.blocked[k]; ok {
		if now.Before(until) {
			return store.Result{Blocked: true, Count: -1, RetryAfter: until.Sub(now)}, nil
		}
		delete(f.blocked, k)
	}

	// The counting window rolls over, as it does in Redis via the counter's TTL.
	if exp, ok := f.expires[k]; !ok || !now.Before(exp) {
		f.counts[k] = 0
		f.expires[k] = now.Add(rule.Window)
	}

	f.counts[k]++
	count := f.counts[k]

	if count > rule.Limit {
		f.blocked[k] = now.Add(rule.Block)
		// Dropped with the block, matching the Lua script: without this a counter
		// that outlives the block re-blocks immediately and `block` silently
		// becomes "the rest of the window".
		delete(f.counts, k)
		delete(f.expires, k)
		return store.Result{Blocked: true, Count: count, RetryAfter: rule.Block}, nil
	}
	return store.Result{Count: count}, nil
}

// Take implements store.Store with a real token bucket, so tests exercising
// capacity limits behave like production rather than being trivially satisfied.
func (f *Fake) Take(_ context.Context, key store.Key, bucket store.Bucket) (store.TakeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Calls++
	if f.Err != nil {
		return store.TakeResult{}, f.Err
	}

	k := key.String()
	now := f.now()

	b, ok := f.buckets[k]
	if !ok {
		b = &bucketState{tokens: float64(bucket.Burst), ts: now}
		f.buckets[k] = b
	}

	if elapsed := now.Sub(b.ts); elapsed > 0 {
		b.tokens += elapsed.Seconds() * float64(bucket.Rate)
		if b.tokens > float64(bucket.Burst) {
			b.tokens = float64(bucket.Burst)
		}
	}
	b.ts = now

	if b.tokens >= 1 {
		b.tokens--
		return store.TakeResult{Allowed: true, Remaining: int64(b.tokens)}, nil
	}

	wait := time.Duration((1 - b.tokens) / float64(bucket.Rate) * float64(time.Second))
	return store.TakeResult{RetryAfter: wait}, nil
}

// Close implements store.Store.
func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Closed = true
	return nil
}

// CallCount returns the number of Consume calls made so far.
func (f *Fake) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls
}
