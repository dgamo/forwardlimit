# Configuration

Two sources, split by what each is good at:

- **A YAML file** declares the **limiters**. A key can be a tree — "the first of
  these headers, otherwise this query parameter" — and that does not flatten into
  environment variables readably.
- **The environment** carries **infrastructure and secrets**: listen address,
  logging, the store connection, the hash secret. These differ per deployment, and
  a secret must never sit in a mounted config file.

Both are **fail-fast**. An unparseable or out-of-range value stops the process at
startup rather than falling back to a default, and *every* problem is reported at
once rather than one restart at a time. A limiter quietly running with the wrong
threshold is worse than one that refuses to start, because the mistake stays
invisible until it matters.

Durations are Go duration strings (`500ms`, `30s`, `1h`, `24h`).

There is **no runtime reload**: changing configuration means a restart. Counters
live in Redis and are unaffected.

## The configuration file

Located by `-config`, else `CONFIG_PATH`, else `/etc/forwardlimit/config.yaml`.

Unknown fields are **rejected**. A mistyped key that was silently ignored is how a
limiter ends up not doing what its author intended.

```yaml
# Optional. The default response when a limiter rejects; a limiter may override it.
response:
  status: 429
  contentType: application/json
  body: '{"error":"rate_limited"}'

# Required, at least one. Evaluated in order; the first to reject decides.
limiters:
  - name: tenant
    key:
      first:
        - {header: [x-api-key, authorization]}
        - {query: [api_key]}
      hash: true
    bucket: {rate: 200, burst: 400}
    retryAfter: true

  - name: login
    key:
      body: email
      normalise: lower
      hash: true
    window: {limit: 15, window: 1h, block: 1h}
    paths: ["/v1/login"]
    dryRun: false
```

