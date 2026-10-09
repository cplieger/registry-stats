# Monitoring and alerts

This page lists what registry-stats emits, how to load its Grafana dashboard, and its alert rules. It is for operators wiring it into Prometheus, Grafana and Loki.

## Metrics

`GET /metrics` serves Prometheus text format on port 9100, with no login.

- `registrystats_image_pulls_total{registry,owner,repo}`: the current pull count of each image.
- `registrystats_collects_total{source}`: checks per registry, successful and failed.
- `registrystats_collect_errors_total{source}`: failed checks per registry.
- `registrystats_collect_duration_seconds`: a histogram of check durations.
- `registrystats_collect_last_success_timestamp_seconds{source}`: the last check in which most reads got an answer, a 404 included.
- `registrystats_collect_complete{source}`: 1 when every read of the last check got an answer.
- `registrystats_image_present{source,owner,repo}`: 1 when read, 0 when definitively absent.
- `registrystats_image_last_pushed_timestamp_seconds{registry,owner,repo}`: GHCR's "Last published" time.
- `registrystats_dockerhub_repository_updated_timestamp_seconds{owner,repo}`: Docker Hub's `last_updated` time.
- `registrystats_image_details_omitted{source}`: images past the detail cap.
- `go_goroutines`, `go_memstats_heap_alloc_bytes`, `process_uptime_seconds` and the other `process_*` series: runtime metrics.

A registry with no configured repos has no per-registry series.

`registrystats_image_pulls_total` is a gauge despite its `_total` name, because a registry can restate its total downward. Use `delta()` over a window, as the dashboard does. `rate()` and `increase()` would read a restated total as a counter reset.

