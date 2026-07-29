// SPDX-License-Identifier: Apache-2.0

package keyer_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/limiter"
	"github.com/dgamo/forwardlimit/internal/limiter/keyer"
)

func bodyReq(body string) *limiter.Request {
	return &limiter.Request{Body: []byte(body), Header: http.Header{}}
}

func headerReq(h http.Header) *limiter.Request {
	return &limiter.Request{Header: h}
}

// ---------- Digits ----------

func TestDigits(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"07700900123", "07700900123"},
		{"07700 900-123", "07700900123"},
		{"+44 (0)7700 900.123", "4407700900123"},
		{"  07700900123  ", "07700900123"},
		{"abcd", ""},
		{"", ""},
		{"0a7b7c0", "0770"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, keyer.Digits(tt.in))
		})
	}
}

// ---------- HMAC ----------

func TestHMACHashIsDeterministicAndOpaque(t *testing.T) {
	t.Parallel()
	h := keyer.NewHMAC("secret")
	value := "07700900123"

	a, ok := h.Hash(value)
	require.True(t, ok)
	b, ok := h.Hash(value)
	require.True(t, ok)

	require.Equal(t, a, b, "hashing must be deterministic")
	require.Len(t, a, 64, "hex-encoded SHA-256 is 64 characters")
	require.NotContains(t, a, value, "the raw value must not appear in the bucket")
}

func TestHMACDiffersBySecretAndValue(t *testing.T) {
	t.Parallel()

	a, _ := keyer.NewHMAC("secret-one").Hash("07700900123")
	b, _ := keyer.NewHMAC("secret-two").Hash("07700900123")
	require.NotEqual(t, a, b, "different secrets must produce different buckets")

	c, _ := keyer.NewHMAC("secret-one").Hash("07700900456")
	require.NotEqual(t, a, c, "different values must produce different buckets")
}

// With no secret configured the hasher must refuse to produce a bucket, so the
// limiter is skipped rather than falling back to an unsalted hash.
func TestHMACWithoutSecretIsUnavailable(t *testing.T) {
	t.Parallel()
	h := keyer.NewHMAC("")

	require.False(t, h.Available())
	_, ok := h.Hash("07700900123")
	require.False(t, ok)
}

func TestPlainHasher(t *testing.T) {
	t.Parallel()

	v, ok := keyer.Plain{}.Hash("192.0.2.1")
	require.True(t, ok)
	require.Equal(t, "192.0.2.1", v)

	_, ok = keyer.Plain{}.Hash("")
	require.False(t, ok)
}

// ---------- BodyField ----------

func TestBodyFieldNormalisesBeforeHashing(t *testing.T) {
	t.Parallel()
	k := keyer.BodyField{
		Field:     "phone_number",
		Normalise: keyer.Digits,
		Hasher:    keyer.NewHMAC("secret"),
	}

	spaced, ok := k.Key(bodyReq(`{"phone_number":"07700 900-123"}`))
	require.True(t, ok)
	plain, ok := k.Key(bodyReq(`{"phone_number":"07700900123"}`))
	require.True(t, ok)

	require.Equal(t, spaced, plain, "equivalent formats of one value must share a bucket")
}

func TestBodyFieldNotApplicable(t *testing.T) {
	t.Parallel()
	k := keyer.BodyField{Field: "phone_number", Normalise: keyer.Digits, Hasher: keyer.NewHMAC("s")}

	tests := []struct {
		name string
		body string
	}{
		{"field missing", `{"token":"abc"}`},
		{"field empty", `{"phone_number":""}`},
		{"no digits after normalisation", `{"phone_number":"not-a-number"}`},
		{"empty body", ``},
		{"malformed json", `{oops`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ok := k.Key(bodyReq(tt.body))
			require.False(t, ok, "limiter must not apply")
		})
	}
}

func TestBodyFieldWithoutSecretDoesNotApply(t *testing.T) {
	t.Parallel()
	k := keyer.BodyField{Field: "phone_number", Normalise: keyer.Digits, Hasher: keyer.NewHMAC("")}

	_, ok := k.Key(bodyReq(`{"phone_number":"07700900123"}`))
	require.False(t, ok)
}