### `limiters[]`

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Unique, matching `^[a-z0-9][a-z0-9_-]*$`. It becomes a metric label and part of every storage key, so it must be stable — renaming one starts its counters from zero. |
| `key` | yes | How to derive the bucket. See [Key specification](#key-specification). |
| `window` | one of | Fixed window with a hard block. Mutually exclusive with `bucket`. |
| `bucket` | one of | Token bucket. Mutually exclusive with `window`. |
| `paths` | no | Path prefixes this limiter applies to. Empty means every path. |
| `dryRun` | no | Evaluate and report without rejecting. Default `false`. |
| `retryAfter` | no | Send `Retry-After` when this limiter rejects. Default `false`. |
| `response` | no | Override the response for this limiter only. |

Exactly one of `window` or `bucket` is required. A limiter enforces one algorithm;
declaring both is rejected rather than resolved by precedence.

Order matters twice over: the first limiter to reject decides the response, and
each preceding limiter costs a store call. Put the cheapest and most decisive
first — a header-keyed capacity limit rejects an over-quota caller before any body
is parsed.

### `window` — fixed window plus a hard block

```yaml
window: {limit: 15, window: 1h, block: 1h}
```

| Field | Bounds | Meaning |
|---|---|---|
| `limit` | 1 – 10000 | Requests allowed per window. |
| `window` | 1s – 1h | Counting window. |
| `block` | 1s – 24h | How long a bucket stays blocked once the limit is exceeded. |

The block is a separate key with its own TTL, so **it outlives the counting
window**: a caller that trips the limit is not released when the window rolls over.
That is the point of this algorithm, and the part no proxy configuration can
express.

Two guarantees worth knowing:

- `block` is the whole duration, whatever `window` is. The counter is reset when the
  block is set, so `window: 1h, block: 5m` blocks for five minutes and then starts a
  fresh window — not for the rest of the hour.
- Continued requests during a block do not extend it. A blocked caller is released
  after `block` however hard it tried in between.

Use it for abuse controls, where a caller over the limit should stay out for a
while.

### `bucket` — token bucket

```yaml
bucket: {rate: 200, burst: 400}
```

| Field | Bounds | Meaning |
|---|---|---|
| `rate` | 1 – 1,000,000 | Sustained requests per second. |
| `burst` | 1 – 1,000,000 | Tokens held, so the maximum instantaneous burst. |

Smooth, with no window boundary to exploit, and recovery is continuous. Use it for
capacity limits, where the caller is cooperating and should be paced rather than
punished — and set `retryAfter: true` with it, or a well-behaved client will retry
immediately and make the overload worse.

A `burst` of roughly twice `rate` is a reasonable starting point: it absorbs normal
jitter without allowing a sustained doubling.

The bounds on both algorithms exist to catch a mistyped extra digit, not to express
policy. The edges themselves are usable.

## Key specification

The key decides *what is being limited*. Exactly one source per node:

| Field | Meaning |
|---|---|
| `body: <path>` | A JSON body field. A dotted path reaches nested fields: `customer.contact.email`. Requires `forwardBody: true` on the middleware. |
| `header: [...]` | An ordered list of header names; the **first non-empty wins**. |
| `query: [...]` | An ordered list of query parameters; the first non-empty wins. |
| `first: [...]` | Child specs tried in order; the first that applies wins. |
| `composite: [...]` | **Every** child must apply; their values are joined. |

Modifiers, valid on any node:

| Field | Meaning |
|---|---|
| `normalise` | Canonicalise the value so equivalent inputs share a bucket. One of `digits`, `lower`, `trim`, `ipsubnet/N[,M]`, `none`. |
| `hash` | HMAC-SHA256 the value before it is used as a key. Requires `HASH_SECRET`. |
| `separator` | Joins `composite` parts. Defaults to `:`. Valid only with `composite`. |

If a limiter's key does not apply to a request — the body field is absent, no listed
header is present, nothing remains after normalisation — that limiter is **skipped**
and evaluation continues. It is not an error, and it is not a rejection.

A body-keyed limiter therefore does nothing at all when `forwardBody` is not set on
the middleware. That is the single most common misconfiguration; see
[traefik.md](traefik.md).

### Normalisers

| Value | Effect |
|---|---|
| `digits` | Keep ASCII digits only, so `07700 900-123` and `07700900123` share a bucket. |
| `lower` | Lowercase, so values differing only by case share a bucket. |
| `trim` | Trim surrounding whitespace. |
| `ipsubnet/N` | Collapse an IP address to its network prefix: `ipsubnet/24` turns `203.0.113.45` into `203.0.113.0/24`. IPv6 defaults to `/64`. |
| `ipsubnet/N,M` | Both prefixes explicitly: `"ipsubnet/24,48"`. **Quote it** — an unquoted comma splits a YAML flow mapping. |
| `none`, or omitted | No normalisation. |

`ipsubnet` is what makes an address-keyed limit resistant to rotation. An attacker
with a pool of addresses usually holds many within a few networks, so limiting per
/24 rather than per address reduces the value of rotating by up to 256×. It also
means legitimate users behind one NAT share a bucket, so raise the limit
accordingly.

A value that is not an IP address passes through unchanged, so a limiter keyed on a
header that sometimes carries something else still functions.

### Hashing

`hash: true` replaces the value with an HMAC-SHA256 of it, hex encoded.

Use it for **anything sensitive**. A bucket becomes a storage key and appears
(truncated) in logs, so a credential or an account identifier used raw would leak
into both. HMAC rather than a bare digest, because a low-entropy value would
otherwise be recoverable by brute force.

**Without `HASH_SECRET`, a limiter that asks to hash is disabled entirely** — not
downgraded to storing the raw value. That fails open, so it is reported as a startup
warning:

```
limiter "login" needs hashing but HASH_SECRET is empty: it is disabled and will not limit anything
```

Hashing is checked recursively: `hash: true` on any node inside a `composite`
disables the whole limiter when no secret is configured.

Changing `HASH_SECRET` changes every bucket, so all counters effectively reset.
Rotate it deliberately, not casually.

### Composing keys

`first` gives fallbacks. Credentials commonly arrive by more than one route, and a
limiter that knows only one leaves the others unlimited:

```yaml
key:
  first:
    - {header: [x-api-key, authorization]}
    - {query: [api_key]}
  hash: true
```

`composite` narrows the scope. Every part must apply, which is correct for a rule
keyed on more than one dimension — "this tenant and this address" is meaningless
with only one of the two:

```yaml
key:
  separator: "|"
  composite:
    - {header: [x-api-key], hash: true}
    - {header: [CF-Connecting-IP], normalise: ipsubnet/24}
```

`hash` applies per node, so one part can be hashed and another left readable.

`first` and `composite` nest, so an arbitrary tree is expressible. A `composite`
with a single child is rejected: it is the same as using that child directly.

## The rejection response

Because ForwardAuth returns a non-2xx response to the client **verbatim**, this is a
public, client-facing contract. Two consequences:

1. You can define exactly what a limited client receives.
2. It must contain nothing diagnostic. No limiter name, no bucket, no counts, no
   timing.

```yaml
response:
  status: 429
  contentType: application/json
  body: '{"error":"rate_limited"}'
```

| Field | Notes |
|---|---|
| `status` | Any 4xx or 5xx. Default `429`. |
| `contentType` | Sent as `Content-Type`. |
| `body` | Inline body. |
| `bodyFile` | Read the body from a path instead, for a payload too large to inline — an HTML page, say. Mutually exclusive with `body`, and read once at startup. |

A per-limiter `response` **inherits** anything it does not set, so an override need
only state what differs:

```yaml
response: {status: 429, contentType: application/json, body: '{"error":"rate_limited"}'}
limiters:
  - name: tenant
    response: {status: 503}      # same content type and body, different status
```

### Should you return 429?

A `429` is the honest answer and what a cooperating client expects. It also confirms
to an adversary that a limit exists and roughly where it sits.

If that matters, the response is entirely yours to choose — a `403`, or a `503` that
looks like ordinary unavailability. `Retry-After` is likewise per limiter: send it on
a capacity limit, where the caller should back off, and withhold it on an abuse
control, where it would state the threshold precisely.

## Command-line flags

| Flag | Purpose |
|---|---|
| `-config <path>` | The limiter file. Overrides `CONFIG_PATH`. |
| `-validate` | Parse the file, build every keyer, print what resolved, exit. Non-zero if it would not start. Warnings go to stderr and do **not** fail it. |
| `-healthcheck` | Request `/healthz` from an instance on `LISTEN_ADDR` and exit 0 if healthy. Exists because the published image is distroless: no shell, no curl, so a container healthcheck has nothing else to call. |
| `-version` | Print the version and exit. |

`-validate` is the pre-flight check to run before a rollout:

```sh
$ forwardlimit -validate -config config.yaml
config:  config.yaml
valid:   yes
active:  tenant, login, ip
dry run: ip
```

## Environment variables

### Service

| Variable | Default | Notes |
|---|---|---|
| `CONFIG_PATH` | `/etc/forwardlimit/config.yaml` | The limiter file. `-config` overrides it. |
| `LISTEN_ADDR` | `:8080` | Bind address. Use `127.0.0.1:8080` for a sidecar, so nothing off-pod can reach it. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `LOG_FORMAT` | `json` | `json` or `text`. |
| `MAX_BODY_BYTES` | `262144` | How much request body is read. See the warning below. |
| `SHUTDOWN_TIMEOUT` | `10s` | Grace period for in-flight checks. |
| `METRICS_NAMESPACE` | `forwardlimit` | Prefixes every metric, including the Go and process collectors. |
| `HASH_SECRET` | *(empty)* | Keys the HMAC for `hash: true`. Use 32+ random bytes. Empty disables every hashing limiter. |

> **`MAX_BODY_BYTES` must stay above the body limit your protected application
> enforces.** If it were lower, a caller could pad the body past this limit, break
> JSON parsing here, and so evade body-keyed limits while the application still
> accepted the request. Truncation is counted and logged, so an attempt is visible.

A blank value is treated as unset, so an empty variable injected by a templating
layer falls back to the default rather than overriding it.

### Store

Redis **5.0 or newer**, tested against 7, single node and cluster. The token-bucket script reads the clock with
`TIME`, which needs the effect-based script replication that became the default in
Redis 5; the window script uses multi-field `HSET`, which needs 4.0. Valkey works.

| Variable | Default | Notes |
|---|---|---|
| `REDIS_ENABLED` | `true` | `false` replaces the store with a no-op: **everything is allowed**. |
| `REDIS_ADDRS` | `127.0.0.1:6379` | Comma-separated. Several for a cluster. |
| `REDIS_MODE` | `single` | `single` or `cluster`. |
| `REDIS_TLS_ENABLED` | `false` | TLS 1.2 minimum. |
| `REDIS_TLS_CA_FILE` | *(system roots)* | PEM bundle to verify the store against. **Set this for a private CA** rather than disabling verification. |
| `REDIS_TLS_CERT_FILE` | *(none)* | Client certificate, for a store requiring mutual TLS. Must be set with the key. |
| `REDIS_TLS_KEY_FILE` | *(none)* | Client key. |
| `REDIS_TLS_SKIP_VERIFY` | `false` | Last resort, for a certificate that does not match its endpoint name. Warns at startup, and warns louder if a CA file is also set, since it overrides it. |
| `REDIS_POOL_SIZE` | `32` | Per replica — multiply by replicas before sizing the store. |
| `REDIS_MAX_RETRIES` | `1` | **Not** the library default of 3: retries multiply fail-open latency during an outage. |
| `REDIS_DIAL_TIMEOUT` | `200ms` | Deliberately short, for the same reason. |
| `REDIS_READ_TIMEOUT` | `500ms` | |
| `REDIS_WRITE_TIMEOUT` | `500ms` | |
| `REDIS_KEY_PREFIX` | `forwardlimit:` | Namespaces every key; a trailing `:` is added if absent. Change it if one store is shared by several deployments. |

With the store disabled, none of its settings are validated — disabling something
should not require valid configuration for what will never be contacted.

### Block cache

An in-memory cache of **blocked verdicts only**, in front of the store.

| Variable | Default | Notes |
|---|---|---|
| `BLOCK_CACHE_ENABLED` | `true` | |
| `BLOCK_CACHE_MAX_ENTRIES` | `200000` | Bounded: at capacity, new blocks are simply not cached. |
| `BLOCK_CACHE_MAX_TTL` | `60s` | Caps each entry well below a typical block, so a manual unblock propagates promptly. Warns above 5m. |

It can only ever produce a rejection, never an incorrect allow, because allowed
verdicts and counts are never cached — see
[architecture.md](architecture.md#block-cache).

### Circuit breaker

| Variable | Default | Notes |
|---|---|---|
| `STORE_BREAKER_THRESHOLD` | `3` | Consecutive failures before the breaker opens. `0` disables it. |
| `STORE_BREAKER_COOLDOWN` | `5s` | Before one probe is admitted to test recovery. |

Its purpose is speed, not correctness: an outage already fails open, and the breaker
makes it fail open in ~1 ms instead of a dial timeout per request.

## Startup warnings

Each of these is valid configuration that will not limit something. Every one
corresponds to a deliberate fail-open path, so they are logged at `warn` and are
worth alerting on:

```
REDIS_ENABLED=false: all rate limiting is disabled and every request will be allowed
limiter "login" needs hashing but HASH_SECRET is empty: it is disabled and will not limit anything
no limiters are active: every request will be allowed
store unreachable at startup; requests will fail open until it recovers
BLOCK_CACHE_MAX_TTL is large: a manual unblock may take that long to take effect on every replica
REDIS_TLS_SKIP_VERIFY=true: the store's certificate is not verified
```

The startup line also lists which limiters are in dry run:

```json
{"msg":"starting","limiters":["tenant","login","ip"],"dry_run":["ip"]}
```

From the outside, a limiter in dry run is indistinguishable from one that is
protecting you. Treat that field as an open action, not background noise.

## Worked examples

Runnable configurations are in [`examples/config/`](../examples/config):

| File | Shows |
|---|---|
| `simple.yaml` | one address-keyed limiter — the smallest useful configuration |
| `body-key.yaml` | keying on a JSON body field, with normalisation and hashing |
| `multi-tenant.yaml` | a capacity bucket plus abuse controls, ordered by cost, one in dry run |

Every file there is parsed by a unit test, so an example that stops loading fails
the build.
