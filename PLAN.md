# Nag — implementation plan

A finer slicing of SPEC.md's four milestones (§11) into small, atomic, individually
reviewable commits. This is the *fil rouge*: it says what each commit contains, why it
sits where it does, and what the review of that commit should focus on. It deliberately
stays at the level of behaviour and spec sections — no code design here.

## Ground rules for every commit

- **It compiles and `go test ./...` is green.** Never a "wire it up later" commit.
- **Tests land in the same commit as the behaviour they pin.** A reviewer reads the
  rule and its proof together.
- **Scope = the spec sections named on the commit.** If a change doesn't trace to the
  named sections, it belongs to another commit.
- **Log lines (§10.4) accrete with their feature**, not as a logging commit at the end.
- Milestone boundaries below follow §11, with three small deliberate deviations, each
  flagged where it happens: `flattenLinks` is pulled into M2 (the push payload needs
  it); the notification body ships with a provisional formatter in M2, replaced by
  the real §9.11 formatters in M3; and `nag version` is built in commit 1 even though
  §11 files it under M4 — it is a few lines and the dispatch skeleton wants one real
  subcommand to prove itself with.

Each milestone ends with a **checkpoint** — the §11 *done when* checks, run by hand.
Checkpoints are gates, not commits; the per-milestone counts below exclude them.

## Pre-flight (before commit 1)

Resolved or owned preparation items, so the commits can assume them:

- **Git / GitHub** — owner handles this before starting. Includes a `.gitignore`
  (`nag.env`, `data/`, `config/`, `*.db*` — a different file from M4's
  `.dockerignore`) and a commit 0 landing `SPEC.md`, this plan, and the two
  reference mockups.
- **Go version** — the spec's `FROM golang:1.24` (§10) reads as "recent stable at
  spec-writing time"; the actual floor is Go 1.22 for the `ServeMux` patterns (§2).
  Decision: align `go.mod` and the Dockerfile on one current version (1.26 matches
  the local toolchain) — nothing in the spec depends on 1.24 specifically.
- **shoutrrr fork verification (§2)** — done 2026-08-12 against pkg.go.dev.
  Module path: **`github.com/nicholas-fedor/shoutrrr`**, actively maintained,
  latest **v0.17.0** (Aug 2026). `router.Send(message, params) []error` is
  unchanged (no context at router level), **but** the fork has grown both things
  §7.2 says to look for: `NewWithOptions` injects a caller-supplied `http.Client`
  and timeout into services that support it, and v0.17.0 added a per-service
  `ContextSender` interface (`SendContext(ctx, message, params) error`) — optional,
  so not every service implements it. Consequence for commit 41: use `SendContext`
  where a service offers it and the injected client elsewhere, but keep the
  goroutine+`select` wrapper as the actual guarantee — §7.2 is explicit that the
  bound must never depend on the library.
