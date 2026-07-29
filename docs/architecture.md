# Architecture

forwardlimit is a **Traefik `ForwardAuth` endpoint**: Traefik asks it whether each
request on a protected route may proceed, and returns its response to the client
verbatim on any non-2xx. See [traefik.md](traefik.md) for the integration contract.

## Shape

```
cmd/forwardlimit          wiring, signals, graceful shutdown
internal/config           YAML limiter file, environment parsing, fail-fast validation
internal/httpapi          /check, /healthz, /metrics
internal/limiter          Request, Keyer, Limiter, Engine, Decision
internal/limiter/keyer    body, header, query, first, composite key extractors
internal/store            Rule, Bucket, Result, Store interface
internal/store/redisstore go-redis plus the two Lua scripts
internal/store/blockcache bounded cache of known-blocked buckets (Store decorator)
internal/store/breaker    circuit breaker (Store decorator)
internal/observability    slog logger, Prometheus registry
```

## The central idea: separate *what to key on* from *how to enforce*

The engine knows nothing about what is being limited. It walks a list of limiters,
asks each for a bucket, and applies a rule through a `Store`:

```go
type Keyer interface {
    // ok=false means "this limiter does not apply" - not an error.
    Key(*Request) (string, bool)
}

type Limiter struct {
    Name   string        // namespaces the bucket and labels metrics
    Rule   store.Rule    // limit, window, block   (fixed window)
    Bucket store.Bucket  // rate, burst            (token bucket)
    Keyer  Keyer
    Paths  []string      // empty = all paths
    DryRun bool
}
```

Three sources and two combinators cover everything so far:

| Keyer | Keys on |
|---|---|
| `BodyField` | a JSON body field, by dotted path |
| `Header` | first non-empty of an ordered header list |
| `QueryParam` | first non-empty of an ordered parameter list |
| `First` | the first child that applies |
| `Composite` | every child, joined; if any part is missing, the limiter does not apply |

Consequences worth noting:

- Adding a limiter is a YAML block. The engine, store, cache, breaker, metrics and
  handler are untouched. `internal/config` builds a `Keyer` tree from the file.
- The JSON body is parsed **once per request**, lazily, and shared, so a second
  body-keyed limiter costs no extra parsing. The query string likewise.
- `First` exists because credentials arrive by more than one route, and a limiter that
  knows only one leaves the others unlimited.

## Evaluation

```
for each limiter:
    skip if the path is out of scope
    skip if the rule is invalid          (a zero limit/window/block disables it)
    skip if the keyer does not apply     (e.g. the body field is absent)
    consume from the store:
        error   -> record failed_open, CONTINUE to the next limiter
        blocked -> record blocked, STOP, return the configured rejection
        allowed -> record allowed, continue
```

**First block wins**, so a blocked request costs at most one store call per preceding
limiter. **A failing limiter does not stop the others**: each fails open
independently, so one degraded rule does not disable the rest.

Panics in a keyer or a store are recovered and treated as fail-open, so a bug in one
limiter cannot take down the endpoint.

## The store algorithms

Two, because the use cases differ materially. A limiter declares which it uses by
setting either `Rule` or `Bucket`; the engine dispatches on that.

| | `Consume` — fixed window + block | `Take` — token bucket |
|---|---|---|
| For | abuse controls | capacity protection |
| Over the limit | blocked for a punitive period | shed until tokens accrue |
| Recovery | steps, at the end of the block | continuous |
| Boundary burst | possible at the window edge | none |
| Burst allowance | implicit and accidental | explicit (`burst`) |
| `Retry-After` | usually withheld | usually provided |
| Block cache | absorbs repeats | not applicable — every request must take a token |

### Fixed window with a hard block

One Redis round trip per decision, via a Lua script:

```lua
if TTL(block) > 0 then return blocked end   -- already blocked
count = INCR(counter)
if count == 1 then EXPIRE(counter, window) end
if count > limit then
  SET(block, 1, EX=blockSeconds)            -- start the hard block
  DEL(counter)                              -- and reset the window with it
  return blocked
end
return allowed
```

