# Deployment

Three shapes, each with a complete runnable example. The choice between the last two
matters, and the reason is one property of the integration:

> **`ForwardAuth` fails closed.** If forwardlimit is unreachable, Traefik returns 5xx
> and the request is refused.

forwardlimit fails *open* internally — every error path answers `200` — but that only
holds while the process can still reply. So the question every deployment has to
answer is: *how do I make "unreachable" impossible, or at least bounded?*

| Mode | Example | Request-path dependency | Recommended for |
|---|---|---|---|
| A. Standalone Traefik (Compose) | [`examples/docker-compose/`](../examples/docker-compose) | none beyond the host | trying it out; single-host deployments |
| B. Kubernetes sidecar | [`examples/kubernetes/sidecar/`](../examples/kubernetes/sidecar) | none — shares the proxy's fate | **production** |
| C. Kubernetes Deployment | [`examples/kubernetes/standalone/`](../examples/kubernetes/standalone) | one extra hop | when a sidecar is not possible |

---

## A. Standalone Traefik, via Docker Compose

The two-minute path, and the first thing to reach for when evaluating.

```sh
cd examples/docker-compose
docker compose up -d

for i in 1 2 3 4; do
  curl -s -o /dev/null -w "$i -> %{http_code}\n" \
    -XPOST localhost:8000/v1/login \
    -H 'Content-Type: application/json' \
    -d '{"email":"user@example.com"}'
done
```

The stack is Traefik (file provider), forwardlimit, Redis and a `whoami` backend. The
middleware lives in `traefik/dynamic.yaml`; the limiters in `forwardlimit.yaml`.

For a real single-host deployment, three changes:

- Put `HASH_SECRET` somewhere other than the compose file.
- Point `REDIS_ADDRS` at a Redis with persistence configured, not the throwaway one.
- Do not publish forwardlimit's port. Traefik reaches it on the compose network;
  nothing else should reach it at all.

---

## B. Kubernetes, sidecar in the Traefik pods — recommended

forwardlimit runs as a **native sidecar** in the Traefik pods: an `initContainer`
with `restartPolicy: Always`.

```yaml
# values.yaml for the official Traefik chart
deployment:
  initContainers:
    - name: forwardlimit
      image: ghcr.io/dgamo/forwardlimit:v0.1.0
      restartPolicy: Always          # this is what makes it a sidecar
      args: ["-config", "/etc/forwardlimit/config.yaml"]
      env:
        - name: LISTEN_ADDR
          value: "127.0.0.1:8080"
        - name: REDIS_ADDRS
          value: "redis:6379"
        - name: HASH_SECRET
          valueFrom:
            secretKeyRef: {name: forwardlimit, key: hash-secret}
      volumeMounts:
        - name: forwardlimit-config
          mountPath: /etc/forwardlimit
          readOnly: true
      readinessProbe:
        httpGet: {path: /healthz, port: 8080}
      resources:
        requests: {cpu: 100m, memory: 64Mi}
        limits: {memory: 128Mi}
```

Why each part:

**`restartPolicy: Always` on an `initContainer`** is the native sidecar mechanism. It
starts *before* Traefik and terminates *after* it. That ordering matters: Traefik
drains connections on shutdown and keeps serving during that window, so a sidecar
stopped first would turn the drain period into a burst of 500s.

**`LISTEN_ADDR: 127.0.0.1:8080`** — only Traefik in the same pod needs to reach it.
Binding to the pod IP would expose an unauthenticated endpoint to the whole cluster
network.

**A readiness probe** is the safety net. A Kubernetes pod is Ready only when *every*
container is Ready, so an unhealthy sidecar removes that pod from the load balancer
instead of letting it serve 500s. This is the mechanism that bounds fail-closed.

**Resource requests** — but note the HPA consequence below.

The full example includes the `Middleware` CRD, an `Ingress` with the
`router.middlewares` annotation, a `ConfigMap` for the limiters and a `Secret` for
`HASH_SECRET`.

### The HPA consequence

Adding a sidecar changes a pod's total resource requests, so an HPA using
`averageUtilization` is now measuring a different denominator and will scale at the
wrong point. Either re-tune the target, or switch to a `ContainerResource` metric
scoped to the proxy container:

```yaml
metrics:
  - type: ContainerResource
    containerResource:
      name: cpu
      container: traefik          # not the sidecar
      target: {type: Utilization, averageUtilization: 70}
```

---

## C. Kubernetes, its own Deployment

Simpler to operate — forwardlimit is upgraded, scaled and rolled independently of the
proxy — with one real cost.

```yaml
spec:
  forwardAuth:
    address: http://forwardlimit.forwardlimit.svc.cluster.local:8080/check
```

**The trade-off, stated plainly.** forwardlimit is now a genuine additional
dependency on the request path. If the Service has no ready endpoints, Traefik gets a
connection failure and returns 5xx — and unlike the sidecar case, that can happen
while Traefik itself is perfectly healthy.

It is survivable, and the example does what it can:

