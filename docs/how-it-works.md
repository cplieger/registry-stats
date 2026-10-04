# How registry-stats works

This page explains how registry-stats reads the pull counts, what each check publishes, and what the health signals mean. It is for operators who want to know why a series is missing or why the container turned unhealthy.

## Design

registry-stats is a Prometheus exporter with no storage of its own. Each check reads the current pull count of every configured image and publishes it on `/metrics`. Your Prometheus-compatible server scrapes those values and builds the history. The registries publish only the current cumulative totals, so nothing can fill in the days before the first scrape.

It reads public images only. That removes any registry credential from the container, and private repositories and packages are out of reach.

## Where the counts come from

- Docker Hub counts come from the unauthenticated Docker Hub API. An `owner/repo` entry reads that repo's `pull_count`. An `owner/*` entry reads the owner's listing, whose rows already carry each count.
- GHCR counts come from the public package pages on github.com, because GitHub publishes no API for download counts. The count is the package-wide "Total downloads" figure each package page prints. registry-stats reads the full number from that figure's `title` attribute, so the count is exact. An `owner/*` entry first reads the owner's package listing.

GHCR requests are spaced 2 to 5 seconds apart. Reading HTML depends on GitHub's page layout, and a change to that layout breaks it. registry-stats checks each page against the shape it expects. When most packages in one check fail that test, it logs an `ERROR` line with a link to this repository's issues. The line reads `ghcr HTML format may be changing, majority of scrapes hit format errors`.

## What one check publishes

A check runs both registries and then replaces the published counts with what it read. An image the check could not read has no sample for that check, rather than a 0 that would look like a drop. The series returns on the next check that reads it. This happens when:

- a Docker Hub owner listing reaches its 198-repo cap, or a GHCR listing reaches its fifty-page cap,
- a listing page fails partway through,
- a GHCR listing page prints no usable package count, so completeness cannot be checked,
- one image's request fails, for example on a rate limit.

Each of these logs a line at `WARN`, or at `ERROR` when the cause is a changed page or response format. The alert rule `RegistryStatsCollectionIncomplete` fires on them. A registry where most requests fail in one check, or whose whole owner listing fails, counts as failed in `registrystats_collect_errors_total`.

A check that collects at least one image logs `collection complete` with its `images=` count. A check that collects nothing logs `no images collected, at least one source failed`, `no images found for the configured refs` or `no repos configured`.

## Scheduling

The first check runs in the background as soon as the HTTP server is listening, so a slow first check cannot trip the Docker healthcheck's start period. Later checks follow every `POLL_INTERVAL_HOURS`. Owner wildcards are read again on every check. With `POLL_INTERVAL_HOURS=0`, registry-stats checks once and then only serves the counts.

## Health and readiness

registry-stats reports two different things.

- Liveness, the Docker healthcheck. The `health` subcommand exits 0 while the marker file `/tmp/.healthy` exists and is fresh.
- Readiness, `GET /api/health`. It answers `{"status":"unready","reason":"..."}` with HTTP 503 until the first check publishes data, then `{"status":"ok"}`. Once ready it stays ready until shutdown, so a later failed check does not turn it back to 503. The Docker healthcheck does not read this endpoint.

The process removes a marker left by a previous container before it binds. It creates a new one as soon as the HTTP server is listening, so the container is healthy on boot. Each check then decides the marker. A check that collects at least one image keeps it, even when some requests failed. A check that collects nothing removes it, and `WARN health state changed healthy=false` then means a check produced no data.

In scheduled mode the marker also has a freshness deadline. registry-stats refreshes the marker before every registry request, so a long `owner/*` walk keeps the container healthy for as long as it makes progress. The deadline is one poll interval plus three minutes, which is twice the longest gap the retry and pacing rules allow between two requests. At the default hourly interval that is one hour and three minutes.

A collector that stops sending requests misses the deadline and turns unhealthy, which a restart can fix. In one-shot mode there is no deadline and no later check, so a failed single check stays unhealthy until a restart.

Docker restart policies act on process exit, never on health, so `restart: unless-stopped` leaves an unhealthy container running. An orchestrator that replaces unhealthy tasks, such as Docker Swarm or a Kubernetes liveness probe, restarts it.

## Limits

- Public repositories and packages only.
- No history before the first scrape.
- GHCR counts depend on GitHub's page layout.
- Up to 198 Docker Hub repos and about 1,500 GHCR packages per `owner/*` entry. List more as separate `owner/repo` entries.
