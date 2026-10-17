# ADR-008: A stateless session cookie keyed from NAG_TOKEN

**Date:** 2026-10-17

## Context

This decision was made when the SPEC was written (2026-08-13). It is recorded here because the
SPEC now states only the rules (§8.1).

The browser must stay logged in for months, in a pinned tab and in an installed app, after the
person typed the token once. There is one user.

## Decision

- The session cookie holds an expiry and an HMAC of it. There is no session table and no user
  id.
- The HMAC key is derived from `NAG_TOKEN`. There is no second secret.
- The cookie lasts 365 days and is renewed when it is older than 30 days.
- The cookie is always `Secure`, never derived from `r.TLS`, because the app sees plain HTTP
  behind the reverse proxy.

## Consequences

- There is one secret to manage, and the server stores no session state.
- A rotation of `NAG_TOKEN` logs out every browser at once. This is the only revocation.
- `/logout` clears the cookie in that browser only. A copied cookie stays valid until it
  expires or the token rotates.
- A change to the cookie encoding logs out every browser once.
