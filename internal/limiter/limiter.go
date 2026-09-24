// SPDX-License-Identifier: Apache-2.0

// Package limiter evaluates rate limit rules against a request.
//
// The design separates *what to key on* from *how to enforce*: a Keyer extracts a
// bucket value from a Request, and the Engine applies the rule through a
// store.Store. The Engine knows nothing about what is being limited, so adding a
// limiter does not require changing it.
package limiter

import (
	"strings"
	"time"

	"github.com/dgamo/forwardlimit/internal/store"
)

// Keyer extracts the bucket value a limiter should count against.
//
// ok=false means the limiter does not apply to this request - the body field was
// absent, say. That is not an error; it means "no opinion", and the request
// proceeds.
type Keyer interface {
	Key(*Request) (string, bool)
}

// KeyerFunc adapts a function to Keyer.
type KeyerFunc func(*Request) (string, bool)

// Key implements Keyer.
func (f KeyerFunc) Key(r *Request) (string, bool) { return f(r) }

// Limiter is one named limit plus the way to derive its bucket.
//
// Exactly one of Rule (fixed window, for abuse controls) or Bucket (token bucket,
// for capacity) selects the algorithm. If neither is valid the limiter is disabled
// and allows everything.
type Limiter struct {
	// Name namespaces the bucket in the store and labels metrics.
	Name   string
	Rule   store.Rule
	Bucket store.Bucket
	Keyer  Keyer
	// Paths restricts the limiter. Empty means all paths.
	//
	// Matching is EXACT: "/v1/orders" does not cover "/v1/orders/123". A
	// trailing "/*" opts into the subtree instead, so "/v1/orders/*" matches
	// "/v1/orders" and everything below it but not "/v1/ordersfoo". A trailing
	// slash is ignored, and matching is case-insensitive.
	Paths []string
	// Methods restricts the limiter to these HTTP methods. Empty means every
	// method. Matching is case-insensitive.
	//
	// ANDed with Paths, so a limiter narrowed by both applies only where the path
	// matches and the method matches. Scoping to POST is what keeps a CORS
	// preflight from spending the caller's budget.
	Methods []string
	// DryRun evaluates fully - the store is called, counters increment - but
	// suppresses enforcement, so a request that would have been rejected is allowed.
	// That fidelity is the point: the projection has to match what enforcement
	// would do.
	DryRun bool
	// AdviseRetryAfter sends Retry-After when this limiter rejects.
	//
	// On for capacity limits, where a cooperating caller that is not told to back
	// off retries immediately and amplifies the overload. Off for abuse controls,
	// where it states the threshold to an adversary.
	AdviseRetryAfter bool
}

// Active reports whether the limiter will be evaluated.
func (l Limiter) Active() bool { return l.Rule.Valid() || l.Bucket.Valid() }

// Applies reports whether the limiter is scoped to the given request.
//
// Path and method are ANDed: a limiter narrowed by both applies only where both
// match. An unscoped limiter applies to everything.
func (l Limiter) Applies(path, method string) bool {
	return l.matchesPath(path) && l.matchesMethod(method)
}

// matchesPath reports whether the limiter is scoped to the given request path.
//
// Exact by default, with a trailing "/*" opting into the subtree. That way round
// because the other one fails silently: a limiter written for a single endpoint
// would quietly count everything beneath it, and the dry-run projection it feeds
// would read high with nothing to say why.
func (l Limiter) matchesPath(path string) bool {
	if len(l.Paths) == 0 {
		return true
	}
	// Case-folded on BOTH sides. RFC 3986 makes a URI path case-sensitive, which is
	// correct for routing and wrong here: these paths are abuse-control scopes, and
	// comparing them byte-for-byte lets a caller walk straight past every
	// path-scoped limiter by varying the case. Whether "/V1/Orders" reaches the same
	// handler is the proxy's and the application's decision, not this one -- where
	// it does, a byte-for-byte limiter silently declines to count it.
	//
	// strings.ToLower returns its argument unchanged when there is nothing to fold,
	// so ordinary lowercase traffic allocates nothing; only the evasion attempt pays.
	path = strings.ToLower(normalisePath(path))
	for _, p := range l.Paths {
		p = strings.ToLower(p)
		if base, subtree := strings.CutSuffix(p, "/*"); subtree {
			// "/*" alone is every path. Otherwise the base itself or anything under
			// it, matching on the separator so "/v1/x/*" excludes "/v1/xy".
			if base == "" || path == base || strings.HasPrefix(path, base+"/") {
				return true
			}
			continue
		}
		if path == normalisePath(p) {
			return true
		}
	}
	return false
}

// normalisePath drops a trailing slash so "/v1/x/" and "/v1/x" are one path. The
// root is left alone: trimming "/" to "" would yield a pattern matching nothing.
func normalisePath(p string) string {
	if len(p) > 1 {
		return strings.TrimSuffix(p, "/")
	}
	return p
}

// matchesMethod reports whether the limiter is scoped to the given request method.
//
// Folded rather than compared after uppercasing the configuration once: folding
// both sides costs no allocation, and it closes an evasion path, since a limiter
// written methods: [POST] must still catch a client that sends "post".
func (l Limiter) matchesMethod(method string) bool {
	if len(l.Methods) == 0 {
		return true
	}
	for _, m := range l.Methods {
		if strings.EqualFold(method, m) {
			return true
		}
	}
	return false
}

// Verdict is the outcome of evaluating a single limiter.
type Verdict string

// Possible verdicts.
const (
	VerdictAllowed Verdict = "allowed"
	VerdictBlocked Verdict = "blocked"
	// VerdictWouldBlock is deliberately distinct from VerdictBlocked: reporting a
	// suppressed decision as a real block would make it impossible to tell from
	// metrics whether production is protected, and would silence any alert watching
	// for "nothing is being limited".
	VerdictWouldBlock Verdict = "would_block"
	VerdictFailedOpen Verdict = "failed_open"
)

// Evaluation records what one limiter decided, for metrics and logs.
type Evaluation struct {
	Limiter string
	Verdict Verdict
}

// Decision is the overall result for a request.
//
// Blocked is the limiters' verdict; Rejected reports whether it is enforced. They
// differ under dry run, and a response must be built from Rejected.
type Decision struct {
	Blocked bool
	DryRun  bool
	// Limiter names the limiter that blocked; empty when allowed.
	Limiter string
	// Count is the count in the current window, or -1 from an existing block.
	Count            int64
	RetryAfter       time.Duration
	AdviseRetryAfter bool
	// Evaluations lists every limiter that applied, in order.
	Evaluations []Evaluation
}

// Rejected reports whether the request should actually be refused.
//
// The only field a response may be built from: a dry-run limiter blocks in the
// verdict but must never reject the request.
func (d Decision) Rejected() bool { return d.Blocked && !d.DryRun }

// FailedOpen reports whether any limiter was skipped because of a store error, so
// the request was allowed without being fully counted.
func (d Decision) FailedOpen() bool {
	for _, e := range d.Evaluations {
		if e.Verdict == VerdictFailedOpen {
			return true
		}
	}
	return false
}
