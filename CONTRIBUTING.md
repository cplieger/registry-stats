# Contributing to registry-stats

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- Change a log message that `alerts/logql.yaml` matches, or its level, in the same commit as that rule file. Half the matched messages have no test that reads the rules, so rewording one of them silently stops its alert.
- Save a `grafana-dashboard.json` change made in the Grafana UI with Export, then Export as code, choosing the V2 Resource model.

## Releases

A dashboard change republishes `grafana-dashboard.json` as an OCI artifact. It reaches a GitHub Release asset only when the commit type releases. Use a releasing commit type when the dashboard needs a new GitHub Release.
