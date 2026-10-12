# Configuration

This page explains every registry-stats setting in depth, for readers who track many images or change more than the two repo lists.

## Where settings live

Every setting is an environment variable in `compose.yaml`. registry-stats reads them once at start, so run `docker compose up -d` again after a change. Each setting is optional. With both repo lists empty, the container starts, logs `no repos configured` and turns unhealthy after its first check.

| Variable | Description | Default |
| --- | --- | --- |
| `DOCKERHUB_REPOS` | Docker Hub repos, comma-separated: `owner/repo`, or `owner/*` for every public repo of one owner, up to 198 | _(unset)_ |
| `GHCR_REPOS` | GHCR packages, comma-separated: `owner/package`, or `owner/*` for every public package of one owner, up to about 1,500 | _(unset)_ |
| `POLL_INTERVAL_HOURS` | Hours between checks. `0` checks once, then keeps serving the counts | `1` |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error`. Keep `info` for the shipped log alert rules | `info` |
| `LISTEN_ADDR` | Listen address in `host:port` form. The port must match the published container port | `:9100` |

## Repo lists

Both lists take comma-separated entries. Spaces around an entry are ignored, an entry is folded to lower case, and a repeated entry is checked once. An entry must be `owner/repo` or `owner/*`, made of letters, digits, `.`, `_` and `-`. Any other entry is skipped with a `skipping unusable repo ref` warning that names the entry and the reason.

- `owner/*` reads the owner's public listing on every check, so a newly published image appears on the next check.
- On Docker Hub, `owner/*` collects up to 198 repos. The unauthenticated API refuses a listing offset of 100 or more, so registry-stats reads two pages of 99.
- On GHCR, `owner/*` reads up to fifty listing pages, about 1,500 packages at the thirty per page GitHub serves today. A longer listing logs `ghcr owner listing hit page cap; results truncated` and keeps the pages it read.
- For a nested GHCR package, write the slash as `%2F`, for example `owner/helm-charts%2Fgrafana-operator`.
- An `owner/*` with no public images logs `docker hub wildcard expanded no repos` or `ghcr owner has no public container packages`.

Each check logs how many images it collected, and the total the registry advertises where the registry publishes one.

### The Docker Hub rate limit

Each `owner/repo` entry on Docker Hub costs one request per check, and an `owner/*` entry one request per listing page.

The unauthenticated API counts requests per client address and advertises 180 per minute in its `x-ratelimit-limit` header. registry-stats starts each Docker Hub request at least half a second after the one before. At two per second, 198 explicit entries take about 100 seconds. If the limit still hits, the affected repos recover on the next check.

## Poll interval

`POLL_INTERVAL_HOURS` is a whole number of hours. The first check starts as soon as the container is up, and the next one follows after the interval.

- `0` checks once and then only serves the counts. The alert rule `RegistryStatsCollectStalled` does not apply in this mode.
- A value that is not a whole number, or is negative, logs `invalid POLL_INTERVAL_HOURS, using default of 1 hour` and uses 1.
- A value above 8,760, one year, logs `POLL_INTERVAL_HOURS clamped` and uses 8,760.

## Log level

`LOG_LEVEL` sets the lowest level written: `debug`, `info`, `warn` or `error`. An unknown value logs `invalid LOG_LEVEL, using default` and uses `info`. Configuration warnings are written before the level applies, so a mistyped setting is reported even at `error`. Keep `info` for the shipped log rules: `RegistryStatsCollectionIncomplete` and some `RegistryStatsConfigRejected` conditions read `WARN` lines. See [Monitoring and alerts](monitoring.md#alerting).

## Listen address

`LISTEN_ADDR` is the address the HTTP server binds inside the container, in `host:port` form. Spaces around it are trimmed with a warning. If you change the port, change the container side of the `ports:` mapping to match.

## Ports

| Port | Description |
| --- | --- |
| `9100` | Prometheus metrics on `/metrics` and the readiness check on `/api/health` |

registry-stats keeps no data on disk apart from its health marker in `/tmp`, so it needs no volume.
