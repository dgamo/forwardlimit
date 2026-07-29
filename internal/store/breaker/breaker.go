// SPDX-License-Identifier: Apache-2.0

// Package breaker decorates a store.Store with a circuit breaker.
//
// Its purpose is speed, not correctness: an outage already fails open, but a load
// test measured go-redis's dial timeout adding ~3s to every request while it lasted,
// which cascades into client timeouts and is worse than the outage.
//
// After Threshold consecutive failures the breaker returns ErrOpen without dialling.
// Callers treat any error as fail-open, so the effect is fast fail-open. After
// Cooldown one probe is let through to detect recovery.
package breaker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dgamo/forwardlimit/internal/store"
)

// ErrOpen is returned while the breaker is open.
var ErrOpen = errors.New("breaker: circuit open, store calls suppressed")

// Options configures the breaker.
type Options struct {
	// Threshold is the number of consecutive failures that opens the circuit.
	// Zero or negative disables the breaker entirely.
	Threshold int
	// Cooldown is how long the circuit stays open before a probe is allowed.
	Cooldown time.Duration
	// Now overrides the clock. Tests use this; production leaves it nil.
	Now func() time.Time
}

// Store is a circuit-breaking store.Store decorator.
type Store struct {
	inner     store.Store
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	probing   bool
}

var _ store.Store = (*Store)(nil)

// New wraps inner with a circuit breaker.
func New(inner store.Store, opts Options) *Store {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{
		inner:     inner,
		threshold: opts.Threshold,
		cooldown:  opts.Cooldown,
		now:       now,
	}
}

// Consume implements store.Store.
func (s *Store) Consume(ctx context.Context, key store.Key, rule store.Rule) (store.Result, error) {
	if !s.allow() {
		return store.Result{}, ErrOpen
	}

	res, err := s.inner.Consume(ctx, key, rule)
	s.record(err == nil)
	return res, err
}

// Take implements store.Store, subject to the same circuit.
func (s *Store) Take(ctx context.Context, key store.Key, bucket store.Bucket) (store.TakeResult, error) {
	if !s.allow() {
		return store.TakeResult{}, ErrOpen
	}

	res, err := s.inner.Take(ctx, key, bucket)
	s.record(err == nil)
	return res, err
}

// Close closes the inner store.
func (s *Store) Close() error { return s.inner.Close() }

// Open reports whether the circuit is currently open.
func (s *Store) Open() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isOpenLocked()
}

// allow decides whether this call may reach the inner store, admitting exactly
// one probe once the cooldown has elapsed.
func (s *Store) allow() bool {
	if s.threshold <= 0 {
		return true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.isOpenLocked() {
		return true
	}
	if s.probing {
		return false
	}
	if s.now().Before(s.openUntil) {
		return false
	}

	s.probing = true
	return true
}

func (s *Store) record(success bool) {
	if s.threshold <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.probing = false

	if success {
		s.failures = 0
		s.openUntil = time.Time{}
		return
	}

	s.failures++
	if s.failures >= s.threshold {
		s.openUntil = s.now().Add(s.cooldown)
	}
}

func (s *Store) isOpenLocked() bool {
	return !s.openUntil.IsZero()
}
