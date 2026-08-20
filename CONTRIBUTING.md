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

`main.go` is a **pure composition root**: it wires config → `*http.Client`
(with the `httpx` redirect policy) → `dockerhub.Client` + `ghcr.Client` →
health marker → `webapi` server, then runs the signal-driven lifecycle. It
contains no business logic, globals, or type aliases; everything testable
lives under `internal/`.

Interfaces live at their consumers (there is no hub package): `collect.Source`
is declared in `internal/collect`, the one seam its orchestrator drives, and
`main.go` holds the unexported one-method `healthSignal` it wires the marker
into. Test fakes implement those. Concrete types live in their own packages:

- `internal/config`: env-var loading and validation (`Load`). Never logs;
  parse problems come back as `Warning` values `main` emits once.
- `internal/dockerhub`, `internal/ghcr`: the two `collect.Source`
  implementations. Docker Hub uses the unauthenticated API; GHCR **scrapes
  public package HTML** (there is no official download-count API).
- `internal/collect`: orchestrates a single collect cycle across sources.
- `internal/webapi`: HTTP server: `/metrics` (Prometheus exposition) and
  `/api/health`.
- `internal/obs`: the observability surface built on
  `github.com/cplieger/metrics`; the `registrystats_*` instances and
  `SetImage`.
- `internal/registry`, `internal/urlsafe`, `internal/testsupport`: the
  container-registry domain types (`Entry`, `RepoRef`, `ID`), URL-segment
  validation, and shared test helpers.

Dependencies flow one direction: `main.go` is the only place that imports
concrete packages together.

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

Mutation testing (`.gremlins.yaml`) runs on a central weekly schedule; you do
not need to run gremlins per-change, but new logic should be killable by a
test rather than relying on the exclude list.

CI is centralized: `.github/workflows/*.yaml` are **synced from
`cplieger/ci` and marked `DO NOT EDIT`**. Change CI behavior upstream, not
here.

## Conventions and gotchas

- **Keep the runtime dependency footprint minimal.** Runtime deps are limited
  to the `cplieger` shared libs (`httpx`, `metrics`, `health`, `webhttp`,
  `scheduler`, `slogx`, `envx`, `keyenc`) and `pgregory.net/rapid` (test-only). Prefer the standard library
  before reaching for a new dependency.
- **The lowercase registry label comes from `Source().String()` alone.** The
  old interface carried a second method (`Name()`) with a prose must-equal
  invariant; deriving the label from the one authoritative method deleted
  that drift surface.
- **Registry tests run on an in-memory network, not a port.** Every
  Docker Hub / GHCR test stands its fake registry up with
  `httptest.NewTestServer(t, handler)` and drives it with `srv.Client()`,
  whose transport routes _every_ request to the handler regardless of scheme
  or host, so production code keeps building its real
  `https://hub.docker.com/...` and `https://github.com/users/...` URLs and
  the handler dispatches on the real path. There is no URL-rewriting
  transport to wire up and no `defer srv.Close()` (the server registers its
  own `t.Cleanup`). The tradeoff to know: the in-memory `Server` leaves
  `Server.URL` and `Server.Listener` unset, so a test that needs a URL string
  passes a representative production one (see `packagePageURL`).
- **The Docker Hub parse core treats a shape change as a signal, never as a
  value.** `internal/dockerhub` decodes with `encoding/json/v2`, which
  rejects duplicate object members and matches field names exactly, and
  `pull_count` / `count` are required (`*int64` / `*int`): absent, null or
  negative is an error. That is deliberate and load-bearing:
  `image_pulls_total` is cumulative, so a silently-substituted 0 reads
  downstream as a pull-count regression rather than as missing data. A
  missing `pull_count` on an owner-listing result fails the whole page, which
  surfaces as "listing wholly failed" plus an unhealthy cycle; dropping the
  entry instead would return zero repos with a nil error and look like an
  empty owner.
- **The GHCR pacing defaults are tested at full speed.** Production paces
  scrapes 2-5 s apart, and
  `TestClient_Collect_pacesAtProductionDefaults` asserts that end to end
  inside a `testing/synctest` bubble, so the real delays elapse on the
  synthetic clock in microseconds. Keep new pacing assertions in the bubble
  rather than shrinking `Options.MinPacing` to dodge the wait.
- **Validate any URL path segment** built from registry data through
  `internal/urlsafe`, which guards against traversal and injection.
- **Health is a file marker** (`/tmp/.healthy`), checked by the
  `registry-stats health` subcommand for the distroless healthcheck. Partial
  collect failures stay healthy as long as one repo succeeds.
- **GHCR scraping is fragile by design.** It parses GitHub HTML and is tested
  against captured fragments in `internal/ghcr`. If you touch it, update the
  fixtures and keep the clear error-with-issue-link behavior on markup
  changes.
- **Logs are UTC.** The `slogx` library (its `UTCTime` `ReplaceAttr`) forces every
  record's timestamp to UTC, so the container needs no `TZ` and the binary
  embeds no `time/tzdata`.

## Commits and PRs

Branch from `main`, keep changes focused with tests, and open a PR. Commit
messages follow [Conventional Commits](https://www.conventionalcommits.org/);
git-cliff parses them to build release notes and pick the version bump
(`feat:` → minor, `fix:`/`sec:` → patch/security, `feat!:` → major; `chore`,
`ci`, `docs`, `test`, etc. don't release). See `cliff.toml` for the parser.

## Conduct and security

By participating you agree to the org-wide
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report vulnerabilities through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.
