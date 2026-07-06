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

# shellcheck disable=SC2329  # invoked indirectly via trap
cleanup() {
  echo "--- container logs (tail) ---"
  docker logs "$NAME" 2>&1 | tail -40 || true
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A stable, unauthenticated Docker Hub repo so the initial collect succeeds and the
# health marker flips healthy; an empty config reports unhealthy by design.
docker run -d --name "$NAME" -e DOCKERHUB_REPOS=library/alpine "$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      echo "registry-stats image smoke: ok (healthy after ${i}s)"
      exit 0
      ;;
    unhealthy)
      echo "FAIL: registry-stats reported unhealthy"
      exit 1
      ;;
    no-healthcheck)
      echo "FAIL: image has no HEALTHCHECK to assert against"
      exit 1
      ;;
    gone)
      echo "FAIL: registry-stats container exited early"
      exit 1
      ;;
  esac
  i=$((i + 1))
  sleep 1
done
echo "FAIL: registry-stats did not become healthy within ${TIMEOUT}s (last status: $status)"
exit 1
