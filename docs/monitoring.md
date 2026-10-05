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

registry-stats reports its state in two places, so the rules ship as one file per expression language, and neither ruler parses the other's expressions. Load each file into its own ruler. Firing alerts from both reach your Alertmanager like any other Prometheus alert.

- The five PromQL rules in [`alerts/promql.yaml`](../alerts/promql.yaml) go to Prometheus or the Mimir ruler, over the `/metrics` endpoint you already scrape.
- The three LogQL rules in [`alerts/logql.yaml`](../alerts/logql.yaml) go to Loki's ruler, over the container log. Their conditions leave no series to read. A repo entry rejected at start is never polled. With both repo lists empty, `registrystats_collects_total` has no series either. A check where only a minority of image requests fail still reports a healthy registry. In each case the published counts go incomplete while no metric moves.

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

### Notes on the rules

#### Scrape target rules

`RegistryStatsTargetDown` and `RegistryStatsTargetAbsent` are two rules because neither covers the other. `up == 0` catches a target that is configured and failing. It keeps the target's labels, so the alert names the instance. `absent(up{...})` catches a target that no longer exists, such as a dropped scrape target, a removed scrape config, or a deleted Kubernetes pod or ServiceMonitor. Then `up` has no series, and `up == 0` can never match.

Use an exact `job` matcher, never a regex. A regex form such as `absent(up{job=~".*x.*"} == 1)` asks whether any matching target is up. One healthy replica then masks every failed one, and the result carries no `job` label to route or group on.

#### `RegistryStatsCollectStalled`

The rule looks for no completed check over 15 minutes, held for 6 hours, rather than over 6 hours directly. A counter that has just started has two samples at the same value, which the direct form reads as a stall. The direct form would then fire about 30 minutes after every container start and clear at the first scheduled check. The shipped form fires 6 hours after the last completed check, and a restart resets its pending timer at the startup check.

The rule watches completed checks, not request progress. If outbound requests stop, the Docker healthcheck trips first, after one poll interval plus a three-minute progress lease. A check that keeps sending requests but does not complete within 6 hours pages here on purpose, because registry-stats has published nothing new for that long. The window assumes the default `POLL_INTERVAL_HOURS` of 1. Set the `for:` window to how long you will accept no new data.

Drop the rule in one-shot mode, `POLL_INTERVAL_HOURS=0`, where the counter advances exactly once by design. With no repos configured, the counter has no series and this rule cannot fire. `RegistryStatsConfigRejected` covers that case.

#### `RegistryStatsSourceDegraded`

`registrystats_collect_errors_total` advances once for a check in which a wildcard owner listing wholly fails, or more than half of the registry's image requests fail. Docker Hub rows read from an owner listing count on neither side of that ratio. A minority of failed image requests does not move this counter, and `RegistryStatsCollectionIncomplete` covers that case.

Other configured owners are unaffected, and their series keep updating. Read the log for the owner rather than assuming the whole registry failed. The log names the reason, such as a rate limit, an HTTP failure or a page-format change.

Both per-registry counters start at zero in process memory, but that zero is not a scrape sample. If the first failure happens before the first scrape, the first sample is already positive and `increase()` has no earlier sample to subtract. The uptime arm of the expression covers that startup window, and the `increase()` arm covers later failures. A registry with no configured repos has no series, so the rule cannot fire for a registry you never poll.

#### `RegistryStatsPullCountRegressed`

A failed scrape is skipped rather than exported as zero. A drop below the 2-day maximum therefore means a scrape produced a wrong count without an error, or the registry restated its total. The GHCR download count comes from a parsing heuristic in `internal/ghcr/scraper.go`, and this rule catches the case where that heuristic reads a wrong number silently. If GitHub changed its package page markup, that parser needs an update.

#### `RegistryStatsConfigRejected`

The rule matches six messages. The two repo-ref messages and the two wildcard-empty messages mean the configuration yields nothing to poll. The two `POLL_INTERVAL_HOURS` messages mean polling continues on a corrected interval.

- A repo entry that fails validation is skipped with a `WARN` at start. Write `DOCKERHUB_REPOS` and `GHCR_REPOS` as comma-separated `owner/repo` or `owner/*` pairs.
- A start with no usable entry logs an `ERROR`, then a `WARN` every check.
- A wildcard whose owner publishes no public image logs a `WARN` every check. Make at least one image public.
- A malformed or negative `POLL_INTERVAL_HOURS` reverts to the 1-hour default. A value above 8,760 hours is capped at 8,760. Until you fix the value, registry-stats polls on an interval you did not ask for.

