# ADR-005: Mark before send, so a reminder notifies at most once

**Date:** 2026-10-17

## Context

This decision was made when the SPEC was written (2026-08-13). It is recorded here because the
SPEC now states only the rules (§7.3).

The sweep writes to the database and sends to push services and channels. A crash, a restart
loop or a slow send can happen between the two. Either the write or the send must come first.

## Decision

Every lifecycle column (`notified_at`, `pushed_at`) is committed before any socket opens. The
sweep marks the rows, commits, and only then sends. A send that fails is not retried, and the
row stays marked.

## Consequences

- A crash loop can never send the same reminder twice. A notification storm is not
  recoverable, so this is the failure that must not happen.
- A crash between the commit and the send loses that notification. The reminder still shows
  in the list and in the badge, so the loss is visible.
- A failed push is not retried. The 30-minute window (ADR-004) is consumed even when the push
  fails, so "one attempt per half hour" stays true.