func TestBodyFieldDefaultsToPlainHasher(t *testing.T) {
	t.Parallel()
	k := keyer.BodyField{Field: "reference"}

	v, ok := k.Key(bodyReq(`{"reference":"order-99"}`))
	require.True(t, ok)
	require.Equal(t, "order-99", v)
}

func TestBodyFieldSupportsDottedPaths(t *testing.T) {
	t.Parallel()
	k := keyer.BodyField{Field: "customer.contact.phone_number", Normalise: keyer.Digits}

	v, ok := k.Key(bodyReq(`{"customer":{"contact":{"phone_number":"07700 900123"}}}`))
	require.True(t, ok)
	require.Equal(t, "07700900123", v)
}

// ---------- Header ----------

func TestHeaderPrecedence(t *testing.T) {
	t.Parallel()
	k := keyer.Header{Names: []string{"CF-Connecting-IP", "X-Original-Ip", "X-Forwarded-For"}}

	v, ok := k.Key(headerReq(http.Header{
		"Cf-Connecting-Ip": {"1.1.1.1"},
		"X-Original-Ip":    {"2.2.2.2"},
		"X-Forwarded-For":  {"3.3.3.3"},
	}))
	require.True(t, ok)
	require.Equal(t, "1.1.1.1", v)

	v, ok = k.Key(headerReq(http.Header{"X-Forwarded-For": {"3.3.3.3, 4.4.4.4"}}))
	require.True(t, ok)
	require.Equal(t, "3.3.3.3", v, "leftmost entry is the original client")
}

func TestHeaderNotApplicableWhenAbsent(t *testing.T) {
	t.Parallel()
	k := keyer.Header{Names: []string{"CF-Connecting-IP"}}

	_, ok := k.Key(headerReq(http.Header{}))
	require.False(t, ok)
}

func TestHeaderCanHashItsValue(t *testing.T) {
	t.Parallel()
	k := keyer.Header{Names: []string{"X-Api-Token"}, Hasher: keyer.NewHMAC("secret")}

	v, ok := k.Key(headerReq(http.Header{"X-Api-Token": {"super-secret-token"}}))
	require.True(t, ok)
	require.NotContains(t, v, "super-secret-token")
	require.Len(t, v, 64)
}

// ---------- Composite ----------

func TestCompositeJoinsAllParts(t *testing.T) {
	t.Parallel()
	k := keyer.Composite{Parts: []limiter.Keyer{
		keyer.Header{Names: []string{"X-Company-Id"}},
		keyer.BodyField{Field: "phone_number", Normalise: keyer.Digits},
	}}

	r := &limiter.Request{
		Header: http.Header{"X-Company-Id": {"co_123"}},
		Body:   []byte(`{"phone_number":"07700 900123"}`),
	}

	v, ok := k.Key(r)
	require.True(t, ok)
	require.Equal(t, "co_123:07700900123", v)
}

func TestCompositeRequiresEveryPart(t *testing.T) {
	t.Parallel()
	k := keyer.Composite{Parts: []limiter.Keyer{
		keyer.Header{Names: []string{"X-Company-Id"}},
		keyer.BodyField{Field: "phone_number", Normalise: keyer.Digits},
	}}

	// Header present, body field missing.
	_, ok := k.Key(&limiter.Request{
		Header: http.Header{"X-Company-Id": {"co_123"}},
		Body:   []byte(`{}`),
	})
	require.False(t, ok)

	// Body field present, header missing.
	_, ok = k.Key(&limiter.Request{
		Header: http.Header{},
		Body:   []byte(`{"phone_number":"07700900123"}`),
	})
	require.False(t, ok)
}

func TestCompositeWithNoPartsDoesNotApply(t *testing.T) {
	t.Parallel()
	_, ok := keyer.Composite{}.Key(bodyReq(`{"a":"b"}`))
	require.False(t, ok)
}

