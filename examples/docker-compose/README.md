# Standalone Traefik, via Docker Compose

A complete stack: Traefik, forwardlimit, Redis and a `whoami` backend.

```sh
docker compose up -d --build
```

## See it limit

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

The fourth request is rejected by Traefik, on forwardlimit's say-so. It never reaches
the backend — the `whoami` container's logs show three requests, not four.

```sh
curl -s -XPOST localhost:8000/v1/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"user@example.com"}'
# {"error":"rate_limited"}
```

## Things worth trying

**The block outlives the window.** `/v1/quick` exists to show this in seconds rather
than a minute: `window: 5s, block: 20s`.

```sh
q() { curl -s -o /dev/null -w "%{http_code}\n" -XPOST localhost:8000/v1/quick \
  -H 'Content-Type: application/json' -d '{"email":"win@example.com"}'; }
q; q; q          # 200 200 429
sleep 7          # the counting window has now rolled over
q                # still 429 - the block has its own TTL
```

That is the property no proxy configuration can express. `/v1/login` shows the same
thing at `window: 1m, block: 2m` if you would rather wait.

**A different value has its own bucket.**

```sh
curl -s -o /dev/null -w "%{http_code}\n" -XPOST localhost:8000/v1/login \
  -H 'Content-Type: application/json' -d '{"email":"other@example.com"}'
# 200
```

**`Retry-After` on the capacity limiter, and not on the abuse control.**

```sh
for i in $(seq 1 12); do
  curl -s -o /dev/null -D - -XPOST localhost:8000/v1/login \
    -H 'X-Api-Key: demo' -H 'Content-Type: application/json' \
    -d "{\"email\":\"user$i@example.com\"}" | grep -iE 'HTTP/|retry-after'
done
```

The token bucket holds 10 and refills at 5/s, so the eleventh request in a burst is
shed — with `Retry-After`, because a cooperating caller should be told when to come
back. The login limiter deliberately sends none.

**An unlimited route, for comparison.** `/open` reaches the same backend with no
middleware attached. Hammer it and nothing is ever rejected — a useful reminder that a
route without `ForwardAuth` is not limited at all.

**What happens when Redis dies.**

```sh
docker compose stop redis
curl -s -o /dev/null -w "%{http_code}\n" -XPOST localhost:8000/v1/login \
  -H 'Content-Type: application/json' -d '{"email":"user@example.com"}'
# 200 - fails open
```

Requests are allowed, not refused. The first costs the dial timeout; after three
consecutive failures the circuit breaker opens and the rest cost about a millisecond.
`docker compose start redis` and it recovers on its own.

**What happens when forwardlimit dies.**

```sh
docker compose stop forwardlimit
curl -s -o /dev/null -w "%{http_code}\n" -XPOST localhost:8000/v1/login \
  -H 'Content-Type: application/json' -d '{"email":"user@example.com"}'
# 500 - ForwardAuth fails CLOSED
```

This is the asymmetry that shapes every deployment decision: the service fails *open*,
but the integration fails *closed*. It is why production should run it as a sidecar in
the proxy's own pods — see [../../docs/deployment.md](../../docs/deployment.md).

**Metrics.**

```sh
curl -s localhost:8081/metrics | grep '^forwardlimit_decisions'
```

Port 8081 is published to loopback **for this demo only**, so metrics are easy to
read while you explore. In production publish nothing: `/check` is unauthenticated by
design, because only the proxy beside it is meant to call it.

Look for `decision="would_block"` on the `ip` limiter: it is in dry run, so it
evaluates and reports without rejecting anything.

## Files

| File | What it is |
|---|---|
| `compose.yaml` | the stack |
| `traefik/dynamic.yaml` | the `forwardAuth` middleware and the routes it is attached to |
| `forwardlimit.yaml` | the limiters |

Traefik is pinned to **v3.7**. `preserveRequestMethod` in the middleware needs
Traefik 3.4 or newer; on an older version the file provider rejects the whole
configuration and every route 404s. See
[../../docs/traefik.md](../../docs/traefik.md).

## Reset

```sh
docker compose down          # counters are lost with Redis
docker compose up -d --build
```

## Before copying this into production

- Move `HASH_SECRET` out of `compose.yaml`.
- Point `REDIS_ADDRS` at a Redis with persistence and a `maxmemory` policy.
- Raise the thresholds in `forwardlimit.yaml` — they are tuned for a four-line demo,
  and drop the `quick` limiter, which exists only to make one property observable.
- Stop publishing port 8081.
- Drop the Traefik dashboard, or put it behind authentication.
- Pin image digests rather than tags.
