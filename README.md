# registry-stats

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/registry-stats/badges/size.json)](https://github.com/cplieger/registry-stats/pkgs/container/registry-stats)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/registry-stats/badges/coverage.json)](https://github.com/cplieger/registry-stats/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/registry-stats/badges/mutation.json)](https://github.com/cplieger/registry-stats/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13219/badge)](https://www.bestpractices.dev/projects/13219)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/registry-stats/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/registry-stats)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/registry-stats/releases)

<!-- hub-overview BEGIN -->
Track how many times your container images are pulled, with a ready-made Grafana dashboard.

## What it does

When you publish a container image to Docker Hub or GitHub Container Registry (GHCR), each registry tracks how many times that image has been downloaded, but there's no built-in way to see those numbers over time, compare trends, or get alerts. Registry Stats solves this by polling the registries on a schedule and exposing the download counts as Prometheus metrics for dashboards and alerting.

- **Prometheus metrics** (`/metrics`): pull counts as gauges, scraped by any Prometheus-compatible collector for native Grafana dashboards
- Supports both explicit repos (`myuser/myapp`) and owner wildcards (`myuser/*`) to automatically discover and track all public repos for an owner. Wildcards are resolved on each poll cycle, so newly published images are picked up automatically.

### Why this design

- **Stateless**: no on-disk persistence required. The app exposes current counts; time-series history lives in your Prometheus/Mimir backend.
- **Minimal dependencies**: the only runtime dependencies are the maintainer's own `httpx`, `health`, `metrics`, `webhttp`, `scheduler`, `slogx`, `envx`, and `keyenc` libraries, which supply retry/backoff, the health probe, Prometheus exposition, the HTTP server lifecycle, the poll loop, UTC logging, environment parsing, and the shared dedupe-key encoder. Small, auditable supply chain.
- **Distroless, rootless container**: runs as `nonroot` on `gcr.io/distroless/static-debian13` with no shell or package manager, minimising attack surface.
- **Public repos only**: avoids credential management entirely.

### Limitations

- **Public repositories only.** Docker Hub uses the unauthenticated API.
  GHCR download counts are scraped from public package pages. Private
  repositories and packages are not supported.
- **GHCR scraping is fragile.** Download counts and package listings
  are extracted from GitHub's HTML, not an official API. If GitHub
  changes their page structure, scraping will break. The container
  logs a clear error with a link to open an issue when this happens.
- **No historical backfill.** The registries only expose current totals.
  Time-series data is built by your Prometheus backend as scrapes
  accumulate.
<!-- hub-overview END -->

## Quick start

The image is published to both `ghcr.io/cplieger/registry-stats` and `docker.io/cplieger/registry-stats`; use whichever registry you prefer.

```yaml
services:
  registry-stats:
    image: ghcr.io/cplieger/registry-stats:latest
    container_name: registry-stats
    restart: unless-stopped

    environment:
      # Set at least one repo; leaving both empty makes the container report unhealthy after the first collect.
      DOCKERHUB_REPOS: ""  # owner/repo or owner/* format, comma-separated
      GHCR_REPOS: ""  # owner/package or owner/* format, comma-separated
      POLL_INTERVAL_HOURS: "1"  # 0 = collect once then serve

    ports:
      - "9100:9100"
```

## Configuration reference

### Environment variables

| Variable | Description | Default | Required |
| --- | --- | --- | --- |
| `DOCKERHUB_REPOS` | Comma-separated list of Docker Hub repositories to track. Use `owner/repo` for specific repos or `owner/*` to auto-discover all public repos for an owner (for example `myuser/*,otheruser/specific-app`) | _(unset)_ | No |
| `GHCR_REPOS` | Comma-separated list of public GHCR packages to track. Use `owner/package` for specific packages or `owner/*` to auto-discover all public packages for an owner (for example `myuser/*,otheruser/specific-app`) | _(unset)_ | No |
| `LOG_LEVEL` | Logging verbosity: `debug`, `info`, `warn`, or `error`. Unrecognized values fall back to `info` | `info` | No |
| `POLL_INTERVAL_HOURS` | Hours between collection cycles. Set to 0 to collect once and then only serve metrics (no recurring polls). Wildcards are re-expanded on each cycle, picking up newly published images | `1` | No |
| `ENABLE_METRICS` | Serve the Prometheus metrics endpoint; `false` or `0` disables it | `true` | No |
| `LISTEN_ADDR` | TCP listen address for the HTTP server in `host:port` form. The port must match the published container port | `:9100` | No |

### Ports

| Port | Description |
| --- | --- |
| `9100` | HTTP server (Prometheus metrics + health endpoint) |

## API reference

### Endpoints

#### `GET /api/health`

Serving-readiness gate. Returns `{"status":"unready","reason":"..."}` with HTTP 503 until the
first collect cycle produces data, then `{"status":"ok"}`. Readiness latches: once set, it is
cleared only on shutdown, so a later failed cycle does not flip the endpoint back to 503
(per-cycle collection health is the file marker's job; see [Healthcheck](#healthcheck)). The
Docker healthcheck runs the `health` subcommand against the marker file, not this endpoint.

#### `GET /metrics`

Prometheus text format metrics. Includes:

- `registrystats_image_pulls_total{registry,owner,repo}`: current pull count per image
- `registrystats_image_tags{registry,owner,repo}`: tag count per image
- `registrystats_http_requests_total{method,path,status}`: HTTP request counters
- `registrystats_http_request_duration_seconds`: request latency histogram
- `registrystats_collects_total{source}`: collect runs per source, successful and failed
- `registrystats_collect_errors_total{source}`: failed collects per source
- `registrystats_collect_duration_seconds`: collect cycle duration histogram
- `go_goroutines`, `go_memstats_heap_alloc_bytes`, `process_uptime_seconds`: runtime metrics

Disabled when `ENABLE_METRICS=false`.

## Grafana integration

Registry Stats exposes Prometheus metrics at `/metrics`. The included
`grafana-dashboard.json` uses PromQL and requires only a standard
Prometheus datasource; no plugins needed.

### Setup

1. Add a scrape target for `registry-stats:9100` in your collector
   (Prometheus, Alloy, or any Prometheus-compatible scraper); the shipped
   alert rules assume `job="registry-stats"`
2. Import `grafana-dashboard.json` in Grafana
3. Select your Prometheus/Mimir datasource when prompted

The dashboard shows cumulative downloads, daily deltas, package
overview, and tracked package count.

## Alerting

registry-stats reports its state in two places, so the group in
[`alerts.yaml`](alerts.yaml) is mixed. Five rules are PromQL, evaluated with
Prometheus or the Mimir ruler over the `/metrics` endpoint you already scrape
(see [Grafana integration](#grafana-integration)). Three are LogQL, evaluated
with Loki's ruler over the container log, because their conditions leave no
series to read: a repo ref rejected at parse time is never polled, and a cycle
that loses a minority of its repos still reports healthy, so the exported counts
go quietly incomplete while no metric moves.

| Alert | Fires when | Severity |
| --- | --- | --- |
| `RegistryStatsTargetDown` | `up{job="registry-stats"} == 0` for 15m: the exporter is not being scraped | warning |
| `RegistryStatsTargetAbsent` | `absent(up{job="registry-stats"})` for 15m: the exporter is not a configured scrape target at all | warning |
| `RegistryStatsCollectStalled` | no collect cycle has completed in 3h, while the exporter is up and serving its last values | warning |
| `RegistryStatsSourceDegraded` | one registry failed for most of its repos in a cycle, so those images drop off `/metrics` | warning |
| `RegistryStatsPullCountRegressed` | a tracked image's pull count falls below its 2-day max: a wrong count that did not error | warning |
| `RegistryStatsConfigRejected` | a `DOCKERHUB_REPOS` or `GHCR_REPOS` entry was skipped, or no entry was usable at all | warning |
| `RegistryStatsCollectFailed` | the container logged an `ERROR`: a fetch or parse failure, a changed GHCR page, or a recovered panic | warning |
| `RegistryStatsCollectionIncomplete` | a cycle lost images without failing: a truncated owner listing or a rate limit | warning |

`RegistryStatsCollectStalled` measures absence over 15m under a 3h `for:`,
rather than absence over 3h directly. A counter that has just started carries
two samples at the same value, which the direct form reads as a stall, so it
fires about 30m after every container start. Set the `for:` window to about
three `POLL_INTERVAL_HOURS`, and drop the rule in one-shot mode
(`POLL_INTERVAL_HOURS=0`), where a single cycle is the point.

`RegistryStatsCollectionIncomplete` is the counterpart to
`RegistryStatsSourceDegraded` on the axis the counters cannot reach. A cycle
counts as healthy while most repos succeed, so a rate limit or a truncated owner
listing takes images off `/metrics` without moving
`registrystats_collect_errors_total`. Keep `LOG_LEVEL` at its `info` default for
that rule and for `RegistryStatsConfigRejected`: both key on `WARN` lines.

Thresholds and the `for:` windows are starting points. The scrape `job` label is
yours: the `up{job="registry-stats"}` selector assumes `job="registry-stats"`
(matching the setup step above), so adjust it to your scrape config. Adjust the
`container` selector on the LogQL rules the same way, or to `job` / `service`,
depending on your log collector. Route by whatever labels your Alertmanager
uses.

## Healthcheck

The container includes a built-in Docker healthcheck: the `health` subcommand (`/registry-stats health`) exits 0 while a marker file at `/tmp/.healthy` is present. The marker is created as soon as the HTTP API is listening, then refreshed after every collection cycle: a cycle that collects at least one repo keeps it, and a cycle in which every configured registry fails removes it. The first collect runs in the background, so a slow initial poll cannot exceed the Docker healthcheck grace window and trigger a restart loop; the container reports healthy on boot, then reflects the first cycle's real outcome once it finishes. In scheduled mode the probe also enforces a freshness deadline: a marker older than three poll intervals reports unhealthy, so a wedged collect loop gets restarted. An unhealthy marker recovers on the next successful poll. In one-shot mode (`POLL_INTERVAL_HOURS=0`) there is no next poll and no freshness deadline: a failed single collect leaves the container unhealthy until it is restarted. Partial failures are tolerated: one successful repo keeps the container healthy, and wildcard expansion failures alone do not cause unhealthy status if explicit repos still succeed.

## Security

The Prometheus metrics endpoint is designed for internal scraping and has no
authentication (standard for internal metrics APIs); do not expose port 9100
to untrusted networks. The container runs as `nonroot` on a distroless base
image with no shell or package manager.

The HTTP client follows redirects only within a `docker.com` / `github.com` /
`githubusercontent.com` host allowlist with a 5-hop cap, so a compromised or
misconfigured upstream cannot bounce the polling request to an arbitrary
third-party host (the registries legitimately redirect to their own CDNs and
blob stores). URL path segments built from registry data are validated
against an `[A-Za-z0-9._-]` allowlist. Response bodies are capped at 10 MB
for JSON and 2 MB for HTML; a GHCR page that exceeds the HTML cap is treated
as a format-change signal, not silently truncated. The HTTP server sets all
four timeouts, and `Retry-After` headers on 429/503 responses are honoured
up to the configured retry backoff ceiling.

One accepted scanner finding: semgrep flags the use of `math/rand/v2`, which
is correct here because it spaces out successive registry requests within a
cycle, and generates no cryptographic material.

### Hardened deployment

To lock the container down further, layer these directives onto
the Quick start service:

```yaml
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - "/tmp:size=1m,mode=1777,noexec,nosuid,nodev"
```

`read_only: true` requires the file-marker health probe to have a writable
`/tmp`; the tmpfs supplies it. `size=1m` is ample: the marker is the only
thing registry-stats writes to disk.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang) |
| Distroless static | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| pgregory.net/rapid | [pkg.go.dev](https://pkg.go.dev/pgregory.net/rapid) |

## Credits

This is an original tool that builds upon [Docker Hub API](https://docs.docker.com/docker-hub/api/latest/).

## Contributing

Issues and pull requests are welcome. Please open an issue first for
larger changes so the approach can be discussed before implementation.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
