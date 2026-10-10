# ADR-002: Move go.mod, mise.toml and the Dockerfile to Go 1.27

**Date:** 2026-10-10

Supersedes [ADR-001](ADR-001-go-version-1.26.md).

## Context

ADR-001 put every build on Go 1.26 and, through it, pinned shoutrrr at v0.18.0: v0.19 and
later declare `go 1.27`. Two things made staying there costly:

- **shoutrrr v0.19+ closes a gap the fan-out had to work around.** In v0.18.0 an abandoned
  smtp send had no deadline after its dial and could hang forever (§7.2 assumes it ends on its
  own client's timeout). v0.19 gives smtp a `SendContext` that turns the context's deadline
  into one on the connection; v0.21.2 also adds xmpp, Home Assistant and fixes to send params.
- **Security fixes.** govulncheck reported ten reachable standard-library vulnerabilities in
  the HTTP/2, `net/http`, `crypto/tls` and `mime/multipart` code the push sender and the
  server use, fixed in 1.27.2 (and backported to 1.26.9).

Go 1.26 stays supported until 1.28 ships, so this is a choice, not a forced move.

## Decision

`go.mod` declares `go 1.27`, `mise.toml` pins `go = "1.27.2"`, and the Dockerfile builds
`FROM golang:1.27`. shoutrrr moves to v0.21.2, and every other module to its latest release.
ADR-001's single-version principle is kept unchanged: one Go version for local builds and the
image.

## Consequences

- An abandoned smtp send now ends on its own; the one-send-per-channel cap in
  `internal/notify/channel.go` stays, because pushbullet, opsgenie, notifiarr and generic
  still fall back to an untimed client in v0.21.2.
- Contributors need Go ≥ 1.27, a stricter floor than the code itself requires.
- `encoding/json` runs on the json/v2 engine in 1.27, and its error text may differ;
  `decodeBody` reads that text to name an unknown field, so a test pins the message.
- Every shoutrrr bump means re-auditing `channel.go`'s assumptions about how each service
  builds its HTTP client — the cost this pin bump paid once already.
