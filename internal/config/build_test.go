// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/config"
	"github.com/dgamo/forwardlimit/internal/limiter"
)

const testSecret = "test-secret"

// build is the whole start-up path: read, parse, validate, construct.
func build(t *testing.T, body, secret string) *config.Built {
	t.Helper()

	f, err := config.LoadFile(write(t, body))
	require.NoError(t, err)

	b, err := f.Build(secret)
	require.NoError(t, err)
	return b
}

// hashed is what a hashing keyer must produce, computed independently so the test
// does not simply assert the implementation against itself.
func hashed(v string) string {
	m := hmac.New(sha256.New, []byte(testSecret))
	_, _ = m.Write([]byte(v))
	return hex.EncodeToString(m.Sum(nil))
}

func req(path string, header http.Header, rawQuery, body string) *limiter.Request {
	if header == nil {
		header = http.Header{}
	}
	return &limiter.Request{
		Method:   http.MethodPost,
		Path:     path,
		Header:   header,
		RawQuery: rawQuery,
		Body:     []byte(body),
	}
}

func TestBuildProducesLimitersInOrder(t *testing.T) {
	t.Parallel()

	b := build(t, `
limiters:
  - name: tenant
    key: {header: [x-api-key]}
    bucket: {rate: 200, burst: 400}
    retryAfter: true
  - name: signup
    key: {body: email, normalise: lower, hash: true}
    window: {limit: 15, window: 1h, block: 2h}
    paths: ["/v1/signup"]
    methods: ["POST", "GET"]
    dryRun: true
`, testSecret)

	require.Equal(t, []string{"tenant", "signup"}, b.Names(),
		"evaluation order must follow the file, since the first block wins")
	require.Equal(t, []string{"signup"}, b.DryRunNames())
	require.Empty(t, b.Warnings)

	tenant := b.Limiters[0]
	require.Equal(t, int64(200), tenant.Bucket.Rate)
	require.Equal(t, int64(400), tenant.Bucket.Burst)
	require.True(t, tenant.AdviseRetryAfter)
	require.Zero(t, tenant.Rule.Limit, "a bucket limiter must leave the window rule unset")
	require.Empty(t, tenant.Methods,
		"an omitted methods list must stay empty, which means every method")

	signup := b.Limiters[1]
	require.Equal(t, int64(15), signup.Rule.Limit)
	require.Equal(t, time.Hour, signup.Rule.Window)
	require.Equal(t, 2*time.Hour, signup.Rule.Block)
	require.Equal(t, []string{"/v1/signup"}, signup.Paths)
	require.Equal(t, []string{"POST", "GET"}, signup.Methods)
	require.True(t, signup.DryRun)
	require.False(t, signup.AdviseRetryAfter)
	require.Zero(t, signup.Bucket.Rate, "a window limiter must leave the bucket unset")
}

