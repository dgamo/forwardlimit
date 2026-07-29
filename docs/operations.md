# Operations

## The one thing to know

**This service fails open by design.** If Redis is gone, the process is confused, or
configuration is incomplete, requests are **allowed**. Rate limiting is an abuse
control, not an availability dependency — it must never be the reason a legitimate
request fails.

The operational consequence: *silence does not mean it is working.* A service that is
failing open looks identical, from the outside, to one with no abusive traffic. Alert
on `forwardlimit_store_errors_total`, not just on blocks.

**Fail-open has a boundary worth understanding.** It works by answering `200`:
ForwardAuth grants access only on a 2xx and passes anything else to the client, so the
service can fail open only while it is still able to reply. Redis outages, timeouts,
breaker trips and panics all resolve to a 200. A process that is **dead or wedged**
cannot, and Traefik then returns 5xx — which is precisely why this runs as a sidecar
with a readiness probe, so an unhealthy pod leaves the load balancer rather than
serving errors. See [deployment.md](deployment.md).

If you see 5xx on a limited route it is the service being unreachable, never a limiter
decision. Check `forwardlimit_decisions_total{limiter="panic"}` for the
recovered-defect case.

## Health checks

`GET /healthz` reports process health only. It deliberately **does not check Redis**.

This is not laziness. When the service runs as a sidecar in the Traefik pods, a
Kubernetes pod is Ready only when *every* container is Ready. If `/healthz` reported
unhealthy during a Redis outage, every Traefik pod would be pulled from the load
balancer and the whole edge would go down — turning a degraded abuse control into a
total outage. Redis health is reported through metrics instead.

Do not "improve" this by adding a Redis check.

## Metrics and alerting

| Metric | Meaning |
|---|---|
| `forwardlimit_decisions_total{limiter,decision}` | `allowed` / `blocked` / `would_block` / `failed_open` |
| `forwardlimit_store_errors_total` | requests allowed **without being counted** |
| `forwardlimit_store_breaker_open` | 1 while the circuit is open |
| `forwardlimit_block_cache_hits` | decisions served without touching Redis |
| `forwardlimit_check_duration_seconds` | check latency |

The namespace is `METRICS_NAMESPACE`, `forwardlimit` by default; adjust the rules below
if you change it.

```promql
# Limiting is degraded - requests are passing uncounted.
rate(forwardlimit_store_errors_total[5m]) > 0

# The store is down and the breaker has given up on it.
max_over_time(forwardlimit_store_breaker_open[5m]) == 1

# Nothing is being ENFORCED. Deliberately excludes would_block, so a limiter left
# in dry run still trips this alert rather than looking healthy.
sum(rate(forwardlimit_decisions_total{decision="blocked"}[30m])) == 0

# Projected impact while rolling out a limiter in dry run.
sum by (limiter) (rate(forwardlimit_decisions_total{decision="would_block"}[5m]))

# Abuse in progress.
sum(rate(forwardlimit_decisions_total{decision="blocked"}[5m])) > <baseline>

# Added latency on the hot path.
histogram_quantile(0.99, rate(forwardlimit_check_duration_seconds_bucket[5m])) > 0.05
```

The third rule is the important and easily-forgotten one: it catches the case where the
service is running happily and limiting nothing.

`forwardlimit_decisions_total{limiter="none"}` is also worth watching. It counts
requests that reached `/check` and matched no limiter at all — expected if the
middleware is attached more broadly than your limiters' `paths`, and a red flag if you
thought everything was covered.

## Rolling out a new limiter

1. Add it with `dryRun: true`. Nothing is rejected.
2. Watch `forwardlimit_decisions_total{decision="would_block"}` for that limiter. That
   is the number of requests enforcement *would* have refused.
3. Compare against total traffic. If the projected block rate is higher than expected,
   the threshold is wrong — not the limiter.
4. Remove `dryRun` and restart.

Counters are shared, so nothing resets when you flip the switch: buckets already over
their limit begin rejecting immediately. That is intended, but it means the change
takes effect faster than a fresh deployment would suggest.

A limiter in dry run cannot affect any other, so it is safe to add one to a running
system without reviewing the order of the rest.

## Startup warnings

The service logs a warning for each configuration that is valid but will not limit
anything. Any of these in the log means limiting is partly or wholly inert:

- `limiter "<name>" needs hashing but HASH_SECRET is empty` — that limiter is not
  registered at all
- `REDIS_ENABLED=false` — everything is allowed
- `no limiters are active` — everything is allowed
- `store unreachable at startup` — failing open until it recovers
- `BLOCK_CACHE_MAX_TTL is large` — unblocks will be slow to propagate
- `REDIS_TLS_SKIP_VERIFY=true` — the store certificate is not verified

The startup line lists the dry-run limiters:

```json
{"msg":"starting","limiters":["tenant","login","ip"],"dry_run":["ip"]}
```

From the outside, dry run is indistinguishable from being protected. Treat that field
as an open action rather than background noise.

## Failure modes

### Redis unreachable

Symptoms: `forwardlimit_store_errors_total` climbing,
`forwardlimit_store_breaker_open` at 1, `failing open` in the logs.

