# Security policy

forwardlimit sits on the request path and decides whether traffic is allowed, so a
defect here can either refuse legitimate requests or let abusive ones through. Reports
are taken seriously.

## Reporting a vulnerability

**Please do not open a public issue for a vulnerability.**

Use GitHub's private reporting: **[Report a
vulnerability](https://github.com/dgamo/forwardlimit/security/advisories/new)**. It
opens a private advisory visible only to the maintainers.

Useful things to include:

- What an attacker can achieve — bypass a limit, cause a denial of service, extract a
  value that should have stayed hidden.
- A configuration and request sequence that reproduces it.
- The version or commit.

What to expect:

| | |
|---|---|
| Acknowledgement | within 5 days |
| Assessment | within 14 days |
| Fix for a confirmed issue | as a patch release, coordinated with you |
| Credit | in the advisory and `CHANGELOG.md`, unless you prefer otherwise |

This is a personal project maintained in spare time. Those are honest intentions, not
a contractual SLA.

## Supported versions

The latest minor release receives security fixes. There is no long-term support
branch.

## Scope

**In scope**

- Bypassing a configured limit — a request that should have been rejected but was not.
- Refusing traffic that should have been allowed, where the cause is inside this
  service.
- Leaking a hashed or sensitive key value through logs, metrics or the rejection
  response.
- Denial of service against the service itself: unbounded memory growth, a panic that
  is not recovered, a request that wedges the process.
- Anything that lets a caller influence another caller's bucket.

**Out of scope**

- **Deliberate fail-open behaviour.** A store outage, a missing `HASH_SECRET`, an
  unparseable body and a recovered panic all allow the request. That is documented,
  intentional, and the whole design premise: rate limiting must not be the reason a
  legitimate request fails. See
  [docs/architecture.md](docs/architecture.md#fail-open-paths).
- **Trusting a client-suppliable header.** A limiter keyed on `CF-Connecting-IP` is
  bypassable if your origin is reachable outside your CDN. That is a deployment
  property, documented in [docs/configuration.md](docs/configuration.md).
- **High-cardinality evasion.** A limiter keyed on a value the attacker can vary
  freely is defeated by varying it. No arithmetic fixes that; key on something they
  cannot vary.
- **ForwardAuth failing closed** when the service is unreachable. That is Traefik's
  behaviour, which is why the sidecar deployment exists; see
  [docs/deployment.md](docs/deployment.md).
- Findings that require an already-compromised host, or write access to the
  configuration file or `HASH_SECRET`.

If you are unsure whether something is in scope, report it privately and let's find
out together.

## Notes for operators

A few properties worth knowing when you deploy it:

- **`HASH_SECRET` is a secret.** It keys the HMAC that keeps sensitive values out of
  storage keys and logs. Deliver it as a `Secret`, never in the config file, and use at
  least 32 random bytes. Rotating it invalidates every hashed bucket.
- **The rejection response is public.** ForwardAuth returns it to the client verbatim,
  so anything you put in it is disclosed. The default deliberately says almost nothing.
- **Bind to localhost when running as a sidecar.** `/check` is unauthenticated by
  design — it is called by the proxy in the same pod. Nothing else should be able to
  reach it.
- **`MAX_BODY_BYTES` must exceed your application's own body limit**, or a caller can
  pad a body past this limit to evade body-keyed limits while the application still
  accepts the request. This is the one configuration mistake with a genuine bypass
  behind it.