// The keyers are what the YAML ultimately configures, so each shape is checked by
// running it against a request rather than by inspecting the constructed type.
func TestBuiltKeyers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		spec string
		req  *limiter.Request
		want string
		ok   bool
	}{
		{
			name: "body field",
			spec: `key: {body: email}`,
			req:  req("/", nil, "", `{"email":"user@example.com"}`),
			want: "user@example.com",
			ok:   true,
		},
		{
			name: "body field with dotted path",
			spec: `key: {body: customer.contact.email}`,
			req:  req("/", nil, "", `{"customer":{"contact":{"email":"a@example.com"}}}`),
			want: "a@example.com",
			ok:   true,
		},
		{
			name: "body field normalised to digits",
			spec: `key: {body: phone_number, normalise: digits}`,
			req:  req("/", nil, "", `{"phone_number":"07700 900-123"}`),
			want: "07700900123",
			ok:   true,
		},
		{
			name: "body field absent",
			spec: `key: {body: email}`,
			req:  req("/", nil, "", `{"other":"x"}`),
			ok:   false,
		},
		{
			// REGRESSION: hash on a composed node used to be dropped entirely, so the
			// bucket was the raw credential. keyer.First had no Hasher, the child fell
			// through to Plain{}, and the secret became the storage key and reached the
			// logs. This case fails loudly if that returns.
			name: "first: hashes at the composed node",
			spec: `key:
      first:
        - {header: [x-secret]}
        - {query: [token]}
      hash: true`,
			req:  req("/", http.Header{"X-Secret": {"raw-credential"}}, "", ""),
			want: hashed("raw-credential"),
			ok:   true,
		},
		{
			// REGRESSION: normalise on a composed node was dropped too, so an ipsubnet
			// written to coarsen an address silently kept the full value - defeating
			// the only reason to write it, and storing the exact address.
			name: "first: normalises at the composed node",
			spec: `key:
      first:
        - {header: [cf-connecting-ip]}
        - {header: [x-forwarded-for]}
      normalise: ipsubnet/24`,
			req:  req("/", http.Header{"Cf-Connecting-Ip": {"203.0.113.77"}}, "", ""),
			want: "203.0.113.0/24",
			ok:   true,
		},
		{
			name: "composite: hashes the joined value",
			spec: `key:
      separator: "|"
      composite:
        - {header: [x-api-key]}
        - {header: [x-tenant]}
      hash: true`,
			req:  req("/", http.Header{"X-Api-Key": {"k1"}, "X-Tenant": {"t1"}}, "", ""),
			want: hashed("k1|t1"),
			ok:   true,
		},
		{
			name: "composite: normalises the joined value",
			spec: `key:
      composite:
        - {header: [x-a]}
        - {header: [x-b]}
      normalise: lower`,
			req:  req("/", http.Header{"X-A": {"AA"}, "X-B": {"BB"}}, "", ""),
			want: "aa:bb",
			ok:   true,
		},
		{
			name: "header, first non-empty wins",
			spec: `key: {header: [x-absent, x-api-key]}`,
			req:  req("/", http.Header{"X-Api-Key": {"abc"}}, "", ""),
			want: "abc",
			ok:   true,
		},
		{
			name: "header lowercased",
			spec: `key: {header: [x-api-key], normalise: lower}`,
			req:  req("/", http.Header{"X-Api-Key": {"ABC"}}, "", ""),
			want: "abc",
			ok:   true,
		},
		{
			name: "header collapsed to an IPv4 subnet",
			spec: `key: {header: [cf-connecting-ip], normalise: ipsubnet/24}`,
			req:  req("/", http.Header{"Cf-Connecting-Ip": {"203.0.113.45"}}, "", ""),
			want: "203.0.113.0/24",
			ok:   true,
		},
		{
			name: "header collapsed to an IPv6 subnet",
			// Quoted: an unquoted comma would split the flow mapping entry.
			spec: `key: {header: [cf-connecting-ip], normalise: "ipsubnet/24,32"}`,
			req:  req("/", http.Header{"Cf-Connecting-Ip": {"2001:db8:dead:beef::1"}}, "", ""),
			want: "2001:db8::/32",
			ok:   true,
		},
		{
			name: "query parameter",
			spec: `key: {query: [api_key]}`,
			req:  req("/", nil, "api_key=abc", ""),
			want: "abc",
			ok:   true,
		},
		{
			name: "first falls through header to query",
			spec: `key:
      first:
        - {header: [x-api-key]}
        - {query: [api_key]}`,
			req:  req("/", nil, "api_key=from-query", ""),
			want: "from-query",
			ok:   true,
		},
		{
			name: "first prefers the earlier source",
			spec: `key:
      first:
        - {header: [x-api-key]}
        - {query: [api_key]}`,
			req:  req("/", http.Header{"X-Api-Key": {"from-header"}}, "api_key=from-query", ""),
			want: "from-header",
			ok:   true,
		},
		{
			name: "first with no source applying",
			spec: `key:
      first:
        - {header: [x-api-key]}
        - {query: [api_key]}`,
			req: req("/", nil, "", ""),
			ok:  false,
		},
		{
			name: "composite joins with the default separator",
			spec: `key:
      composite:
        - {header: [x-api-key]}
        - {header: [cf-connecting-ip]}`,
			req: req("/", http.Header{
				"X-Api-Key":        {"abc"},
				"Cf-Connecting-Ip": {"203.0.113.45"},
			}, "", ""),
			want: "abc:203.0.113.45",
			ok:   true,
		},
		{
			name: "composite with an explicit separator",
			spec: `key:
      separator: "|"
      composite:
        - {header: [x-api-key]}
        - {header: [cf-connecting-ip]}`,
			req: req("/", http.Header{
				"X-Api-Key":        {"abc"},
				"Cf-Connecting-Ip": {"203.0.113.45"},
			}, "", ""),
			want: "abc|203.0.113.45",
			ok:   true,
		},
		{
			name: "composite requires every part",
			spec: `key:
      composite:
        - {header: [x-api-key]}
        - {header: [cf-connecting-ip]}`,
			req: req("/", http.Header{"X-Api-Key": {"abc"}}, "", ""),
			ok:  false,
		},
		{
			name: "hash replaces the value",
			spec: `key: {body: phone_number, normalise: digits, hash: true}`,
			req:  req("/", nil, "", `{"phone_number":"07700 900 123"}`),
			want: hashed("07700900123"),
			ok:   true,
		},
		{
			name: "hash applies per composite part",
			spec: `key:
      composite:
        - {header: [x-api-key], hash: true}
        - {header: [cf-connecting-ip]}`,
			req: req("/", http.Header{
				"X-Api-Key":        {"abc"},
				"Cf-Connecting-Ip": {"203.0.113.45"},
			}, "", ""),
			want: hashed("abc") + ":203.0.113.45",
			ok:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := build(t, `
limiters:
  - name: subject
    `+tc.spec+`
    window: {limit: 5, window: 1m, block: 1m}
`, testSecret)
			require.Len(t, b.Limiters, 1)

			got, ok := b.Limiters[0].Keyer.Key(tc.req)
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				require.Equal(t, tc.want, got)
			}
		})
	}
}

