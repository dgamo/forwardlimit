// SPDX-License-Identifier: Apache-2.0

// Package keyer provides the bucket extractors used by limiters: BodyField, Header
// and QueryParam as sources, First and Composite to combine them.
//
// These are what the configuration file builds, so adding a limiter is normally a
// matter of declaring one. Anything genuinely new implements limiter.Keyer.
package keyer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strconv"
	"strings"

	"github.com/dgamo/forwardlimit/internal/limiter"
)

// Hasher converts a raw extracted value into a storage-safe bucket.
//
// ok=false means hashing is unavailable, which disables the limiter rather than
// weakening it into storing the raw value.
type Hasher interface {
	Hash(string) (string, bool)
}

// HMAC hashes values with HMAC-SHA256, hex encoded.
//
// A bucket becomes a storage key and reaches the logs, so anything sensitive must
// be hashed first. HMAC rather than a bare digest, because a low-entropy value
// would otherwise fall to brute force.
type HMAC struct {
	secret []byte
}

// NewHMAC returns an HMAC hasher. An empty secret reports itself unavailable,
// disabling any limiter that depends on it.
func NewHMAC(secret string) HMAC { return HMAC{secret: []byte(secret)} }

// Available reports whether a secret is configured.
func (h HMAC) Available() bool { return len(h.secret) > 0 }

// Hash implements Hasher.
func (h HMAC) Hash(v string) (string, bool) {
	if len(h.secret) == 0 {
		return "", false
	}
	m := hmac.New(sha256.New, h.secret)
	_, _ = m.Write([]byte(v))
	return hex.EncodeToString(m.Sum(nil)), true
}

// Plain passes values through unchanged. Only for values that are neither sensitive
// nor unbounded in length - an IP address, say.
type Plain struct{}

// Hash implements Hasher.
func (Plain) Hash(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	return v, true
}

// Normaliser canonicalises a raw value so that equivalent inputs share a bucket.
//
// On every keyer, Normalise is optional and Hasher defaults to Plain.
type Normaliser func(string) string

// Digits keeps only ASCII digits, so "07700 900-123" and "07700900123" share a
// bucket.
func Digits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// TrimSpace trims surrounding whitespace.
func TrimSpace(s string) string { return strings.TrimSpace(s) }

// Lower lowercases, so values differing only by case share a bucket.
func Lower(s string) string { return strings.ToLower(s) }

// IPSubnet collapses an IP address to its network prefix, which is what makes an
// address-keyed limit resistant to rotation: a pool of addresses usually spans few
// networks, so limiting per /24 devalues rotating by up to 256x.
//
// A value that is not an IP address passes through unchanged, so a limiter keyed on
// a header that sometimes carries something else still works.
func IPSubnet(v4Bits, v6Bits int) Normaliser {
	return func(s string) string {
		addr := net.ParseIP(strings.TrimSpace(s))
		if addr == nil {
			return s
		}

		bits := v6Bits
		if v4 := addr.To4(); v4 != nil {
			addr, bits = v4, v4Bits
		}

		size := len(addr) * 8
		if bits < 0 || bits > size {
			return s
		}

		mask := net.CIDRMask(bits, size)
		return addr.Mask(mask).String() + "/" + strconv.Itoa(bits)
	}
}

// BodyField keys on a field of the JSON request body.
type BodyField struct {
	// Field is the field name, optionally a dotted path.
	Field     string
	Normalise Normaliser
	Hasher    Hasher
}

var _ limiter.Keyer = BodyField{}

// Key implements limiter.Keyer.
func (b BodyField) Key(r *limiter.Request) (string, bool) {
	raw, ok := r.Field(b.Field)
	if !ok {
		return "", false
	}
	if b.Normalise != nil {
		raw = b.Normalise(raw)
	}
	if raw == "" {
		return "", false
	}
	return hasher(b.Hasher).Hash(raw)
}

// Header keys on the first non-empty value among Names, in order.
type Header struct {
	Names     []string
	Normalise Normaliser
	Hasher    Hasher
}

var _ limiter.Keyer = Header{}

// Key implements limiter.Keyer.
func (h Header) Key(r *limiter.Request) (string, bool) {
	raw, ok := r.HeaderValue(h.Names...)
	if !ok {
		return "", false
	}
	if h.Normalise != nil {
		raw = h.Normalise(raw)
	}
	if raw == "" {
		return "", false
	}
	return hasher(h.Hasher).Hash(raw)
}

// QueryParam keys on the first non-empty query parameter among Names. It exists
// because a credential may arrive this way, and a header-only limiter would leave
// those callers unlimited.
type QueryParam struct {
	Names     []string
	Normalise Normaliser
	Hasher    Hasher
}

var _ limiter.Keyer = QueryParam{}

// Key implements limiter.Keyer.
func (q QueryParam) Key(r *limiter.Request) (string, bool) {
	raw, ok := r.QueryValue(q.Names...)
	if !ok {
		return "", false
	}
	if q.Normalise != nil {
		raw = q.Normalise(raw)
	}
	if raw == "" {
		return "", false
	}
	return hasher(q.Hasher).Hash(raw)
}

// First returns the bucket from the first keyer that applies - the counterpart to
// Composite, which requires every part. Use it for a value that may arrive by
// several routes, with a defined precedence.
type First struct {
	Parts []limiter.Keyer
	// Normalise and Hasher apply to whichever part won, so a normaliser or a hash
	// declared on the composed node covers every source under it rather than
	// having to be repeated on each.
	Normalise Normaliser
	Hasher    Hasher
}

var _ limiter.Keyer = First{}

// Key implements limiter.Keyer.
func (f First) Key(r *limiter.Request) (string, bool) {
	for _, p := range f.Parts {
		v, ok := p.Key(r)
		if !ok {
			continue
		}
		if f.Normalise != nil {
			v = f.Normalise(v)
		}
		// Normalising to nothing means this source yielded no usable value, so try
		// the next one rather than giving up on the limiter.
		if v == "" {
			continue
		}
		return hasher(f.Hasher).Hash(v)
	}
	return "", false
}

// Composite joins several keyers into one bucket.
//
// Every part must yield a value; if any does not apply, the composite does not
// apply. That is the correct behaviour for a scoped limiter - a rule keyed on
// "this tenant and this address" is meaningless with only one of the two.
type Composite struct {
	Parts []limiter.Keyer
	// Sep separates the parts. Defaults to ":".
	Sep string
	// Normalise and Hasher apply to the joined value. A normaliser that only makes
	// sense per part - digits, ipsubnet - belongs on the part instead.
	Normalise Normaliser
	Hasher    Hasher
}

var _ limiter.Keyer = Composite{}

// Key implements limiter.Keyer.
func (c Composite) Key(r *limiter.Request) (string, bool) {
	if len(c.Parts) == 0 {
		return "", false
	}
	sep := c.Sep
	if sep == "" {
		sep = ":"
	}

	parts := make([]string, 0, len(c.Parts))
	for _, p := range c.Parts {
		v, ok := p.Key(r)
		if !ok {
			return "", false
		}
		parts = append(parts, v)
	}
	joined := strings.Join(parts, sep)
	if c.Normalise != nil {
		joined = c.Normalise(joined)
	}
	if joined == "" {
		return "", false
	}
	return hasher(c.Hasher).Hash(joined)
}

func hasher(h Hasher) Hasher {
	if h == nil {
		return Plain{}
	}
	return h
}
