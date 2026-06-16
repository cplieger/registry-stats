# Contributing to registry-stats

Notes on the architecture, local workflow, and conventions specific to this
repo. The generic `cplieger` defaults still apply; this file adds the
code-grounded detail a contributor needs to land a change without tripping
over the load-bearing patterns.

## Architecture

`registry-stats` is a single Go binary that polls Docker Hub and GHCR on a
schedule and exposes download-count metrics as Prometheus time series
(`/metrics`) plus a health endpoint (`/api/health`) on port 9100. History is
owned by the scraping backend (Mimir/Prometheus); the app itself is stateless.

`main.go` is a **pure composition root** — it wires config → `httpx.Client` →
`dockerhub.Client` + `ghcr.Client` → health marker → `webapi` server, then
runs the signal-driven lifecycle. It contains no business logic, globals, or
type aliases; everything testable lives under `internal/`.

`internal/api/interfaces.go` is the composition spine. The small interfaces
there (`RegistrySource`, `HealthSignal`, `HTTPDoer`) are what every other
package depends on, and what test fakes implement. Concrete types live in their
own packages:

- `internal/config` — env-var loading and validation (`LoadConfig`).
- `internal/dockerhub`, `internal/ghcr` — the two `RegistrySource`
  implementations. Docker Hub uses the unauthenticated API; GHCR **scrapes
  public package HTML** (there is no official download-count API).
- `internal/collect` — orchestrates a single collect cycle across sources.
- `internal/webapi` — HTTP server: `/metrics` (Prometheus exposition) and
  `/api/health`.
- `internal/metrics` — thin wrapper around `github.com/cplieger/metrics`
  holding the `registrystats_*` instances and `SetImageMetrics`.
- `internal/model`, `internal/urlsafe`, `internal/testsupport` — domain
  types, URL-segment validation, and shared test helpers.

Dependencies flow one direction: concrete packages depend on `internal/api`,
and `main.go` is the only place that imports concrete packages together.

## Local development

The module targets the Go version pinned in `go.mod`;
the container builds on the Alpine `golang` builder.

```sh
go build ./...
```

Run it locally by pointing the repo env vars at some public images:

```sh
DOCKERHUB_REPOS="library/alpine" \
GHCR_REPOS="cplieger/*" \
POLL_INTERVAL_HOURS=0 \
go run .
```

`POLL_INTERVAL_HOURS=0` collects once then just serves, which is the fastest
loop for iterating on the API or metrics output. The full env-var reference is
in the README's configuration table.

## Running checks

```sh
go test ./...                 # unit + property-based (rapid) + table-driven
go test -race ./...           # the lifecycle/concurrency paths
golangci-lint run             # lint (also flags unformatted files)
golangci-lint fmt             # apply gofumpt + gci formatting
```

Linting is configured in `.golangci.yaml` (golangci-lint v2). Formatting is
`gofumpt` (with `extra-rules`) plus `gci` import grouping; `golangci-lint run`
reports unformatted files as issues, so run `fmt` before pushing.

Fuzz targets exist in several packages (`internal/dockerhub`, `internal/ghcr`).
Run one with:

```sh
go test -run='^$' -fuzz=FuzzName -fuzztime=30s ./internal/ghcr
```

Mutation testing (`.gremlins.yaml`) runs on a central weekly schedule — you do
not need to run gremlins per-change, but new logic should be killable by a
test rather than relying on the exclude list.

CI is centralized: `.github/workflows/*.yaml` are **synced from
`cplieger/ci` and marked `DO NOT EDIT`**. Change CI behavior upstream, not
here.

## Conventions and gotchas

- **Keep the runtime dependency footprint minimal.** Runtime deps are limited
  to the `cplieger` shared libs (`httpx`, `metrics`, `health`) and
  `pgregory.net/rapid` (test-only). Prefer the standard library before
  reaching for a new dependency.
- **`RegistrySource.Name()` must equal `Source().String()`.** Both surface the
  same lowercase registry label — one in log k/v pairs, the other for typed
  routing. They must never drift.
- **Validate any URL path segment** built from registry data through
  `internal/urlsafe` — guards against traversal and injection.
- **Health is a file marker** (`/tmp/.healthy`), checked by the
  `registry-stats health` subcommand for the distroless healthcheck. Partial
  collect failures stay healthy as long as one repo succeeds.
- **GHCR scraping is fragile by design.** It parses GitHub HTML and is tested
  against captured fragments in `internal/ghcr`. If you touch it, update the
  fixtures and keep the clear error-with-issue-link behavior on markup
  changes.

## Commits and PRs

Branch from `main`, keep changes focused with tests, and open a PR. Commit
messages follow [Conventional Commits](https://www.conventionalcommits.org/) —
git-cliff parses them to build release notes and pick the version bump
(`feat:` → minor, `fix:`/`sec:` → patch/security, `feat!:` → major; `chore`,
`ci`, `docs`, `test`, etc. don't release). See `cliff.toml` for the parser.

## Conduct and security

By participating you agree to the org-wide
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report vulnerabilities through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md) —
never in a public issue.
