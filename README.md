# registry-stats

![License: GPL-3.0](https://img.shields.io/badge/license-GPL--3.0-blue)
[![GitHub release](https://img.shields.io/github/v/release/cplieger/registry-stats)](https://github.com/cplieger/registry-stats/releases)
[![Image Size](https://ghcr-badge.egpl.dev/cplieger/registry-stats/size)](https://github.com/cplieger/registry-stats/pkgs/container/registry-stats)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)

Docker Hub and GHCR download tracker with Grafana dashboard

## Overview

Collects download statistics from Docker Hub and GHCR for your public
container images. Exposes the data two ways:

- **Prometheus metrics** (`/metrics`) — pull counts as gauges, scraped
  by Alloy/Prometheus into Mimir/Thanos for native Grafana dashboards
- **JSON API** (`/api/*`) — raw data for scripts, automation, or any
  tool that speaks HTTP

Supports both explicit repos (`myuser/myapp`) and owner wildcards
(`myuser/*`) to automatically discover and track all public repos
for an owner. Wildcards are resolved on each poll cycle, so newly
published images are picked up automatically.

Either surface can be disabled independently via environment variables
(`ENABLE_METRICS`, `ENABLE_JSON_API`) to save resources.

This is a distroless, rootless container — it runs as `nonroot` on
`gcr.io/distroless/static` with no shell or package manager. It has
zero external Go dependencies (stdlib-only).

### Limitations

- **Public repositories only.** Docker Hub uses the unauthenticated API.
  GHCR download counts are scraped from public package pages. Private
  repositories and packages are not supported.
- **GHCR scraping is fragile.** Download counts and package listings
  are extracted from GitHub's HTML, not an official API. If GitHub
  changes their page structure, scraping will break. The container
  logs a clear error with a link to open an issue when this happens.
- **No historical backfill.** The registries only expose current totals.
  Time-series data is built locally as snapshots accumulate. If you
  start today, you only have data from today forward.


## Container Registries

This image is published to both GHCR and Docker Hub:

| Registry | Image |
|----------|-------|
| GHCR | `ghcr.io/cplieger/registry-stats` |
| Docker Hub | `docker.io/cplieger/registry-stats` |

```bash
# Pull from GHCR
docker pull ghcr.io/cplieger/registry-stats:latest

# Pull from Docker Hub
docker pull cplieger/registry-stats:latest
```

Both registries receive identical images and tags. Use whichever you prefer.

## Quick Start

```yaml
services:
  registry-stats:
    image: ghcr.io/cplieger/registry-stats:latest
    container_name: registry-stats
    restart: unless-stopped
    user: "1000:1000"  # match your host user

    environment:
      TZ: "Europe/Paris"
      DOCKERHUB_REPOS: ""  # owner/repo or owner/* format, comma-separated
      GHCR_REPOS: ""  # owner/package or owner/* format, comma-separated
      LOG_LEVEL: "info"
      POLL_INTERVAL_HOURS: "1"  # 0 = collect once then serve
      RETENTION_DAYS: "90"  # 0 = keep forever

    ports:
      - "9100:9100"

    volumes:
      - "/opt/appdata/registry-stats:/data"  # daily JSON snapshots
```

## Deployment

1. Set `DOCKERHUB_REPOS` to a comma-separated list of Docker Hub
   repositories in `owner/repo` format
   (e.g. `myuser/myapp,myuser/otherapp`). Use `owner/*` to
   automatically track all public repos for an owner
   (e.g. `myuser/*`).
2. Set `GHCR_REPOS` to a comma-separated list of public GHCR packages
   in `owner/package` format. Use `owner/*` to automatically track
   all public packages for an owner. Only public packages are
   supported.
3. You can mix wildcards and explicit refs freely
   (e.g. `myuser/*,otheruser/specific-app`). Duplicates are
   automatically deduplicated — if `myuser/*` discovers `myapp` and
   you also list `myuser/myapp`, it's only collected once.
4. Mount a persistent directory to `/data` for snapshot storage.
5. The container starts collecting immediately and serves the HTTP API
   on port 9100. With the default 1-hour poll interval, you'll have
   your first data point within minutes.
6. For Grafana integration, see the
   [Grafana Integration](#grafana-integration) section below. If you
   use a different dashboard tool, see the
   [API Reference](#api-reference) for endpoint documentation and
   examples.


## Environment Variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `TZ` | Container timezone | `Europe/Paris` | No |
| `DOCKERHUB_REPOS` | Comma-separated list of Docker Hub repositories to track. Use `owner/repo` for specific repos or `owner/*` to auto-discover all public repos for an owner (e.g. `myuser/*,otheruser/specific-app`) | `` | No |
| `GHCR_REPOS` | Comma-separated list of public GHCR packages to track. Use `owner/package` for specific packages or `owner/*` to auto-discover all public packages for an owner (e.g. `myuser/*,otheruser/specific-app`) | `` | No |
| `LOG_LEVEL` | - | `info` | No |
| `POLL_INTERVAL_HOURS` | Hours between collection cycles. Set to 0 to collect once and then only serve the API (no recurring polls). Wildcards are re-expanded on each cycle, picking up newly published images | `1` | No |
| `RETENTION_DAYS` | Number of days to keep snapshot files. Older snapshots are automatically deleted. Set to 0 to keep all snapshots forever | `90` | No |


## Volumes

| Mount | Description |
|-------|-------------|
| `/data` | Snapshot storage directory. Contains one JSON file per day (e.g. `2025-01-15.json`) within the configured retention period. Size is minimal — typically under 2 MB for 90 days of data. |

## Ports

| Port | Description |
|------|-------------|
| `9100` | HTTP API for Grafana and other consumers |

## API Reference

The HTTP API serves JSON on port 9100. All endpoints return `[]`
(not `null`) for empty results and use ISO 8601 timestamps.

### Filtering

All data endpoints support these query parameters:

| Parameter | Description | Example |
|-----------|-------------|---------|
| `registry` | Filter by registry (`dockerhub` or `ghcr`) | `?registry=dockerhub` |
| `repo` | Filter by package name | `?repo=myuser/myapp` |

Omitting a filter returns all data. Multiple repos can be
comma-separated or passed as repeated parameters.

### Endpoints

#### `GET /api/health`

Returns `{"status":"ok"}` when healthy, or `{"status":"unready","reason":"..."}` with HTTP 503
during startup (before the first successful collect). Used as the Docker healthcheck endpoint.

#### `GET /metrics`

Prometheus text format metrics. Includes:
- `registrystats_image_pulls_total{registry,owner,repo}` — current pull count per image
- `registrystats_image_tags{registry,owner,repo}` — tag count per image
- `registrystats_http_requests_total{method,path,status}` — HTTP request counters
- `registrystats_http_request_duration_seconds` — request latency histogram
- `registrystats_collects_total{source}` — successful collects per source
- `registrystats_collect_errors_total{source}` — failed collects per source
- `registrystats_collect_duration_seconds` — collect cycle duration histogram
- `process_goroutines`, `process_heap_bytes`, `process_uptime_seconds` — runtime metrics

Disabled when `ENABLE_METRICS=false`.

#### `GET /api/summary`

Current snapshot overview — one row per package per registry.

```json
[
  {"registry":"dockerhub","name":"myuser/myapp","pull_count":1234,"tag_count":5},
  {"registry":"ghcr","name":"myuser/myapp","pull_count":567,"tag_count":0}
]
```

#### `GET /api/pulls`

Cumulative pull counts over time — one row per package per day.
When both registries track the same package, their counts are
merged (summed) per day.

```json
[
  {"timestamp":"2025-01-15T00:00:00Z","repo":"myuser/myapp","pull_count":1801}
]
```

#### `GET /api/pulls/daily`

Daily download deltas — the difference in pull counts between
consecutive days. Counter resets are clamped to zero. Missing
days (transient scrape failures) are smoothed by dividing the
delta across the gap, so a one-day outage doesn't show as a
spike the day after.

When a repo is present in both registries and one scrape
transiently fails, the missing registry's value is carried
forward from the last successful day so the merged daily delta
reflects only the real per-registry change.

The first day a repo appears in the retained snapshot window
reports `daily_pulls: 0` (no previous day to compare) and
carries a `first_seen: true` flag so dashboards can annotate it
rather than misread the zero as a drop in activity.

```json
[
  {"timestamp":"2025-01-15T00:00:00Z","repo":"myuser/myapp","daily_pulls":0,"first_seen":true},
  {"timestamp":"2025-01-16T00:00:00Z","repo":"myuser/myapp","daily_pulls":42}
]
```

#### `GET /api/snapshot`

Raw snapshot for debugging. Returns the full snapshot file including
all Docker Hub tag metadata and GHCR download counts. Accepts
`?date=YYYY-MM-DD` to fetch a specific day (defaults to the most
recent snapshot).

### Using Without Grafana

The API returns standard JSON that any HTTP client, dashboard tool,
or script can consume. Examples:

```bash
# Total downloads across all repos
curl -s http://localhost:9100/api/summary | jq '[.[].pull_count] | add'

# Daily deltas for a specific repo
curl -s 'http://localhost:9100/api/pulls/daily?repo=myuser/myapp' | jq .

# Docker Hub repos only
curl -s 'http://localhost:9100/api/summary?registry=dockerhub' | jq .

# Export raw snapshot for backup or external processing
curl -s http://localhost:9100/api/snapshot > backup.json
```

For periodic reporting, point a cron job at `/api/summary` and pipe
the output to your notification system, spreadsheet, or monitoring
tool.

## Grafana Integration

Registry Stats exposes Prometheus metrics at `/metrics`. The included
`grafana-dashboard.json` uses PromQL and requires only a standard
Prometheus datasource — no plugins needed.

### Setup

1. Add a scrape target for `registry-stats:9100` in your
   Prometheus/Alloy/Grafana Agent config
2. Import `grafana-dashboard.json` in Grafana
3. Select your Prometheus/Mimir datasource when prompted

**Alloy example:**

```alloy
prometheus.scrape "registry_stats" {
  targets         = [{ __address__ = "registry-stats:9100" }]
  forward_to      = [prometheus.remote_write.default.receiver]
  scrape_interval = "60s"
  job_name        = "registry-stats"
  metrics_path    = "/metrics"
}
```

**Prometheus example (`prometheus.yml`):**

```yaml
scrape_configs:
  - job_name: registry-stats
    scrape_interval: 60s
    static_configs:
      - targets: ["registry-stats:9100"]
```

The dashboard shows cumulative downloads, daily deltas, package
overview, and tracked package count — all via standard PromQL.

To save disk I/O, set `ENABLE_JSON_API=false` once the Prometheus
path is confirmed working. This stops writing daily JSON snapshot
files to disk.

### Non-Grafana / non-Prometheus ingestion

The JSON API provides the same data in a tool-agnostic format for
any HTTP client (scripts, custom dashboards, Slack bots, spreadsheet
imports):

- `GET /api/summary` — current pull counts per package
- `GET /api/pulls` — cumulative pull counts over time
- `GET /api/pulls/daily` — daily download deltas

Set `ENABLE_METRICS=false` if you only use the JSON API and don't
run a Prometheus scraper.

## Docker Healthcheck

The container includes a built-in Docker healthcheck. After each
collection cycle, the main process creates or removes a marker file
at `/tmp/.healthy`. The `health` subcommand checks for this file.

**When it becomes unhealthy:**
- All configured Docker Hub repos fail to respond (partial failures
  are tolerated — one successful repo keeps the container healthy)
- All configured GHCR packages fail to scrape
- The snapshot file cannot be written to disk
- Wildcard expansion failures alone do not cause unhealthy status
  if explicit repos still succeed

**When it recovers:**
- The next collection cycle where at least one registry responds
  successfully recreates the marker file. No restart required.

**On startup:** The container collects immediately. If both registries
are unreachable on first boot, it starts unhealthy and recovers on
the next successful poll.

To check health manually:
```bash
docker inspect --format='{{json .State.Health.Log}}' registry-stats | python3 -m json.tool
```

| Type | Command | Meaning |
|------|---------|---------|
| Docker | `/registry-stats health` | Exit 0 = last collection succeeded |


## Code Quality

| Metric | Value |
|--------|-------|
| [Test Coverage](https://go.dev/blog/cover) | 82.1% |
| Tests | 244 |
| [Cyclomatic Complexity](https://en.wikipedia.org/wiki/Cyclomatic_complexity) (avg) | 4.1 |
| [Cognitive Complexity](https://www.sonarsource.com/docs/CognitiveComplexity.pdf) (avg) | 4.1 |
| [Mutation Efficacy](https://en.wikipedia.org/wiki/Mutation_testing) | 84.7% (59 runs) |
| Test Framework | Property-based ([rapid](https://github.com/flyingmutant/rapid)) + table-driven |

Tests cover all HTTP API endpoints (health, summary, pulls,
pulls/daily, snapshot) with registry and repo filtering, snapshot
persistence (save, load, list, prune with boundary dates, path
traversal rejection), Docker Hub and GHCR collection (wildcard
expansion, pagination, partial failures, deduplication), daily
delta calculation with counter-reset clamping, config validation,
JSON serialization round-trips, HTTP retry backoff with
Retry-After header parsing, and atomic snapshot writes with
cleanup on failure. Property-based tests verify that parsing
functions never panic on arbitrary input and that URL segments
are safely validated.

Not tested: `main()` and the HTTP server bind — thin runtime
wrappers around the tested core logic. GHCR HTML scraping is
tested against captured page fragments but may break if GitHub
changes their markup.

## Security Review

**No vulnerabilities found.** All scans clean across 8 tools.

| Tool | Result |
|------|--------|
| [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | No vulnerabilities in call graph |
| [golangci-lint](https://golangci-lint.run/) (gosec) | 0 issues |
| [trivy](https://trivy.dev/) | 0 vulnerabilities (distroless base) |
| [grype](https://github.com/anchore/grype) | 0 vulnerabilities |
| [gitleaks](https://github.com/gitleaks/gitleaks) | No secrets detected |
| [semgrep](https://semgrep.dev/) | 1 info (false positive) |
| [hadolint](https://github.com/hadolint/hadolint) | Clean |

Read-only JSON API designed for internal Grafana consumption.
No authentication required (standard for internal metrics APIs).
Stdlib-only (zero external Go dependencies). Runs as `nonroot`
on a distroless base image with no shell. The HTTP client
refuses cross-host redirects
(`CheckRedirect = ErrUseLastResponse`) so a compromised or
misconfigured upstream cannot bounce the polling request to a
third-party host.

**Details for advanced users:** URL path segments validated via
`isSafeURLSegment` (rejects `/%\?#@:`). Snapshot filenames are
date-format-validated before disk access (prevents path
traversal). Response bodies capped via `io.LimitReader` (10 MB
JSON, 4 MB HTML). HTTP server sets all five timeouts. Atomic
writes (temp file + rename) prevent snapshot corruption; stale
temp files from interrupted writes are swept on startup.
Retry-After response headers are honoured on 429/503 responses
(capped at the configured retry backoff ceiling). Semgrep flags
`math/rand/v2` usage, which is correct for jitter timing
(not crypto).

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Version | Source |
|------------|---------|--------|
| golang | `1.26-alpine` | [Go](https://hub.docker.com/_/golang) |
| gcr.io/distroless/static-debian13 | `nonroot` | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| golang.org/x/sync | `v0.20.0` | [Go stdlib](https://pkg.go.dev/golang.org/x/sync) |
| pgregory.net/rapid | `v1.3.0` | [pkg.go.dev](https://pkg.go.dev/pgregory.net/rapid) |

## Design Principles

- **Always up to date**: Base images, packages, and libraries are updated automatically via Renovate. Unlike many community Docker images that ship outdated or abandoned dependencies, these images receive continuous updates.
- **Minimal attack surface**: When possible, pure Go apps use `gcr.io/distroless/static:nonroot` (no shell, no package manager, runs as non-root). Apps requiring system packages use Alpine with the minimum necessary privileges.
- **Digest-pinned**: Every `FROM` instruction pins a SHA256 digest. All GitHub Actions are digest-pinned.
- **Multi-platform**: Built for `linux/amd64` and `linux/arm64`.
- **Healthchecks**: Every container includes a Docker healthcheck.
- **Provenance**: Build provenance is attested via GitHub Actions, verifiable with `gh attestation verify`.

## Credits

This is an original tool that builds upon [Docker Hub API](https://docs.docker.com/docker-hub/api/latest/).

## Disclaimer

These images are built with care and follow security best practices, but they are intended for **homelab use**. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
