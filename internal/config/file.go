// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// File is the declarative configuration: the limiters to evaluate, and the response
// to send when one rejects.
//
// Limiters live in a file because a key can be a tree - "first of these headers,
// else this query parameter" - which does not flatten into environment variables
// readably.
type File struct {
	// Response is the default sent when a limiter rejects. A limiter may override
	// it.
	Response *ResponseSpec `yaml:"response"`
	// Limiters are evaluated in the order given, and the first to reject decides.
	Limiters []LimiterSpec `yaml:"limiters"`
}

// ResponseSpec describes a rejection response.
type ResponseSpec struct {
	Status      int    `yaml:"status"`
	ContentType string `yaml:"contentType"`
	Body        string `yaml:"body"`
	// BodyFile loads the body from a path, for payloads too large to inline.
	BodyFile string `yaml:"bodyFile"`
}

// LimiterSpec is one limiter. Exactly one of Window (a fixed window with a punitive
// block, for abuse controls) or Bucket (a token bucket, for capacity) selects the
// algorithm.
type LimiterSpec struct {
	Name   string      `yaml:"name"`
	Key    *KeySpec    `yaml:"key"`
	Window *WindowSpec `yaml:"window"`
	Bucket *BucketSpec `yaml:"bucket"`
	// Paths restricts the limiter. Empty means every path. Matching is exact;
	// a trailing "/*" opts into the subtree.
	Paths []string `yaml:"paths"`
	// Methods restricts the limiter to these HTTP methods. Empty means every
	// method. Matching is case-insensitive, and ANDed with Paths.
	Methods []string `yaml:"methods"`
	// DryRun evaluates and reports without rejecting.
	DryRun bool `yaml:"dryRun"`
	// RetryAfter adds a Retry-After header. For a cooperating caller, not for an
	// adversary who would use it to locate the threshold.
	RetryAfter bool `yaml:"retryAfter"`
	// Response overrides the global response for this limiter only.
	Response *ResponseSpec `yaml:"response"`
}

// WindowSpec is a fixed window with a hard block.
type WindowSpec struct {
	Limit  int64         `yaml:"limit"`
	Window time.Duration `yaml:"window"`
	Block  time.Duration `yaml:"block"`
}

// BucketSpec is a token bucket.
type BucketSpec struct {
	Rate  int64 `yaml:"rate"`
	Burst int64 `yaml:"burst"`
}

// KeySpec describes how to derive the bucket a request counts against.
//
// Exactly one source must be set. First and Composite nest, so a key can be a
// tree: "the first of these headers, otherwise this query parameter", or "this
// tenant identifier combined with that body field".
type KeySpec struct {
	// Body is a JSON body field. A dotted path reaches nested fields.
	Body string `yaml:"body"`
	// Header is an ordered list of header names; the first non-empty one wins.
	Header []string `yaml:"header"`
	// Query is an ordered list of query parameters; the first non-empty one wins.
	Query []string `yaml:"query"`
	// First tries each child in order and uses the first that applies.
	First []KeySpec `yaml:"first"`
	// Composite requires every child and joins them with Separator.
	Composite []KeySpec `yaml:"composite"`
	// Separator joins Composite parts. Defaults to ":".
	Separator string `yaml:"separator"`

	// Normalise canonicalises the value so equivalent inputs share a bucket:
	// digits, lower, trim, none, or ipsubnet/N[,M].
	Normalise string `yaml:"normalise"`
	// Hash replaces the value with an HMAC of it. Required for anything
	// sensitive, since the value becomes a storage key and appears in logs.
	Hash bool `yaml:"hash"`
}