func TestCompositeCustomSeparator(t *testing.T) {
	t.Parallel()
	k := keyer.Composite{
		Sep: "|",
		Parts: []limiter.Keyer{
			keyer.BodyField{Field: "a"},
			keyer.BodyField{Field: "b"},
		},
	}

	v, ok := k.Key(bodyReq(`{"a":"1","b":"2"}`))
	require.True(t, ok)
	require.Equal(t, "1|2", v)
}

// A hashed bucket must not leak any part of the value it was derived from.
func TestHashedBucketLeaksNoInputDigits(t *testing.T) {
	t.Parallel()
	const value = "07700900123"
	k := keyer.BodyField{Field: "phone_number", Normalise: keyer.Digits, Hasher: keyer.NewHMAC("secret")}

	v, ok := k.Key(bodyReq(`{"phone_number":"` + value + `"}`))
	require.True(t, ok)
	require.NotContains(t, v, value)
	require.NotContains(t, v, value[:6], "not even a prefix may appear")
	require.NotContains(t, v, value[len(value)-4:], "nor a suffix")
	require.True(t, isHex(v))
}

func isHex(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) == -1
}

// ---------- QueryParam ----------

func queryReq(rawQuery string) *limiter.Request {
	return &limiter.Request{Header: http.Header{}, RawQuery: rawQuery}
}

func TestQueryParamPrecedenceAndDecoding(t *testing.T) {
	t.Parallel()
	k := keyer.QueryParam{Names: []string{"token", "api_key"}}

	v, ok := k.Key(queryReq("api_key=pk_123&token=at_456"))
	require.True(t, ok)
	require.Equal(t, "at_456", v, "first name listed wins")

	v, ok = k.Key(queryReq("api_key=pk_123"))
	require.True(t, ok)
	require.Equal(t, "pk_123", v)

	// Percent-encoding must be decoded, or the same credential would land in two
	// different buckets depending on how the client encoded it.
	v, ok = k.Key(queryReq("api_key=pk%2Babc"))
	require.True(t, ok)
	require.Equal(t, "pk+abc", v)
}

func TestQueryParamNotApplicable(t *testing.T) {
	t.Parallel()
	k := keyer.QueryParam{Names: []string{"token"}}

	for _, q := range []string{"", "other=1", "token=", "token=%20"} {
		_, ok := k.Key(queryReq(q))
		require.False(t, ok, "query %q must not yield a bucket", q)
	}
}

func TestQueryParamCanHashItsValue(t *testing.T) {
	t.Parallel()
	k := keyer.QueryParam{Names: []string{"token"}, Hasher: keyer.NewHMAC("s")}

	v, ok := k.Key(queryReq("token=super-secret"))
	require.True(t, ok)
	require.NotContains(t, v, "super-secret")
	require.Len(t, v, 64)
}

// ---------- First ----------

// The credential may arrive as a header or a query parameter; both must land in
// the same bucket scheme, with the header taking precedence.
func TestFirstPrefersTheEarlierKeyer(t *testing.T) {
	t.Parallel()
	k := keyer.First{Parts: []limiter.Keyer{
		keyer.Header{Names: []string{"X-Api-Key"}},
		keyer.QueryParam{Names: []string{"token"}},
	}}

	both := &limiter.Request{
		Header:   http.Header{"X-Api-Key": {"from-header"}},
		RawQuery: "token=from-query",
	}
	v, ok := k.Key(both)
	require.True(t, ok)
	require.Equal(t, "from-header", v)
}

func TestFirstFallsThroughToLaterKeyers(t *testing.T) {
	t.Parallel()
	k := keyer.First{Parts: []limiter.Keyer{
		keyer.Header{Names: []string{"X-Api-Key"}},
		keyer.QueryParam{Names: []string{"token"}},
	}}

	// Header absent: the query parameter must still produce a bucket, otherwise
	// callers authenticating that way would be exempt from the limit.
	v, ok := k.Key(&limiter.Request{Header: http.Header{}, RawQuery: "token=from-query"})
	require.True(t, ok)
	require.Equal(t, "from-query", v)
}

