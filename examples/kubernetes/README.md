# Kubernetes examples

Two shapes. The choice between them comes down to one property of the integration:

> **`ForwardAuth` fails closed.** If forwardlimit is unreachable, Traefik returns 5xx
> and the request is refused.

forwardlimit fails *open* internally — every error path answers `200` — but only while
the process can still reply. So the question each shape answers differently is: *how
do I make "unreachable" impossible, or at least bounded?*

## [`sidecar/`](sidecar) — recommended

forwardlimit runs as a native sidecar in the Traefik pods, reached on `127.0.0.1`.

"Unreachable" cannot happen independently of "this Traefik pod is already broken", and
a Kubernetes pod is Ready only when every container is Ready — so an unhealthy sidecar
removes that pod from the load balancer rather than letting it serve 500s.

```sh
helm repo add traefik https://traefik.github.io/charts
kubectl apply -f sidecar/manifests.yaml          # limiters, secret, middleware, ingress
helm upgrade --install traefik traefik/traefik \
  -n traefik --create-namespace -f sidecar/traefik-values.yaml
```

Costs: forwardlimit is upgraded and scaled with the proxy, not independently, and
adding a sidecar changes the pod's resource requests — which an HPA using
`averageUtilization` is measuring. Both are covered in `traefik-values.yaml`.

Needs Kubernetes 1.29+ for native sidecars (1.28 with the `SidecarContainers` gate).

## [`standalone/`](standalone) — when a sidecar is not possible

Its own Deployment and Service, reached over the network.

Simpler to operate — independent upgrades, independent scaling — at the cost of a real
additional dependency on the request path. Use it when you cannot modify the Traefik
pod spec, for instance with a managed ingress controller.

```sh
kubectl apply -f standalone/manifests.yaml
```

The manifest is built around making that dependency survivable: three replicas spread
across nodes, `maxUnavailable: 0` on updates, a PodDisruptionBudget so a node drain
cannot take the last replica, a readiness gate, and a NetworkPolicy restricting who may
call `/check`.

## Both

**Replace the secret.** Both manifests ship `hash-secret: "replace-me-..."`. Generate a
real one:

```sh
kubectl -n traefik create secret generic forwardlimit \
  --from-literal=hash-secret="$(openssl rand -base64 32)"
```

Without it, every limiter using `hash: true` is **disabled** and says so at startup.
Rotating it changes every hashed bucket, so those counters effectively reset.

**Point `REDIS_ADDRS` at your Redis.** Both examples assume
`redis-master.redis.svc.cluster.local:6379`. Cluster mode and TLS are commented in
place. Sharing a Redis with other workloads is fine — `REDIS_KEY_PREFIX` namespaces
every key.

**Changing limiters needs a restart.** There is no runtime reload:

```sh
kubectl -n traefik rollout restart deploy/traefik           # sidecar
kubectl -n forwardlimit rollout restart deploy/forwardlimit # standalone
```

Counters live in Redis and survive it. Local block caches do not, which is the fastest
way to make a manual unblock take effect everywhere at once.

**Validate before applying.** The binary exits non-zero on an invalid configuration, so
this is a usable pre-flight check:

```sh
docker run --rm -v "$PWD/config.yaml:/c.yaml" \
  ghcr.io/dgamo/forwardlimit:v0.1.0 -config /c.yaml
```

**Start in dry run.** Both examples put the newest limiter in `dryRun: true`. Watch
`forwardlimit_decisions_total{decision="would_block"}` before enforcing anything; see
[../../docs/operations.md](../../docs/operations.md).

Full discussion of every trade-off: [../../docs/deployment.md](../../docs/deployment.md).
