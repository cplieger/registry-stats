# Monitoring and alerts

This page lists what registry-stats emits, how to load the bundled Grafana dashboard, and the alert rules it ships. It is for operators wiring it into Prometheus, Grafana and Loki.

## Metrics

`GET /metrics` serves Prometheus text format on port 9100, with no login.

- `registrystats_image_pulls_total{registry,owner,repo}`: the current pull count of each image.
- `registrystats_collects_total{source}`: checks per registry, successful and failed.
- `registrystats_collect_errors_total{source}`: failed checks per registry.
- `registrystats_collect_duration_seconds`: a histogram of check durations.
- `go_goroutines`, `go_memstats_heap_alloc_bytes`, `process_uptime_seconds` and the other `process_*` series: runtime metrics.

`registrystats_image_pulls_total` is a gauge even though its name ends in `_total`. A registry can restate its total downward, which a counter may never do, and the rule `RegistryStatsPullCountRegressed` alerts on exactly that. Use `delta()` for pulls over a window, as the dashboard does. `rate()` and `increase()` would read a restated total as a counter reset.

`GET /api/health` is the readiness check: HTTP 503 until the first check publishes data, then `{"status":"ok"}` until shutdown. [How registry-stats works](how-it-works.md#health-and-readiness) explains it beside the Docker healthcheck.

## Logs

registry-stats writes logfmt records to standard error, with UTC timestamps. Levels are uppercase, as in `level=WARN` and `level=ERROR`. Requests to `/metrics` and `/api/health` that succeed are logged at `DEBUG`, so scrapes stay out of the default log. The lines that matter:

| Line | Level | Meaning |
| --- | --- | --- |
| `collection complete` | INFO | A check published its `images=` count |
| `partial collection failure` | WARN | A check published data, but at least one registry failed |
| `no images collected, at least one source failed` | ERROR | A check published nothing, and the container turns unhealthy |
| `skipping unusable repo ref` | WARN | A repo list entry was rejected at start |
| `ghcr HTML format may be changing, majority of scrapes hit format errors` | ERROR | GitHub changed its package pages |

## Dashboard

[`grafana-dashboard.json`](../grafana-dashboard.json) uses PromQL and needs only a Prometheus data source, with no plugin. It shows total downloads, the tracked package count, a per-package table, cumulative downloads and daily download deltas, and filters by registry, owner and repo.

1. Add a scrape job named `registry-stats` in Prometheus, Grafana Alloy or any Prometheus-compatible scraper. Its target is `registry-stats:9100` when the scraper shares a Docker network with the container, or the address you published the port on. The example compose publishes the port on `127.0.0.1`, so a collector on another host needs `"<trusted-ip>:9100:9100"` instead.
2. In Grafana, open Dashboards, then New, then Import, and upload the file.
3. Select your Prometheus or Mimir data source when Grafana asks for one.

The dashboard is versioned with the app. The file at release `<tag>` matches the metrics that image emits, and its `uid` stays the same, so a new import updates the existing dashboard in place. Pin it the way you pin the image, with the tag of the image you run.

- The release asset is `https://github.com/cplieger/registry-stats/releases/download/<tag>/grafana-dashboard.json`, with `grafana-dashboard.json.sha256` beside it. It works as grafana-operator `spec.url`, as the Grafana Helm chart's `dashboards.<provider>.<name>.url`, or as a Terraform `http` data source.
- The OCI artifact is `ghcr.io/cplieger/registry-stats/dashboard:<tag>`, for grafana-operator `spec.oci`.
- Renovate can track either form, with its `github-releases` datasource for the URL and its `docker` datasource for the OCI tag.

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata:
  name: registry-stats
spec:
  instanceSelector:
    matchLabels:
      dashboards: grafana
  oci:
    reference: ghcr.io/cplieger/registry-stats/dashboard:<tag>
    path: grafana-dashboard.json
```

## Alerting

registry-stats reports its state in two places, so the rules ship as one file per expression language, and neither ruler parses the other's expressions. Load each file into its own ruler.

- The five PromQL rules in [`alerts/promql.yaml`](../alerts/promql.yaml) go to Prometheus or the Mimir ruler, over the `/metrics` endpoint you already scrape.
- The three LogQL rules in [`alerts/logql.yaml`](../alerts/logql.yaml) go to Loki's ruler, over the container log. Their conditions leave no series to read. A repo entry rejected at start is never polled. A check where only a minority of image requests fail still reports a healthy registry. In both cases the published counts go incomplete while no metric moves.

| Alert | Fires when | Severity |
| --- | --- | --- |
| `RegistryStatsTargetDown` | `up{job="registry-stats"} == 0` for 15m: the exporter is not being scraped | warning |
| `RegistryStatsTargetAbsent` | `absent(up{job="registry-stats"})` for 15m: the exporter is not a configured scrape target at all | warning |
| `RegistryStatsCollectStalled` | no check has completed within the 6h published-data staleness budget, while the exporter is up and serving its last values | warning |
| `RegistryStatsSourceDegraded` | one registry failed for most of its repos in a check, so those images drop off `/metrics` | warning |
| `RegistryStatsPullCountRegressed` | a tracked image's pull count falls below its 2-day max: a wrong count that did not error | warning |
| `RegistryStatsConfigRejected` | a repo entry was skipped, an `owner/*` matched no public images, no repo was set, or `POLL_INTERVAL_HOURS` was invalid or above 8,760 | warning |
| `RegistryStatsError` | the container logged an `ERROR`, other than the readiness check's 503 at startup and shutdown | warning |
| `RegistryStatsCollectionIncomplete` | a check lost images without failing: a truncated listing, an unreadable listing count, a rate limit or a minority of GHCR packages | warning |

### Notes on three rules

`RegistryStatsCollectStalled` looks for no completed check over 15 minutes, held for 6 hours, rather than over 6 hours directly. A counter that has just started has two samples at the same value, which the direct form reads as a stall. The direct form would then fire about 30 minutes after every container start. Set the `for:` window to how long you will accept no new data. The Docker healthcheck covers a stalled collector separately, through request progress. Drop the rule in one-shot mode, `POLL_INTERVAL_HOURS=0`, where a single check is the point.

`RegistryStatsCollectionIncomplete` covers incomplete output that the registry health counters do not always show. Registry health compares the image requests that succeeded with the ones attempted. Docker Hub rows read from an owner listing count on neither side, so a truncated listing can drop images without moving `registrystats_collect_errors_total`. Keep `LOG_LEVEL` at `info` for this rule and for the check-time conditions of `RegistryStatsConfigRejected`, because they read `WARN` lines. `RegistryStatsConfigRejected` still catches a bad repo entry at any level, because configuration warnings are written before the level applies.

`RegistryStatsError` is level-wide. It covers a failed registry request or parse, a changed GHCR page, an unusable configuration and a 5xx from registry-stats' own endpoint.

### Adapting the rules

Thresholds and the `for:` windows are starting points. The `up{job="registry-stats"}` selector assumes your scrape job is named `registry-stats`, so change it to match your scrape config. Change the `container` selector on the LogQL rules the same way, or to `job` or `service`, depending on your log collector. Route by whatever labels your Alertmanager uses.
