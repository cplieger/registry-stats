# registry-stats

[![Image Size](https://ghcr-badge.egpl.dev/cplieger/registry-stats/size)](https://github.com/cplieger/registry-stats/pkgs/container/registry-stats)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/registry-stats)](https://goreportcard.com/report/github.com/cplieger/registry-stats)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/registry-stats/badges/coverage.json)](https://github.com/cplieger/registry-stats/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/registry-stats/badges/mutation.json)](https://github.com/cplieger/registry-stats/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13219/badge)](https://www.bestpractices.dev/projects/13219)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/registry-stats/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/registry-stats)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/registry-stats/releases)

Track how many times your container images are pulled — with a ready-made Grafana dashboard.

## What it does

When you publish a container image to Docker Hub or GitHub Container Registry (GHCR), each registry tracks how many times that image has been downloaded — but there's no built-in way to see those numbers over time, compare trends, or get alerts. Registry Stats solves this by polling the registries on a schedule and exposing the download counts as Prometheus metrics for dashboards and alerting.

- **Prometheus metrics** (`/metrics`) — pull counts as gauges, scraped by Alloy/Prometheus into Mimir/Thanos for native Grafana dashboards
- Supports both explicit repos (`myuser/myapp`) and owner wildcards (`myuser/*`) to automatically discover and track all public repos for an owner. Wildcards are resolved on each poll cycle, so newly published images are picked up automatically.

### Why this design

- **Stateless** — no on-disk persistence required. The app polls registries and exposes current counts as Prometheus metrics. Time-series history lives in your Prometheus/Mimir backend.
- **Minimal dependencies** — no non-`cplieger` runtime deps beyond `golang.org/x/sync`; the `cplieger` `httpx` / `health` / `metrics` libraries supply retry/backoff, the health probe, and Prometheus exposition. Small, auditable supply chain.
- **Distroless, rootless container** — runs as `nonroot` on `gcr.io/distroless/static-debian13` with no shell or package manager, minimising attack surface.
- **Public repos only** — avoids credential management entirely; Docker Hub uses the unauthenticated API and GHCR counts are scraped from public package pages.

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

## Quick start

The image is published to both `ghcr.io/cplieger/registry-stats` and `docker.io/cplieger/registry-stats` — use whichever registry you prefer.

```yaml
services:
  registry-stats:
    image: ghcr.io/cplieger/registry-stats:latest
    container_name: registry-stats
    restart: unless-stopped

    environment:
      TZ: "Europe/Paris"
      DOCKERHUB_REPOS: ""  # owner/repo or owner/* format, comma-separated
      GHCR_REPOS: ""  # owner/package or owner/* format, comma-separated
      LOG_LEVEL: "info"
      POLL_INTERVAL_HOURS: "1"  # 0 = collect once then serve

    ports:
      - "9100:9100"
```

## Configuration reference

### Environment variables

| Variable              | Description                                                                                                                                                                                              | Default        | Required |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------- | -------- |
| `TZ`                  | Container timezone                                                                                                                                                                                       | `Europe/Paris` | No       |
| `DOCKERHUB_REPOS`     | Comma-separated list of Docker Hub repositories to track. Use `owner/repo` for specific repos or `owner/*` to auto-discover all public repos for an owner (e.g. `myuser/*,otheruser/specific-app`)       | ``             | No       |
| `GHCR_REPOS`          | Comma-separated list of public GHCR packages to track. Use `owner/package` for specific packages or `owner/*` to auto-discover all public packages for an owner (e.g. `myuser/*,otheruser/specific-app`) | ``             | No       |
| `LOG_LEVEL`           | Logging verbosity: `debug`, `info`, `warn`, or `error`. Unrecognized values fall back to `info`                                                                                                          | `info`         | No       |
| `POLL_INTERVAL_HOURS` | Hours between collection cycles. Set to 0 to collect once and then only serve metrics (no recurring polls). Wildcards are re-expanded on each cycle, picking up newly published images                   | `1`            | No       |
| `ENABLE_METRICS`      | Enable Prometheus metrics endpoint                                                                                                                                                                       | `true`         | No       |
| `LISTEN_ADDR`         | TCP listen address for the HTTP server in `host:port` form. The port must match the published container port                                                                                             | `:9100`        | No       |

### Ports

| Port   | Description                                        |
| ------ | -------------------------------------------------- |
| `9100` | HTTP server (Prometheus metrics + health endpoint) |

## API reference

The HTTP server listens on port 9100.

### Endpoints

#### `GET /api/health`

Returns `{"status":"ok"}` when healthy, or `{"status":"unready","reason":"..."}` with HTTP 503
when the most recent collect cycle returned no data (every configured registry failed, or no repos
are configured). The marker is set healthy as soon as the HTTP API is listening, so a slow first
collect cannot trip the Docker healthcheck grace window; it flips to 503 only once a collect cycle
completes with an empty result. Used as the Docker healthcheck endpoint.

#### `GET /metrics`

Prometheus text format metrics. Includes:

- `registrystats_image_pulls_total{registry,owner,repo}` — current pull count per image
- `registrystats_image_tags{registry,owner,repo}` — tag count per image
- `registrystats_http_requests_total{method,path,status}` — HTTP request counters
- `registrystats_http_request_duration_seconds` — request latency histogram
- `registrystats_collects_total{source}` — total collect runs per source (successful + failed; `collect_errors_total` is the failed subset, so `collect_errors_total / collects_total` is the per-source failure ratio)
- `registrystats_collect_errors_total{source}` — failed collects per source
- `registrystats_collect_duration_seconds` — collect cycle duration histogram
- `process_goroutines`, `process_heap_bytes`, `process_uptime_seconds` — runtime metrics

Disabled when `ENABLE_METRICS=false`.

## Grafana integration

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

## Healthcheck

The container includes a built-in Docker healthcheck using a marker file at `/tmp/.healthy`. The marker is created as soon as the HTTP API is listening, then refreshed after every collection cycle: a cycle that collects at least one repo keeps the marker present, and a cycle in which every configured registry fails removes it. The `health` subcommand (`/registry-stats health`) checks for this file and exits 0 when healthy. The first collect runs in the background so a slow initial poll (GHCR paces each package by a few seconds) cannot exceed the Docker healthcheck grace window and trigger a restart loop: the container reports healthy on boot, then reflects the first cycle's real outcome once it finishes. If both registries are unreachable on first boot the marker flips to unhealthy after that cycle and recovers on the next successful poll. Partial failures are tolerated: one successful repo keeps the container healthy. Wildcard expansion failures alone do not cause unhealthy status if explicit repos still succeed.

## Security

**No vulnerabilities found.** All scans clean across the full scanner battery.

| Tool                                                                | Result                              |
| ------------------------------------------------------------------- | ----------------------------------- |
| [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | No vulnerabilities in call graph    |
| [golangci-lint](https://golangci-lint.run/) (gosec)                 | 0 issues                            |
| [trivy](https://trivy.dev/)                                         | 0 vulnerabilities (distroless base) |
| [grype](https://github.com/anchore/grype)                           | 0 vulnerabilities                   |
| [gitleaks](https://github.com/gitleaks/gitleaks)                    | No secrets detected                 |
| [semgrep](https://semgrep.dev/)                                     | 1 info (false positive)             |
| [hadolint](https://github.com/hadolint/hadolint)                    | Clean                               |

Prometheus metrics endpoint designed for internal scraping.
No authentication required (standard for internal metrics APIs).
Minimal dependencies (no non-`cplieger` runtime deps beyond
`golang.org/x/sync`; uses the `cplieger` `httpx` / `health` /
`metrics` libraries). Runs as `nonroot` on a distroless base
image with no shell. The HTTP client follows redirects only
within a host allowlist (`httpx.DockerGitHubRedirectPolicy`:
`docker.com` / `github.com` / `githubusercontent.com`, 5-hop
cap) so a compromised or misconfigured upstream cannot bounce
the polling request to an arbitrary third-party host (the
registries legitimately redirect to their own CDNs/blob
stores).

**Details for advanced users:** URL path segments validated via
`isSafeURLSegment` (rejects `/%\?#@:`). Response bodies capped
via `io.LimitReader` (10 MB JSON, 2 MB HTML). HTTP server sets
all five timeouts. Retry-After response headers are honoured on
429/503 responses (capped at the configured retry backoff
ceiling). A GHCR page that exceeds the HTML body cap is treated
as a format-change signal, not silently truncated. Semgrep flags
`math/rand/v2` usage, which is correct for jitter timing (not
crypto).

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

`read_only: true` makes the root filesystem read-only, so the
file-marker health probe needs a writable `/tmp`; the tmpfs
supplies it. `size=1m` is ample for the bare `/tmp/.healthy`
marker, the only thing registry-stats writes to disk.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency         | Source                                                           |
| ------------------ | ---------------------------------------------------------------- |
| golang             | [Go](https://hub.docker.com/_/golang)                            |
| Distroless static  | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| golang.org/x/sync  | [Go stdlib](https://pkg.go.dev/golang.org/x/sync)                |
| pgregory.net/rapid | [pkg.go.dev](https://pkg.go.dev/pgregory.net/rapid)              |

## Credits

This is an original tool that builds upon [Docker Hub API](https://docs.docker.com/docker-hub/api/latest/).

## Contributing

Issues and pull requests are welcome. Please open an issue first for
larger changes so the approach can be discussed before implementation.

## Disclaimer

These images are built with care and follow security best practices, but they are intended for **homelab use**. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