Three properties matter:

**The block outlives the counting window.** Exceeding the limit sets a separate block
key with its own TTL, so a caller is not released when the window rolls over. Neither
Traefik's token bucket nor a typical proxy rate limiter can express this — a
significant part of why this is a service rather than proxy configuration.

**`block` means exactly `block`.** The counter is dropped when the block is set. Skip
that and a counter outliving the block is still over the limit when the block lifts, so
the next request re-blocks immediately and the effective duration silently becomes the
rest of the window — which is not what the operator configured. A regression test
covers it.

**Hammering does not extend a block.** Once blocked, the script returns before the
`INCR`, so continued probing neither counts nor refreshes the TTL. A blocked caller is
released after `block` regardless of how hard it tried in between.

**Keys share a cluster hash tag.**

```
forwardlimit:login:{<bucket>}:c    counter
forwardlimit:login:{<bucket>}:b    block marker
```

The `{...}` tag forces both keys into the same Redis Cluster slot. Without it a
multi-key script is rejected with `CROSSSLOT` on a sharded cluster. Sub-second
durations round *up* to one second, because `EXPIRE 0` in Redis means "no expiry" and
would leak keys.

### Token bucket

Also one round trip. State is a hash of `tokens` plus a last-update timestamp; each
call refills by the elapsed time, caps at `burst`, then takes a token or computes the
wait:

```lua
now    = redis TIME                       -- one clock for all replicas
tokens = min(burst, tokens + elapsed * rate)
if tokens >= 1 then tokens = tokens - 1; allow
else retry_after = (1 - tokens) / rate; shed
```

The clock is read **inside** the script from Redis rather than passed in by the
caller, so replica clock skew cannot distort the refill. Idle buckets are given a TTL
of the time they would take to refill completely, so a large key space of inactive
keys does not accumulate.

Only one key is involved, so there is no CROSSSLOT concern; the hash tag is kept for a
consistent key shape across limiters.

## Store decorators

The chain, outermost first:

```
engine -> blockcache -> breaker -> redisstore
```

Both layers are `Store` implementations rather than special cases inside the engine,
which keeps each independently testable and makes the order explicit.

### Block cache

When the same buckets are retried repeatedly — the normal shape of an automated attack
— a load test measured this absorbing **97.5%** of requests without touching Redis. It
is essential to the performance profile, not an optimisation.

**Why it is safe:**

- Only **blocked** verdicts are cached — never counts, never "allowed". Counting
  always reaches the authoritative store, so the cache cannot make a request go
  uncounted.
- It can therefore only ever produce a rejection, never an incorrect allow. For an
  abuse control that is the safe direction to fail.
- A block with no known TTL is not cached at all.
- Entries are capped at `BLOCK_CACHE_MAX_TTL`, not the full block duration, so a
  manual unblock propagates within the cap instead of being pinned in every replica's
  memory for the whole block.
- The cache is **bounded**: at capacity, new blocks are simply not cached rather than
  evicting arbitrarily, so a high-cardinality attack cannot grow memory without limit.

The cache gives **no benefit against genuinely fresh values** — an attack using a new
value each attempt reaches Redis every time. Size for that case.

### Circuit breaker

After `STORE_BREAKER_THRESHOLD` consecutive failures the breaker opens and returns an
error immediately without dialling. Since callers treat any error as fail-open, the
effect is *fast* fail-open for the duration of an outage. After the cooldown a single
probe is admitted to detect recovery.

Without it, an outage costs a connection attempt per request. Measured: ~0.84 s per
request while dialling versus ~1 ms once the breaker opens.

## Dry run

A limiter with `dryRun` set is evaluated in full — keyer, store call, counter
increment, block state — and only the final enforcement step is suppressed. That
fidelity is the point: the projection has to match what enforcement would do.

Two consequences follow, both deliberate:

- The verdict is recorded as `would_block`, never `blocked`, so metrics can always
  distinguish "protected" from "measuring".
