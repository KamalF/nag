# ADR-003: A proxy-agnostic compose.yaml, caddy-docker-proxy as an add-on, images on GHCR

**Date:** 2026-10-11

## Context

The spec shipped one `compose.yaml` built for caddy-docker-proxy: an external `caddy`
network and proxy labels. Most self-hosters do not run that stack. For them every
`docker compose` command failed, `genkeys` included, until they created a network their
proxy would never use. And the container published no port their own proxy could reach.

The image was also built on the VPS from a full checkout. There was no published image, so
updating meant `git pull` and a rebuild on the host.

## Decision

- `compose.yaml` assumes no proxy. It publishes `127.0.0.1:8080:8080`, so only an HTTPS
  proxy on the same host can reach the app. The README shows a two-line Caddyfile first.
- `compose.caddy-docker-proxy.yaml` adds only the network and the labels. It is layered on
  the base file through `COMPOSE_FILE` in `.env`, so the documented commands are the same on
  both kinds of host. It is a layered file, not a full copy, so the service has one definition.
- Pushing a `v*` tag runs `.github/workflows/release.yml`. It publishes
  `ghcr.io/kamalf/nag:<version>` and `:latest` for amd64 and arm64. The Dockerfile
  cross-compiles (`--platform=$BUILDPLATFORM`), so the build needs no emulation.
- `compose.yaml` names both the GHCR image and `build: .`. Compose pulls the image and falls
  back to building the checkout if the pull fails.

## Consequences

- A host runs with two files (`compose.yaml`, `nag.env.example`). It does not need a checkout.
  Updating is `docker compose pull` and then `up -d`.
- A proxy must forward the original `Host` header. §10.2's `Origin`-against-`Host` fallback
  depends on it. Caddy does this by default, but nginx does not.
- Under the add-on, the loopback port is still published. Nothing uses it there.
- A new GHCR package is private until it is made public once in the package settings.
  Until then, anonymous pulls fail and Compose builds from the checkout instead.
- `docker compose build` tags its result `ghcr.io/kamalf/nag:latest`, so the next `pull`
  replaces a local build with the released image.
