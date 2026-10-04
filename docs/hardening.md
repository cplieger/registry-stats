# Security

This page covers what registry-stats exposes, how it treats the pages it reads, the hardened compose settings and what the image contains. It is for operators who run it beside other services or audit its supply chain.

## Exposure

The HTTP server on port 9100 serves `/metrics` and `/api/health` with no login, which is usual for an internal metrics endpoint. Keep the port on your own network. The example compose publishes it on `127.0.0.1` only. For a collector on another host, publish it on a trusted interface, never on every interface. The server sets read-header, read, write and idle timeouts.

registry-stats holds no credential. It reads public GHCR package pages and the unauthenticated Docker Hub API, so a leaked container exposes no registry account.

## How it treats what it reads

- The HTTP client follows redirects only to `docker.com`, `github.com` and `githubusercontent.com` hosts, up to five hops. The registries redirect to their own CDNs and blob stores, and a compromised or misconfigured upstream cannot send a request on to another host.
- URL path segments built from configuration or registry data must match `[A-Za-z0-9._-]`.
- Response bodies are capped at 1 MiB for the Docker Hub API and 2 MiB for a GHCR page. A response over its cap is treated as a format change and is not read in part.
- A `Retry-After` header on a 429 or 503 response is honored for up to 60 seconds.
- Text read from the registries is sanitized and length-capped before it reaches a log line.

One scanner finding is accepted. semgrep flags the use of `math/rand/v2`, which here only spaces out successive GHCR requests within a check and generates no secret.

## Hardened deployment

To lock the container down further, add these lines to the service in `compose.yaml`:

```yaml
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - "/tmp:size=1m,mode=1777,noexec,nosuid,nodev"
```

`read_only: true` needs a writable `/tmp` for the health marker, which the tmpfs supplies. `size=1m` is ample, because the marker is the only file registry-stats writes.

## What the image contains

The image is one statically linked Go binary on the distroless `gcr.io/distroless/static-debian13:nonroot` base, for `amd64` and `arm64`. It runs as the `nonroot` user and has no shell or package manager. The binary uses the Go standard library plus nine libraries by the same author: [envx](https://github.com/cplieger/envx), [health](https://github.com/cplieger/health), [httpx](https://github.com/cplieger/httpx), [metrics](https://github.com/cplieger/metrics), [pathinside](https://github.com/cplieger/pathinside), [runesafe](https://github.com/cplieger/runesafe), [scheduler](https://github.com/cplieger/scheduler), [slogx](https://github.com/cplieger/slogx) and [webhttp](https://github.com/cplieger/webhttp). They supply environment parsing, the health marker, retries and redirects, Prometheus exposition, path checks, log-text bounds, the check loop, logging and the HTTP server. The license text of every one is in the image under `/usr/share/licenses/`.

Every release ships an SPDX software bill of materials on its [release page](https://github.com/cplieger/registry-stats/releases).

## Update automation

[Renovate](https://github.com/renovatebot/renovate) updates every dependency, and each one is pinned by digest or version. That covers the [Go](https://hub.docker.com/_/golang) builder image, the [distroless](https://github.com/GoogleContainerTools/distroless) base image and the Go modules, including [pgregory.net/rapid](https://pkg.go.dev/pgregory.net/rapid), which the tests use and the image does not contain.