Behaviour: all requests allowed. The first few cost roughly the dial timeout (~0.84 s
measured with the defaults), then the breaker opens and subsequent requests cost ~1 ms.
Buckets already known to be blocked continue to be blocked from the local cache until
their cached TTL expires.

Action: fix Redis. Nothing to do here — recovery is automatic, with one probe admitted
per cooldown.

### Latency spike on the protected route

Check `forwardlimit_check_duration_seconds` and Redis latency. If the breaker is
flapping — opening and closing repeatedly — Redis is likely degraded rather than down;
consider lowering `STORE_BREAKER_THRESHOLD` so the service gives up sooner and stops
adding dial latency.

Remember that body buffering for `forwardBody` is Traefik-side work and will not appear
in this service's own timing.

### One caller is being limited by a capacity limiter

Symptoms: `forwardlimit_decisions_total{limiter="tenant",decision="blocked"}` rising
for one credential, and that caller receiving the rejection with `Retry-After`.

This is the limiter working: that caller is over its quota and is being shed so the
rest of the platform is unaffected. Decide whether the quota is right, not whether the
limiter should be off.

- **Their traffic is legitimate and the quota is too low** — raise `rate`, and `burst`
  with it. Requires a restart.
- **Their traffic is a flood they are not filtering** — the durable fix is on their
  side (a queue or waiting room in front of their entry point). The limiter is a shield
  for you, not a solution for them.
- **Everyone is being limited at once** — the quota is per credential, so this means
  many callers are over. Check whether the quota was set too low globally before
  assuming an attack.

The burst allowance means a caller can briefly exceed the sustained rate; short spikes
above `rate` getting through is by design.

### A legitimate caller is blocked

1. Confirm it is this service. A rejection from here matches your configured
   `response` exactly, and carries `Retry-After` only if that limiter sets
   `retryAfter`. The application may have its own limiter producing something similar,
   so establish which layer rejected the request first.
2. To clear a block, delete the block key. Keys are
   `<REDIS_KEY_PREFIX><limiter>:{<bucket>}:b`. If the limiter hashes, you **cannot**
   construct the bucket from the original value — that is deliberate. Find it from the
   truncated hash prefix in the log line for the block event:

   ```sh
   redis-cli --scan --pattern 'forwardlimit:login:{a1b2c3d4*'
   ```
3. **The deletion is not effective immediately on every replica.** Each holds a local
   cache of known blocks for up to `BLOCK_CACHE_MAX_TTL` (default 60 s). Wait that
   long, or restart the pods.
4. To relieve pressure quickly, raise the limiter's `limit` or set `dryRun: true`, and
   restart. Configuration is read only at startup.

### Suspected bypass

- Body-keyed limits do not apply when the body cannot be parsed or exceeds
  `MAX_BODY_BYTES`. Check
  `forwardlimit_decisions_total{limiter="body",decision="failed_open"}` — a rising
  count means callers are sending bodies too large to inspect, which may be an evasion
  attempt. `MAX_BODY_BYTES` must stay above the application's own limit; see
  [configuration.md](configuration.md).
- Address-keyed limits are only meaningful if the origin cannot be reached directly. If
  the load balancer accepts traffic from outside your CDN, `CF-Connecting-IP` is
  attacker-controlled and the limit is bypassable.
- A limiter only applies to its `paths`. Confirm the route actually has `ForwardAuth`
  attached in Traefik — a route without it is not limited at all.
- High-cardinality inputs defeat a limiter keyed on them: an attacker with an unlimited
  supply of fresh values never repeats a bucket. Nothing in the arithmetic can fix that;
  the answer is to key on something the attacker cannot vary freely, such as the
  credential or the network prefix.

## Changing limits

All configuration is read at startup; there is no runtime reload. Changing a limit
means a rolling restart.

Counters are **not** reset by a restart — they live in Redis. Local block caches are
lost, so a restart is one way to make a manual unblock take effect everywhere at once.

Changing `HASH_SECRET` changes every hashed bucket, which effectively resets those
counters. Rotate it deliberately.

## Capacity

Measured on two replicas:

| Scenario | Throughput | p50 | p99 |
|---|---|---|---|
| Worst case: a unique key per request, every request reaching Redis | 5,415 req/s | 6.95 ms | 49 ms |
| Repeated keys, absorbed by the block cache | 20,515 req/s | 7.97 ms | 56 ms |
| At ~3,000 req/s, every request reaching Redis | — | 2.99 ms | 8.06 ms |

Added cost of limiting at realistic load: roughly **+1.2 ms p50, +4.4 ms p99**.

`REDIS_POOL_SIZE` is per replica. With many replicas, multiply before sizing the
store's connection limit; idle connections cost nothing useful.

## Replacing an existing limiter

Enable this one **before** disabling the application's own, so there is never a window
with neither active. Both fail open, so running both briefly is safe — the only effect
is that a caller may be counted twice, which makes the effective limit stricter, not
looser.

Verify `forwardlimit_decisions_total{decision="blocked"}` is actually incrementing
before turning the application's limiter off.