- Evaluation **continues** past a dry-run block, even though it stops at an enforced
  one. A limiter in dry run must not change what any other limiter does; otherwise
  adding one for measurement would silently disable enforcement behind it, defeating
  the purpose. The cost is that later limiters' projections read slightly high, which
  is a far smaller price than losing protection by accident.

`Decision.Blocked` is the verdict; `Decision.Rejected()` is whether it is acted on.
Only the latter may be used to build a response.

## Fail-open paths

Every one of these allows the request:

| Condition | Recorded as |
|---|---|
| Body field absent, or not a usable value | limiter does not apply |
| Nothing left after normalisation | limiter does not apply |
| `HASH_SECRET` unset, with `hash: true` | limiter not registered (startup warning) |
| `REDIS_ENABLED=false` | no-op store |
| Invalid rule (zero limit/window/block) | limiter skipped |
| Store unreachable, timing out, or erroring | `failed_open` |
| Circuit breaker open | `failed_open` |
| Body larger than `MAX_BODY_BYTES` | `failed_open`, limiter `body` |
| Panic in a keyer or store | `failed_open` |
| Panic anywhere else in the check handler | `failed_open`, limiter `panic` |

### The limit of fail-open

Fail-open is a property of the **response code**, not of the process. ForwardAuth
grants access only on a 2xx and returns anything else to the client verbatim, so "fail
open" here means *this service must answer 200*.

That is why a panic must be recovered into a 200 rather than left to the HTTP server:
an unhandled panic writes no response, Traefik treats that as a failure, and the
client gets a 5xx. A defect in an abuse control would then refuse a legitimate
request. `recoverAndAllow` in `internal/httpapi` closes that, and a test asserts it
holds through the real mux.

Two failure modes remain outside the service's control, and both fail **closed**:

- **The process is gone or unreachable.** Traefik returns 5xx. This is what the
  sidecar deployment and the readiness gate exist to bound: a pod whose sidecar is
  unhealthy leaves the load balancer instead of serving errors.
- **The process is wedged** — deadlocked or out of memory — so it answers neither 200
  nor anything else. The readiness probe is the only thing that catches this.

There is no way to make those fail open from inside the service; the mitigation is
deployment shape, not code. See [deployment.md](deployment.md).

## Handling sensitive values

A bucket becomes a storage key and appears in logs, so anything sensitive must be
hashed before it becomes one.

- With `hash: true`, the value is HMAC-SHA256 hashed. The raw value is never a key.
  HMAC rather than a bare digest, because a low-entropy value would otherwise be
  recoverable by brute force.
- Logs carry at most a **truncated** hash prefix — enough to correlate lines, not
  enough to be a lookup key. A full hash is never logged.
- Request bodies are never logged at any level.
- The rejection response carries no bucket, no hash and no limiter name. It is
  returned to the client verbatim, so anything in it is public. A test asserts the
  response contains neither the keyed value, nor a prefix of it, nor a suffix.
- Without `HASH_SECRET`, a limiter that asked to hash is **not registered at all**,
  rather than falling back to the raw value. That fails open, loudly.

## Testing

- `storetest.Fake` reproduces the real window-and-block and token-bucket semantics in
  memory, so engine tests are meaningful rather than trivially satisfied.
- `miniredis` covers the Lua scripts, TTLs and key naming, including time travel to
  prove a block outlives its window.
- `internal/httpapi/stack_test.go` wires the real components in the same order as
  `main` and exercises the documented request sequences, plus outage behaviour.
- A golden file locks the default rejection body, and separate tests prove a configured
  response and a per-limiter override are returned verbatim.
- Every file in `examples/config/` is parsed by a test, so a documentation example that
  stops loading fails the build.
- `e2e/` asserts the documented behaviour through a real Traefik against the Compose
  example: the quickstart sequence, that a normaliser collapses equivalent values,
  that a block outlives its counting window, that dry run projects without rejecting,
  that a store outage fails open, and that an unreachable service fails closed. `make
  e2e` runs it, which also builds the `Dockerfile` on every change.
- Cluster slot routing cannot be covered in-process — `miniredis` does not emulate it —
  so `TestIntegrationRealRedis` is opt-in against a real cluster.
