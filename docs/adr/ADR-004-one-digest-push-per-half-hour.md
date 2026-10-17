# ADR-004: One digest push per 30 minutes, and nothing for a reminder over 30 minutes late

**Date:** 2026-10-17

## Context

This decision was made when the SPEC was written (2026-08-13). It is recorded here because the
SPEC now states only the rules (§7.1, §7.3, §7.6).

Reminders often come due close together: a morning batch, or the catch-up after an outage or
a night offline. One notification per reminder turns that into a burst on the phone. After a
long outage, a burst of old reminders is noise: the list already shows them.

## Decision

- The sweep sends at most one push per 30 minutes. A push is a digest: it carries every
  reminder that came due since the last one, with a count `n`.
- A reminder more than 30 minutes late when the sweep first sees it gets no push and no
  fan-out. It appears in the list only. The same number as the cooldown, so that there is one
  rule to remember.
- Every push uses the constant `Topic` `"nag"` and the constant notification `tag`, with
  `renotify: true`. The push service then keeps at most one pending message per device, and
  the tray holds at most one Nag notification.
- The TTL is about 3 days. This is safe only because of the constant `Topic`.
- The badge and the list are never held by the cooldown. Only the notification waits.

## Consequences

- A burst of due reminders is one notification. A device that comes back after a day offline
  gets one notification, not one per reminder.
- A reminder can arrive up to 30 minutes after its due time, when it falls inside a window
  that an earlier push opened.
- After an outage of more than 30 minutes, the overdue reminders arrive silently. The person
  must look at the list.
- The count `n` is the number of reminders in this push, not the overdue total. The badge
  shows the total.
- Channels (ntfy, Telegram) are not rate-limited, so a channel can receive more messages than
  the phone gets notifications.
