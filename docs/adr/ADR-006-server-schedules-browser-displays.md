# ADR-006: The server schedules, the browser displays

**Date:** 2026-10-17

## Context

This decision was made when the SPEC was written (2026-08-13). It is recorded here because the
SPEC now states only the rules (§6, §9.6, §9.11).

A preset such as "Tomorrow 9:00" needs a timezone. The server has one (`general.timezone`).
The browser has its own, which changes when the person travels. Times on screen also need a
timezone.

## Decision

- Presets resolve on the server, in `general.timezone`, also when the person travels.
- The browser shows every instant in its own timezone and locale.
- The picker reads the chosen wall-clock time in the browser's timezone.
- Preset labels are config strings. They do not change with the timezone.
- The client corrects for server clock skew, but never for the timezone.

## Consequences

- Only one preset evaluator exists, in Go. A client-side evaluator needs `kind`, `at`, `days`
  and the timezone, so it is a second implementation in another language.
- From a phone two zones away, the picker gives 09:00 local time, and the "Tomorrow 9:00" chip
  gives 09:00 at the server. A row then reads "tomorrow at 10:00" under a chip labeled 9:00.
  For a single user, this is better than showing the server's wall clock.
