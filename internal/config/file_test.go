// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/config"
)

// write puts a config file in a temporary directory and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// load is the whole path a real start-up takes: read, parse, validate.
func load(t *testing.T, body string) (*config.File, error) {
	t.Helper()
	return config.LoadFile(write(t, body))
}

func TestLoadFileParsesEveryKeyShape(t *testing.T) {
	t.Parallel()

	f, err := load(t, `
response:
  status: 403
  contentType: text/plain
  body: "no"

limiters:
  - name: signup
    key:
      body: customer.contact.email
      normalise: lower
      hash: true
    window: {limit: 15, window: 1h, block: 2h}
    paths: ["/v1/signup"]
    dryRun: true

  - name: tenant
    key:
      first:
        - {header: [x-api-key, authorization]}
        - {query: [api_key]}
      hash: true
    bucket: {rate: 200, burst: 400}
    retryAfter: true

  - name: ip
    key:
      header: [CF-Connecting-IP, X-Forwarded-For]
      normalise: ipsubnet/24,48
    window: {limit: 100, window: 1m, block: 1m}

  - name: tenant-ip
    key:
      composite:
        - {header: [x-api-key]}
        - {header: [CF-Connecting-IP]}
      separator: "|"
    window: {limit: 10, window: 1m, block: 1m}
    response:
      status: 429
      body: '{"error":"slow down"}'
`)
	require.NoError(t, err)

	require.Equal(t, 403, f.Response.Status)
	require.Equal(t, "text/plain", f.Response.ContentType)
	require.Len(t, f.Limiters, 4)

	signup := f.Limiters[0]
	require.Equal(t, "signup", signup.Name)
	require.Equal(t, "customer.contact.email", signup.Key.Body)
	require.Equal(t, "lower", signup.Key.Normalise)
	require.True(t, signup.Key.Hash)
	require.True(t, signup.DryRun)
	require.Equal(t, int64(15), signup.Window.Limit)
	require.Equal(t, time.Hour, signup.Window.Window)
	require.Equal(t, 2*time.Hour, signup.Window.Block)
	require.Equal(t, []string{"/v1/signup"}, signup.Paths)

	tenant := f.Limiters[1]
	require.Len(t, tenant.Key.First, 2)
	require.Equal(t, []string{"x-api-key", "authorization"}, tenant.Key.First[0].Header)
	require.Equal(t, []string{"api_key"}, tenant.Key.First[1].Query)
	require.Equal(t, int64(200), tenant.Bucket.Rate)
	require.Equal(t, int64(400), tenant.Bucket.Burst)
	require.True(t, tenant.RetryAfter)

	require.Equal(t, "ipsubnet/24,48", f.Limiters[2].Key.Normalise)

	composite := f.Limiters[3]
	require.Len(t, composite.Key.Composite, 2)
	require.Equal(t, "|", composite.Key.Separator)
	require.Equal(t, `{"error":"slow down"}`, composite.Response.Body)
}

// A mistyped field that is silently ignored is how a limiter ends up not doing
// what its author intended, so unknown fields must be fatal.
func TestUnknownFieldIsRejected(t *testing.T) {
	t.Parallel()

	_, err := load(t, `
limiters:
  - name: signup
    key: {body: email}
    windows: {limit: 5, window: 1m, block: 1m}
`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "windows")
}

func TestMissingFileIsAnError(t *testing.T) {
	t.Parallel()

	_, err := config.LoadFile(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "reading config")
}

func TestMalformedYAMLIsAnError(t *testing.T) {
	t.Parallel()

	_, err := load(t, "limiters: [ unclosed")
	require.Error(t, err)
	require.Contains(t, err.Error(), "parsing config")
}

func TestValidationRejects(t *testing.T) {
	t.Parallel()

	cases := map[struct{ name, wantIn string }]string{
		{"no limiters", "at least one limiter"}: `
limiters: []
`,
		{"missing name", "name is required"}: `
limiters:
  - key: {body: email}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"invalid name", "name must match"}: `
limiters:
  - name: Signup!
    key: {body: email}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"duplicate name", "duplicate name"}: `
limiters:
  - name: signup
    key: {body: a}
    window: {limit: 5, window: 1m, block: 1m}
  - name: signup
    key: {body: b}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"no algorithm", "one of window or bucket"}: `
limiters:
  - name: signup
    key: {body: email}
`,
		{"both algorithms", "mutually exclusive"}: `
limiters:
  - name: signup
    key: {body: email}
    window: {limit: 5, window: 1m, block: 1m}
    bucket: {rate: 10, burst: 20}
`,
		{"missing key", "key is required"}: `
limiters:
  - name: signup
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"empty key", "one of body, header, query"}: `
limiters:
  - name: signup
    key: {normalise: digits}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"two key sources", "exactly one key source"}: `
