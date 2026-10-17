# Architecture Decision Records

One file per architectural decision, numbered sequentially and never renumbered. A decision
that no longer holds is superseded by a new ADR, not edited away, so that the
history stays.

| # | Decision | Date |
|---|----------|------|
| [ADR-001](ADR-001-go-version-1.26.md) | Align go.mod and Dockerfile on Go 1.26, not the spec's literal 1.24 (superseded by ADR-002) | 2026-08-19 |
| [ADR-002](ADR-002-go-version-1.27.md) | Move go.mod, mise.toml and the Dockerfile to Go 1.27 | 2026-10-10 |
| [ADR-003](ADR-003-proxy-agnostic-compose-and-ghcr.md) | A proxy-agnostic compose.yaml, caddy-docker-proxy as an opt-in add-on, release images on GHCR | 2026-10-11 |
| [ADR-004](ADR-004-one-digest-push-per-half-hour.md) | One digest push per 30 minutes, and nothing for a reminder over 30 minutes late | 2026-10-17 |
| [ADR-005](ADR-005-mark-before-send.md) | Mark before send, so a reminder notifies at most once | 2026-10-17 |
| [ADR-006](ADR-006-server-schedules-browser-displays.md) | The server schedules, the browser displays | 2026-10-17 |
| [ADR-007](ADR-007-env-for-secrets-toml-for-behavior.md) | Secrets in env, behavior in TOML, channels in the database, no CLI flags | 2026-10-17 |
| [ADR-008](ADR-008-stateless-session-cookie.md) | A stateless session cookie keyed from NAG_TOKEN | 2026-10-17 |
| [ADR-009](ADR-009-no-frontend-build-step.md) | No build step for the frontend | 2026-10-17 |
