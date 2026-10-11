# Architecture Decision Records

One file per architectural decision, numbered sequentially and never renumbered. A decision
that no longer holds is superseded by a new ADR, not edited away — the history is the point.

See also: [Feature Decision Records](../fdr/INDEX.md).

| # | Decision | Date |
|---|----------|------|
| [ADR-001](ADR-001-go-version-1.26.md) | Align go.mod and Dockerfile on Go 1.26, not the spec's literal 1.24 (superseded by ADR-002) | 2026-08-19 |
| [ADR-002](ADR-002-go-version-1.27.md) | Move go.mod, mise.toml and the Dockerfile to Go 1.27 | 2026-10-10 |
| [ADR-003](ADR-003-proxy-agnostic-compose-and-ghcr.md) | A proxy-agnostic compose.yaml, caddy-docker-proxy as an opt-in add-on, release images on GHCR | 2026-10-11 |
