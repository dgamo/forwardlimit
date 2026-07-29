# Contributing

Contributions are welcome. This is a small, focused project, so the fastest route to a
merged change is a short issue first — especially for anything that adds a
configuration surface.

## Getting set up

Go 1.25 or newer, and Docker if you want to run the example stack.

```sh
git clone https://github.com/dgamo/forwardlimit
cd forwardlimit

make tools     # the pinned golangci-lint and kubeconform, once
make ci        # everything CI checks, bar e2e
```

Everything CI does goes through `make`, deliberately: a green local `make ci` must
mean a green pipeline. If the two ever diverge, that is a bug in the Makefile. Tool
versions are pinned there rather than in the workflow for the same reason.

```sh
make test          # unit tests
make test-race     # with the race detector
make cover         # coverage summary
make build         # bin/forwardlimit
make docker        # container image
make validate      # run the binary over every example config
make manifests     # schema-check the Kubernetes examples
```

The end-to-end suite needs Docker. It builds the image, brings up the Compose
example, and asserts the documented behaviour through Traefik — including that a
store outage fails open and an unreachable service fails closed:

```sh
make e2e
```

`./e2e` also runs against a deployment you already have, which makes it a usable
smoke test:

```sh
FORWARDLIMIT_E2E=1 \
FORWARDLIMIT_E2E_URL=https://api.example.com \
FORWARDLIMIT_E2E_METRICS=http://localhost:8080 \
  go test ./e2e/...
```

Integration tests against a real Redis — the only way to cover cluster slot routing —
are opt-in:

```sh
FORWARDLIMIT_INTEGRATION=1 REDIS_ADDRS=localhost:6379 make test-integration
```

To run it by hand:

```sh
docker run -d -p 6379:6379 redis:7
HASH_SECRET=dev-secret make build && \
  ./bin/forwardlimit -config examples/config/simple.yaml
```

## What makes a good pull request

**Tests first, or at least tests.** Every behaviour here is asserted somewhere, and a
rate limiter is exactly the kind of component where an untested edge becomes a
production incident. If you are fixing a bug, a test that fails before your change is
the most useful thing in the diff.

**Keep the fail-open invariant.** Every error path allows the request. If your change
introduces a way for the service to answer anything other than `200` or the configured
rejection, that is a design change and needs discussion first — see
[docs/architecture.md](docs/architecture.md#the-limit-of-fail-open).

**Explain *why* in comments, not *what*, and be brief.** The code says what it does.
Comments carry the reasoning that would otherwise be lost — why the dial timeout is
200 ms, why dry run continues evaluating, why `/healthz` must not check Redis.

Two failure modes to avoid, both of which this codebase has had to be cleaned of:

- **Restating the code.** `// Window is the counting period` above `Window
  time.Duration` earns nothing. Delete it; a reader loses no information.
- **Repeating a rationale that lives in `docs/`.** Say it once, in the doc, and let
  the code comment be a sentence. Three copies of an explanation is three things to
  keep true, and two of them will go stale.

A rough gauge: if a comment is longer than the thing it describes, it probably wants
to be shorter or to be in `docs/`. Doc comments on exported symbols are required by
`revive` regardless, so small API files sit at a high ratio legitimately.

**Prefer configuration over code.** If a new limit can be expressed with an existing
keyer, it needs no code at all. New Go code is for genuinely new capability; see
[docs/extending.md](docs/extending.md).

**Update the docs in the same change.** A configuration field that is not in
`docs/configuration.md` does not exist as far as users are concerned.

## Style

- `gofmt` — enforced; `make lint` fails on unformatted files.
- Exported identifiers are documented. `revive` enforces it.
- British spelling in prose and comments, since the rest of the project uses it. Go
  identifiers follow whatever the standard library calls the thing.
- No new dependencies without a reason in the pull request. The current set is
  deliberately small, and all of it is permissively licensed (MIT / ISC / BSD /
  Apache-2.0).

## Commits and pull requests

- One logical change per pull request. Two unrelated fixes are two pull requests.
- Write commit messages in the imperative: "add a query-parameter keyer", not "added".
- Explain the reasoning in the pull request description. What broke, or what you could
  not express before.
- Rebase rather than merge to update a branch.

By contributing you agree that your contribution is licensed under
[Apache-2.0](LICENSE), as stated in section 5 of the licence. No CLA, no separate
sign-off.

## Releasing

Maintainers only.

```sh
# CHANGELOG.md first: move Unreleased into a version heading.
git tag -a v0.2.0 -m 'v0.2.0' && git push origin v0.2.0
```

The tag triggers lint, tests and the end-to-end suite, then publishes binaries with
checksums to a GitHub release and `vX.Y.Z` / `vX.Y` / `vX` to ghcr.io. It does not
move `latest` — that follows main, and every merge there publishes `latest` and
`main-<sha>` once the whole pipeline is green.

## Reporting bugs

Include:

- The configuration file, redacted (limiters and keys matter; secrets do not).
- The relevant environment variables.
- What you expected, and what happened.
- Log lines around the event, and the value of
  `forwardlimit_decisions_total{limiter,decision}` if you have it.

For anything with a security impact, do **not** open an issue — see
[SECURITY.md](SECURITY.md).

## Ideas that would be welcome

- **Envoy `ext_authz` support.** The decision logic is proxy-agnostic already; this is
  a new adapter in `internal/httpapi`, not a redesign. The obvious second integration.
- **More normalisers**: a hostname canonicaliser, a JWT claim extractor, a
  path-template collapser.
- **More key sources**: a cookie, a client certificate subject.
- **Cardinality limiting** — "this credential has now presented 5,000 distinct values
  this hour" — which catches the high-cardinality evasion that per-value limits cannot.
  Redis HyperLogLog makes it cheap. This is the most interesting open idea.
- **A Helm chart**, if you would maintain it.