// sources reports which key sources are set. Used for validation.
func (k KeySpec) sources() []string {
	var set []string
	if k.Body != "" {
		set = append(set, "body")
	}
	if len(k.Header) > 0 {
		set = append(set, "header")
	}
	if len(k.Query) > 0 {
		set = append(set, "query")
	}
	if len(k.First) > 0 {
		set = append(set, "first")
	}
	if len(k.Composite) > 0 {
		set = append(set, "composite")
	}
	return set
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// methodRe is the RFC 9110 token production, which is what an HTTP method is.
//
// Deliberately wider than the familiar verbs: an extension method such as
// PROPFIND or MKCALENDAR is legitimate, and rejecting one as a typo would be
// worse than accepting a method that never arrives.
var methodRe = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

// Validate checks the whole file, collecting every problem rather than stopping
// at the first. A configuration error should be fixed in one pass, not
// discovered one restart at a time.
func (f *File) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if len(f.Limiters) == 0 {
		fail("limiters: at least one limiter is required")
	}
	if f.Response != nil {
		validateResponse("response", f.Response, fail)
	}

	seen := make(map[string]bool, len(f.Limiters))
	for i, l := range f.Limiters {
		validateLimiter(i, l, seen, fail)
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
}

// validateLimiter checks one entry. seen carries the names already used, so
// duplicates are caught across the whole file.
func validateLimiter(i int, l LimiterSpec, seen map[string]bool, fail func(string, ...any)) {
	where := fmt.Sprintf("limiters[%d]", i)
	if l.Name != "" {
		where = fmt.Sprintf("limiters[%d] (%s)", i, l.Name)
	}

	switch {
	case l.Name == "":
		fail("%s: name is required", where)
	case !nameRe.MatchString(l.Name):
		fail("%s: name must match %s - it becomes a metric label and a key prefix",
			where, nameRe)
	case seen[l.Name]:
		fail("%s: duplicate name; names must be unique because they namespace the store", where)
	default:
		seen[l.Name] = true
	}

	switch {
	case l.Window == nil && l.Bucket == nil:
		fail("%s: one of window or bucket is required", where)
	case l.Window != nil && l.Bucket != nil:
		fail("%s: window and bucket are mutually exclusive - a limiter enforces one algorithm", where)
	case l.Window != nil:
		validateWindow(where, l.Window, fail)
	case l.Bucket != nil:
		validateBucket(where, l.Bucket, fail)
	}

	if l.Key == nil {
		fail("%s: key is required", where)
	} else {
		validateKey(where+".key", *l.Key, fail)
	}

	validatePaths(where, l.Paths, fail)
	validateMethods(where, l.Methods, fail)

	if l.Response != nil {
		validateResponse(where+".response", l.Response, fail)
	}
}

// validatePaths checks the path filter. An empty list is valid and means every
// path.
//
// A "*" is only meaningful as a trailing "/*". Anywhere else it is almost certainly
// someone expecting glob matching, and since paths are compared literally such a
// pattern would match nothing at all - a limiter that silently never fires. Better
// to refuse to start.
func validatePaths(where string, paths []string, fail func(string, ...any)) {
	for i, p := range paths {
		at := fmt.Sprintf("%s.paths[%d]", where, i)

		switch {
		case p == "":
			fail("%s: must not be empty", at)
		case !strings.HasPrefix(p, "/"):
			fail("%s: %q must start with /", at, p)
		case strings.Contains(strings.TrimSuffix(p, "/*"), "*"):
			fail("%s: %q - * is only valid as a trailing /*, which matches the path "+
				"and everything under it; paths are otherwise compared exactly", at, p)
		}
	}
}

// validateMethods checks the method filter. An empty list is valid and means
// every method, mirroring paths.
func validateMethods(where string, methods []string, fail func(string, ...any)) {
	seen := make(map[string]bool, len(methods))
	for i, m := range methods {
		at := fmt.Sprintf("%s.methods[%d]", where, i)

		if m == "" {
			fail("%s: must not be empty", at)
			continue
		}
		if !methodRe.MatchString(m) {
			fail("%s: %q is not a valid HTTP method", at, m)
			continue
		}

		// Matching is case-insensitive, so GET and get are one filter written
		// twice - a mistake worth reporting rather than silently collapsing.
		up := strings.ToUpper(m)
		if seen[up] {
			fail("%s: duplicate method %q", at, m)
			continue
		}
		seen[up] = true
	}
}

func validateWindow(where string, w *WindowSpec, fail func(string, ...any)) {
	if w.Limit < MinLimit || w.Limit > MaxLimit {
		fail("%s.window.limit must be between %d and %d, got %d", where, MinLimit, MaxLimit, w.Limit)
	}
	if w.Window < MinWindow || w.Window > MaxWindow {
		fail("%s.window.window must be between %s and %s, got %s", where, MinWindow, MaxWindow, w.Window)
	}
	if w.Block < MinBlock || w.Block > MaxBlock {
		fail("%s.window.block must be between %s and %s, got %s", where, MinBlock, MaxBlock, w.Block)
	}
}

func validateBucket(where string, b *BucketSpec, fail func(string, ...any)) {
	if b.Rate < MinRate || b.Rate > MaxRate {
		fail("%s.bucket.rate must be between %d and %d, got %d", where, MinRate, MaxRate, b.Rate)
	}
	if b.Burst < MinBurst || b.Burst > MaxBurst {
		fail("%s.bucket.burst must be between %d and %d, got %d", where, MinBurst, MaxBurst, b.Burst)
	}
}

func validateResponse(where string, r *ResponseSpec, fail func(string, ...any)) {
	if r.Status != 0 && (r.Status < 400 || r.Status > 599) {
		fail("%s.status must be a 4xx or 5xx code, got %d", where, r.Status)
	}
	if r.Body != "" && r.BodyFile != "" {
		fail("%s: body and bodyFile are mutually exclusive", where)
	}
	if r.BodyFile != "" {
		if _, err := os.Stat(r.BodyFile); err != nil {
			fail("%s.bodyFile: %v", where, err)
		}
	}
}

func validateKey(where string, k KeySpec, fail func(string, ...any)) {
	switch src := k.sources(); len(src) {
	case 0:
		fail("%s: one of body, header, query, first or composite is required", where)
	case 1: // as expected
	default:
		fail("%s: %s are all set, but exactly one key source is allowed",
			where, strings.Join(src, ", "))
	}

	if k.Separator != "" && len(k.Composite) == 0 {
		fail("%s: separator only applies to composite", where)
	}
	if _, err := parseNormaliser(k.Normalise); err != nil {
		fail("%s.normalise: %v", where, err)
	}

	for i, child := range k.First {
		validateKey(fmt.Sprintf("%s.first[%d]", where, i), child, fail)
	}
	for i, child := range k.Composite {
		validateKey(fmt.Sprintf("%s.composite[%d]", where, i), child, fail)
	}
	if len(k.Composite) == 1 {
		fail("%s.composite: a single child is the same as using it directly", where)
	}
}

// parseNormaliser resolves a normaliser name. It returns nil for "" and "none",
// meaning no normalisation.
//
// ipsubnet takes prefix lengths: "ipsubnet/24" sets IPv4 and leaves IPv6 at /64,
// "ipsubnet/24,48" sets both.
func parseNormaliser(name string) (normaliserKind, error) {
	base, arg, hasArg := strings.Cut(strings.TrimSpace(name), "/")

	switch base {
	case "", "none":
		if hasArg {
			return normaliserKind{}, fmt.Errorf("%q takes no argument", base)
		}
		return normaliserKind{}, nil
	case "digits", "lower", "trim":
		if hasArg {
			return normaliserKind{}, fmt.Errorf("%q takes no argument", base)
		}
		return normaliserKind{name: base}, nil
	case "ipsubnet":
		v4, v6 := 24, 64
		if hasArg {
			parts := strings.Split(arg, ",")
			if len(parts) > 2 {
				return normaliserKind{}, fmt.Errorf("ipsubnet takes at most two prefix lengths, got %q", arg)
			}
			var err error
			if v4, err = strconv.Atoi(strings.TrimSpace(parts[0])); err != nil {
				return normaliserKind{}, fmt.Errorf("ipsubnet: %q is not a prefix length", parts[0])
			}
			if len(parts) == 2 {
				if v6, err = strconv.Atoi(strings.TrimSpace(parts[1])); err != nil {
					return normaliserKind{}, fmt.Errorf("ipsubnet: %q is not a prefix length", parts[1])
				}
			}
		}
		if v4 < 0 || v4 > 32 {
			return normaliserKind{}, fmt.Errorf("ipsubnet IPv4 prefix must be 0-32, got %d", v4)
		}
		if v6 < 0 || v6 > 128 {
			return normaliserKind{}, fmt.Errorf("ipsubnet IPv6 prefix must be 0-128, got %d", v6)
		}
		return normaliserKind{name: "ipsubnet", v4: v4, v6: v6}, nil
	default:
		return normaliserKind{}, fmt.Errorf(
			"unknown normaliser %q; valid: digits, lower, trim, none, ipsubnet/N[,M]", base)
	}
}

// normaliserKind is a resolved normaliser, ready to be turned into a function.
type normaliserKind struct {
	name   string
	v4, v6 int
}