limiters:
  - name: signup
    key: {body: email, header: [x-api-key]}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"limit out of bounds", "window.limit"}: `
limiters:
  - name: signup
    key: {body: email}
    window: {limit: 999999, window: 1m, block: 1m}
`,
		{"window out of bounds", "window.window"}: `
limiters:
  - name: signup
    key: {body: email}
    window: {limit: 5, window: 48h, block: 1m}
`,
		{"block out of bounds", "window.block"}: `
limiters:
  - name: signup
    key: {body: email}
    window: {limit: 5, window: 1m, block: 0s}
`,
		{"rate out of bounds", "bucket.rate"}: `
limiters:
  - name: tenant
    key: {header: [x-api-key]}
    bucket: {rate: 0, burst: 10}
`,
		{"burst out of bounds", "bucket.burst"}: `
limiters:
  - name: tenant
    key: {header: [x-api-key]}
    bucket: {rate: 10, burst: 0}
`,
		{"unknown normaliser", "unknown normaliser"}: `
limiters:
  - name: signup
    key: {body: email, normalise: base64}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"normaliser with unwanted argument", "takes no argument"}: `
limiters:
  - name: signup
    key: {body: email, normalise: digits/4}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"ipsubnet prefix out of range", "IPv4 prefix"}: `
limiters:
  - name: ip
    key: {header: [CF-Connecting-IP], normalise: ipsubnet/64}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"separator without composite", "separator only applies"}: `
limiters:
  - name: signup
    key: {body: email, separator: "|"}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"single-child composite", "single child"}: `
limiters:
  - name: signup
    key:
      composite:
        - {header: [x-api-key]}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"nested key invalid", "first[1]"}: `
limiters:
  - name: tenant
    key:
      first:
        - {header: [x-api-key]}
        - {normalise: digits}
    bucket: {rate: 10, burst: 20}
`,
		{"response status not an error code", "4xx or 5xx"}: `
response: {status: 200}
limiters:
  - name: signup
    key: {body: email}
    window: {limit: 5, window: 1m, block: 1m}
`,
		{"body and bodyFile together", "body and bodyFile"}: `
limiters:
  - name: signup
    key: {body: email}
    window: {limit: 5, window: 1m, block: 1m}
    response: {body: "no", bodyFile: /nonexistent}
`,
		{"missing bodyFile", "bodyFile"}: `
limiters:
  - name: signup
    key: {body: email}
    window: {limit: 5, window: 1m, block: 1m}
    response: {bodyFile: /nonexistent/response.json}
`,
	}

	for tc, body := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := load(t, body)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantIn)
		})
	}
}

// Every problem in the file should be reported at once: a configuration error
// should be fixed in one pass, not discovered one restart at a time.
func TestAllValidationErrorsAreReportedTogether(t *testing.T) {
	t.Parallel()

	_, err := load(t, `
limiters:
  - name: BAD NAME
    key: {normalise: nonsense}
  - name: other
    key: {header: [x-api-key]}
    window: {limit: 5, window: 1m, block: 1m}
    bucket: {rate: 10, burst: 20}
`)
	require.Error(t, err)

	msg := err.Error()
	require.Contains(t, msg, "name must match")
	require.Contains(t, msg, "one of window or bucket")
	require.Contains(t, msg, "unknown normaliser")
	require.Contains(t, msg, "mutually exclusive")
}

// The name becomes a metric label and part of a storage key, so the accepted set
// has to be narrow - and it has to actually accept ordinary names.
func TestNameCharacterSet(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"signup", "per-ip", "tenant_bucket", "v2", "0x"} {
		t.Run("accepts "+name, func(t *testing.T) {
			t.Parallel()

			_, err := load(t, `
limiters:
  - name: `+name+`
    key: {header: [x-api-key]}
    window: {limit: 5, window: 1m, block: 1m}
`)
			require.NoError(t, err)
		})
	}

	for _, name := range []string{"Signup", "-leading", "_leading", "has space", "dots.here", "has:colon"} {
		t.Run("rejects "+name, func(t *testing.T) {
			t.Parallel()

			_, err := load(t, `
limiters:
  - name: "`+name+`"
    key: {header: [x-api-key]}
    window: {limit: 5, window: 1m, block: 1m}
`)
			require.Error(t, err)
		})
	}
}

// Bounds exist to catch a mistyped extra digit, so the edges themselves must be
// usable.
func TestBoundsAtTheEdgesAreAccepted(t *testing.T) {
	t.Parallel()

	f, err := load(t, `
limiters:
  - name: min-window
    key: {header: [x-api-key]}
    window: {limit: 1, window: 1s, block: 1s}
  - name: max-window
    key: {header: [x-api-key]}
    window: {limit: 10000, window: 1h, block: 24h}
  - name: min-bucket
    key: {header: [x-api-key]}
    bucket: {rate: 1, burst: 1}
  - name: max-bucket
    key: {header: [x-api-key]}
    bucket: {rate: 1000000, burst: 1000000}
`)
	require.NoError(t, err)
	require.Len(t, f.Limiters, 4)
}
