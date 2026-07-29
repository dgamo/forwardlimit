# Traefik integration

forwardlimit is called by **Traefik's `ForwardAuth` middleware**. That is its only
supported integration: it is not a Traefik plugin, and it is not a library. Traefik
asks whether a request may proceed, and it answers with a status code.

## How ForwardAuth works here

`ForwardAuth` is normally used for authentication. The mechanism is what we want, so
it is used for rate limiting instead:

1. A request arrives at Traefik on a route with the middleware attached.
2. Traefik pauses it and issues a request to `/check`, passing the original
   request's headers and — when `forwardBody` is set — its body.
3. **2xx** → Traefik forwards the original request upstream unchanged.
4. **Anything else** → Traefik returns *our* response to the client **verbatim**:
   status, headers and body.

Step 4 is why the service can define the exact response a limited client receives,
and equally why that response must contain nothing diagnostic — it is public.

## Middleware definition

Kubernetes (`traefik.io/v1alpha1`):

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: forwardlimit
spec:
  forwardAuth:
    address: http://127.0.0.1:8080/check
    forwardBody: true
    maxBodySize: 262144
    preserveRequestMethod: true  # Traefik 3.4+
    authRequestHeaders:
      - Content-Type
      - CF-Connecting-IP
      - X-Api-Key
```

Attach it to a route with the usual annotation:

```yaml
metadata:
  annotations:
    traefik.ingress.kubernetes.io/router.middlewares: <namespace>-forwardlimit@kubernetescrd
```

File provider equivalent, for standalone Traefik:

```yaml
http:
  middlewares:
    forwardlimit:
      forwardAuth:
        address: "http://forwardlimit:8080/check"
        forwardBody: true
        maxBodySize: 262144
        preserveRequestMethod: true  # Traefik 3.4+
  routers:
    api:
      rule: "PathPrefix(`/v1/login`)"
      middlewares: ["forwardlimit"]
      service: app
```

A complete, runnable version of the second form is in
[`examples/docker-compose/`](../examples/docker-compose).

> **One unknown field rejects the whole file.** Traefik's file provider does not skip
> a field it does not recognise - it abandons the entire dynamic configuration, so
> *every* route 404s, not just the one you were editing. `preserveRequestMethod` is
> the likely culprit here, since it needs Traefik 3.4+:
>
> ```
> ERR Error while building configuration (for the first time)
>     error="/etc/traefik/dynamic/dynamic.yaml: field not found, node: preserveRequestMethod"
> ```
>
> Drop the field on an older Traefik. And whenever a whole entrypoint starts
> returning 404, read the proxy's own logs before suspecting anything else.

## Each setting, and why

| Setting | Why it matters |
|---|---|
| `address` | `127.0.0.1` for a sidecar; the Service DNS name for a standalone Deployment. See [deployment.md](deployment.md). |
| `forwardBody: true` | **Required for any body-keyed limiter.** Without it the request body never reaches the service, so a `body:` key silently never applies. |
| `maxBodySize` | Traefik's default is unlimited. Set it, and set it **above** the body limit your application enforces — exceeding it makes Traefik return **401**, and you want oversized bodies to reach the application and get its own rejection instead. |
| `preserveRequestMethod: true` | **Traefik 3.4+ only.** Without it Traefik issues a `GET`. The service handles either, so this is optional - but on an older Traefik the field is not merely ignored, it is rejected. See the warning below. |
| `authRequestHeaders` | Optional allowlist. If set, it must include **every header a limiter keys on** — omitting one silently disables that limiter. |
| `trustForwardHeader` | Leave it off unless you understand the consequence: it makes Traefik trust client-supplied `X-Forwarded-*`, which lets a caller choose the path a path-scoped limiter sees. |

## Attach it only where it is needed

`forwardBody: true` makes Traefik buffer the whole request body before calling the
service. Attach the middleware to the specific routes that need limiting, never
globally, and never to file-upload routes.

Each limiter can also declare `paths`, as defence in depth, so a limiter cannot fire
on an unintended route even if the middleware is attached more broadly than intended.

## How the original request is identified

Traefik describes the original request with `X-Forwarded-*` headers. The service
reads:

| Header | Used for |
|---|---|
| `X-Forwarded-Uri` | the original path, for `paths` matching (the query string is stripped from the path but remains available to `query:` keys) |
| `X-Forwarded-Method` | the original method |

The URL of the `/check` call itself is *not* the request being authorised. If these
headers are absent the service falls back to the received URL and method, which is
what makes `curl` against `/check` work for local testing:

```sh
curl -i -XPOST localhost:8080/check \
  -H 'Content-Type: application/json' \
  -H 'X-Forwarded-Uri: /v1/login' \
  -d '{"email":"user@example.com"}'
```

## Why it fails closed, and what to do about it

`ForwardAuth` **fails closed**: if the service is unreachable, Traefik returns **500**
to the client. This is measured behaviour, not an assumption, and it is the one
property that shapes the whole deployment story.

The service itself fails *open* — every internal error path answers `200` — but that
only holds while it can still reply. A dead or wedged process cannot fail open.

Running it as a container in the Traefik pod means "the service is unreachable" cannot
happen independently of "this Traefik pod is already broken". Combined with a
readiness probe — a Kubernetes pod is Ready only when every container is Ready — an
unhealthy sidecar removes that pod from the load balancer rather than serving 500s.

Prefer a **native sidecar** (an `initContainer` with `restartPolicy: Always`) over an
ordinary extra container, so it starts before Traefik and terminates after it. Traefik
drains connections on shutdown and keeps serving during that window; if the sidecar
were stopped first, the drain period would turn into a burst of 500s.

[deployment.md](deployment.md) covers all three shapes and their trade-offs.

## Troubleshooting

**Every route returns 404, including ones you did not touch.**
The dynamic configuration failed to parse and Traefik loaded none of it. Check its
logs for `field not found` - one unrecognised field discards the whole file. With the
Kubernetes CRD the same mistake shows up as a rejected `Middleware` object instead.

**Nothing is being limited on a route.**
Check, in order: the middleware is attached to that route; `forwardBody: true` is set
if the limiter keys on the body; the path matches the limiter's `paths`; the limiter
is not disabled by a missing `HASH_SECRET`. The metric
`forwardlimit_decisions_total{limiter="none"}` incrementing means requests are
arriving but no limiter applied to them.

**Clients receive 500 on a limited route.**
The service is unreachable from Traefik — it cannot be a limiter decision, because
every decision path answers 200 or the configured rejection, and even a panic is
recovered into a 200. Check the container is running and its readiness probe passes.

**Clients receive 401 on a limited route.**
The body exceeded `maxBodySize`. Raise it above the application's own body limit.

**A header-keyed limiter never fires.**
Either `authRequestHeaders` is set and omits the header, or the header is absent. Note
also that a client-suppliable header is only trustworthy if the origin cannot be
reached directly — see [configuration.md](configuration.md).

**Requests are slower than expected.**
Compare `forwardlimit_check_duration_seconds` against the added latency you see at the
edge. Body buffering for `forwardBody` is Traefik-side work and will not appear in the
service's own timing.
