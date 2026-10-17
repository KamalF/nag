# ADR-007: Secrets in env, behavior in TOML, channels in the database, no CLI flags

**Date:** 2026-10-17

## Context

This decision was made when the SPEC was written (2026-08-13). It is recorded here because the
SPEC now states only the rules (§3, §5, §7.4).

Nag has three kinds of settings: secrets (the token and the VAPID keys), behavior (timezone,
presets, picker hours, retention), and channel URLs, which contain secrets. The image is
`FROM scratch`, so it has no shell to edit a file inside the container.

## Decision

- Secrets come from the environment (`nag.env`), never from the TOML.
- Behavior comes from `nag.toml` in a mounted directory. A person edits it, checks it with
  `nag config check`, and reloads it with SIGHUP.
- Channel URLs live in the database. The `nag channel` subcommand manages them. The API never
  returns them, and the TOML never holds them.
- No subcommand takes a command-line flag.

## Consequences

- Each value has exactly one place. A flag would be a third place, hidden in the `command:`
  of `compose.yaml`.
- The TOML can be shared, backed up or committed without leaking a key.
- A change of a secret needs a restart. A change of behavior needs only a reload.
- A channel cannot be configured from the web UI. The operator needs a shell on the host.