// Without a secret a hashing limiter must be left out entirely, not silently
// downgraded to storing the raw value: that would put a credential or personal
// data into the store and the logs.
func TestHashingLimiterIsDisabledWithoutASecret(t *testing.T) {
	t.Parallel()

	b := build(t, `
limiters:
  - name: signup
    key: {body: email, hash: true}
    window: {limit: 5, window: 1m, block: 1m}
  - name: ip
    key: {header: [cf-connecting-ip]}
    window: {limit: 5, window: 1m, block: 1m}
`, "")

	require.Equal(t, []string{"ip"}, b.Names(),
		"the hashing limiter must not be registered at all, so metrics do not imply it is active")
	require.Len(t, b.Warnings, 1)
	require.Contains(t, b.Warnings[0], "signup")
	require.Contains(t, b.Warnings[0], "HASH_SECRET")
}

// A nested hash requirement is just as disqualifying as a top-level one; missing
// it would leave a credential unhashed inside a composite key.
func TestNestedHashRequirementIsDetected(t *testing.T) {
	t.Parallel()

	b := build(t, `
limiters:
  - name: tenant-email
    key:
      composite:
        - {header: [x-api-key]}
        - {body: email, hash: true}
    window: {limit: 5, window: 1m, block: 1m}
`, "")

	require.Empty(t, b.Limiters)
	require.Contains(t, b.Warnings[0], "tenant-email")
}

// Every limiter being disabled is valid but means nothing is limited, which is a
// fail-open path and must be loud.
func TestNoActiveLimitersWarns(t *testing.T) {
	t.Parallel()

	b := build(t, `
limiters:
  - name: signup
    key: {body: email, hash: true}
    window: {limit: 5, window: 1m, block: 1m}
`, "")

	require.Empty(t, b.Limiters)
	require.Contains(t, joined(b.Warnings), "every request will be allowed")
}

func TestResponseDefaults(t *testing.T) {
	t.Parallel()

	b := build(t, `
limiters:
  - name: signup
    key: {body: email}
    window: {limit: 5, window: 1m, block: 1m}
`, testSecret)

	require.Equal(t, config.DefaultResponse, b.Default)
	require.Empty(t, b.Responses)
}

// An override states only what differs, so unset fields must come from the base
// rather than becoming a zero status or an empty body.
func TestResponseOverridesInherit(t *testing.T) {
	t.Parallel()

	b := build(t, `
response:
  status: 503
  body: "global"
limiters:
  - name: status-only
    key: {header: [x-api-key]}
    window: {limit: 5, window: 1m, block: 1m}
    response: {status: 429}
  - name: body-only
    key: {header: [x-api-key]}
    window: {limit: 5, window: 1m, block: 1m}
    response: {body: "specific"}
  - name: inherits
    key: {header: [x-api-key]}
    window: {limit: 5, window: 1m, block: 1m}
`, testSecret)

	require.Equal(t, 503, b.Default.Status)
	require.Equal(t, []byte("global"), b.Default.Body)
	require.Equal(t, config.DefaultResponse.ContentType, b.Default.ContentType,
		"a partial global response must inherit the built-in content type")

	require.Equal(t, 429, b.Responses["status-only"].Status)
	require.Equal(t, []byte("global"), b.Responses["status-only"].Body)

	require.Equal(t, 503, b.Responses["body-only"].Status)
	require.Equal(t, []byte("specific"), b.Responses["body-only"].Body)

	require.NotContains(t, b.Responses, "inherits",
		"a limiter without an override must not get an entry, so the default is used")
}

func TestResponseBodyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	page := filepath.Join(dir, "blocked.html")
	require.NoError(t, os.WriteFile(page, []byte("<html>slow down</html>"), 0o600))

	b := build(t, `
limiters:
  - name: signup
    key: {header: [x-api-key]}
    window: {limit: 5, window: 1m, block: 1m}
    response:
      contentType: text/html
      bodyFile: `+page+`
`, testSecret)

	require.Equal(t, "text/html", b.Responses["signup"].ContentType)
	require.Equal(t, []byte("<html>slow down</html>"), b.Responses["signup"].Body)
}
