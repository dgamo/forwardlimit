# forwardlimit

**Rate limiting via Traefik ForwardAuth.** A small Go service that Traefik asks
about each request on a protected route; it answers **200** (allow) or **429**
(limited), counting in Redis so limits hold across every replica.

[![CI](https://github.com/dgamo/forwardlimit/actions/workflows/ci.yml/badge.svg)](https://github.com/dgamo/forwardlimit/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/dgamo/forwardlimit)](https://goreportcard.com/report/github.com/dgamo/forwardlimit)
[![Go Reference](https://pkg.go.dev/badge/github.com/dgamo/forwardlimit.svg)](https://pkg.go.dev/github.com/dgamo/forwardlimit)

```
route WITH forwardlimit attached

                       ┌─────────────► Redis
                       │
 client ──► Traefik ──► forwardlimit
               ▲             │
               │   allow ────┴──── limit
               │   (200)          (429)
               │     │              │
               │     ▼              ▼
               │ your application  429 to client
               │     │             (app never called)
               └─────┴──────────────► client

route WITHOUT it

 client ──► Traefik ──────────────► your application ──► client
```

- **Key on anything in the request** — a JSON body field (by dotted path), a
  header, a query parameter, or a combination of them.
- **Two algorithms**: a fixed window with a punitive block, for abuse controls; a
  token bucket, for capacity protection.
- **Scope by path and method** — `methods: ["POST"]` keeps a browser's CORS preflight
  from spending the budget the real request needs.
- **Distributed** — counting happens in Redis 5+ (or Valkey), single node or cluster,
  TLS optional, so limits are global rather than per-replica.
- **Fails open** on every error path, and answers `200` while doing so, because
  ForwardAuth grants access only on a 2xx.
- **Dry run per limiter** — measure a new limit in production before it rejects
  anything.
- **Declarative** — limiters live in a YAML file, so adding one needs no code.

## Why this exists

Traefik's built-in `RateLimit` middleware is per-instance by default, cannot key on
a request body, and cannot express "once over the limit, stay blocked for an hour" —
the shape most abuse controls actually need. Writing it as a Traefik plugin is not
an option either: plugins run under the Yaegi interpreter, which cannot import a
Redis client library.

`ForwardAuth` is the escape hatch. It is designed for authorisation, but the
mechanism — *pause the request, ask a service, honour its answer* — is exactly what
a rate limiter needs, and it lets the decision logic be an ordinary Go service with
ordinary dependencies.

| | Traefik `RateLimit` | Community plugins | forwardlimit |
|---|---|---|---|
| Distributed counting | Redis support, with caveats | varies | ✅ Redis, single **and** cluster |
| Redis TLS | — | rarely | ✅ |
| Key on a body field | ❌ | ❌ | ✅ dotted path |
| Key on a header or query parameter | source IP / header | header | ✅ either, with fallbacks |
| Composite keys | ❌ | ❌ | ✅ |
| Block window beyond the counting window | ❌ | ❌ | ✅ |
| Token bucket with `Retry-After` | ✅ | varies | ✅ |
| Dry run | ❌ | ❌ | ✅ per limiter |
| Hashing of sensitive key values | ❌ | ❌ | ✅ HMAC-SHA256 |

## Quickstart

A complete, runnable stack — Traefik, forwardlimit, Redis and a backend:

```sh
git clone https://github.com/dgamo/forwardlimit
cd forwardlimit/examples/docker-compose
docker compose up -d
```

```sh
for i in 1 2 3 4; do
  curl -s -o /dev/null -w "$i -> %{http_code}\n" \
    -XPOST localhost:8000/v1/login \
    -H 'Content-Type: application/json' \
    -d '{"email":"user@example.com"}'
done
# 1 -> 200
# 2 -> 200
# 3 -> 200
# 4 -> 429
```

The fourth request is rejected by Traefik itself, on forwardlimit's say-so — it
never reaches the backend.

## Configuration

Two sources, split by what each is good at. **Limiters** go in a YAML file, because
a key can be a tree and that does not flatten into environment variables readably.
**Infrastructure and secrets** stay in the environment.

```yaml
# /etc/forwardlimit/config.yaml
limiters:
  # Capacity: stop one caller consuming everyone's headroom.
  - name: tenant
    key:
      first:                              # first source that applies wins
        - {header: [x-api-key, authorization]}
        - {query: [api_key]}
      hash: true                          # a credential must not become a raw key
    bucket: {rate: 200, burst: 400}
    retryAfter: true                      # a cooperating caller should be told

  # Abuse control: a value that trips the limit stays blocked for an hour.
  - name: login
    key:
      body: email                         # dotted paths reach nested fields
      normalise: lower
      hash: true
    window: {limit: 15, window: 1h, block: 1h}
    paths: ["/v1/login"]                  # empty means every path

  # Address-keyed, coarsened to a /24 so rotating within a network does not help.
  - name: ip
    key:
      header: [CF-Connecting-IP, X-Forwarded-For]
      normalise: ipsubnet/24
    window: {limit: 100, window: 1m, block: 1m}
    dryRun: true                          # measure first, enforce later
```

```sh
REDIS_ADDRS=redis:6379
HASH_SECRET=<32+ random bytes>    # required by any limiter with hash: true
```

Limiters are evaluated in order and **the first to reject decides**, so put the
cheapest first: `tenant` above is header-keyed and rejects an over-quota caller
before any body is parsed.

Full schema and every environment variable: **[docs/configuration.md](docs/configuration.md)**.

## The ForwardAuth contract

| | |
|---|---|
| Called by | Traefik's `ForwardAuth` middleware, once per request on a protected route |
| Endpoint | `POST /check` (also accepts `GET`) |
| Request | the original request's headers and, with `forwardBody`, its body |
| Original path | read from `X-Forwarded-Uri`, **not** the URL of the `/check` call |
| Allow | `200`, empty body — Traefik forwards the request upstream |
| Reject | the configured response — Traefik returns it to the client **verbatim** |

Minimum middleware:

```yaml
forwardAuth:
  address: http://127.0.0.1:8080/check
  forwardBody: true          # required, or body-keyed limiters never apply
  maxBodySize: 262144        # above your application's own body limit
  preserveRequestMethod: true # Traefik 3.4+; on older versions it is rejected outright
```

`forwardBody` is the setting most likely to be missed: without it the request body
never reaches the service and a body-keyed limiter silently does nothing. Every
setting and its failure mode: **[docs/traefik.md](docs/traefik.md)**.

Because the rejection is returned to the client verbatim, it is a public contract.
The default says as little as possible:

```json
{"error":"rate_limited"}
```

Override it globally or per limiter — status, content type and body, inline or from
a file.

## Deployment

Three supported shapes, each with a complete example:

| Mode | Example | Notes |
|---|---|---|
| Standalone Traefik | [`examples/docker-compose/`](examples/docker-compose) | the two-minute path |
| Kubernetes, sidecar in the Traefik pods | [`examples/kubernetes/sidecar/`](examples/kubernetes/sidecar) | **recommended** |
| Kubernetes, its own Deployment | [`examples/kubernetes/standalone/`](examples/kubernetes/standalone) | simpler, one extra dependency on the request path |

The sidecar is recommended for one reason: **ForwardAuth fails closed.** If the
service is unreachable, Traefik returns 5xx and the request is refused. In the same
pod, "unreachable" cannot happen independently of "this proxy pod is already
broken", and a failing readiness probe removes that pod from the load balancer
rather than serving errors. See **[docs/deployment.md](docs/deployment.md)**.

## Behaviour

**Two algorithms, chosen per limiter.**

*Fixed window plus a hard block* — requests are counted within `window`; once
`limit` is exceeded the bucket is blocked for `block`, which **persists after the
counting window rolls over**. A blocked caller is not released at the next window
boundary. This is the shape an abuse control needs and the one no proxy
configuration can express.

*Token bucket* — `burst` tokens, refilling at `rate` per second. Smooth, no window
boundary to exploit, and recovery is continuous. Right for capacity limits, where
the caller is cooperating and should simply be paced.

**Dry run.** `dryRun: true` makes a limiter evaluate fully — keyer, store call,
counter increment, block state — and suppress only the final rejection. Counters
increment, the decision is logged and recorded as `would_block`, and the request is
allowed. That is how a limit is rolled out: measure, then enforce.

It is per limiter, and a limiter in dry run **cannot** affect any other, so adding
one for measurement never disables protection you already rely on.

**Fails open, always.** A store error, an unreachable Redis, a missing
`HASH_SECRET`, an unparseable body, an invalid rule, or a panic all result in the
request being allowed. Every such event is counted as `failed_open` and logged.

Fail-open works by answering `200`, because ForwardAuth grants access only on a 2xx.
So the service fails open for as long as it can still reply — a panic is recovered
into a `200` rather than becoming a 5xx. A process that is dead or wedged cannot
fail open, which is what the sidecar shape and the readiness probe are for.

**Sensitive values never become keys.** With `hash: true` a value is HMAC-SHA256
hashed before it is used as a storage key; only a truncated hash prefix reaches the
logs, and never the raw value or a request body. Without `HASH_SECRET` such a
limiter is left out entirely rather than downgraded.

## Observability

`GET /metrics`, namespaced `forwardlimit` by default (`METRICS_NAMESPACE`):

| Metric | Purpose |
|---|---|
| `forwardlimit_decisions_total{limiter,decision}` | `allowed` / `blocked` / `would_block` / `failed_open` |
| `forwardlimit_store_errors_total` | requests allowed **without being counted** |
| `forwardlimit_store_breaker_open` | 1 while the store circuit is open |
| `forwardlimit_block_cache_hits` | decisions served without touching Redis |
| `forwardlimit_check_duration_seconds` | check latency |

`GET /healthz` reports process health only and deliberately **never** checks Redis —
[docs/operations.md](docs/operations.md) explains why that matters, along with
alerting rules and a runbook.

## Performance

Measured on two replicas against a managed Redis:

| Scenario | Throughput | p50 | p99 |
|---|---|---|---|
| Worst case: a unique key per request, every request reaching Redis | 5,415 req/s | 6.95 ms | 49 ms |
| Repeated keys, absorbed by the block cache | 20,515 req/s | 7.97 ms | 56 ms |
| At ~3,000 req/s, every request reaching Redis | — | 2.99 ms | 8.06 ms |

Added cost at realistic load: roughly **+1.2 ms p50, +4.4 ms p99**.

## Documentation

- [docs/configuration.md](docs/configuration.md) — the YAML schema and every environment variable
- [docs/traefik.md](docs/traefik.md) — the ForwardAuth contract, setting by setting
- [docs/deployment.md](docs/deployment.md) — Docker Compose, Kubernetes sidecar, Kubernetes standalone
- [docs/architecture.md](docs/architecture.md) — design, both algorithms, why the block cache is safe
- [docs/extending.md](docs/extending.md) — a new limiter in YAML, or a custom `Keyer` in Go
- [docs/operations.md](docs/operations.md) — runbook, alerting, dry-run rollout

## Development

```sh
make tools         # the pinned golangci-lint and kubeconform, once
make ci            # everything the pipeline checks, bar e2e
make e2e           # bring up the Compose stack, run ./e2e against it, tear it down
```

Individually:

```sh
make help          # list targets
make test          # unit tests
make test-race     # with the race detector
make cover         # coverage summary
make lint          # gofmt, go vet and golangci-lint
make build         # bin/forwardlimit
make docker        # container image
make validate      # run the binary over every example config
make manifests     # schema-check the Kubernetes examples
```

CI runs these same targets and nothing else, so a green `make ci` means a green
pipeline. `make e2e` needs Docker; it builds the image and asserts the quickstart
still behaves as documented.

## Releases

| Tag | Published on |
|---|---|
| `ghcr.io/dgamo/forwardlimit:vX.Y.Z` (plus `vX.Y`, `vX`) | a semver tag |
| `ghcr.io/dgamo/forwardlimit:latest` and `:main-<sha>` | every merge to main |

`latest` is the development tip, not the newest release — it only moves once lint,
tests, examples and the end-to-end suite have all passed, but it is unreleased code.
Pin `vX.Y.Z` in production. See [docs/deployment.md](docs/deployment.md#container-image).

Integration tests against a real Redis — the only way to cover cluster slot routing
— are opt-in:

```sh
FORWARDLIMIT_INTEGRATION=1 REDIS_ADDRS=host:6379 REDIS_MODE=cluster make test-integration
```

Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md). To report a
vulnerability, see [SECURITY.md](SECURITY.md).

## Licence

[Apache-2.0](LICENSE) © Daniel Gallardo
