# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Since the configuration file and the metric names are the interfaces users depend on,
a breaking change to either is a major-version change.

## [Unreleased]

### Added

- **`methods` on a limiter** — scope a limiter to specific HTTP methods, ANDed with
  `paths`. Matching folds case on both sides, so `methods: ["POST"]` also catches a
  client sending `post`; folding only the configuration would leave an evasion path.

  The usual reason to set it is CORS: an unscoped limiter counts the browser's
  `OPTIONS` preflight as well as the request itself, which silently halves the
  effective limit.

  Omitting the field means every method, so existing configurations are unaffected.

### Changed

- The README diagram now shows the allow and limit outcomes separately, and
  distinguishes a route with the middleware attached from one without. The previous
  version drew ForwardAuth and the upstream forward as though they were alternatives,
  and labelled the forward with a `200` the application never receives.

## [0.1.0] - 2026-07-29

First public release.

### Added

- **Traefik `ForwardAuth` endpoint** — `POST /check` answers `200` (allow) or the
  configured rejection, which Traefik returns to the client verbatim. The original
  request is read from `X-Forwarded-Uri` and `X-Forwarded-Method`.
- **Two limiting algorithms**, chosen per limiter:
  - a fixed window with a hard block that **outlives the counting window**, for abuse
    controls;
  - a token bucket with an explicit burst and `Retry-After`, for capacity protection.
- **Redis-backed counting** in single-node and cluster mode (Redis 5+ or Valkey), with
  optional TLS — including `REDIS_TLS_CA_FILE` for a private CA and client
  certificates for mutual TLS, so verification need not be disabled to use it. One
  round trip per decision via Lua, with cluster hash tags so a multi-key script cannot
  straddle two slots, and the clock read inside the script so replica skew cannot
  distort a token-bucket refill.
- **Declarative limiters** in YAML. Keys may be a JSON body field (by dotted path), a
  header, or a query parameter; `first` gives fallbacks and `composite` narrows scope,
  both nesting arbitrarily. Unknown fields are rejected, and validation reports every
  problem at once.
- **Normalisers** — `digits`, `lower`, `trim`, and `ipsubnet/N[,M]` for collapsing an
  address to its network prefix so rotation within a network stops helping.
- **HMAC-SHA256 hashing** of sensitive key values. Without `HASH_SECRET` such a limiter
  is left out entirely rather than storing the raw value, and says so at startup.
- **Dry run per limiter** — full evaluation, `would_block` verdict, no rejection. A
  limiter in dry run cannot mask an enforcing one behind it.
- **Configurable rejection response** — status, content type and body, inline or from a
  file, globally or per limiter, with overrides inheriting what they do not set.
- **Block cache** — a bounded in-memory cache of *blocked verdicts only*, so it can
  never produce an incorrect allow, TTL-capped so a manual unblock propagates promptly.
- **Circuit breaker** on the store, so an outage fails open in ~1 ms instead of a dial
  timeout per request.
- **Fail-open on every error path**, including a recovered panic, because ForwardAuth
  grants access only on a 2xx.
- **Prometheus metrics** under a configurable namespace (`METRICS_NAMESPACE`, default
  `forwardlimit`), and structured `slog` logging that never emits a request body or a
  full bucket value.
- **Deployment examples** for standalone Traefik via Docker Compose, a Kubernetes
  native sidecar, and a standalone Kubernetes Deployment.
- **`-validate`** to check a configuration and exit, and **`-healthcheck`** so the
  distroless image can have a container healthcheck at all.
- **An end-to-end suite** (`make e2e`) that runs the documented behaviour through a
  real Traefik against the Compose example, including the store-outage and
  service-outage paths.
- **Container images** on `ghcr.io/dgamo/forwardlimit`: `:latest` and `:main-<sha>` on
  every merge to main, `:vX.Y.Z` plus the moving `:vX.Y` and `:vX` on a semver tag.
  `latest` tracks the development tip rather than the newest release, so pin a version
  tag in production.

### Notes

- The Traefik middleware examples use `preserveRequestMethod`, which needs **Traefik
  3.4 or newer**. On an older version the file provider rejects the entire dynamic
  configuration rather than ignoring the field, and every route returns 404. Drop the
  field if you are on 3.3 or below.

[0.1.0]: https://github.com/dgamo/forwardlimit/releases/tag/v0.1.0