`GET /api/health` is the readiness check, which [How registry-stats works](how-it-works.md#health-and-readiness) explains.

[Push times and presence](how-it-works.md#push-times-and-presence) explains the 250-name cap on detail series and when presence reads 0.

## Logs

registry-stats writes logfmt records to standard error, with UTC timestamps. Levels are uppercase, as in `level=WARN` and `level=ERROR`. Requests to `/metrics` and `/api/health` that succeed are logged at `DEBUG`, so scrapes stay out of the default log. The lines that matter:

| Line | Level | Meaning |
| --- | --- | --- |
| `collection complete` | INFO | A check published its `images=` count |
| `partial collection failure` | WARN | A check published data, but at least one registry failed |
| `no images collected, at least one source failed` | ERROR | A check published nothing, and the container turns unhealthy |
| `skipping unusable repo ref` | WARN | A repo list entry was rejected at start |
| `ghcr HTML format may be changing, majority of scrapes hit format errors` | ERROR | GitHub changed its package pages |
| `ghcr package field unreadable` | WARN | A package page had no readable push time |
| `image details omitted` | WARN | More than 250 names are tracked |

## Dashboard

[`grafana-dashboard.json`](../grafana-dashboard.json) needs Grafana 13.2 or newer. It uses PromQL and needs only a Prometheus data source, with no plugin. It filters by registry, owner and repo, and adds Docker Hub and GHCR together per package until you turn on Split by registry. Its three tabs are Overview, Packages and Collector health, whose Job list scopes it to one scrape job. Every download figure covers the dashboard time range, except the all-time total that Images nobody pulled shows beside each image. A chart's bar counts a whole hour, 6 hours or day, so the first bar can start before the range and the newest part of the range waits for its bar. A change compares it with the range of the same length just before it, and shows only once the counts reach back to the start of that earlier range.

Registries out of step lists a package that one registry has and the other definitively does not, pairing images by owner and name. A GHCR package with a slash in its name is never compared, because a Docker Hub repository name cannot hold one. With `owner/*` on both, a package you publish to one registry on purpose stays listed while the configuration covers it. A stale or incomplete read, or a stopped collector, shows an Unknown row.

Collector health shows the worst selected collector, except the checks and restarts panels, which add collectors up, and Check duration, which pools them. A collector counts once however many tenants hold its series, and a package counts once whether collectors read it together or in turn. The age colours and the 2-hour stale cutoff of Registries out of step assume the default hourly check, so a longer `POLL_INTERVAL_HOURS` reads stale between checks.

1. Add a scrape job named `registry-stats` in Prometheus, Grafana Alloy or any Prometheus-compatible scraper. Its target is `registry-stats:9100` when the scraper shares a Docker network with the container, or the address you published the port on. The example compose publishes the port on `127.0.0.1`, so a collector on another host needs `"<trusted-ip>:9100:9100"` instead.
2. Load the dashboard, as [Importing an app's dashboard](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#importing-an-apps-dashboard) shows.
3. Pick your Prometheus or Mimir data source in the Data source list at the top of the dashboard.

The dashboard is versioned with the app, so pin it to the tag of the image you run. Its `metadata.name` is `registry-stats`, which Grafana uses as the dashboard UID, so importing a newer copy, or a file provider reading it, updates it in place.

- The release asset is `https://github.com/cplieger/registry-stats/releases/download/<tag>/grafana-dashboard.json`, with `grafana-dashboard.json.sha256` beside it. It works as the Grafana Helm chart's `dashboards.<provider>.<name>.url` with `curlOptions: "-sLf"`, because the release URL redirects, or as a Terraform `http` data source.
- The OCI artifact is `ghcr.io/cplieger/registry-stats/dashboard:<tag>`, with the artifact type `application/vnd.grafana.dashboard.v2+json`.
- Renovate can track the release URL with its `github-releases` datasource.

On Grafana 13.1 or older, use the `grafana-dashboard.json` of release [v4.1.1](https://github.com/cplieger/registry-stats/releases/tag/v4.1.1), the last one in the older dashboard format. That file gets no further changes, so you maintain it yourself.

The file is a Grafana dashboard resource with `apiVersion: dashboard.grafana.app/v2`, which grafana-operator's `GrafanaDashboard` does not accept. Load it with a `GrafanaManifest`, as grafana-operator's [dashboards v2 example](https://grafana.github.io/grafana-operator/docs/examples/manifests/dashboards-v2/) shows. A `GrafanaManifest` takes the dashboard inline and has no URL or OCI source. Copy the `spec` object of the release's `grafana-dashboard.json` in place of the comment below, and copy it again when you move to a new tag.

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaManifest
metadata:
  name: registry-stats
spec:
  instanceSelector:
    matchLabels:
      dashboards: grafana
  template:
    apiVersion: dashboard.grafana.app/v2
    kind: Dashboard
    metadata:
      name: registry-stats
    spec:
      # The spec object of grafana-dashboard.json, unchanged.
```

For a Grafana instance the operator does not manage, also set `namespace` under `template.metadata` to that instance's namespace. You can leave it out when the `Grafana` resource sets `tenantNamespace` in `spec.external`.

If a `GrafanaDashboard` already loads this dashboard from an older tag, keep it on that tag, including in any Renovate rule that bumps it. On grafana-operator v5.25.0, a `GrafanaDashboard` that receives a file in this format deletes the dashboard it manages. The issue is [grafana/grafana-operator#2955](https://github.com/grafana/grafana-operator/issues/2955). Replace it with the `GrafanaManifest` above.

With Terraform, the Grafana provider's [`grafana_apps_dashboard_dashboard_v2`](https://registry.terraform.io/providers/grafana/grafana/latest/docs/resources/apps_dashboard_dashboard_v2) resource takes the file's `spec` object as JSON, and the dashboard's name as `uid`:

```hcl
data "http" "registry_stats_dashboard" {
  url = "https://github.com/cplieger/registry-stats/releases/download/<tag>/grafana-dashboard.json"
}

resource "grafana_apps_dashboard_dashboard_v2" "registry_stats" {
  metadata {
    uid = "registry-stats"
  }
  spec {
    json = jsonencode(jsondecode(data.http.registry_stats_dashboard.response_body).spec)
  }
}
```

## Alerting

registry-stats reports its state in two places, so the rules ship as two files, one for each ruler. [Loading metric alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-metric-alert-rules) and [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) show how to load them.

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

The rule looks for no completed check over 15 minutes, held for 6 hours, rather than over 6 hours directly. A counter that has just started has two samples at the same value, which the direct form reads as a stall. The shipped form fires 6 hours after the last completed check, and a restart resets its pending timer at the startup check.

The rule watches completed checks, not request progress. If outbound requests stop, the Docker healthcheck trips first, after one poll interval plus a three-minute progress lease. A check that keeps sending requests but does not complete within 6 hours pages here on purpose, because registry-stats has published nothing new for that long. The window assumes the default `POLL_INTERVAL_HOURS` of 1. Set the `for:` window to how long you will accept no new data.

Drop the rule in one-shot mode, `POLL_INTERVAL_HOURS=0`, where the counter advances exactly once by design. With no repos configured, the counter has no series and this rule cannot fire. `RegistryStatsConfigRejected` covers that case.

#### `RegistryStatsSourceDegraded`

`registrystats_collect_errors_total` advances once for a check in which a wildcard owner listing wholly fails, or more than half of the registry's image requests fail. Docker Hub rows read from an owner listing count on neither side of that ratio. A minority of failed image requests does not move this counter, and `RegistryStatsCollectionIncomplete` covers that case.

Other configured owners are unaffected, and their series keep updating. Read the log for the owner rather than assuming the whole registry failed. The log names the reason, such as a rate limit, an HTTP failure or a page-format change.

Both per-registry counters start at zero in process memory, but that zero is not a scrape sample. If the first failure happens before the first scrape, the first sample is already positive and `increase()` has no earlier sample to subtract. The uptime arm of the expression covers that startup window, and the `increase()` arm covers later failures.

#### `RegistryStatsPullCountRegressed`

A failed scrape is skipped rather than exported as zero. A drop below the 2-day maximum therefore means a scrape produced a wrong count without an error, or the registry restated its total. The GHCR download count comes from a parsing heuristic in `internal/ghcr/scraper.go`, and this rule catches the case where that heuristic reads a wrong number silently. If GitHub changed its package page markup, that parser needs an update.

#### `RegistryStatsConfigRejected`

The rule matches six messages. The two repo-ref messages and the two wildcard-empty messages mean the configuration yields nothing to poll. The two `POLL_INTERVAL_HOURS` messages mean polling continues on a corrected interval.

- A repo entry that fails validation is skipped with a `WARN` at start. Write `DOCKERHUB_REPOS` and `GHCR_REPOS` as comma-separated `owner/repo` or `owner/*` pairs.
- A start with no usable entry logs an `ERROR`, then a `WARN` every check.
- A wildcard whose owner publishes no public image logs a `WARN` every check. Make at least one image public.
- A malformed or negative `POLL_INTERVAL_HOURS` reverts to the 1-hour default. A value above 8,760 hours is capped at 8,760. Until you fix the value, registry-stats polls on an interval you did not ask for.

None of these conditions has a metric, as the Alerting list above explains. A skip line is logged once per start, so the alert clears one hour after a restart with a corrected value. The no-repos and wildcard-empty lines repeat every check.

#### `RegistryStatsError`

The rule is level-wide. It covers a failed registry request or parse, a changed GHCR page, an unusable configuration, a check in which every registry failed, and a 5xx from registry-stats' own endpoint. Both level comparisons are case-sensitive. The log writes levels in uppercase, so a lowercase matcher matches nothing.

`/api/health` answers 503 at every start until the first check completes, and again during shutdown. The HTTP access log reports a 5xx at `ERROR`, so the second line filter excludes that one access record. The exclusion keys on the access record's own `msg`, so diagnostics a failing hook logs for the same request still reach the rule.

#### `RegistryStatsCollectionIncomplete`

The rule reports a check that left image series off `/metrics`, whether or not the registry's health threshold is crossed. `RegistryStatsSourceDegraded` may fire at the same time. A truncated listing can drop images without moving `registrystats_collect_errors_total`, because listing rows count on neither side of the health ratio.

- `hit page cap` means an owner listing reached its page limit. On Docker Hub, the owner publishes more repositories than an unauthenticated listing can enumerate. On GHCR, a `ghcr listing refused package candidates` record comes with it, and the extra match does not change whether the rule fires.
- `listing partially failed` means an owner listing lost a page.
- `docker hub fetch failed` and `ghcr scrape failed` mean one repository request failed, which removes one image series.
- `ghcr listing refused package candidates` means a listing returned usable refs without an error but left out published candidates. No counter can see this.
- `ghcr listing page states no usable package count` means a listing returned the names it found but could not check that the list is complete.

A GHCR listing that fails wholly logs `ghcr package listing failed`, which this rule does not match. Its refused-candidates record is then this rule's only match, and `RegistryStatsSourceDegraded` reports the outage. A majority of failed GHCR packages also marks the registry unhealthy, so `RegistryStatsSourceDegraded` covers it. One failed explicit repo is a majority when it is the only one.

A Docker Hub parse failure also removes one image series. Its `docker hub parse failed` record is always `ERROR`, so `RegistryStatsError` covers it.

### Log level

The LogQL rules assume the default `LOG_LEVEL=info`. `RegistryStatsCollectionIncomplete` matches only lines that can also log at `WARN`, and lines that are always `ERROR` belong to `RegistryStatsError`. At `LOG_LEVEL=error`, these change:

- `RegistryStatsCollectionIncomplete` matches only records caused by markup or response-schema drift. Docker Hub listing and fetch failures and GHCR partial listings log at `ERROR` on those causes, and the rule still matches them by message.
- A GHCR listing that fails wholly on a drift cause also logs at `ERROR`. Its message is not one of the six that `RegistryStatsCollectionIncomplete` matches, so `RegistryStatsError` covers it.
- The recurring no-repos `WARN` and both wildcard-empty `WARN` lines are silenced. The no-repos condition stays visible through its startup record until that record leaves the one-hour window. The wildcard-empty conditions reach no rule.

The startup records of `RegistryStatsConfigRejected` work at every level. The repo-ref and `POLL_INTERVAL_HOURS` warnings are written before the configured level applies. The startup no-repos record is an `ERROR`, which no level silences.

### Adapting the rules

Thresholds and the `for:` windows are starting points. The `up{job="registry-stats"}` selector assumes your scrape job is named `registry-stats`, so change it to match your scrape config. Change the `container` selector on the LogQL rules the same way, or to `job` or `service`, depending on your log collector. The LogQL rules match the text of each log line with no parser, so labels your collector adds, such as `level` or `msg`, cannot change what they match. Route by whatever labels your Alertmanager uses.