- **VPS prerequisites** — a box with caddy-docker-proxy, the external `caddy`
  network, and a DNS record must exist **by step 28 (M2's device testing)**, not
  M4: phones cannot reach `localhost`, and Web Push needs real HTTPS.
- **Icon assets** — the four PNGs (32, 192, 512, 512-maskable) need a design
  source; produce them before commit 23, which is otherwise blocked on a non-code
  input.

---

## M1 — core (16 commits)

Order inside M1: foundations (skeleton → store → config → presets → CLI plumbing),
then the HTTP surface (skeleton → auth → reminders), then the sweep, then a minimal UI
on top. Each layer only depends on the ones before it.

### 1. Repo skeleton and subcommand dispatch
`go.mod`, the §3 directory layout, `cmd/nag/main.go` dispatching on `os.Args[1]`,
the usage block (printed for missing/unknown/-h, exit 2), `nag version` (a flagged
deviation — see ground rules).
**Review:** dispatch semantics match §3 exactly — no flags anywhere, no default
subcommand in Go (that lives in the Dockerfile), no index panic on empty args.

### 2. Store: open, pragmas, migrations, initial schema
`internal/store`: the §4.3 DSN and connection settings, the §4.2 migration runner
(`user_version`, refuse on a newer DB), migration 1 = the full §4 schema.
Tests: fresh file is created and migrated; re-open is a no-op; a `user_version`
ahead of the binary refuses to start naming both numbers.
**Review:** schema is byte-faithful to §4 (columns, index, `UNIQUE`s); refusal
wording; refuse-to-start on unopenable file (§4.3).

### 3. Config: types, embedded default, first-boot write
`internal/config`: the config struct, `nag.default.toml` embedded in the package
(§3), load from `NAG_CONFIG`, write the default out when the path is absent, refuse
to start when the file is present but unreadable/broken (§5.3).
**Review:** the absent-vs-broken distinction — the default is written *only* on a
genuinely missing path; the shipped default matches §5.3's sample.

### 4. Config validation and `nag config check`
The full §5.4 per-kind field matrix and §5.5 rule list, including decoder strict
mode (unknown key/section anywhere is an error). `nag config check` on top (§5.5).
Tests: one case per §5.5 rule, plus a cross-kind field and an unknown key.
**Review:** every error names the preset `key` or `[section].field` and the rejected
value; the rejected-fields table is complete; zero/negative `offset` is caught.

### 5. Preset evaluation
`internal/presets`: the three kinds per §6, all arithmetic in `general.timezone`.
Tests: the §6 required list — DST spring-forward/fall-back on `clock`, the
in-the-gap `at`, `weekday` on its own day for both `same_day_ok` values, `offset`
across midnight, `clock` at 23:50, `clock` with `days = 0` both sides of `at`.
**Review:** calendar-day arithmetic (never `+24h`), gap normalisation asserted not
special-cased.

### 6. `nag genkeys`
§5.2: complete pipeable env file on stdout, advisory text on stderr only, the
placeholder subject emitted literally.
**Review:** stdout purity (nothing but the four lines), values well-formed
(43-char token, valid VAPID pair).

### 7. Environment loading and boot checks
The §5.1 table: required vars, the token length floor, VAPID shape checks, subject
placeholder rejection, the FATAL message with the fix, and the per-subcommand
requirements matrix (only `serve` enforces the full table; `channel` needs only
`NAG_DB`, etc. — §5.1). Every DB-opening subcommand goes through the one
`store.Open` (migrates on first touch).
**Review:** each refusal names the variable, expected shape, and what was found;
`genkeys` runs with nothing set.

### 8. HTTP skeleton: static, healthz, API catch-all
`internal/httpapi` + `web/embed.go`: serve the embedded `web/` (placeholder
`index.html` for now), `GET /` unauthenticated (§8.1), `/healthz` with its
`SELECT 1` (§8.2), the explicit `/api/` catch-all 404 in the §8.3 error shape, the
`{"error": …}` shape for 4xx/5xx including the fixed 5xx sentence, request body
cap and server timeouts (§8.2), request logging rules (§10.4: mutations and errors
only), and the §10.4 boot lines as far as they exist at this point — version,
address, resolved paths, timezone, preset count (the `config_version` and
subscription-count lines join in commits 14 and 19).
**Review:** mux registration order vs §8.2's catch-all reasoning; the documented
404-not-405 behaviour; the 5xx sentence is the constant one.

### 9. Auth
§8.1 in full: cookie format (every pinned piece — decimal exp, unpadded base64url,
the derived key, attributes), sliding renewal, `/login` with the sleep+mutex rate
limit, `/logout`, bearer on `/api/*`, the auth middleware.
Tests: cookie round-trip, tamper rejection, expiry, renewal threshold computed from
the `maxAge` constant, bearer comparison.
**Review:** each §8.1 bullet against the code — this is the commit where "two
readings are two incompatible implementations", so the review is a line-by-line
check of the pinned encoding.

### 10. Reminders: create and state
`POST /api/reminders` (exactly one of `preset`/`due_at`, the §8.3 validation set
for `text`/`due_at`/`preset`, backdated writes stamped per §4.1, and
`extra_channels` acceptance — max 16, de-duplicated and sorted before write,
names must exist in `channels`, §8.3) and `GET /api/state` (single `now` per
request, server-sorted lists with the id tiebreak, `overdue_count`,
`server_time`, reminder object shape with `pushed_at` absent — §8.2; the
`config_version` and `push_subscribed` fields join in commits 14 and 19).
Tests: the validation matrix; a backdated create carries both stamps;
canonicalisation of `extra_channels`; the overdue/later split and count derive
from one clock read.
**Review:** §4.1 write-path semantics; the reminder object field list; `text`
returned raw.

### 11. Reminders: done, undone, delete
`/done` (no-op on already-done, original `done_at` kept), `/undone` (the guarded
`pushed_at` stamp, both §4.1 halves — fired-and-held vs never-fired), `DELETE`.
Tests: every §4.1 bullet naming one of these operations, including the no-op
mirrors.
**Review:** the undone guard is the whole commit — read §4.1's undone bullet
first, then the tests, then the code.

### 12. Reminders: PATCH
The full §8.3 PATCH machinery: presence detection via raw message map,
unknown-field rejection naming the lexically first key, re-time rules (§4.1),
`extra_channels` re-canonicalisation + the clear-`delivery_error`-on-change rule,
the orphan-carrying asymmetry (stored names pass, new unknown names 400).
Tests: absent vs null vs empty, empty object, both-timing-fields, each re-time
consequence, orphan carry vs introduction.
**Review:** §8.3's PATCH bullet and §4.1's re-time and clear-on-change bullets,
line by line against the tests.

### 13. State ETag
Weak ETag on `/api/state` excluding `server_time`, `If-None-Match` → 304 (§8.2).
Tests: the 304 fires across two requests with unchanged data; changing a row
changes the tag; `server_time` alone doesn't.
**Review:** small on purpose — the exclusion rule is subtle and deserves its own
diff.

### 14. `/api/config` and `/api/channels`
§8.2 shapes: the config projection (`{key,label,quick}` only — no kinds, no
timezone), picker block, `vapid_public`, `config_version` (a static counter until
M4's reload makes it move) — surfaced in **both** `/api/config` and `/api/state`
(§8.2); `/api/channels` as `[{name, enabled}]` ordered by name (returns `[]`
until M4 populates the table). The boot line for `config_version` joins the
commit-8 set.
**Review:** the projection excludes everything §5.5's two-hash argument says it
must; no URL field in channels; the two endpoints report the same counter.

### 15. Sweep, log-only
The sweep goroutine: 30 s ticker, one immediate run at boot, one `now` per tick,
phase 1 exactly as §7.3 (mark, `LIMIT 50`, the too-late gate with its INFO line) —
but *logging instead of sending* (§11 M1). No phases 2–4 yet.
Tests: a marked row never fires twice (idempotency); a backdated row is invisible
to phase 1 and never logged as notified; the gate stamps both columns.
**Review:** phase 1's transaction shape; the single clock read; log lines carry
ids, never text.

### 16. Minimal unstyled UI
`web/`: capture bar (input + preset chips from `/api/config` in file order,
chips disabled while empty, save-on-tap, clear-on-success), the two zones from
`/api/state`, `Clear`, the 45 s visible poll, token field on 401, `location.reload()`
after login (§8.1, §9.2 behaviourally — zero styling).
**Review:** no client-side persistence; the client resolves `default_preset` on
Enter (server has no fallback, §8.3); chip disabled state.

### 17. ✅ M1 checkpoint (no commit)
§11 M1 *done when*: a UI-created reminder lands in the right zone; the sweep logs
it within 30 s; `genkeys >> nag.env` boots once the subject is edited and names
exactly that when it isn't.

---

## M2 — notifications (10 commits)

Order: server-side sending machinery first (subscription endpoints, sender, sweep
phases), then the browser half (manifest → worker → permission flow → poll engine →
badge). Each browser commit is manually testable against the already-finished
server half. The milestone ends with a dev deployment on the VPS (step 28), because
its checkpoint includes the phone and a phone needs real HTTPS.

### 18. `flattenLinks`
The server-side §9.10 helper with the full case table as Go tests.
*Deviation flagged in the ground rules: §11 files markdown under M3, but the digest
payload (commit 21) needs flattened text, so the server half moves here. The client
renderer stays in M3.*
**Review:** the eligibility gate (scheme + no-`(`) matches the renderer's rule;
every table row has a test.

### 19. Push subscription endpoints
`POST /api/push/subscribe` (shape checks on all three fields, upsert that
overwrites `p256dh`/`auth`/`vapid_public` and preserves `created_at`, records the
*client's* key — §8.2, §7.1) and `/api/push/unsubscribe`. Both 204, no body.
Both log the truncated endpoint form in the reminder-id field, never the URL
(§10.4). `push_subscribed` joins `/api/state` here (`COUNT > 0`, instance-wide —
§8.2), and the subscription-count boot line completes the commit-8 set.
Tests: upsert semantics; each shape-check 400; the stored key is the posted one.
**Review:** the upsert column list — this is what makes key rotation recoverable;
the log lines carry the truncated form only.

### 20. Push sender and response ladder
The one send-and-handle helper both the sweep and `/api/push/test` will share
(§7.5): context-carrying send with the §7.1 options (TTL, constant `Topic`,
urgency), the 404/410 delete, the 401/403 stale-key branch, truncated-endpoint
logging (§7.1, §10.4), the boot mismatch WARNING.
Tests against a fake push service: each ladder branch, the truncation form.
**Review:** one helper, not two ladders; endpoint never logged in full.

### 21. Sweep phase 2: digest and cooldown
Phase 2 per §7.3: the held-set query, predicate-not-id-list UPDATE, in-memory
`last_push_at` starting at zero, window consumed on selection, sequential sends,
zero-subscription WARNING; the §7.1 payload (both forms, three texts, truncation,
`due_at`/`id` presence rules).
Tests: the §7.6 checklist rows §11 assigns to M2, against a fake clock and fake
sender — two-rows-one-digest, held-then-sent with correct `n`, cleared/re-timed
while held, 31-minutes-late stamped silently, the 60-row backlog split, the
cleared-before-fired undo pair, the cleared-while-held undo, failed-sends-consume-
window, and more per the list.
**Review:** the biggest commit of M2 — review is the §7.6 table read against the
test names, one row at a time.

### 22. `POST /api/push/test`
§7.5: fixed payload, 3 s per send under the 12 s budget, `not attempted` entries,
the closed result vocabulary (§8.2), cooldown bypassed and window untouched,
its §10.4 log line (attempted count + deleted count).
Tests: budget exhaustion produces complete results; a stale-key 403 reports
`deleted: true`; empty subscriptions → `{"results":[]}`.
**Review:** budget arithmetic vs `WriteTimeout`; vocabulary matches §8.2's closed
set exactly.

### 23. Manifest and icons
`manifest.webmanifest` with the §9.3 minimum content; the four PNGs in
`web/icons/` (192, 512, 512-maskable, 32) from the pre-flight design source.
Must land before commits 24 and 27, which name these files.
**Review:** `id`/`start_url`/`scope`/`display` values; maskable safe zone; nothing
else sneaks into the manifest.

### 24. Service worker
`sw.js` per §9.5: `push` (one notification always, both payload table rows, the
malformed-payload fallback, `waitUntil` around draw + `postMessage`), constant
`tag` + `renotify`, `icon`/`badge` from commit 23's PNGs, `notificationclick`
(focus-or-open, `?focus=<id>` on the open-window path, message type per payload),
`pushsubscriptionchange` (key from `/api/config`), `skipWaiting`/`claim`, no
fetch handler. In `app.js`: unconditional registration, the single
feature-detect boolean, register-rejection handling. Notification body uses a
provisional short formatter — replaced in M3 (commit 32).
**Review:** the §9.5 bullet list is the checklist; especially `waitUntil`
coverage, `includeUncontrolled`, and the no-`data.id` branches.

### 25. Permission flow and unconditional re-post
§9.8: the notification slot and its visibility conditions, the awaited re-post
before the slot renders, key compare in unpadded base64url with the
null-readback rule, subscribe flow on click, `denied` line, iOS standalone line.
**Review:** the slot's four conditions; the compare-then-post order; the decode/
encode round-trip being exact inverses.

### 26. Poll engine and page-side worker messages
The client fetch machinery §9.3 specifies around `/api/state`: single
`AbortController` (aborted responses write nothing, ETag included), 304 handling
(re-render from memory, clock from `Date` header), server-clock offset and the
seconds-pinned `now()`, hidden-tab 5-minute cadence, `visibilitychange` and
post-mutation refetches, the one-shot due timer with its ~23-day clamp. Plus the
page-side half of §9.5: the message switch (`state-changed` → refresh only;
`focus-reminder` → refresh, scroll into view, ~2.4 s flash honouring
`prefers-reduced-motion`), `?focus=<id>` parsed from `location.search` and
treated as the same message with `history.replaceState` after, and the
id-in-neither-list → do-nothing rule.
**Review:** the abort rule ("newest request is the only writer"); the 304 path
re-rendering; ms/seconds conversion confined to its two call sites; both message
types and the URL path behaving per §9.5.

### 27. Badge
`setBadge(n)` per §9.3: Badging API + canvas favicon (pinned colours, 9+ rule,
drawn onto commit 23's `32.png`) + title prefix from a constant base;
`setBadge(0)` as a full reset. Driven by the same render pass as the list.
**Review:** the three outputs reset correctly; the favicon pair hardcoded, not
token-read.

### 28. 📱 Dev deployment on the VPS (no commit — infrastructure)
Deploy the current build to the VPS behind caddy-docker-proxy over real HTTPS so
devices can reach it. The M4 Dockerfile doesn't exist yet — this is a throwaway
dev deployment (a trivial dev container or the bare `CGO_ENABLED=0` binary behind
Caddy), and nothing about it is a deliverable; the production packaging is
commits 48–49 and gets its own acceptance at step 50. Prerequisites are the
pre-flight VPS items (caddy network, DNS).
Then the **device pass**, per §7.1's platform table:
- **Android** (Chrome and/or Firefox): subscribe from a normal tab, close the
  tab, receive an OS notification at the due time, click → app focuses and the
  row flashes.
- **iPhone** (iOS 16.4+): confirm the notification slot shows the Home-Screen
  line in Safari, install via Add to Home Screen, type the token once inside the
  installed app (the §11 cookie-partition note), subscribe, and receive a push
  with the app closed.
- **Both**: the badge/title behaviour on the phone, capture from the phone
  keyboard (no zoom on focus, no autofocus — §9.9), and a digest (`n > 1`)
  arriving as one notification.

### 29. ✅ M2 checkpoint (no commit)
§11 M2 *done when*: laptop Firefox fires an OS notification with the tab closed;
click focuses and flashes the row; `/api/push/test` via curl reports a live
endpoint; and the step-28 device pass is green on both platforms.

---

## M3 — design (9 commits)

Order: tokens before layout, formatters and keyed rendering before the editor
(the editor depends on both), the picker last among the big pieces (it depends on
formatters + editor), polish at the end. Everything here is verified by hand
against the two mockups — no JS test runner exists by design (§11).

### 30. Design tokens and base styling
`app.css`: the §9.1 token table on `:root` + the `prefers-color-scheme` media
query (no `data-theme` anywhere), font stacks, base typography. The §9.7
interaction states (tap highlight, `:active`, `:focus-visible`).
**Review:** token values byte-checked against the §9.1 table (the light-set
deviations are deliberate); no inline styles; the mockups' switcher machinery not
carried over.

### 31. Layout
The §9.2 column: capture bar styling, zones with headings/count/spine, row
anatomy, action reveal (`:hover, :focus-within`), quick chips on overdue rows
only, empty state, §9.9 mobile rules (wrap, 16px input, no mobile autofocus).
**Review:** side-by-side with `reminder-theme-preview.html`; keyboard reachability
of revealed actions.

### 32. Time formatters
`whenText`/`fullText` per §9.11 (ordered condition table, calendar-vs-duration
split, year rule, locale-formatted parts with English connectives), wired into
rows; the worker's own copy replaces commit 24's provisional body.
**Review:** the §9.11 table order *is* the spec — check first-match-wins and the
calendar-day comparisons; both copies kept to the same table.

### 33. Keyed list rendering
The §9.2 render rules: `Map` from id to node, in-place updates, skip rows with an
open editor, remove/insert only what changed. The 304 path and every poll go
through this pass.
**Review:** the three bullet rules of §9.2; focus and scroll surviving a poll.

### 34. Inline row editor
Edit = the capture bar reused (§9.2): prefilled raw text, chips + picker chip,
`Enter`/`Esc` semantics, the always-send-every-field PATCH body, click-on-text
opens it (links excepted). The §9.4 keyboard rules in full (`/`, `n`, `Esc`
innermost-first stack).
**Review:** the PATCH body composition rule; `Esc` one-layer-per-press; `/`/`n`
inert in fields and under sheets.

### 35. Markdown link rendering (client)
The §9.10 renderer: single regex, eligibility gate, node-building with
`textContent` (no `innerHTML` anywhere — §9.7), link styling per §9.10.
**Review:** the case table checked by hand in the browser, and the client gate
agreeing with commit 18's Go tests row for row.

### 36. Picker sheet
§9.6 in full: wheels (scroll-snap, buttons, ranges from config), calendar
(week start, ring/fill/dimmed), live summary via `fullText` + relative line with
the fixed past sentence, `Set reminder`/`Cancel` two-step commit, both opening-
state rules (create and edit, minute always `00`, hour clamp), modal semantics
(focus trap, return-to-chip, scrim, `Esc`), browser-local interpretation of the
chosen time.
**Review:** the biggest UI commit — review against `reminder-datetime-picker.html`
plus the §9.6 bullets; the two opening rules and the never-opens-on-the-warning
property.

### 37. Channel selector
The "Also send to" affordance (capture bar: hidden when the catalogue is empty)
and the editor's channel chips with both §9.2 asymmetries (disabled-but-attached
deselectable, orphan chips selected/deselectable/not-reselectable), selection
reset after save. Manual testing needs a hand-inserted channel row until M4's CLI
exists — noted, accepted.
**Review:** the two asymmetries and the editor's always-send-full-list rule; the
capture-bar-only suppression.

### 38. Toasts, undo, failure states
§9.2's failure block: `aria-live` region, action-failure toasts, the 5 s undo
toast on Clear (`/undone`), the two-consecutive-failures "Not connected" line,
the unknown-preset 400 self-repair (refetch config, redraw chips in place), the
quiet-moment rule for `config_version` reloads, double-submit guard.
**Review:** poll failures never toast; the self-repair preserves typed text and
open editors.

### 39. ✅ M3 checkpoint (no commit)
§11 M3 *done when*: matches both mockups on desktop and phone (redeploy to the
step-28 VPS instance for the phone half); a `[label](https://…)` reminder renders
a link in the row, plain text in the notification, raw markdown in the editor.

---

## M4 — ship (10 commits)

Order: channels CLI before fan-out (fan-out needs rows to resolve), fan-out before
its UI surfacing, then the independent operational pieces (purge, reload,
headers, shutdown), then packaging, then docs. The last four commits have no code
dependencies between them.

### 40. `nag channel` subcommand
`add`/`list`/`rm`/`enable`/`disable` per §7.4: parse-before-write on `add`, the
slug rule with its message, the pinned `list` mask, `rm`'s warn-and-proceed,
`busy_timeout` behaviour on a locked DB.
Tests: slug matrix, mask forms (userinfo/query elision, no-path URL), duplicate
rejection.
**Review:** the mask against §7.4's two examples; no URL ever printed unmasked.

### 41. shoutrrr wrapper, classification, `nag channel test`
The wrapper imposing the deadline (§7.2), the `errors.As`-only classification
into the §4.1 closed set (never text parsing), and `nag channel test` on top
(10 s, sends to disabled too). Per the pre-flight verification: the fork
(`github.com/nicholas-fedor/shoutrrr` v0.17+) offers an injectable `http.Client`
via `NewWithOptions` and a per-service `ContextSender.SendContext` — use both
where available, but the goroutine+`select` bound stays, because §7.2 says the
guarantee must never depend on the library (and not every service implements the
optional interface).
Tests: each classification branch from a typed error; an untypeable error is
`send failed`; **the §11-required test that a failure against a URL carrying a
token stores neither URL nor token**; abandonment on deadline → `timeout`.
**Review:** the classification reads types only; the wrapper's abandon semantics.

### 42. Sweep phase 3: fan-out
§7.2 + §7.3 phase 3: eligible-rows hand-off from phase 1 (gate-stamped rows
excluded), the 20 s whole-phase deadline from a fresh clock read threaded into
every send, per-row `delivery_error` write/clear as attempts finish, orphan INFO
line vs disabled silent skip, dropped-rows INFO line.
Tests: the §7.6 fan-out checklist §11 assigns to M4 (fake clock + fake sender) —
31-minutes-late never fanned out, fan-out fires while the push is held, deadline
drops remainder without delaying the next push, the many-dead-channels row still
returns at ~20 s, phase-2-delay doesn't eat phase 3's budget.
**Review:** same method as commit 21 — the checklist read against the test names;
the deadline threaded, not checked between rows.

### 43. `delivery_error` surfacing
The row marker per §9.2: quiet when channels attached, amber with accessible
label on error, full text in the expanded editor. (The clear-on-change rule
already landed with commit 12; this commit makes it visible.)
**Review:** the marker states; error text shown only where §9.2 says.

### 44. Sweep phase 4: retention purge
§4.4: the DELETE, `retention_days = 0` skips entirely, purge log line.
Tests: boundary at exactly N days; zero means forever.
**Review:** small on purpose.

### 45. SIGHUP reload and the two hashes
§5.5: signal handling, re-validate, keep-old-on-bad (including missing file),
the full hash (log only) and client hash (`config_version` only), atomic pointer
publication, snapshot-per-request/tick discipline.
Tests: the §11 M4 hash list — offset-only edit applies without moving the
counter, label rename moves it, byte-identical file logs `config unchanged`,
failed validation leaves both untouched; plus invalid-at-boot exits 1 without
overwriting.
**Review:** the two-hash separation (§5.5's argument); no reader holds the pointer
across a boundary.

### 46. Security headers and Content-Type rule
§10.2: the one middleware (CSP as pinned, nosniff, referrer), the caching split
(`no-store` API / `no-cache` + ETag assets), the JSON `Content-Type` requirement
on bodied mutations with the bodyless exemptions.
Tests: header presence on every response class; a bodied mutation without the
header is rejected; `/done` without one isn't.
**Review:** CSP string against §10.2 byte for byte; the exemption list matches.

### 47. Graceful shutdown and `nag healthcheck`
§10.1: SIGTERM/SIGINT → server shutdown, sweep context cancel + wait, then
`db.Close()`; every sweep send already context-bound so the wait is short.
`nag healthcheck` with the `NAG_ADDR` host rules.
Tests: shutdown ordering; healthcheck URL derivation for `:8080` / `0.0.0.0` /
explicit host.
**Review:** the close-after-wait order and why (§10.1's mid-write argument).

### 48. Dockerfile, .dockerignore, compose
§10: the multi-stage build as specified (including the `/empty` → `/` tmp trick
and its ownership), `.dockerignore` with the required exclusions, `compose.yaml`
with the caddy labels and external network. The Go image version follows the
pre-flight decision, not the spec's literal `1.24`.
**Review:** against §10's files nearly line by line; `.dockerignore` covers
`nag.env`, `data/`, the mockups.

### 49. `nag.env.example` and README
The comments-only env example (§5.2), README with the four setup commands, the
caddy-network prerequisite, ownership and backup notes (§10.1, §10.3), and the
one iOS install line §11 requires.
**Review:** the env example declares no variables; the four commands verbatim,
`-T` included and explained.

### 50. ✅ M4 checkpoint / final acceptance (no commit)
§11 M4 *done when*, on the VPS from an empty checkout — replacing the step-28
throwaway deployment with the real one: four commands + `up -d` → default config
written, HTTPS serving, refuses to start with keys removed; `kill -s HUP` reloads
an edited preset list without dropping a request. Re-run the step-28 device pass
once against the production image (both phones, notification + click-through),
since the packaging, headers, and CSP all changed since the dev deployment.

---

## Dependency map (what can move, what can't)

Hard edges — reordering across these breaks a commit's "compiles + green" rule:

- 2 (store) → everything that touches the DB.
- 3–4 (config) → 5 (presets) → 10 (create resolves presets).
- 9 (auth) → 10–14 (every `/api/*` commit sits behind the middleware).
- 18 (flattenLinks) → 21 (digest payload).
- 19–20 (subscriptions, sender) → 21–22 (digest, push test).
- 23 (manifest + icons) → 24 (worker names `icon`/`badge`) and 27 (favicon draws
  on `32.png`).
- 24–27 → 28 (the device pass exercises the worker, permission flow, and badge).
- 32 (formatters) + 33 (keyed render) → 34 (editor) → 36 (picker), 37 (channels UI).
- 40 (channel CLI) → 41 (wrapper/test) → 42 (fan-out) → 43 (surfacing).

Soft spots — safe to reorder or interleave if review pacing wants it:

- 6 (genkeys) is independent of 2–5.
- 13 (ETag) and 14 (config/channels endpoints) can swap.
- 23 (manifest) can land anywhere in M2 before 24.
- 44, 45, 46, 47 are mutually independent; 48–49 only need everything before them.

## Suggested review lens per commit type

- **Pinned-format commits** (9, 20, 30, 46): byte-level comparison against the spec's
  pinned values. These are the ones where "close enough" ships a bug.
- **Semantics commits** (10–12, 21, 42, 45): read the named §4.1/§7.3/§7.6/§5.5
  bullets first, then the tests, then the code. The test names should read as the
  spec's checklist.
- **UI commits** (31, 34, 36, 37): open the mockup next to the browser; the spec's
  bullets are the checklist, the mockup is the arbiter of look.
- **Plumbing commits** (1, 8, 48): mostly structure; check that nothing beyond the
  named scope crept in.