func TestFirstDoesNotApplyWhenNoPartDoes(t *testing.T) {
	t.Parallel()
	k := keyer.First{Parts: []limiter.Keyer{
		keyer.Header{Names: []string{"X-Api-Key"}},
		keyer.QueryParam{Names: []string{"token"}},
	}}

	_, ok := k.Key(&limiter.Request{Header: http.Header{}})
	require.False(t, ok)

	_, ok = keyer.First{}.Key(&limiter.Request{Header: http.Header{}})
	require.False(t, ok)
}

// ---------- IPSubnet ----------

// Collapsing an address to its network is what makes an address-keyed limit
// resistant to rotation: a pool of addresses inside one network shares a bucket.
func TestIPSubnetCollapsesAddressesInTheSameNetwork(t *testing.T) {
	t.Parallel()
	n := keyer.IPSubnet(24, 64)

	require.Equal(t, n("203.0.113.1"), n("203.0.113.254"),
		"addresses in the same /24 must share a bucket")
	require.NotEqual(t, n("203.0.113.1"), n("203.0.114.1"),
		"a different /24 must be a different bucket")
	require.Equal(t, "203.0.113.0/24", n("203.0.113.1"))
}

func TestIPSubnetHandlesIPv6(t *testing.T) {
	t.Parallel()
	n := keyer.IPSubnet(24, 64)

	require.Equal(t, n("2001:db8::1"), n("2001:db8::ffff"),
		"addresses in the same /64 must share a bucket")
	require.NotEqual(t, n("2001:db8::1"), n("2001:db9::1"))
}

// A header may carry something that is not an address; the limiter must keep
// working rather than collapsing every such value into one bucket.
func TestIPSubnetLeavesNonAddressesAlone(t *testing.T) {
	t.Parallel()
	n := keyer.IPSubnet(24, 64)

	for _, v := range []string{"not-an-ip", "", "example.com", "1.2.3"} {
		require.Equal(t, v, n(v))
	}
}

func TestIPSubnetRejectsOutOfRangePrefix(t *testing.T) {
	t.Parallel()
	require.Equal(t, "203.0.113.1", keyer.IPSubnet(33, 64)("203.0.113.1"),
		"an impossible prefix must pass the value through rather than panic")
}

func TestLowerNormaliser(t *testing.T) {
	t.Parallel()
	require.Equal(t, "abc", keyer.Lower("AbC"))
}

// ---------- Normalise on header and query keyers ----------

func TestHeaderKeyerAppliesNormaliser(t *testing.T) {
	t.Parallel()
	k := keyer.Header{Names: []string{"CF-Connecting-IP"}, Normalise: keyer.IPSubnet(24, 64)}

	a, ok := k.Key(headerReq(http.Header{"Cf-Connecting-Ip": {"198.51.100.7"}}))
	require.True(t, ok)
	b, ok := k.Key(headerReq(http.Header{"Cf-Connecting-Ip": {"198.51.100.200"}}))
	require.True(t, ok)

	require.Equal(t, a, b, "two addresses in one /24 must produce one bucket")
}

func TestHeaderKeyerDoesNotApplyWhenNormaliserEmptiesTheValue(t *testing.T) {
	t.Parallel()
	k := keyer.Header{Names: []string{"X-Digits"}, Normalise: keyer.Digits}

	_, ok := k.Key(headerReq(http.Header{"X-Digits": {"no-digits-here"}}))
	require.False(t, ok, "nothing left after normalisation means the limiter does not apply")
}

func TestQueryKeyerAppliesNormaliser(t *testing.T) {
	t.Parallel()
	k := keyer.QueryParam{Names: []string{"token"}, Normalise: keyer.Lower}

	a, ok := k.Key(queryReq("token=ABC"))
	require.True(t, ok)
	b, ok := k.Key(queryReq("token=abc"))
	require.True(t, ok)
	require.Equal(t, a, b)
}
