#!/bin/sh
# Runtime image smoke test for registry-stats. Invoked by the central CI docker
# job:  sh tests/image-smoke.sh <image-ref>
#
# Starts the assembled image and waits for the container's own HEALTHCHECK
# (the distroless `registry-stats health` file-marker probe) to report
# "healthy" — proving the binary runs in the distroless image, binds its HTTP
# server, and the health probe works. At least one repo must be configured
# (DOCKERHUB_REPOS on the run line) because the file-marker HEALTHCHECK tracks
# collect success, not just HTTP-server liveness: an empty config is unhealthy.
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-registry-stats-$$"
TIMEOUT=90

# shellcheck disable=SC2317,SC2329  # invoked indirectly via trap
cleanup() {
  code=$?
  # Dump container logs only on failure (a passing run stays quiet).
  if [ "$code" -ne 0 ]; then
    printf '%s\n' "--- container logs (tail) ---" >&2
    docker logs "$NAME" 2>&1 | tail -40 >&2 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A stable, unauthenticated Docker Hub repo so the initial collect succeeds and the
# health marker flips healthy; an empty config reports unhealthy by design.
docker run -d --name "$NAME" -e DOCKERHUB_REPOS=library/alpine "$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
  # Fail fast on an early exit: poll .State.Running before the health status so
  # a crash-boot is caught by its exit code (more debuggable than "unhealthy")
  # and the verdict never depends on what health a stopped container reports.
  if [ "$(docker inspect --format '{{ .State.Running }}' "$NAME" 2>/dev/null || echo missing)" != "true" ]; then
    ec=$(docker inspect --format '{{ .State.ExitCode }}' "$NAME" 2>/dev/null || echo '?')
    printf 'FAIL: registry-stats container exited early (exit code %s)\n' "$ec" >&2
    exit 1
  fi
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      printf 'registry-stats image smoke: ok (healthy after %ss)\n' "$i"
      exit 0
      ;;
    unhealthy)
      printf 'FAIL: registry-stats reported unhealthy\n' >&2
      exit 1
      ;;
    no-healthcheck)
      printf 'FAIL: image has no HEALTHCHECK to assert against\n' >&2
      exit 1
      ;;
    gone)
      printf 'FAIL: registry-stats container is gone\n' >&2
      exit 1
      ;;
  esac
  i=$((i + 1))
  sleep 1
done
printf 'FAIL: registry-stats did not become healthy within %ss (last status: %s)\n' "$TIMEOUT" "$status" >&2
exit 1
