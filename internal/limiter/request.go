// SPDX-License-Identifier: Apache-2.0

package limiter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Request is the subject of a rate limit decision.
//
// The JSON body is parsed at most once, lazily, and shared by every limiter, so
// adding a second body-keyed limiter costs no extra parsing.
type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
	// RawQuery is the original request's query string, without the leading "?".
	//
	// It matters because a credential may arrive as a query parameter rather than
	// a header; keying only on headers would leave those callers unlimited.
	RawQuery string

	parseOnce sync.Once
	obj       map[string]any

	queryOnce sync.Once
	query     url.Values
}

// Field returns a string value from the JSON body. name may be a dotted path
// ("customer.contact.email").
//
// JSON numbers are accepted as well as strings, or a caller could escape limiting
// by sending the value unquoted. Decoded with UseNumber so long digit strings keep
// full precision instead of passing through float64.
func (r *Request) Field(name string) (string, bool) {
	r.parseOnce.Do(r.parse)
	if r.obj == nil || name == "" {
		return "", false
	}

	var cur any = r.obj
	for _, part := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[part]
		if !ok {
			return "", false
		}
	}

	switch v := cur.(type) {
	case string:
		if v == "" {
			return "", false
		}
		return v, true
	case json.Number:
		s := v.String()
		if s == "" {
			return "", false
		}
		return s, true
	default:
		return "", false
	}
}

func (r *Request) parse() {
	if len(r.Body) == 0 {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.UseNumber()

	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		// A body we cannot parse yields no fields, so body-keyed limiters do not
		// apply and the request is allowed. Rejecting a malformed payload is the
		// application's job, not ours.
		return
	}
	r.obj = obj
}

// QueryValue returns the first non-empty query parameter among names, in order.
//
// The query string is parsed at most once and shared.
func (r *Request) QueryValue(names ...string) (string, bool) {
	if r.RawQuery == "" {
		return "", false
	}
	r.queryOnce.Do(func() {
		// Parse errors yield whatever was decodable; a malformed query must not
		// prevent limiting on the parts that did decode.
		r.query, _ = url.ParseQuery(r.RawQuery)
	})

	for _, n := range names {
		if v := strings.TrimSpace(r.query.Get(n)); v != "" {
			return v, true
		}
	}
	return "", false
}

// HeaderValue returns the first non-empty value among names, in order.
//
// For list-valued headers such as X-Forwarded-For the leftmost entry is
// returned, which is the original client rather than an intermediate proxy.
func (r *Request) HeaderValue(names ...string) (string, bool) {
	for _, n := range names {
		v := strings.TrimSpace(r.Header.Get(n))
		if v == "" {
			continue
		}
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if v != "" {
			return v, true
		}
	}
	return "", false
}
