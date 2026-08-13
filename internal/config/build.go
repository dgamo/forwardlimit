// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/dgamo/forwardlimit/internal/httpapi"
	"github.com/dgamo/forwardlimit/internal/limiter"
	"github.com/dgamo/forwardlimit/internal/limiter/keyer"
	"github.com/dgamo/forwardlimit/internal/store"
)

// DefaultResponse is sent when a limiter rejects and nothing overrides it.
//
// Deliberately minimal and generic: the useful signal is the status code, and any
// detail here is public because the proxy returns this response to the client
// verbatim.
var DefaultResponse = httpapi.Response{
	Status:      429,
	ContentType: "application/json",
	Body:        []byte(`{"error":"rate_limited"}`),
}

// LoadFile reads and validates a configuration file.
func LoadFile(path string) (*File, error) {
	// The path is supplied by the operator running the process, by flag or
	// environment. There is no lesser-privileged source for it to come from.
	//nolint:gosec // G304: operator-supplied configuration path.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var f File
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// Reject unknown fields: a mistyped key that is silently ignored is how a
	// limiter ends up quietly not doing what its author intended.
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// Built is the result of turning a File into runnable limiters.
type Built struct {
	Limiters []limiter.Limiter
	// Responses holds per-limiter overrides, keyed by limiter name.
	Responses map[string]httpapi.Response
	// Default applies to any limiter without an override.
	Default httpapi.Response
	// Warnings describe configurations that are valid but will not limit
	// anything. Each corresponds to a deliberate fail-open path and must be
	// surfaced at startup.
	Warnings []string
}

// Build turns the declarative configuration into limiters.
//
// hashSecret keys the HMAC used wherever a key spec sets hash: true. When it is
// empty, any limiter needing it is left out entirely rather than falling back to
// storing raw values - a limiter that quietly writes personal data or a credential
// into Redis would be worse than no limiter.
func (f *File) Build(hashSecret string) (*Built, error) {
	b := &Built{
		Responses: make(map[string]httpapi.Response),
		Default:   DefaultResponse,
	}

	if f.Response != nil {
		r, err := resolveResponse(f.Response, DefaultResponse)
		if err != nil {
			return nil, err
		}
		b.Default = r
	}

	hasher := keyer.NewHMAC(hashSecret)

	for _, spec := range f.Limiters {
		k, needsHash, err := buildKeyer(*spec.Key, hasher)
		if err != nil {
			return nil, fmt.Errorf("limiter %q: %w", spec.Name, err)
		}
		if needsHash && !hasher.Available() {
			b.Warnings = append(b.Warnings, fmt.Sprintf(
				"limiter %q needs hashing but HASH_SECRET is empty: it is disabled and will not limit anything",
				spec.Name))
			continue
		}

		l := limiter.Limiter{
			Name:             spec.Name,
			Keyer:            k,
			Paths:            spec.Paths,
			Methods:          spec.Methods,
			DryRun:           spec.DryRun,
			AdviseRetryAfter: spec.RetryAfter,
		}
		if spec.Window != nil {
			l.Rule = store.Rule{
				Limit:  spec.Window.Limit,
				Window: spec.Window.Window,
				Block:  spec.Window.Block,
			}
		} else {
			l.Bucket = store.Bucket{Rate: spec.Bucket.Rate, Burst: spec.Bucket.Burst}
		}
		b.Limiters = append(b.Limiters, l)

		if spec.Response != nil {
			r, err := resolveResponse(spec.Response, b.Default)
			if err != nil {
				return nil, fmt.Errorf("limiter %q: %w", spec.Name, err)
			}
			b.Responses[spec.Name] = r
		}
	}

	if len(b.Limiters) == 0 {
		b.Warnings = append(b.Warnings,
			"no limiters are active: every request will be allowed")
	}
	return b, nil
}

// DryRunNames lists active limiters whose enforcement is suppressed.
func (b *Built) DryRunNames() []string {
	var names []string
	for _, l := range b.Limiters {
		if l.DryRun {
			names = append(names, l.Name)
		}
	}
	return names
}

// Names lists the active limiters, in evaluation order.
func (b *Built) Names() []string {
	names := make([]string, 0, len(b.Limiters))
	for _, l := range b.Limiters {
		names = append(names, l.Name)
	}
	return names
}

// buildKeyer turns a key spec into a Keyer, reporting whether any node in the
// tree requires hashing.
func buildKeyer(spec KeySpec, hasher keyer.HMAC) (limiter.Keyer, bool, error) {
	norm, err := parseNormaliser(spec.Normalise)
	if err != nil {
		return nil, false, err
	}
	n := norm.fn()

	var h keyer.Hasher
	if spec.Hash {
		h = hasher
	}

	switch {
	case spec.Body != "":
		return keyer.BodyField{Field: spec.Body, Normalise: n, Hasher: h}, spec.Hash, nil

	case len(spec.Header) > 0:
		return keyer.Header{Names: spec.Header, Normalise: n, Hasher: h}, spec.Hash, nil

	case len(spec.Query) > 0:
		return keyer.QueryParam{Names: spec.Query, Normalise: n, Hasher: h}, spec.Hash, nil

	case len(spec.First) > 0:
		parts, needsHash, err := buildChildren(spec.First, hasher)
		if err != nil {
			return nil, false, err
		}
		return keyer.First{Parts: parts}, needsHash || spec.Hash, nil

	case len(spec.Composite) > 0:
		parts, needsHash, err := buildChildren(spec.Composite, hasher)
		if err != nil {
			return nil, false, err
		}
		return keyer.Composite{Parts: parts, Sep: spec.Separator}, needsHash || spec.Hash, nil
	}

	// Unreachable via LoadFile, which validates first.
	return nil, false, fmt.Errorf("key spec has no source")
}

func buildChildren(specs []KeySpec, hasher keyer.HMAC) ([]limiter.Keyer, bool, error) {
	parts := make([]limiter.Keyer, 0, len(specs))
	anyHash := false
	for _, s := range specs {
		k, needsHash, err := buildKeyer(s, hasher)
		if err != nil {
			return nil, false, err
		}
		anyHash = anyHash || needsHash
		parts = append(parts, k)
	}
	return parts, anyHash, nil
}

// fn turns a resolved normaliser into a function. A zero kind means none.
func (n normaliserKind) fn() keyer.Normaliser {
	switch n.name {
	case "digits":
		return keyer.Digits
	case "lower":
		return keyer.Lower
	case "trim":
		return keyer.TrimSpace
	case "ipsubnet":
		return keyer.IPSubnet(n.v4, n.v6)
	default:
		return nil
	}
}

// resolveResponse fills unset fields from base, so an override need only state
// what differs.
func resolveResponse(spec *ResponseSpec, base httpapi.Response) (httpapi.Response, error) {
	out := base

	if spec.Status != 0 {
		out.Status = spec.Status
	}
	if spec.ContentType != "" {
		out.ContentType = spec.ContentType
	}
	switch {
	case spec.BodyFile != "":
		body, err := os.ReadFile(spec.BodyFile)
		if err != nil {
			return httpapi.Response{}, fmt.Errorf("reading bodyFile: %w", err)
		}
		out.Body = body
	case spec.Body != "":
		out.Body = []byte(spec.Body)
	}
	return out, nil
}