None of these conditions has a metric. A ref that never parsed has no series, and an empty wildcard has no image series. With both lists empty, `registrystats_collects_total` has no series either, so `RegistryStatsCollectStalled` cannot fire. A skip line is logged once per start, so the alert clears one hour after a restart with a corrected value. The no-repos and wildcard-empty lines repeat every check.

#### `RegistryStatsError`

The rule is level-wide. It covers a failed registry request or parse, a changed GHCR page, an unusable configuration, a check in which every registry failed, and a 5xx from registry-stats' own endpoint. Both level comparisons are case-sensitive. The log writes levels in uppercase, so a lowercase matcher matches nothing.

`/api/health` answers 503 at every start until the first check completes, and again during shutdown. The HTTP access log reports a 5xx at `ERROR`, so the last line of the expression excludes that one access record. The exclusion keys on the access record's own `msg`, so diagnostics a failing hook logs for the same request still reach the rule.

#### `RegistryStatsCollectionIncomplete`

The rule reports a check that left image series off `/metrics`, whether or not the registry's health threshold is crossed. `RegistryStatsSourceDegraded` may fire at the same time. Registry health compares the image requests that succeeded with the ones attempted. Docker Hub rows read from an owner listing count on neither side, so a truncated listing can drop images without moving `registrystats_collect_errors_total`.

- `hit page cap` means an owner listing reached its page limit. On Docker Hub, the owner publishes more repositories than an unauthenticated listing can enumerate. On GHCR, a `ghcr listing refused package candidates` record comes with it, and the extra match does not change whether the rule fires.
- `listing partially failed` means an owner listing lost a page.
- `docker hub fetch failed` and `ghcr scrape failed` mean one repository request failed, which removes one image series.
- `ghcr listing refused package candidates` means a listing returned usable refs without an error but left out published candidates. No counter can see this.
- `ghcr listing page states no usable package count` means a listing returned the names it found but could not check that the list is complete.

A GHCR listing that fails wholly logs `ghcr package listing failed`, which this rule does not match. Its refused-candidates record is then this rule's only match, and `RegistryStatsSourceDegraded` reports the outage. A majority of failed GHCR packages also marks the registry unhealthy, so `RegistryStatsSourceDegraded` covers it. One failed explicit repo is a majority when it is the only one. This rule is for minority failures, refused candidates and listings with an unusable package count.

A Docker Hub parse failure also removes one image series. Its `docker hub parse failed` record is always `ERROR`, so `RegistryStatsError` covers it.

### Log level

The LogQL rules assume the default `LOG_LEVEL=info`. `RegistryStatsCollectionIncomplete` matches only lines that can also log at `WARN`, and lines that are always `ERROR` belong to `RegistryStatsError`. At `LOG_LEVEL=error`, these change:

- `RegistryStatsCollectionIncomplete` matches only records caused by markup or response-schema drift. Docker Hub listing and fetch failures and GHCR partial listings log at `ERROR` on those causes, and the rule still matches them by message.
- A Docker Hub metadata response that decodes without a usable pull count logs `docker hub parse failed` at `ERROR` at every level, so `RegistryStatsError` covers it.
- A GHCR listing that fails wholly on a drift cause also logs at `ERROR`. Its message is not one of the six that `RegistryStatsCollectionIncomplete` matches, so `RegistryStatsError` covers it.
- The recurring no-repos `WARN` and both wildcard-empty `WARN` lines are silenced. The no-repos condition stays visible through its startup record until that record leaves the one-hour window. The wildcard-empty conditions reach no rule.

The startup records of `RegistryStatsConfigRejected` work at every level. The repo-ref and `POLL_INTERVAL_HOURS` warnings are written before the configured level applies. The startup no-repos record is an `ERROR`, which no level silences.

### Adapting the rules

Thresholds and the `for:` windows are starting points. The `up{job="registry-stats"}` selector assumes your scrape job is named `registry-stats`, so change it to match your scrape config. Change the `container` selector on the LogQL rules the same way, or to `job` or `service`, depending on your log collector. The LogQL rules extract log fields under an `rs_` prefix. Without it, Loki renames a field to `<name>_extracted` when your collector already sets `level`, `msg`, `path` or `status` as a stream label. Route by whatever labels your Alertmanager uses.
