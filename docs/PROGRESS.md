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

## M2 — notifications (code complete and reviewed; device pass outstanding)

Plan 21–27 went through six independent `/code-review` rounds, each a fresh
reviewer against `49a716c..HEAD`. Findings by round: 6, 6, 4, 4, 4, 0 — the loop
stopped when round 6 came back clean. Rounds 4 and 5 found nothing wrong with the
original code, only with earlier rounds' fixes.

Then three `/simplify` rounds over the same range: 6, 3, 1 changes, stopping when
round 3 turned up nothing in production code. Every change was read and committed
by hand — one was reverted and rewritten, since it "fixed" a comment that was
correct.

| | # | Step | Commit |
|---|---|---|---|
| [x] | 18 | `flattenLinks` | `0d77603` |
| [x] | 19 | Push subscription endpoints | `e77d80c` |
| [x] | 20 | Push sender and response ladder | `49a716c` |
| [x] | 21 | Sweep phase 2: digest and cooldown | `ba55c2d` |
| [x] | 22 | `POST /api/push/test` | `6ca8121` |
| [x] | 23 | Manifest and icons | `b60d6aa` |
| [x] | 24 | Service worker | `eda1e09` |
| [x] | 25 | Permission flow and unconditional re-post | `e2a4d42` |
| [x] | 26 | Poll engine and page-side worker messages | `e3ee132` |
| [x] | 27 | Badge | `c7dfe41` |
| [B] | 28 | **Dev deployment on the VPS** (no commit) | needs the VPS, DNS, and physical devices |
| [~] | 29 | **M2 checkpoint** (no commit) | laptop half is testable now; devices wait on 28 |

### Whole-codebase review before M3

A `/review-loop` over everything since `57eea7c`, three area reviewers plus
a security one per round. Accepted blocker/should-fix by round: 3, 3, 0,
then a `/simplify` pass, then 1, 0. The one blocker was the VAPID `sub`
claim going out as `mailto:mailto:…`.

## M3 — design (code complete; phone pass outstanding)

| | # | Step | Commit |
|---|---|---|---|
| [x] | 30 | Design tokens and base styling | `4e60b3d` |
| [x] | 31 | Layout | `26340e5` |
| [x] | 32 | Time formatters | `aff395f` |
| [x] | 33 | Keyed list rendering | `c2b6af0` |
| [x] | 34 | Inline row editor | `2238ea5` |
| [x] | 35 | Markdown link rendering (client) | `5c0cf13` |
| [x] | 36 | Picker sheet | `dac3e93` |
| [x] | 37 | Channel selector | `8270579` |
| [x] | 38 | Toasts, undo, failure states | `5925c71` |
| [~] | 39 | **M3 checkpoint** (no commit) | desktop verified 2026-10-10 against both mockups, light and dark, in headless Chromium; link renders in the row, flattens in the payload, raw in the editor. Phone half waits on 28 |

## M4 — ship

| | # | Step | Commit |
|---|---|---|---|
| [x] | 40 | `nag channel` subcommand | `ab90a8b` |
| [x] | 41 | shoutrrr wrapper, classification, `nag channel test` | `f882778` |
| [x] | 42 | Sweep phase 3: fan-out | `b419b8c` |
| [x] | 43 | `delivery_error` surfacing | `470623d` |
| [x] | 44 | Sweep phase 4: retention purge | `0cae508` |
| [x] | 45 | SIGHUP reload and the two hashes | `351f47e` |
| [x] | 46 | Security headers and Content-Type rule | |
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
| `daf5750` | this file |
| `4815e35` | `.webmanifest` served as `application/manifest+json` |
| `d53d6c5` | code review round 1 — aborted body read, flash lifetime, async races |
| `781a169` | code review round 2 — unexamined key on the click path, flash ownership |
| `5dd2965` | code review round 3 — a rejected subscribe POST was silent |
| `227883b` | code review round 4 — flash restructured, `pushAvailable` recovers |
| `5fccb27` | code review round 5 — the push test's budget bounds the whole handler |
| `2fa758b` | this file: the M2 review loop |
| `64b8183` | phase 2's lateness bound (§7.3, §7.6) — closed the review's open spec question |
| `f5c6204` | `/simplify` round 1 — one log mechanism, one held-set predicate |
| `4d7b388` | `/simplify` round 2 — the push-test budget derives from `WriteTimeout` |
| `5a5df85` | `/simplify` round 3 — test-helper reuse; loop stopped here |
| `8d89499` | final review — a guard that could not fail, and a one-tick margin |
| `525561e` | whole-codebase review: notify — VAPID `sub` claim, payload size, link gate (SPEC §7.1, §7.3 +1 sentence each) |
| `e3858eb` | whole-codebase review: httpapi — body decoding, login queue, one clock per PATCH |
| `f094500` | whole-codebase review: boot — no key in the FATAL line, located TOML errors |
| `a973120` | whole-codebase review: web — small frontend fixes |
