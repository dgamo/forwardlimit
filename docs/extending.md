# Extending

Most new limits need no code at all — they are a block in the configuration file. This
covers both cases: declaring a limiter, and writing a `Keyer` when the built-in
sources genuinely cannot express what you need.

## Adding a limiter in YAML

Suppose you want at most 1000 requests per minute per API key, taken from
`X-Api-Key`, across all paths, with a 5-minute block for anyone who exceeds it:

```yaml
limiters:
  - name: apikey
    key:
      header: [x-api-key]
      hash: true                 # a credential must not become a raw storage key
    window: {limit: 1000, window: 1m, block: 5m}
```

That is the whole change. Metrics gain a `limiter="apikey"` label automatically, and
the block cache, circuit breaker and fail-open behaviour all apply. Restart to pick it
up — there is no runtime reload.

### Choosing an algorithm

| You want | Use |
|---|---|
| A caller over the limit to stay out for a while | `window` — fixed window plus a hard block |
| A caller to be paced continuously, with a burst allowance | `bucket` — token bucket, with `retryAfter: true` |

The distinction is whether the caller is an adversary or a cooperating integration. An
adversary should be shut out; an integration should be told to slow down. See
[configuration.md](configuration.md#window--fixed-window-plus-a-hard-block).

### Choosing a key

| You want to limit per | Key |
|---|---|
| A JSON body field | `body: email`, or a dotted path like `customer.contact.email` |
| A header, with fallbacks | `header: [x-api-key, authorization]` |
| A query parameter | `query: [api_key]` |
| A credential that may arrive either way | `first: [{header: [...]}, {query: [...]}]` |
| Two dimensions at once | `composite: [...]` — every part must apply |

### Order it deliberately

Limiters are evaluated in file order and **the first to reject decides**. Two reasons
to think about it:

- Each preceding limiter costs a store call, so put the cheapest first.
- A header-keyed limiter rejects before the body is parsed at all.

### Roll it out in dry run

```yaml
    dryRun: true
```

The limiter evaluates fully and rejects nothing. Watch
`forwardlimit_decisions_total{limiter="apikey",decision="would_block"}` to see what
enforcement would have refused, then remove the flag.

A limiter in dry run cannot affect any other, so it is safe to add one to a running
system without reviewing the order of the rest.

## Rules of thumb

**Hash anything sensitive.** `hash: true`. A bucket becomes a storage key and appears
truncated in logs, so a credential or an account identifier used raw leaks into both.
Without `HASH_SECRET` the limiter is left out entirely rather than downgraded —
failing open, and warning loudly at startup.

**Normalise so equivalent inputs collide.** `normalise: lower` is what makes
`User@Example.com` and `user@example.com` share a bucket, and `normalise: digits`
what makes `07700 900-123` and `07700900123` share one. Without a normaliser,
trivial reformatting defeats the limit. `normalise: ipsubnet/24` is the same idea
applied to address rotation.

**Never trust a client-suppliable value without a guarantee.** A header the caller
controls lets them choose their own bucket. `CF-Connecting-IP` is only meaningful if
the origin is reachable exclusively through the CDN and the proxy strips any inbound
value.

**Scope with `paths` when a limiter reads the body.** Body inspection means Traefik
buffers the whole request, so only route the paths that need it — never file-upload
endpoints.

**A missing value means the limiter does not apply.** Never substitute a placeholder
to force it to. Collapsing every anonymous request into one shared bucket causes mass
false blocking, which is far worse than not limiting.

## Writing a custom Keyer

If none of the built-in sources can express the key, implement `limiter.Keyer`:

```go
// Keyer derives the bucket a request counts against.
//
// ok=false means "this limiter does not apply" - not an error.
type Keyer interface {
    Key(*Request) (string, bool)
}
```

`Request` gives you the method, path, headers, raw query and body, plus helpers that
parse the JSON body and query string **at most once** each and share the result:

```go
func (r *Request) Field(name string) (string, bool)          // JSON body, dotted path
func (r *Request) HeaderValue(names ...string) (string, bool) // first non-empty
func (r *Request) QueryValue(names ...string) (string, bool)  // first non-empty
```

A worked example — key on the first path segment, so each API version gets its own
bucket:

```go
// PathSegment keys on one segment of the request path.
type PathSegment struct {
    Index int // 0-based, after the leading slash
}

func (p PathSegment) Key(r *limiter.Request) (string, bool) {
    parts := strings.Split(strings.Trim(r.Path, "/"), "/")
    if p.Index < 0 || p.Index >= len(parts) || parts[p.Index] == "" {
        return "", false // does not apply; the request is not limited by this rule
    }
    return parts[p.Index], true
}
```

Three obligations:

1. **Return `ok=false` when the value is absent.** Not an error, not a placeholder.
2. **Do not panic** — though if you do, the engine recovers it into a skip rather than
   letting it take down the endpoint.
3. **Hash it yourself if it is sensitive.** Embed a `keyer.Hasher` and honour it; the
   engine does not hash on your behalf.

Wire it in `main`, or wrap a plain function with `limiter.KeyerFunc`:

```go
limiters = append(limiters, limiter.Limiter{
    Name:  "version",
    Rule:  store.Rule{Limit: 1000, Window: time.Minute, Block: time.Minute},
    Keyer: PathSegment{Index: 0},
})
```

To make it available in YAML too, add a field to `KeySpec` in
`internal/config/file.go`, a case in `buildKeyer` in `internal/config/build.go`, and
extend `KeySpec.sources()` so the "exactly one source" validation still holds.

## Testing

| What | Where |
|---|---|
| Keyer behaviour | `internal/limiter/keyer/keyer_test.go` |
| YAML parsing and validation | `internal/config/file_test.go` |
| Spec → limiter construction | `internal/config/build_test.go` |
| End-to-end through the handler | `internal/httpapi/httpapi_test.go` — `newHarness` accepts an explicit limiter list |
| Against a real store | `internal/httpapi/stack_test.go` — miniredis-backed |

Cover at minimum:

- the limiter applies and blocks at the limit;
- it does **not** apply when its source value is absent, and the request is allowed;
- a different value gets its own bucket;
- if the value is sensitive, it appears in neither the response nor the logs.

## Contributing it back

If your keyer or normaliser is generally useful, a pull request is welcome —
see [CONTRIBUTING.md](../CONTRIBUTING.md). Things that fit well:

- new normalisers (a hostname canonicaliser, a JWT claim extractor);
- new key sources (a cookie, a client certificate subject);
- support for another proxy's external-authorisation protocol.
