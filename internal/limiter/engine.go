// SPDX-License-Identifier: Apache-2.0

package limiter

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dgamo/forwardlimit/internal/store"
)

// Engine evaluates a fixed set of limiters against each request.
//
// Two behaviours are load-bearing. First block wins, so a blocked request costs at
// most one store call per preceding limiter. And it fails open always - a store
// error, an invalid rule or a panic in a Keyer all allow the request, because rate
// limiting is an abuse control, not an availability dependency.
type Engine struct {
	limiters []Limiter
	store    store.Store
	log      *slog.Logger
}

// NewEngine returns an Engine. Limiters are evaluated in the order supplied, so put
// cheaper or more decisive rules first.
func NewEngine(st store.Store, log *slog.Logger, limiters ...Limiter) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{limiters: limiters, store: st, log: log}
}

// Limiters returns the configured limiters.
func (e *Engine) Limiters() []Limiter { return e.limiters }

// Evaluate decides whether the request may proceed.
func (e *Engine) Evaluate(ctx context.Context, req *Request) Decision {
	var d Decision

	for _, l := range e.limiters {
		if !l.Applies(req.Path) || !l.Active() {
			continue
		}

		bucket, ok := e.bucket(l, req)
		if !ok {
			continue
		}

		res, err := e.apply(ctx, l, bucket)
		switch {
		case err != nil:
			// Truncated: the bucket may be a hash of something sensitive, and
			// logging it in full would undo the point of hashing it.
			e.log.Warn("rate limit store error, failing open",
				slog.String("limiter", l.Name),
				slog.String("bucket", truncate(bucket)),
				slog.String("error", err.Error()))
			d.Evaluations = append(d.Evaluations, Evaluation{l.Name, VerdictFailedOpen})

		case res.blocked && l.DryRun:
			// Record the projection and carry on. A dry-run limiter must not change
			// what any other limiter does, or adding one for measurement would
			// silently disable enforcement behind it. The cost is that later
			// projections read slightly high, which is the far smaller price.
			d.Evaluations = append(d.Evaluations, Evaluation{l.Name, VerdictWouldBlock})
			if !d.Blocked {
				d.Blocked = true
				d.DryRun = true
				d.Limiter = l.Name
				d.Count = res.count
				d.RetryAfter = res.retryAfter
			}

		case res.blocked:
			// An enforced block decides the response and overrides any dry-run
			// verdict already recorded.
			d.Blocked = true
			d.DryRun = false
			d.Limiter = l.Name
			d.Count = res.count
			d.RetryAfter = res.retryAfter
			d.AdviseRetryAfter = l.AdviseRetryAfter
			d.Evaluations = append(d.Evaluations, Evaluation{l.Name, VerdictBlocked})
			return d

		default:
			d.Evaluations = append(d.Evaluations, Evaluation{l.Name, VerdictAllowed})
		}
	}

	return d
}

// bucket runs the Keyer, converting a panic into "does not apply" so a bug in one
// keyer cannot take down the endpoint.
func (e *Engine) bucket(l Limiter, req *Request) (key string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("keyer panicked, skipping limiter",
				slog.String("limiter", l.Name),
				slog.Any("panic", r))
			key, ok = "", false
		}
	}()
	return l.Keyer.Key(req)
}

// outcome normalises the two store algorithms into one shape.
type outcome struct {
	blocked    bool
	count      int64
	retryAfter time.Duration
}

// apply runs the store call for whichever algorithm the limiter selects,
// converting a panic into an error so it is treated as fail-open rather than
// crashing the process.
func (e *Engine) apply(ctx context.Context, l Limiter, bucket string) (out outcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("store panicked: %v", r)
		}
	}()

	key := store.Key{Limiter: l.Name, Bucket: bucket}

	// Bucket takes precedence, so a limiter configured with both is enforced as a
	// token bucket rather than silently applying two limits.
	if l.Bucket.Valid() {
		res, terr := e.store.Take(ctx, key, l.Bucket)
		if terr != nil {
			return outcome{}, terr
		}
		return outcome{
			blocked:    !res.Allowed,
			count:      res.Remaining,
			retryAfter: res.RetryAfter,
		}, nil
	}

	res, cerr := e.store.Consume(ctx, key, l.Rule)
	if cerr != nil {
		return outcome{}, cerr
	}
	return outcome{blocked: res.Blocked, count: res.Count, retryAfter: res.RetryAfter}, nil
}

// truncate shortens a bucket for logging: enough to correlate log lines, not enough
// to be a lookup key.
func truncate(s string) string {
	const n = 12
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
