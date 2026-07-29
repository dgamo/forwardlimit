// SPDX-License-Identifier: Apache-2.0

// Package store defines the counter backend that enforces rate limit rules.
//
// Two algorithms, because the use cases differ: Consume is a fixed window whose
// Block outlives the counting window, for abuse controls; Take is a token bucket,
// for capacity. A limiter uses exactly one.
package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Rule is a fixed window with a hard block.
type Rule struct {
	Limit  int64
	Window time.Duration
	// Block is how long a bucket stays blocked once Limit is exceeded. It has its
	// own TTL, so it is not released when the counting window rolls over.
	Block time.Duration
}

// Valid reports whether the rule is usable.
//
// An incomplete rule must be treated as "limiter disabled, allow the request", so
// a half-configured limiter fails open rather than rejecting traffic.
func (r Rule) Valid() bool {
	return r.Limit > 0 && r.Window > 0 && r.Block > 0
}

// Key identifies a bucket. Limiter namespaces it so two limiters never share a
// counter; Bucket is the extracted value, already hashed where applicable.
//
// Turning a Key into concrete storage keys is the Store's job - which is what lets
// the Redis implementation apply a cluster hash tag without the engine knowing
// anything about Redis.
type Key struct {
	Limiter string
	Bucket  string
}

// String renders a Key for logs and errors. It is not the storage key.
func (k Key) String() string { return k.Limiter + ":" + k.Bucket }

// Result is the outcome of consuming one unit against a bucket.
type Result struct {
	Blocked bool
	// Count is the requests recorded in the current window, or -1 when the decision
	// came from an existing block rather than a fresh count.
	Count int64
	// RetryAfter is how long the block lasts, zero when not blocked. Whether it
	// reaches the client is the limiter's choice, not the store's.
	RetryAfter time.Duration
}

// Bucket is a token bucket: Burst tokens, refilling at Rate per second.
//
// Preferred over Rule for capacity limits. A fixed window has a boundary a caller
// can straddle to push twice the limit through in a moment - precisely the spike
// capacity protection exists to prevent - and it recovers in steps rather than
// continuously.
type Bucket struct {
	Rate  int64
	Burst int64
}

// Valid reports whether the bucket is usable. As with Rule, incomplete disables the
// limiter rather than rejecting traffic.
func (b Bucket) Valid() bool { return b.Rate > 0 && b.Burst > 0 }

// TakeResult is the outcome of taking a single token.
type TakeResult struct {
	Allowed   bool
	Remaining int64
	// RetryAfter is how long until the next token, zero when allowed.
	RetryAfter time.Duration
}

// Store is the counter backend. Implementations must be safe for concurrent use.
type Store interface {
	Consume(ctx context.Context, key Key, rule Rule) (Result, error)
	Take(ctx context.Context, key Key, bucket Bucket) (TakeResult, error)
	Close() error
}

// ErrDisabled indicates the store has been switched off by configuration.
var ErrDisabled = errors.New("store disabled")

// SanitizeBucket strips braces, which would otherwise interfere with the Redis
// cluster hash tag the store adds. Bucket values are hex digests or addresses in
// practice, so this is defensive.
func SanitizeBucket(s string) string {
	if !strings.ContainsAny(s, "{}") {
		return s
	}
	return strings.NewReplacer("{", "", "}", "").Replace(s)
}