- **Multiple replicas** across nodes, with anti-affinity.
- **A PodDisruptionBudget**, so a node drain cannot take the last replica.
- **Tight timeouts** at the middleware, so a slow instance degrades rather than
  hangs.
- **A readiness probe**, so a rolling update never routes to a starting pod.

Choose this shape deliberately, not by default. If you cannot modify the Traefik pod
spec — a managed ingress controller, say — it is the right answer.

---

## Delivering configuration

**Limiters** are a `ConfigMap` mounted as a file:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: forwardlimit-config
data:
  config.yaml: |
    limiters:
      - name: ip
        key: {header: [CF-Connecting-IP, X-Forwarded-For], normalise: ipsubnet/24}
        window: {limit: 100, window: 1m, block: 1m}
```

**`HASH_SECRET`** is a `Secret`, never the ConfigMap. It keys the HMAC that protects
sensitive values, so it belongs with the secrets, and changing it changes every
bucket — so rotate it deliberately.

There is **no runtime reload**. A ConfigMap change requires a restart to take effect:

```sh
kubectl rollout restart deploy/traefik      # or deploy/forwardlimit
```

Counters live in Redis and survive the restart. Local block caches do not, which is
in fact the fastest way to make a manual unblock take effect everywhere at once.

Validate a configuration before shipping it. `-validate` parses the file, builds
every keyer, prints what resolved and exits non-zero if it would not start:

```sh
docker run --rm -v "$PWD/config.yaml:/c.yaml:ro" \
  ghcr.io/dgamo/forwardlimit:v0.1.0 -validate -config /c.yaml
# config:  /c.yaml
# valid:   yes
# active:  tenant, login, ip
# dry run: ip
```

Warnings go to stderr and do not fail the check — a limiter disabled by a missing
`HASH_SECRET` is the operator's call, not a syntax error. Read them anyway; each one
means something is not limiting.

## Redis

| Topology | Configuration |
|---|---|
| Single node | `REDIS_ADDRS=redis:6379` |
| Cluster | `REDIS_MODE=cluster`, several addresses in `REDIS_ADDRS` |
| TLS, public CA | `REDIS_TLS_ENABLED=true` |
| TLS, private CA | add `REDIS_TLS_CA_FILE=/certs/ca.crt` — verification stays on |
| Mutual TLS | add `REDIS_TLS_CERT_FILE` and `REDIS_TLS_KEY_FILE` |
| TLS, certificate name mismatch | `REDIS_TLS_SKIP_VERIFY=true` — last resort, warns at startup |

Cluster mode is fully supported: each limiter's keys share a hash tag so a multi-key
script cannot land on two slots. See
[architecture.md](architecture.md#fixed-window-with-a-hard-block).

**Sharing a Redis with other workloads is fine and expected.** Every key carries
`REDIS_KEY_PREFIX` (default `forwardlimit:`), so keys are identifiable and separable.
Give each deployment its own prefix if one store serves several.

**Sizing.** `REDIS_POOL_SIZE` is per replica; multiply by replica count before sizing
the store's connection limit. Memory is modest — two small keys per active bucket for
a window limiter, one hash for a token bucket — and every key has a TTL, so an idle
key space drains itself.

## Probes

| Probe | Endpoint | Notes |
|---|---|---|
| Readiness | `GET /healthz` | Process health **only**. |
| Liveness | `GET /healthz` | Same endpoint; a wedged process is the case it catches. |

`/healthz` deliberately **does not check Redis**. If it did, a Redis outage would
make every Traefik pod unready and take the whole edge down — turning a degraded
abuse control into a total outage. Redis health is reported through metrics instead.
See [operations.md](operations.md#health-checks).

## Rolling out safely

1. Deploy with every limiter `dryRun: true`. Nothing is rejected.
2. Watch `forwardlimit_decisions_total{decision="would_block"}` per limiter. That is
   what enforcement *would* have refused.
3. If the projected rate is higher than expected, the threshold is wrong — not the
   limiter.
4. Turn off `dryRun` for one limiter at a time and restart.

If you are replacing an existing in-application limiter, enable this one **before**
disabling that one, so there is never a window with neither active. Both fail open,
so running both briefly is safe: the only effect is that a caller may be counted
twice, which makes the effective limit stricter, not looser.

## Container image

| Tag | Published on | Meaning |
|---|---|---|
| `vX.Y.Z` | a semver tag | an immutable release — **use this in production** |
| `vX.Y`, `vX` | a semver tag | moved to the newest release in that line; never moved by a prerelease |
| `latest` | every merge to main | the development tip, **not** the newest release |
| `main-<sha>` | every merge to main | one immutable tag per merge, for rollback |

> **`latest` tracks main, not releases.** It is built and tested the same way — the
> pipeline will not move it unless lint, tests, the examples and the end-to-end suite
> all pass — but it is unreleased code, and it can move under you at any time. Pin
> `vX.Y.Z` for anything you care about.

Built `FROM gcr.io/distroless/static:nonroot` — no shell, no package manager,
non-root, `linux/amd64` and `linux/arm64`, from one Dockerfile whichever tag you
pull. Binaries and checksums are attached to each GitHub release.
