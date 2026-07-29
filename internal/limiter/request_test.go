// SPDX-License-Identifier: Apache-2.0

package limiter_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgamo/forwardlimit/internal/limiter"
)

func TestRequestField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		body  string
		field string
		want  string
		ok    bool
	}{
		{"top level string", `{"phone_number":"447700900123"}`, "phone_number", "447700900123", true},
		{"missing field", `{"token":"abc"}`, "phone_number", "", false},
		{"empty string is absent", `{"phone_number":""}`, "phone_number", "", false},
		{"null is absent", `{"phone_number":null}`, "phone_number", "", false},
		{"object is not a value", `{"phone_number":{"a":1}}`, "phone_number", "", false},
		{"array is not a value", `{"phone_number":[1]}`, "phone_number", "", false},
		{"bool is not a value", `{"phone_number":true}`, "phone_number", "", false},
		{"nested dotted path", `{"customer":{"contact":{"phone_number":"07700900123"}}}`,
			"customer.contact.phone_number", "07700900123", true},
		{"nested missing intermediate", `{"customer":{}}`, "customer.contact.phone_number", "", false},
		{"nested through non-object", `{"customer":"x"}`, "customer.phone_number", "", false},
		{"empty body", ``, "phone_number", "", false},
		{"malformed json", `{not json`, "phone_number", "", false},
		{"json array body", `[1,2,3]`, "phone_number", "", false},
		{"empty field name", `{"a":"b"}`, "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &limiter.Request{Body: []byte(tt.body), Header: http.Header{}}
			got, ok := r.Field(tt.field)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

// A numeric phone_number must not escape limiting, and UseNumber must keep long
// digit strings exact rather than round-tripping through float64.
func TestRequestFieldAcceptsJSONNumbersWithoutPrecisionLoss(t *testing.T) {
	t.Parallel()

	tests := []struct{ body, want string }{
		{`{"phone_number":447700900123}`, "447700900123"},
		// 19 digits, well beyond the 2^53 that float64 represents exactly - the
		// case UseNumber exists to handle.
		{`{"phone_number":4477009001234567890}`, "4477009001234567890"},
	}

	for _, tt := range tests {
		r := &limiter.Request{Body: []byte(tt.body), Header: http.Header{}}
		got, ok := r.Field("phone_number")
		require.True(t, ok)
		require.Equal(t, tt.want, got)
	}
}

func TestRequestFieldParsesBodyOnce(t *testing.T) {
	t.Parallel()
	r := &limiter.Request{Body: []byte(`{"a":"1","b":"2"}`), Header: http.Header{}}

	for i := 0; i < 3; i++ {
		v, ok := r.Field("a")
		require.True(t, ok)
		require.Equal(t, "1", v)
	}
	v, ok := r.Field("b")
	require.True(t, ok)
	require.Equal(t, "2", v)
}

func TestRequestHeaderValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header http.Header
		names  []string
		want   string
		ok     bool
	}{
		{
			name:   "first name wins",
			header: http.Header{"Cf-Connecting-Ip": {"1.1.1.1"}, "X-Forwarded-For": {"2.2.2.2"}},
			names:  []string{"CF-Connecting-IP", "X-Forwarded-For"},
			want:   "1.1.1.1", ok: true,
		},
		{
			name:   "falls through to later name",
			header: http.Header{"X-Forwarded-For": {"2.2.2.2"}},
			names:  []string{"CF-Connecting-IP", "X-Forwarded-For"},
			want:   "2.2.2.2", ok: true,
		},
		{
			name:   "empty value is skipped",
			header: http.Header{"Cf-Connecting-Ip": {""}, "X-Forwarded-For": {"2.2.2.2"}},
			names:  []string{"CF-Connecting-IP", "X-Forwarded-For"},
			want:   "2.2.2.2", ok: true,
		},
		{
			name:   "leftmost of a comma list",
			header: http.Header{"X-Forwarded-For": {"3.3.3.3, 4.4.4.4, 5.5.5.5"}},
			names:  []string{"X-Forwarded-For"},
			want:   "3.3.3.3", ok: true,
		},
		{
			name:   "whitespace trimmed",
			header: http.Header{"Cf-Connecting-Ip": {"  9.9.9.9  "}},
			names:  []string{"CF-Connecting-IP"},
			want:   "9.9.9.9", ok: true,
		},
		{
			name:   "none present",
			header: http.Header{},
			names:  []string{"CF-Connecting-IP"},
			want:   "", ok: false,
		},
		{
			name:   "comma with empty first entry",
			header: http.Header{"X-Forwarded-For": {", 4.4.4.4"}},
			names:  []string{"X-Forwarded-For"},
			want:   "", ok: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &limiter.Request{Header: tt.header}
			got, ok := r.HeaderValue(tt.names...)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.want, got)
		})
	}
}
