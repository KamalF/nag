# Progress

Where the [PLAN.md](../PLAN.md) implementation stands. One plan step = one commit, whose
message ends with `(plan N)`. Tick the box in the commit that lands the step; the hash is
backfilled by the next one, since a commit cannot name its own. `git log --grep '(plan N)'`
finds any of them directly.

Legend: `[x]` landed · `[ ]` not started · `[~]` in progress · `[B]` blocked (reason given)

## M1 — core ✅

| | # | Step | Commit |
|---|---|---|---|
| [x] | 1 | Repo skeleton and subcommand dispatch | `d100319` |
| [x] | 2 | Store: open, pragmas, migrations, initial schema | `de393f6` |
| [x] | 3 | Config: types, embedded default, first-boot write | `7cb8ae8` |
| [x] | 4 | Config validation and `nag config check` | `4caacd2` |
| [x] | 5 | Preset evaluation | `917b0cf` |
| [x] | 6 | `nag genkeys` | `2e7f032` |
| [x] | 7 | Environment loading and boot checks | `75f0c5f` |
| [x] | 8 | HTTP skeleton: static, healthz, API catch-all | `e830047` |
| [x] | 9 | Auth | `56d5c2e` |
| [x] | 10 | Reminders: create and state | `a327734` |
| [x] | 11 | Reminders: done, undone, delete | `2d6c743` |
| [x] | 12 | Reminders: PATCH | `8a2d439` |
| [x] | 13 | State ETag | `d2f669b` |
| [x] | 14 | `/api/config` and `/api/channels` | `4b96f81` |
| [x] | 15 | Sweep, log-only | `22a4fc0` |
| [x] | 16 | Minimal unstyled UI | `8ecbf94` |
| [x] | 17 | **M1 checkpoint** (no commit) | verified 2026-08-19 |

## M2 — notifications

| | # | Step | Commit |
|---|---|---|---|
| [x] | 18 | `flattenLinks` | `0d77603` |
| [x] | 19 | Push subscription endpoints | `e77d80c` |
| [x] | 20 | Push sender and response ladder | `49a716c` |
| [x] | 21 | Sweep phase 2: digest and cooldown | |
| [ ] | 22 | `POST /api/push/test` | |
| [B] | 23 | Manifest and icons | needs the four icon PNGs (pre-flight design item) |
| [ ] | 24 | Service worker | |
| [ ] | 25 | Permission flow and unconditional re-post | |
| [ ] | 26 | Poll engine and page-side worker messages | |
| [ ] | 27 | Badge | |
| [B] | 28 | **Dev deployment on the VPS** (no commit) | needs the VPS, DNS, and physical devices |
| [ ] | 29 | **M2 checkpoint** (no commit) | |

## M3 — design

| | # | Step | Commit |
|---|---|---|---|
| [ ] | 30 | Design tokens and base styling | |
| [ ] | 31 | Layout | |
| [ ] | 32 | Time formatters | |
| [ ] | 33 | Keyed list rendering | |
| [ ] | 34 | Inline row editor | |
| [ ] | 35 | Markdown link rendering (client) | |
| [ ] | 36 | Picker sheet | |
| [ ] | 37 | Channel selector | |
| [ ] | 38 | Toasts, undo, failure states | |
| [ ] | 39 | **M3 checkpoint** (no commit) | |

## M4 — ship

| | # | Step | Commit |
|---|---|---|---|
| [ ] | 40 | `nag channel` subcommand | |
| [ ] | 41 | shoutrrr wrapper, classification, `nag channel test` | |
| [ ] | 42 | Sweep phase 3: fan-out | |
| [ ] | 43 | `delivery_error` surfacing | |
| [ ] | 44 | Sweep phase 4: retention purge | |
| [ ] | 45 | SIGHUP reload and the two hashes | |
| [ ] | 46 | Security headers and Content-Type rule | |
| [ ] | 47 | Graceful shutdown and `nag healthcheck` | |
| [ ] | 48 | Dockerfile, .dockerignore, compose | |
| [ ] | 49 | `nag.env.example` and README | |
| [ ] | 50 | **M4 checkpoint / final acceptance** (no commit) | |

## Commits outside the plan

Corrections and cross-cutting passes belong here, so the table above stays a
one-step-one-commit index.

| Commit | What |
|---|---|
| `57eea7c` | specs and implementation plan |
| `3519d9f` | bootstrap ADR/FDR decision records |
| `3d2ae3e` | `/simplify` pass over M1 (4-angle review) |
| `e29774a` | config: reject empty or `Local` timezone (§5.5) |
| `0072687` | code-review findings — PATCH 404 on vanished row, boot resilience |
