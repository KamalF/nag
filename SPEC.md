# Nag — implementation spec

A personal, self-hosted reminder app. It reproduces the Slack "remind me about this" workflow:
capture a note, pick a preset delay, get an OS notification. The note then stays in a list until
you clear it manually.

Single user. Single instance. No multi-tenancy, no accounts, no sync protocol.

---

## 1. Goals

1. **Capture in one interaction**: type text, tap a preset, done. No date picker in the common path. (untested)
2. **Notify at the due time** with a real OS notification in Firefox and Chrome, on laptop and phone. (partly tested: TestDueRowSendsSingleForm)
3. **Persist until dismissed**: after an item fires, it stays in the overdue list until you clear it. This is the core behavior, and the reason why existing tools do not fit. (test: TestState, TestPurgeBoundary)
4. **Glanceable from a pinned tab**: the favicon shows the count of items that need attention. (partly tested: TestState)
5. **Optional fan-out** to ntfy or IM, per reminder. (test: TestFanOutSendsTheFlattenedText)

### Non-goals (v1)

Do not build these (untested):

- Recurring reminders (no rrule), natural-language date parsing, multi-user, sharing, teams.
- A native mobile app, or mobile push other than standard Web Push.
- Full-text search, tags, projects, priorities.
- Keyboard shortcuts for individual presets (§9.4).
- Deferred to a later version: notification action buttons (Snooze and Clear from the notification), a bookmarklet or Firefox extension for "remind me about this page", Web Share Target on the phone, recurrence, and a history view of the retained cleared rows. The bookmarklet needs no new field: it posts `[title](url)` as the text, with the parentheses in the URL percent-encoded and `[`/`]` removed from the title.

---

## 2. Stack

Dependencies and their versions are in `go.mod`.

- The binary is static (`CGO_ENABLED=0`) and runs in the scratch image (§10). For this reason the SQLite driver is the pure-Go `modernc.org/sqlite`. (untested)
- The binary imports `time/tzdata`, because the scratch image has no tzdata. (test: TestMainEmbedsTheTimezoneDatabase)
- Logs: `log/slog` text handler to stderr. Logs are the only observability. §10.4 says which events get a line. (partly tested: the log-line assertions of the package tests)
- An HTTPS reverse proxy on the host forwards to `127.0.0.1:8080`. The caddy-docker-proxy labels are in an opt-in Compose file (§10). (untested)
- HTTPS is required for any deployment that another device reaches, because service workers and Web Push need a secure context. Browsers treat `localhost` as secure, so a try on the same laptop works over plain HTTP (README, "Try it on your laptop"). (untested)

---

## 3. Repo layout

- `sw.js` is embedded at the root of `web/` and served as `/sw.js` (§9.5). (test: TestServiceWorkerIsEmbeddedAtTheRoot, TestCachingSplit)
- `web/` ships `manifest.webmanifest` (§9.3). (test: TestManifestPinnedValues, TestManifestContentType)
- No subcommand has command-line flags. Each value that varies is an environment variable (§5.1) or a TOML key (§5.3). Why: a flag is a third place to look for the same value, and Compose then hides it in `command:`. (partly tested: TestDispatch)
- A missing or unknown subcommand, or `-h`, prints the usage block below to stderr and exits `2`. It never panics. (test: TestDispatch)
- `main` has no default subcommand. The default is the Dockerfile's `CMD ["serve"]` (§10) and nowhere else. (partly tested: TestDispatch)

Subcommands (§5.2, §7.4, §10) (test: TestVersionOutput, TestChannelUsage):

```
nag serve          # the default, per CMD
nag genkeys        # emit a complete env file to stdout
nag healthcheck    # exit 0 if the local instance is serving (for Docker HEALTHCHECK)
nag version        # build version + Go version to stdout, exit 0
nag config check   # validate NAG_CONFIG, exit 0 if a reload accepts it
nag channel add|list|rm|enable|disable|test
```

---

## 4. Data model

- SQLite. All timestamps are Unix seconds, UTC. Never store local time. (partly tested: TestEvaluateReturnsUTC)
- The schema is migration 1 in `internal/store/store.go`. Its column comments give the meaning of each NULL. It has three tables: `reminders`, `push_subscriptions`, `channels`. (partly tested: TestOpenCreatesAndMigrates)
- `push_subscriptions.endpoint` and `channels.name` are UNIQUE. (test: TestSubscribeUpsert, TestChannelAddRefusals)

### 4.1 Semantics

- Schedule and lifecycle are independent. `due_at` is when to notify. `done_at` tells whether the item still needs you. **Overdue** = `done_at IS NULL AND due_at <= now`. **Later** = `done_at IS NULL AND due_at > now`. (test: TestState)
- `notified_at` means "the sweep has handled this row". The sweep sets it on each row that it handles, whether it sent anything or not, so a marked row never fires twice. (test: TestSweepMarkIdempotent, TestZeroSubscriptionsWarnsAndConsumesTheWindow)
- `pushed_at` means "a Web Push carried this row". This is a later event, because pushes are limited to one per 30 minutes (§7.3). The set that the cooldown holds is a query, `notified_at IS NOT NULL AND pushed_at IS NULL AND done_at IS NULL`, not memory. So a restart during a cooldown loses no notification. (partly tested: TestSweepCollectDigestStampsExactlyTheHeldSet)
- The reminder object returns `notified_at`, for `curl` users. It never returns `pushed_at` (§8.2). (test: TestCreateReminderShape, TestState)
- **Re-time** = one UPDATE that sets the new `due_at`, `notified_at = NULL`, `pushed_at = NULL`, `delivery_error = NULL`, and `done_at = NULL`. It is the same before firing, after firing, and on a cleared row. (test: TestUpdateReTime, TestPatchReTimeUnclears)
- **A write whose `due_at <= now` never notifies.** On create and on re-time, the same statement sets `notified_at = pushed_at = now`, not NULL. The row shows in "Needs you now" at once, phase 1 never picks it up, and it never joins a digest. Why: this keeps the picker promise of §9.6 for any past time, also a few seconds in the past, and leaves the too-late gate of §7.3 with one job, which is to catch up after an outage. (test: TestBackdatedCreateCarriesBothStamps, TestPatchBackdatedReTimeStamps, TestSweepMarkSkipsBackdatedWrites)
- A backdated write sends nothing to its channels either: no push, no ntfy, no Telegram. It keeps `extra_channels`, and a later re-time to a future time fans out normally. (partly tested: TestSweepMarkSkipsBackdatedWrites)
- **A re-time with an `offset` preset resolves from now**, never from the old `due_at`. "30 min" on an item three days overdue means 30 minutes from now. (test: TestPatchOffsetPresetResolvesFromNow)
- **Clear** = set `done_at = now`. Keep the row. (test: TestDoneUndoneFlow, TestMarkDoneKeepsOriginalStamp)
- **Undone** = set `done_at = NULL` and, in the same UPDATE, `pushed_at = now` when `notified_at IS NOT NULL AND pushed_at IS NULL`. On a row that is not done, it is a successful no-op. So a row that already fired comes back silently, and only a re-time can notify it again. (test: TestMarkUndoneGuard, TestHeldRowClearedThenUndoneStaysSilent)
- A row cleared before it fired keeps `notified_at` NULL. If you un-clear it after its due time, the sweep notifies it when it is inside the too-late gate (§7.3). The §7.6 table shows both undone cases. (test: TestRowClearedBeforeItFiredNotifiesAfterTheUndo, TestRowUndonePastTheGateIsSilent)
- **`delivery_error` never holds remote text.** It holds one entry per failed channel, joined with `; `, from this closed set: `<name>: timeout`, `<name>: refused`, `<name>: dns`, `<name>: http <code>`, `<name>: send failed`. At most three entries, then `; +N more`. Why: shoutrrr errors often embed the channel URL, and the API returns this column (§8.2), the row shows it (§9.2), and the log writes it (§10.4). (test: TestFormatDeliveryError, TestFanOutDeliveryError)
- A send that the phase 3 deadline (§7.3) or the shutdown cancellation (§10.1) cuts mid-flight records `<name>: timeout`. A channel that was never attempted records nothing: the budget did not reach it, or §7.2 skipped it. (test: TestFanOutDeadlineDropsRemainingRows, TestFanOutDeadlineBoundsARowOfDeadChannels, TestSendChannelAbandonsOnDeadline. Partly tested for the shutdown: TestShutdownMidFanOut)
- The shape comes only from the error's type, with `errors.As` on the error chain: `net.Error` timeout → `timeout`, `syscall.ECONNREFUSED` → `refused`, `*net.DNSError` → `dns`, a failure status carried as a value on a typed error → `http <code>`. Anything else is `send failed`, which is a normal result. Never parse an error string or remote text to find a shape. (test: TestClassifyChannelError, TestFailedSendCarriesNoURLAndNoToken)
- A later fully successful fan-out clears `delivery_error`. (partly tested: TestFanOutDeliveryError)
- Any re-time clears `delivery_error`, and so does any change to `extra_channels`, in the same statement. An identical re-save keeps it. `extra_channels` is stored canonical (§8.3), so "changed" is a byte-for-byte compare of the two ordered arrays. (test: TestUpdateChannelsClearRule)
- `delivery_error` describes only the latest attempt. A later failure replaces the whole string. Entries never accumulate. (partly tested: TestFanOutDeliveryError)
- A `delivery_error` that names a channel that `nag channel rm` removed stays until a re-time or an edit of the channel list. Nothing is ever attempted for an orphan (§7.2). (untested)
- **`channels.url` never appears in an API response in any form**: not masked, and not as a boolean that says a URL exists. `GET /api/channels` returns `{name, enabled}` only (§8.2). (test: TestChannelsEndpoint)
- The only reader of the URL is `nag channel list`, which masks it (§7.4). (test: TestChannelListMasksAndShowsState, TestMaskChannelURL)
- `extra_channels` holds names, not ids, with no foreign key. There is no rename command. After `nag channel rm` (§7.4), the reminders that name the channel keep the orphan name. To change the URL of a channel, remove it and add it again under the same name. (test: TestChannelRemoveWarnsAndProceeds, TestPatchChannelSemantics)

### 4.2 Migrations

- Each migration runs in one transaction that also bumps `PRAGMA user_version`. (partly tested: TestOpenCreatesAndMigrates, TestReopenIsANoOp)
- If `user_version` is higher than the number of known migrations, refuse to start and name both numbers. (test: TestNewerDatabaseRefusesNamingBothNumbers)

### 4.3 Connection

The DSN and the pool settings are in `store.Open` (`internal/store/store.go`), with their reasons.

- `busy_timeout(5000)`, so that `nag channel …` works while `serve` holds the write lock. (untested)
- `synchronous(NORMAL)` with WAL, pinned in the DSN: a commit survives a process crash, and only a host power loss can lose the last transactions. (untested)
- If the file cannot be opened or the migrations cannot run, refuse to start and name the path. (test: TestUnopenableFileRefusesNamingPath)

### 4.4 Retention

- The sweep deletes the cleared rows with `done_at < now - retention_days*86400` (strict). The default is 30 days. `retention_days = 0` keeps cleared rows forever: no DELETE runs. (test: TestPurgeBoundary, TestPurgeZeroKeepsForever, TestLoadAbsentPathWritesDefault)

---

## 5. Configuration

- Secrets (VAPID keys, auth token) are only in the environment, never in the TOML. Behavior is only in the TOML, a hand-edited file in a mounted directory. (partly tested: TestValidationRules)

### 5.1 Environment

| Var | Required | Rule |
|---|---|---|
| `NAG_DB` | no | default `/data/nag.db` |
| `NAG_CONFIG` | no | default `/config/nag.toml` |
| `NAG_ADDR` | no | default `:8080` |
| `NAG_TOKEN` | **yes** | at least 20 characters |
| `NAG_VAPID_PUBLIC` | **yes** | unpadded base64url that decodes to exactly 65 bytes (an uncompressed P-256 point) |
| `NAG_VAPID_PRIVATE` | **yes** | unpadded base64url that decodes to exactly 32 bytes (a P-256 scalar) |
| `NAG_VAPID_SUBJECT` | **yes** | an absolute URL with scheme `mailto:` or `https:` (RFC 8292), and not the literal `mailto:you@example.com` that `genkeys` writes |

(test: TestLoadServeEnvValid, TestLoadServeEnvRefusals, TestLoadServeEnvHTTPSSubject)

- If a required variable is missing or fails its rule, refuse to start.
- `NAG_TOKEN` is the only secret. It is the bearer token, exchanged for a signed cookie at `/login`, and the only input to the cookie signing key (§8.1). (test: TestCookieEncodingIsPinned)
- The VAPID key check is the one that §8.2 applies to a client-supplied `application_server_key`. A refusal names the variable, the expected decoded length, and what it found.
- A refusal of the subject placeholder names the variable, says that it found the placeholder, and tells you to put your own address there.
- If both VAPID keys are missing, the error prints the fix:

  ```
  FATAL: no VAPID keypair configured.
    Generate one:  docker compose run --rm -T nag genkeys >> nag.env
    Then restart.
  ```

- **Only `nag serve` enforces the table.** `genkeys` and `version` need nothing, `channel` needs only `NAG_DB`, `healthcheck` needs only `NAG_ADDR`, and `config check` needs only `NAG_CONFIG`. Why: `genkeys` must run before the keys that it makes exist. (partly tested: TestGenkeysNeedsNoEnvironment)
- Each subcommand that opens the database creates and migrates it through the one `store.Open`, as `serve` does. So `nag channel add` works on a box that never ran `serve`. (test: TestChannelAddCreatesTheDatabase)

### 5.2 `nag genkeys` subcommand

- `genkeys` writes a complete env file to stdout and nothing else (no banner, no prompt), so that `docker compose run --rm -T nag genkeys >> nag.env` works. Advice goes to stderr, and says that the subject edit is required (§5.1). (test: TestGenkeysStdoutIsACompleteEnvFile)

  ```
  NAG_TOKEN=<32 random bytes, base64url: 43 characters>
  NAG_VAPID_PUBLIC=<from webpush.GenerateVAPIDKeys()>
  NAG_VAPID_PRIVATE=<from webpush.GenerateVAPIDKeys()>
  NAG_VAPID_SUBJECT=mailto:you@example.com
  ```

- The README "Setup" section holds the four server setup commands and the reasons for their order. `-T` on the `genkeys` command is mandatory. (untested)
- `compose.yaml` declares a required `env_file: ./nag.env`, not `required: false`. So `cp nag.env.example nag.env` comes before `genkeys`. (untested)
- With the caddy-docker-proxy add-on (§10), the external `caddy` network must exist before any `docker compose` command, setup included. Compose does not create it. The base `compose.yaml` declares no external network. (untested)
- `nag.env.example` is comments only: every line starts with `#`, and it declares no variable. It documents each variable, its default, and whether it is required. The file says why. (test: TestEnvExampleDeclaresNothing)

### 5.3 TOML

- If `NAG_CONFIG` does not exist, write the embedded default `internal/config/nag.default.toml` to that path and log it. (test: TestLoadAbsentPathWritesDefault, but the log line is untested)
- If the directory is missing or unwritable, refuse to start and name the path. Never run without a config. (test: TestLoadMissingDirectoryRefuses)
- If the config exists but cannot be read or does not validate, refuse to start with exit `1`, and name the path and the located error of §5.5. Never write the default over a file that is present. (test: TestLoadBrokenFileRefusesAndPreserves, TestLoadUnreadableFileRefuses, but the exit code of `serve` is untested)
- The default file holds: `timezone = "Europe/Paris"`, `default_preset = "tomorrow"` (used by a bare Enter in the capture bar), `retention_days = 30`. `[picker]`: `hour_min = 8`, `hour_max = 18`, `minute_step = 15`, `default_time = "09:00"`, `week_start = "monday"`. Presets, in this order: `30min` ("30 min", offset `30m`, quick), `3h` ("3 hours", offset `3h`, quick), `tomorrow` ("Tomorrow 9:00", clock, `days = 1`, `at = "09:00"`, quick), `next-monday` ("Next Monday", weekday `monday`, `at = "09:00"`, `same_day_ok = false`). (test: TestLoadAbsentPathWritesDefault, TestLoadKeepsFieldPresence)
- There is no theme setting (§9.1). (untested)

### 5.4 Rules

- `key` identifies a preset. `label` is for display only. A label rename must not break stored history. (untested: rows store no preset reference)
- **File order is UI order.** No `order` field. (test: TestConfigProjection, TestConfigCheckValid)
- The file's preset list replaces the default list wholesale. Never merge the two. (partly tested: TestLoadExistingFileIsNotOverwritten)
- `kind` is a closed set: `offset` | `clock` | `weekday`. Reject anything else. No free-form parsing.
- Each kind has an exact field set. A field of another kind is an error, not something to ignore. Why: an ignored field makes the chip do something other than what the file says.

  | `kind` | required | optional | rejected |
  |---|---|---|---|
  | `offset` | `offset` (a positive `time.ParseDuration` string) | — | `at`, `days`, `weekday`, `same_day_ok` |
  | `clock` | `at` | `days` (integer `>= 0`, default `0`) | `offset`, `weekday`, `same_day_ok` |
  | `weekday` | `weekday`, `at` | `same_day_ok` (default `false`) | `offset`, `days` |

  `key`, `label`, and `quick` (default `false`) apply to every kind. `key` and `label` must not be empty. (test: TestValidationRules, TestValidationAccepts, TestLoadKeepsFieldPresence)
- "Pick a time" is not a preset. The client shows it after the configured presets. (untested)
- `[picker]` constrains only the picker sheet. A preset with `at = "07:00"` under `hour_min = 8` is valid. (test: TestValidationAccepts)

### 5.5 Validation (at boot, and on reload)

Reject the file on any of these (test: TestValidationRules):

- duplicate `key`
- empty `key` or `label`
- `default_preset` not matching a key
- empty preset list
- a field outside its kind's set per the table in §5.4
- a missing required field for the kind
- unparseable `offset`
- `offset` zero or negative (`-30m` parses, and schedules into the past on every tap)
- `days < 0`
- `at` not `HH:MM` in 24-hour form
- unknown `weekday`
- unknown `timezone`
- `week_start` not `monday` or `sunday`
- `hour_min` or `hour_max` outside `0..23`
- `hour_min > hour_max`
- `minute_step` not in `1..60`, or `60 % minute_step` non-zero
- `retention_days < 0`
- `default_time` not `HH:MM`, outside `[hour_min, hour_max]`, or not on a `minute_step` boundary
- an unknown key or an unknown section anywhere in the file, named in the error (same reason as the §5.4 field rule, and as §8.3's unknown JSON fields)

More rules:

- Each message names the preset `key` (or the `[section].field`) and the rejected value. (test: TestValidationRules, TestConfigCheckInvalid)
- On **SIGHUP**, re-read and re-validate. If the file is invalid, missing or unreadable, log the error and keep the running config. A reload never writes the default file. The reload command is `docker compose kill -s HUP nag`. (partly tested: TestReloadFailureKeepsTheRunningConfig, but the signal wiring in `serve` is untested)
- **`nag config check`** loads and validates `NAG_CONFIG` through the boot code path. It prints the resolved preset list in file order or the located error, and exits `0` or `1`. Its environment is in §5.1. It never writes the default file: an absent config is an error. Why: a reload's outcome exists only in the log, so this is the way to test an edit before the HUP. (test: TestConfigCheckValid, TestConfigCheckInvalid, TestConfigCheckAbsentFileIsAnErrorAndWritesNothing)
- A change of `general.timezone` never rewrites stored `due_at` values. Only future preset evaluation moves. (untested)
- `/api/config` and `/api/state` return the same `config_version`. The page reloads when it changes. (partly tested: TestConfigVersionInBothEndpoints)
- Each successful load replaces the running config. If the resolved config is identical, the log gets one `config unchanged` line. Otherwise it gets one line that names the reload. (test: TestReloadOffsetOnlyAppliesWithoutMovingTheCounter, TestReloadIdenticalFileChangesNothing)
- `config_version` moves when, and only when, what `/api/config` returns (§8.2) changes. It does not move on a failed load, on an identical file, or on an edit that the client cannot see, such as an `offset`. (test: TestReloadLabelRenameMovesTheCounter, TestReloadOffsetOnlyAppliesWithoutMovingTheCounter, TestReloadIdenticalFileChangesNothing)
- The counter is in memory and starts at 1, so it can move backward across a restart. The client compares with `!=`, never `>`. (partly tested: the live_test.go tests check version 1 at start)

---

## 6. Preset evaluation

- Presets resolve in `general.timezone`, also when you travel. Build each time with `time.Date(y, m, d, hh, mm, 0, 0, loc)`, so that the stdlib handles DST, and convert the result to UTC. (test: TestClockAcrossSpringForward, TestEvaluateReturnsUTC)
- **The server schedules, the browser displays.** The client formats each instant in the browser's timezone and locale. The picker reads the wall-clock time that you choose as browser-local (§9.6). From a phone two zones away, the picker gives 09:00 on your watch and the `tomorrow` chip gives 09:00 at the server. This is accepted: foreign wall-clock times are worse for a single user. Clock skew is still corrected (§9.3). (partly tested: TestWorkerFormatterMatchesThePage)
- A preset's `label` is a config string, never adjusted for the timezone. From a phone two zones east, "Tomorrow 9:00" makes a row that reads "tomorrow at 10:00". Why: a client-side label needs `kind`, `at`, `days` and the timezone, which is a second evaluator in another language. (untested)
- **`offset`**: `due = now + offset`. No rounding. Pure elapsed duration: "3 hours" is three hours, also across a DST change. (test: TestOffsetAcrossMidnight, TestOffsetAcrossSpringForwardIsPureDuration)
- **`clock`**: take today's date in `loc`, add `days` calendar days (never `days*24h`), and build the time at `at`. If the result is `<= now`, add one more calendar day and rebuild. "Tomorrow 09:00" is 09:00 across a DST change. (test: TestClockAcrossSpringForward, TestClockAcrossFallBack, TestClockAt2350, TestClockDaysZeroBothSidesOfAt)
- **`weekday`**: the next date whose weekday matches, at `at`, built the same way. Today counts only if it matches, `same_day_ok = true`, and `at` is still in the future. Otherwise take the next match strictly after today: with `same_day_ok = false`, "next Monday" on a Monday is +7 days, as in Slack. (test: TestWeekdayOnItsOwnDay, TestWeekdayLaterThisWeek)
- A `clock` `at` inside a spring-forward gap (02:30 in a zone that jumps 02:00→03:00) normalizes forward to 03:30. Do not add gap detection. (test: TestClockAtInsideSpringForwardGap)

---

## 7. Notifications

### 7.1 Web Push (always on)

- The VAPID keypair is generated once, stored in the env file, and is not rotated casually. Every subscription is bound to the public key that it was created under. After a rotation, the server deletes each dead row on its first 403 (below), and every device must open the app once to re-subscribe (§9.8). (untested)
- **A push send reaches only a public address.** The push client checks each address at dial time, after DNS resolution, and refuses any address that is not public unicast (loopback, private, link-local, `100.64.0.0/10`, unspecified). The failure is classified `refused`. Why: an endpoint is any URL that a token holder posted, so without the check the server sends requests into its own network. (test: TestRefuseNonPublic, TestPushClientStaysPublic)
- The push client follows no redirect (a 3xx is handled below as `http 3xx`) and uses no proxy. (test: TestPushClientStaysPublic)
- Each subscription row stores in `vapid_public` the `application_server_key` that the client posted (§8.2), never the configured key. (test: TestSubscribeUpsert)
- At boot, if any subscription's `vapid_public` differs from the configured key, log a WARNING that names the count. (untested)
- Send options: `TTL: 259200`, `Urgency: webpush.UrgencyHigh`, `Topic: "nag"`, `RecordSize: 2560`, and `Subscriber` = the subject without its `mailto:` scheme, so the signed `sub` claim holds exactly one `mailto:`. (test: TestSendToOptionsAndLogging, TestVAPIDSubjectClaimIsOneMailto, TestLargestPayloadFitsMozillaLimit)
- **`Topic` is the constant `"nag"`, never the reminder id**, so at most one push is pending per device. The ~3-day TTL is safe only because of this constant topic: the two are a pair. (test: TestSendToOptionsAndLogging)
- Payload: one message per push, and it can cover several reminders (§7.3):

  ```json
  {"n": 1,  "id": 123, "due_at": 1786742400, "texts": ["Renew the domain"]}
  {"n": 14,                                  "texts": ["Renew the domain", "Call the plumber", "Move the standup"]}
  ```

  - `n` is how many reminders this push covers. (test: TestPayloadForms)
  - `due_at` is present only when `n == 1`. `id` is present only when `n == 1` and a real row exists: the test push (§7.5) has `n == 1` and no `id`. A consumer checks for the field and does not infer it from `n`. (test: TestPayloadForms, TestPushTestSendsThePinnedPayload)
  - `texts` holds at most three entries, in due order. (test: TestPayloadTextsCappedAtThree, TestTwoRowsInOneTickAreOneDigest)
  - Each entry is link-flattened (§9.10). (test: TestPayloadFlattensLinks)
  - When `n == 1`, the entry is the full text. When `n > 1`, each entry is truncated to 120 bytes on a rune boundary, `…` included. (test: TestPayloadTruncatesOnlyTheMultiForm, TestPayloadTruncatesOnARuneBoundary)
  - The largest payload (a 1000-byte text, every byte JSON-escaped) fits the push limit. (test: TestPayloadWorstCaseTextFits, TestLargestPayloadFitsMozillaLimit)
  - Nothing else goes in: no channel list, no history, no overdue total. The worker derives "+N more" from `n - texts.length` (§9.5) and draws the notification with no network call. (untested)
- Every send takes its deadline from the caller's context: the sweep (§7.3) or `POST /api/push/test` (§7.5). The deadline is the only difference between the two callers, which share one send-and-response helper. (partly tested: TestPushTestBudgetExhaustionStillReportsEveryRow)
- On `404` or `410`, delete the subscription row. (test: TestSendToLadder)
- On `401` or `403`: if the row's `vapid_public` differs from the configured key, delete the row and log a WARNING. If it matches, log an ERROR and keep the row. (test: TestSendToLadder)
- On any other error, log and keep the row. (test: TestSendToLadder, TestSendToTransportClassification)
- These log lines show an endpoint only in the truncated form of §7.5 (§10.4). (test: TestSendToOptionsAndLogging, TestTruncateEndpoint, TestPushEndpointsLogTruncatedForm)
- Push failures are classified, never stringified. A push failure changes no reminder column: only the log line records it. (partly tested: TestSendToTransportClassification)
- On success, nothing is recorded. (partly tested: TestSendToLadder)
- Never send a silent push: the service worker always calls `showNotification`, or browsers revoke the subscription. (untested)
- If no subscription row exists, the sweep still marks the rows and logs a WARNING that names the number of reminders in the unsent push and the id of the first one. (test: TestZeroSubscriptionsWarnsAndConsumesTheWindow)

**Platform note:** desktop and Android Chrome and Firefox work from a normal tab. On iPhone (iOS 16.4+, every browser is WebKit), Web Push works only after the site is added to the Home Screen. That is why `manifest.webmanifest` is required (§9.3).

### 7.2 shoutrrr fan-out (opt-in per reminder)

- **Fan-out is not subject to the push cooldown and is never coalesced.** It runs in the tick that marks the row (phase 3 of §7.3) and sends one message per reminder to each of its enabled channels. Why: a channel is an output that you asked for on that reminder, often "tell me now", and two channel messages cannot merge. (test: TestFanOutIsNotHeldByTheCooldown, TestFanOutSendsTheFlattenedText)
- Names in `extra_channels` resolve against `channels`. A name with no row is skipped with one INFO line that names the reminder id and the name. A disabled channel is skipped silently. Neither writes `delivery_error` (§4.1). (test: TestFanOutDisabledAndOrphan)
- The message body is the link-flattened text (§9.10). (test: TestFanOutSendsTheFlattenedText)
- Failures are classified into the `delivery_error` shape of §4.1 and logged in that same form. (test: TestClassifyChannelError, TestFailedSendCarriesNoURLAndNoToken, TestFanOutDeliveryError)
- A failed send never blocks and is never retried: the overdue list is the safety net. (partly tested: TestFanOutDeadlineDropsRemainingRows)
- `delivery_error` is written, or cleared on success, per reminder as soon as that row's attempts finish. It is not batched to the end of the phase and not carried into the next tick. (test: TestFanOutDeliveryError, TestFanOutDeadlineDropsRemainingRows)
- Each send is bounded by a context with a 10-second timeout, derived from phase 3's context. So its effective timeout is the smaller of 10 s and what is left of the 20-second phase budget (§7.3). (partly tested: TestFanOutDeadlineBoundsARowOfDeadChannels)
- The send runs on its own goroutine, and the caller waits on the result or the context. If the context ends first, the caller abandons the send, which records what §4.1 says. The bound must never depend on what the library supports. (test: TestSendChannelAbandonsOnDeadline)
- Where a service offers a context-aware send (`types.ContextSender.SendContext`, or the service's own `SendWithContext`), it gets the context, so the deadline and a shutdown reach the socket. Other services get a plain `Send`. (partly tested: compile-time assertions in channel_iface_test.go)
- Only the `generic` service gets an injected `http.Client` with the 10-second timeout, so an abandoned generic send ends on its own. Other services keep their own clients, because an injected client removes their TLS and auth settings. (test: TestAbandonedGenericSendEnds, TestNtfyKeepsItsTLSOptions, TestNoClientInjectedIntoSelfConfiguredServices)
- **One send in flight per channel URL.** While an earlier send to the same URL still runs and started less than 30 s ago (`busyFor`), a new send does not start. The sweep logs the skip at INFO as "still busy", with the reminder id and the channel name, and records nothing in `delivery_error`. After 30 s, a new send takes over the slot, so a channel is never muted for good. A finished send frees its slot before its verdict returns. Why: some abandoned sends do not end (pushbullet has no deadline, discord retries for minutes), and this caps them at one goroutine per channel per 30 s. (test: TestStuckSendBlocksItsChannelForAWhile, TestBackToBackSendsAreNeverBusy, TestFanOutSkipsABusyChannel)
- **A channel endpoint is trusted like any server that you point nag at.** shoutrrr reads each reply whole, with no size cap, and nag does not cap it either. `compose.yaml`'s `mem_limit` (§10) bounds the worst case: a container restart, not a host out of memory. The database loses nothing, because every row was marked before its fan-out (§7.3). The in-flight fan-out of that tick is lost. (untested)

### 7.3 The sweep

**The governing rule: at most one Web Push notification in any 30-minute window, and it carries every reminder that has come due since the last one.** The badge and the lists (§9.3) and the shoutrrr fan-out (§7.2) are not rate-limited. (test: TestRowDueInsideTheWindowWaitsForIt, TestRowsHeldAcrossTheWindowRideOneDigest)

- The sweep is one goroutine with a 30-second ticker. It runs once at boot, before the first tick. When a tick runs long, the next tick is skipped, not queued. (partly tested: TestSweepRunsImmediatelyAtBoot)
- `now` is read once per tick and used for every statement and stamp of that tick. The phase-3 deadline is the one exception (below). (untested)
- A tick has four phases, in this order: mark, push, fan-out, purge. `notified_at` and `pushed_at` are committed in phases 1 and 2, before any socket opens. A dropped notification still shows in the overdue list, but a crash loop that re-sends never stops, so the lifecycle writes always come first. (partly tested: TestFanOutBudgetStartsWithThePhase, TestSweepCollectDigestStampsExactlyTheHeldSet)

**Phase 1 — mark.** Database writes only, no network.

```
tx: SELECT id, text, due_at, extra_channels FROM reminders
    WHERE done_at IS NULL AND notified_at IS NULL AND due_at <= now
    ORDER BY due_at, id LIMIT 50
tx: UPDATE reminders SET notified_at = now WHERE id IN (<all selected>)
tx: UPDATE reminders SET pushed_at   = now WHERE id IN (<selected with due_at < now - 1800>)
```

(test: TestSweepMarkIdempotent, TestSweepMarkLimit50, TestSweepMarkSkipsDoneRows)

- `LIMIT 50` is a deliberate rate limit on shoutrrr messages per tick. The sweep does not loop within a tick, so a backlog of 200 is marked over four ticks. (test: TestSweepMarkLimit50, TestSixtyRowBacklogSplitsAcrossTwoDigests)
- **The too-late gate (1800 s) applies in phase 1, at mark time, and nowhere else.** A row more than 1800 s past due when the sweep first sees it gets `notified_at` and `pushed_at` and one INFO line that names the id. It is never pushed and never fanned out. It only appears in the list. (test: TestSweepMarkTooLateGate, TestRowPastTheGateIsSilentAndKeepsTheWindow, TestMixedTickDigestsOnlyTheRowsInsideTheGate)
- The gate sees only rows that were in the future when written and then went unswept (an outage). A backdated write never reaches phase 1, because the write path already set both columns (§4.1). (test: TestSweepMarkSkipsBackdatedWrites)
- Phase 1 hands its selected rows, `text` and `extra_channels` included, to phase 3, minus the rows that the gate stamped. Phase 3 never re-queries. (test: TestSweepMarkHandsOffTextAndChannels, TestFanOutSkipsRowsPastTheGate)

**Phase 2 — the digest push.** Runs only when the window is open: `now - last_push_at >= 1800`, or `now < last_push_at`.

```
tx: SELECT id, text, due_at FROM reminders
    WHERE done_at IS NULL AND notified_at IS NOT NULL AND pushed_at IS NULL
    ORDER BY due_at, id
tx: UPDATE reminders SET pushed_at = now
    WHERE done_at IS NULL AND notified_at IS NOT NULL AND pushed_at IS NULL
then: if the select returned anything — set last_push_at = now, then send ONE push
      (§7.1) to each subscription in turn
```

(test: TestEverySubscriptionGetsTheSamePayload, TestSweepCollectDigestStampsExactlyTheHeldSet)

- The held set is a query (§4.1), so a restart in the window loses nothing. A held row that is cleared drops out through `done_at IS NULL`, and an undo does not put it back (§4.1). (test: TestHeldRowClearedDropsOutOfTheDigest, TestHeldRowClearedThenUndoneStaysSilent; partly tested for the restart: TestSweepCollectDigestStampsExactlyTheHeldSet)
- No `LIMIT` in phase 2: one push carries the whole held set, and `texts` is capped anyway (§7.1). (partly tested: TestSixtyRowBacklogSplitsAcrossTwoDigests)
- `last_push_at` is in memory only and starts at zero, so the boot tick finds the window open. A restart can give one extra notification, never a missed one. It is not derived from `MAX(pushed_at)`. (test: TestDueRowSendsSingleForm)
- `now < last_push_at` (a backward clock step) counts as an open window. (test: TestBackwardClockStepOpensTheWindow)
- The window is consumed whenever phase 2 selected rows that it sends about, whatever the sends then did, and even with zero subscriptions. `last_push_at` is set before the first socket opens. (test: TestFailedSendsStillConsumeTheWindow, TestZeroSubscriptionsWarnsAndConsumesTheWindow)
- Sends are sequential, one subscription at a time, each with a 10-second timeout. Phase 2 makes one call per subscription, not per reminder. (untested)
- **Phase 2 refuses a held row more than 3900 s past due:** it stamps the row like the others, does not send it, and logs one INFO line that names its id. The bound is the gate plus one window plus a 5-minute margin, so the worst legitimate lateness (3570 s) still arrives. (test: TestHeldRowStrandedByAnOutageIsSilent, TestSweepCollectDigestStampsTheRowsItRefusesToSend, TestTheWorstLegitimateLatenessStillArrives, TestRowsMarkedLateAndHeldAFullWindowStillArrive)
- If every held row was refused, the window is not consumed. (test: TestAStrandedHeldRowLeavesTheWindowOpen)

Both 30-minute constants are one number for the user: *nothing more than half an hour late produces a notification, and no more than one notification per half hour.*

**Phase 3 — fan out** (§7.2) to the rows that phase 1 handed over, sequentially, one message per reminder.

- Phase 3 runs after phase 2, under a whole-phase deadline of 20 s. The deadline comes from a fresh clock read at the start of phase 3, not from the tick's `now`. (test: TestFanOutBudgetStartsWithThePhase, TestFanOutDeadlineDropsRemainingRows)
- The deadline is a context passed into every send, not a check between rows, so phase 3 ends at ~20 s whatever the rows hold. (test: TestFanOutDeadlineBoundsARowOfDeadChannels)
- Rows that the deadline did not reach, and the remaining channels of a row that it interrupted, are skipped, logged at INFO with their ids and how many were dropped, never retried, and record nothing in `delivery_error`. A send cut mid-flight records what §4.1 says. (test: TestFanOutDeadlineDropsRemainingRows, TestFanOutDeadlineBoundsARowOfDeadChannels)

**Phase 4 — purge** old `done` rows (§4.4).

- After the network, the sweep writes only two things: a subscription row deleted on `404`, `410` or a stale-key `403` (§7.1), and `delivery_error` (§7.2). Nothing that the sweep learns from the network changes whether a row was handled. (partly tested: TestFanOutVerdictYieldsToAChannelEdit)

### 7.4 `nag channel …`

The final image has no shell and no `sqlite3`, so this subcommand is the only way to manage channels. It runs against the same `NAG_DB` and opens it as §5.1 says:

```
nag channel add <name> <shoutrrr-url>    # rejects a duplicate name and an unparseable URL
nag channel list                         # URLs masked, shows enabled state
nag channel rm <name>                    # prints how many reminders reference it, then proceeds
nag channel enable <name>
nag channel disable <name>               # mute without orphaning extra_channels
nag channel test <name>                  # send "Nag test notification" now, print the result
```

(test: TestChannelUsage, TestChannelListMasksAndShowsState)

- `add` gives the URL to shoutrrr's router before it writes the row, and refuses a URL that the router cannot parse, with the parse error. (test: TestChannelAddRefusals)
- **The mask is the scheme, then a literal `…`**, always: `ntfy://user:tok_abc123@ntfy.example.com/alerts` prints as `ntfy://…`. The same mask applies wherever `nag channel` shows a URL, also a URL typed where a name belongs. Why: depending on the service, the secret can be in any part of the URL, the host included. (test: TestMaskChannelURL, TestChannelListMasksAndShowsState, TestChannelAddRefusalHidesTheURL, TestURLInTheNameSlotIsMasked)
- `name` must match `^[a-z0-9][a-z0-9_-]{0,31}$`. `add` refuses anything else and prints the pattern. (test: TestChannelAddSlugRule)
- `test` sends even to a disabled channel and says that the channel is disabled. It uses the 10-second wrapper and the classification of §7.2. (partly tested: TestChannelTest)
- No API creates or edits a channel. The URL holds secrets, so channels are not in the TOML (§5), and the only writer is a shell on the host: `docker compose run --rm nag channel add ntfy 'ntfy://…'`. (partly tested: TestChannelsEndpoint, TestAPICatchAll)
- `rm` warns and proceeds. There is no `--force` (§3). Orphaned names are defined behavior (§4.1). (test: TestChannelRemoveWarnsAndProceeds)
- If the serving container holds the write lock past `busy_timeout(5000)`, the subcommand says so plainly and suggests a retry. (untested)

### 7.5 `POST /api/push/test`

- Sends a fixed "Nag test notification" push to every stored subscription and returns `200` with one result per stored subscription. (test: TestPushTestSendsThePinnedPayload, TestPushTestBudgetExhaustionStillReportsEveryRow)
- It applies the §7.1 response handling: 404/410 delete, and 401/403 delete only when the row's key differs (`deleted: true`). (test: TestPushTestReportsStaleKeyDeletion, TestSendToLadder)
- It ignores the 30-minute cooldown and does not touch `last_push_at`. (untested)
- The payload is pinned. `due_at` is the server's `now`, there is no `id`, and the `Topic` is the same constant (§7.1). (test: TestPushTestSendsThePinnedPayload)

  ```json
  {"n": 1, "due_at": 1786742400, "texts": ["Nag test notification"]}
  ```

- Each send gets the smaller of 3 s and what is left of a 12-second handler budget, taken from one clock read before the first send. The budget is `WriteTimeout` (15 s, §8.2) minus 3 s. A subscription that the budget did not reach gets `status: null`, `error: "not attempted"`, `deleted: false`. (test: TestPushTestBudgetExhaustionStillReportsEveryRow, TestPushTestBudgetLeavesHeadroomUnderWriteTimeout)
- The result vocabulary is closed. `status` is the push service's HTTP code, or `null` when the attempt produced none (timeout, DNS failure, refused connection). `error` is `null` on success, else one of the §4.1 set (`timeout`, `refused`, `dns`, `http <code>`, `send failed`), plus `stale vapid key` (a 401/403 on a row with a different key, `deleted: true`) and `not attempted` (above). Remote text never reaches the response. (test: TestPushTestReportsNullStatusOnATransportFailure, TestPushTestReportsStaleKeyDeletion, TestSendToTransportClassification)
- The response:

  ```json
  {"results":[{"endpoint":"https://updates.push.services.mozilla.com/…f3Ka9tQ2",
               "status":201,"deleted":false,"error":null},
              {"endpoint":"https://fcm.googleapis.com/…kQ7bT1xz",
               "status":403,"deleted":true,"error":"stale vapid key"},
              {"endpoint":"https://web.push.apple.com/…9pQ4mW7v",
               "status":null,"deleted":false,"error":"not attempted"}]}
  ```

- `endpoint` is truncated to scheme, host, and the last 8 characters of the path. (test: TestPushTestSendsThePinnedPayload)
- With zero subscriptions, the response is `{"results":[]}` and a 200. (test: TestPushTestWithNoSubscriptions)
- The UI does not show this endpoint. It is for `curl`. (untested)
- It takes no body, so it is exempt from the JSON `Content-Type` rule (§10.2). (test: TestJSONContentTypeRule)

  ```
  curl -X POST -H "Authorization: Bearer $NAG_TOKEN" https://nag.example.com/api/push/test
  ```

### 7.6 What actually arrives, case by case

This table is normative: where a sentence elsewhere disagrees with it, the table wins. Each row assumes that the cooldown window is open, unless it says otherwise.

| Situation | Notifications on the device | Why |
|---|---|---|
| One reminder comes due, device online | **1**, at the due time (≤30 s late) | Cooldown was open. Phase 2 sends the `n == 1` form, so the title is the reminder text |
| Two reminders due in the same tick | **1**, at the due time | One digest, `n == 2`, both texts in the body |
| A reminder comes due 7 minutes after the last notification | **1**, but at the 30-minute mark, up to 23 minutes late | Held by the cooldown. It is in the list and the badge immediately (§9.3). Only the nudge waits |
| Six reminders spread over a 25-minute stretch, all of it inside one cooldown | **1**, when the window opens, `n == 6` | Every one was held, and all go in the same digest. If the stretch *starts* with an open window, the first pings on time, and the other five arrive as a second digest with `n == 5` |
| Device asleep or offline for 20 minutes | **1**, on wake | The push service queued the pushes, and the constant `Topic` replaced each pending message with the next, so exactly one survives |
| Device offline for 2 days, server up throughout | **1**, on reconnect: the most recent digest, possibly hours stale | Same replacement rule. It names the reminders from its own tick, not all of them. The badge you are about to look at names the total |
| Device offline past the 3-day TTL | **0** | The push service dropped it. The list and badge are correct on the next load, which is the fallback that the whole design relies on |
| Server down 20 minutes, 14 reminders due in that window | **1**, `n == 14`, within 30 s of restart | All 14 are ≤1800 s late at mark time, so all 14 are pushable and land in one digest |
| Server down 2 days, 40 reminders due | **0** | Every row is >1800 s late at mark time: marked, `pushed_at` stamped, one INFO line each, no push and no fan-out. You come back to a full list |
| Server down 40 minutes, 3 due at the start and 2 near the end | **1**, `n == 2` | The first three are past the gate and are marked silently. The last two are inside it |
| Server down 25 minutes, **60** reminders due and all inside the gate | **1** with `n == 50`, then **1** with `n == 10` half an hour later | Phase 1's `LIMIT 50` bounds a tick, so ten rows are marked on the next tick. By then, this digest has just closed the window. This is the only case where the two rate limits compound, and it needs a 50-row backlog to reach |
| A held reminder is cleared before the window opens | **0** for that row | Phase 2 filters on `done_at IS NULL`. You already dealt with it |
| The same held row, **undone** an hour later | **0**, until you re-time it | Undone stamps `pushed_at` on a row whose `notified_at` is set (§4.1), so the row never re-enters the held set at all. That settles this row, not phase 2's age bound, so the outcome does not depend on how long "later" was. It is back in "Needs you now" silently. The nudge takes an explicit re-time |
| A reminder is cleared while still in "Waiting quietly", then undone 6 minutes after its due time | **1**, within one tick of the undo (≤30 s) | A done row is invisible to phase 1, so this one was never marked and `notified_at` is still NULL. Undone, it is due, unmarked, and inside the gate, so the next tick finds a live candidate and pings. Unlike §4.1's "already fired, comes back without re-notifying", this reminder genuinely never fired |
| The same row, undone **40 minutes** after its due time | **0** | Now past the too-late gate: marked silently with one INFO line, straight into the list |
| A reminder marked on time, held, and still held after a long outage | **0**, marked silently with one INFO line | Phase 2 refuses a held row more than 3900 s past due (§7.3). The phase-1 gate never saw it, because it was fresh when it was marked. Without the bound, the open window of the boot tick carries it as though it had just come due. The window is untouched, so a reminder due after the restart still pings on time |
| A reminder is re-timed while held | **0** now, **1** at the new time | Re-time nulls both columns (§4.1), so it leaves the held set and re-enters phase 1 later |
| A backdated `due_at` (picker or `curl`) | **0**, ever | The write path stamps both columns (§4.1). This is the promise §9.6 makes |
| A backdated `due_at` **with `extra_channels`** | **0**, and no ntfy/IM message either | Fan-out only ever covers rows the sweep just marked, and this row arrived marked (§4.1). Nothing is sent on any output. The channels stay stored, so a later re-time into the future fans out normally |
| Zero subscriptions stored | **0**, plus a WARNING naming the count (§7.1) | Nothing to send to. The rows are still marked, so they are not re-attempted every 30 s. The window is still consumed, because phase 2 did select rows (§7.3) |
| Every send in a digest fails: push service down, or a dead endpoint | **0**, plus a log line per endpoint | `pushed_at` is committed before the first socket opens, so those rows are gone from the held set. The window is consumed too. The list is the safety net, exactly as with mark-before-send (§7.3) |
| Three devices subscribed | **1 each** | Phase 2 makes one call per subscription with the same payload. `Topic` and `tag` are per-subscription, so collapsing is per-device |
| Reminder with `extra_channels`, held by the cooldown | ntfy/IM message **immediately**, OS notification at the 30-minute mark | Fan-out is not rate-limited, and it runs in the same tick that marks the row (§7.2) |
| A tick whose fan-out targets are unreachable: 30 rows, dead ntfy host | Push notification **on time**. The ntfy messages that fit in 20 s are attempted, the rest are dropped with one INFO line | Phase 3 runs after the push, under a whole-phase deadline that bounds every individual send (§7.2, §7.3). So the phase ends at ~20 s, whatever the row-and-channel shape. A dead channel can cost you channel messages. It can never delay a notification past the next tick |
| `POST /api/push/test` during a cooldown | **1**, immediately | It bypasses the cooldown and does not consume the window (§7.5) |

(test: TestDueRowSendsSingleForm, TestTwoRowsInOneTickAreOneDigest, TestRowDueInsideTheWindowWaitsForIt, TestRowsHeldAcrossTheWindowRideOneDigest, TestSixtyRowBacklogSplitsAcrossTwoDigests, TestRowPastTheGateIsSilentAndKeepsTheWindow, TestMixedTickDigestsOnlyTheRowsInsideTheGate, TestHeldRowClearedDropsOutOfTheDigest, TestHeldRowClearedThenUndoneStaysSilent, TestRowClearedBeforeItFiredNotifiesAfterTheUndo, TestRowUndonePastTheGateIsSilent, TestHeldRowStrandedByAnOutageIsSilent, TestHeldRowRetimedLeavesAndComesBack, TestZeroSubscriptionsWarnsAndConsumesTheWindow, TestFailedSendsStillConsumeTheWindow, TestEverySubscriptionGetsTheSamePayload, TestFanOutIsNotHeldByTheCooldown, TestFanOutDeadlineDropsRemainingRows. Partly tested: the device-offline rows (push-service behavior, only the Topic and TTL values: TestSendToOptionsAndLogging) and the backdated rows (no fan-out test: TestSweepMarkSkipsBackdatedWrites, TestBackdatedCreateCarriesBothStamps). Untested: the push test during a cooldown.)

The count `n` in a notification is never the badge count: `n` covers the reminders of that push, and the badge covers everything overdue. A notification is never the record of anything: every case that ends in **0** ends in a correct list.

---

## 8. HTTP API

The API uses JSON. `/api/*` is the only authenticated surface. (test: TestAPIRequiresAuth, TestRootServesIndexUnauthenticated)

### 8.1 Auth

**Cookie** `nag_session`, value `b64(exp) + "." + b64(mac)`. No session table and no user id: there is one user. Every piece of the value is pinned:

- `exp` is the expiry as a decimal ASCII string (`"1786742400"`).
- `b64` is unpadded base64url (`base64.RawURLEncoding`).
- `mac = HMAC-SHA256(key: k, message: "v1|" + exp)`, over the same decimal string.
- `k = HMAC-SHA256(key: NAG_TOKEN, message: "nag-cookie-v1")`. So rotating `NAG_TOKEN` invalidates every cookie.

(test: TestCookieEncodingIsPinned, TestSessionRejections)

- Attributes: `HttpOnly`, `Secure` (always set, never derived from `r.TLS`), `SameSite=Lax` (so that a notification click that opens a new tab arrives authenticated), `Path=/`, `Max-Age` 365 days. (test: TestLoginIssuesThePinnedCookie)
- **Sliding renewal**: a valid cookie older than 30 days is re-issued in the response, so a pinned tab never logs out. The check reads the same `maxAge` constant as the issuer. (test: TestSlidingRenewal)
- Verify the MAC with `hmac.Equal`. Reject a cookie whose expiry is before `now`. (partly tested: TestSessionRejections)
- A logout acts only in that browser. A copied cookie stays valid until it expires. Only a rotation of `NAG_TOKEN` revokes. (untested)

**Bearer.** `Authorization: Bearer <NAG_TOKEN>` is accepted on all of `/api/*`, permanently, compared with `subtle.ConstantTimeCompare`. (test: TestAPIRequiresAuth)

**Login.**

- `GET /` serves `index.html` unauthenticated. The page calls `/api/state`, and on a 401 it shows a token field instead of the list. (partly tested: TestRootServesIndexUnauthenticated)
- `POST /login` `{token}` → `204` + cookie on a match, `401` otherwise. (test: TestLoginIssuesThePinnedCookie, TestLoginWrongToken)
- A failed login sleeps ~250 ms, and failures are serialized. A request whose client gave up leaves the queue. That is the entire rate limit. (partly tested: TestAbandonedFailedLoginLeavesTheQueue)
- After a `204`, the client calls `location.reload()`, and that is the whole post-login path. (untested)
- `POST /logout` → `204`, cookie cleared. (test: TestLogoutClearsTheCookie)
- Static assets (`/app.js`, `/app.css`, `/sw.js`, `/manifest.webmanifest`, `/icons/*`) are unauthenticated. (partly tested: TestCachingSplit, TestSecurityHeadersOnEveryResponse)

### 8.2 Endpoints

| Method | Path | Body / notes |
|---|---|---|
| POST | `/login` | `{token}`, see §8.1 |
| POST | `/logout` | see §8.1 |
| GET | `/api/config` | `{config_version, vapid_public, default_preset, presets:[{key,label,quick}], picker:{hour_min,hour_max,minute_step,default_time,week_start}}`. The client never hardcodes these |
| GET | `/api/state` | `{server_time, config_version, overdue_count, push_subscribed, overdue:[…], later:[…]}`. The single source for list, badge, and title. Both lists are sorted by `due_at`, then `id`, and the client never re-sorts. `push_subscribed` is true when any subscription row exists (instance-wide, §9.8). `now` is read once per request, and `server_time`, `overdue_count` and the list split all derive from it |
| POST | `/api/reminders` | `{text, extra_channels?}` plus exactly one of `preset`/`due_at` → 201 + the reminder |
| POST | `/api/reminders/{id}/done` | → 200 + the reminder. On a row that is already done: a 200 no-op that keeps the original `done_at` |
| POST | `/api/reminders/{id}/undone` | → 200 + the reminder. Rules in §4.1 |
| PATCH | `/api/reminders/{id}` | `{text?, extra_channels?, preset?, due_at?}` → 200 + the reminder (§8.4) |
| DELETE | `/api/reminders/{id}` | hard delete → 204. No UI shows it |
| GET | `/api/channels` | `[{name, enabled}]`, `ORDER BY name`, and the client never re-sorts. The URL is never in the response (§4.1). Read-only: writes are §7.4 |
| POST | `/api/push/subscribe` | `{endpoint, keys:{p256dh, auth}, application_server_key}` → 204 with no body. Stores `application_server_key` as `vapid_public` (§7.1). `ON CONFLICT(endpoint) DO UPDATE` overwrites `p256dh`, `auth` and `vapid_public`, and keeps `created_at`. Shape checks, each a 400 that names the field: `endpoint` is an absolute `https:` URL. `application_server_key` and `p256dh` are unpadded base64url that decodes to 65 bytes. `auth` is unpadded base64url that decodes to 16 bytes |
| POST | `/api/push/unsubscribe` | `{endpoint}` → 204, also for an unknown endpoint. No UI shows it |
| POST | `/api/push/test` | see §7.5 |
| GET | `/healthz` | no auth. Answers 200 only after `SELECT 1` succeeds |

(test: TestConfigProjection, TestConfigVersionInBothEndpoints, TestState, TestUnsubscribeAndPushSubscribed, TestCreateReminderShape, TestMarkDoneKeepsOriginalStamp, TestDoneUndoneFlow, TestMarkUndoneGuard, TestDeleteReminderEndpoint, TestChannelsEndpoint, TestSubscribeUpsert, TestSubscribeShapeChecks, TestHealthz, TestHealthzOverBrokenDatabase. Partly tested: the single `now` of `/api/state`, in TestState)

- **`/api/` is registered as a catch-all that returns a JSON `404`**, behind the same auth, so a misspelled endpoint never gets `index.html` with a 200. A wrong method on a real path also gets `404`, not `405`. (test: TestAPICatchAll, TestAPIRequiresAuth)
- Reminder object: `{id, text, due_at, notified_at, done_at, extra_channels, delivery_error}`. `pushed_at` is absent (§4.1). `text` is raw, and the client renders it (§9.10). `extra_channels` is always an array, `[]` and not `null`. `notified_at`, `done_at` and `delivery_error` are `null` when unset. Timestamps are raw. All human formatting is client-side (§9.11). (test: TestCreateReminderShape, TestState)
- Every mutating endpoint returns the affected reminder, or 204 for a delete. The client follows every successful mutation with a `/api/state` fetch and does not splice the response into its list. (untested)
- Request bodies are capped at **16 KB** (`http.MaxBytesReader`). A body over the cap is a 400, on `/login` too. (test: TestBodyOverTheCapIsRejectedBeforeAuth)
- Server timeouts: `ReadHeaderTimeout: 10s`, `ReadTimeout: 15s`, `WriteTimeout: 15s`, `IdleTimeout: 120s`. The §7.5 budget derives from `WriteTimeout`. (partly tested: TestPushTestBudgetLeavesHeadroomUnderWriteTimeout)
- `/api/state` returns the full `later` list, unpaginated.
- `/api/state` carries a weak `ETag` (`W/"…"`) that hashes the body with `server_time` excluded, and answers a matching `If-None-Match` with a `304` and no body. On a `304`, the client reads the clock from the `Date` header (§9.3) and sends the ETag that it holds in a variable. (test: TestStateETag, TestStateETagExcludesServerTime)
- `/api/*` responses carry `Cache-Control: no-store` (§10.2). (test: TestCachingSplit)
- No history endpoint in v1: cleared reminders are kept (§4.4) but not exposed. (untested)

### 8.3 Validation and errors

- Every rejection is `{"error":"<lowercase sentence>"}` with a `4xx`. The client shows the string verbatim, so write it for a human. (test: assertBadRequest and assertErrorShape across the httpapi tests)
- Every `5xx` is `{"error":"internal error, see the server log"}` and never carries an internal error string. The real error goes to the request's log line (§10.4). (partly tested: TestHealthzOverBrokenDatabase)
- `text`: required, trimmed, non-empty after trimming, at most **1000 bytes** (bytes, not runes, to bound the push payload), valid UTF-8, and **no control characters**, newlines included. The 400 names the character class. (test: TestCreateReminderValidation, TestCreateReminderShape, TestPatchValidation)
- Markdown links inside `text` are not validated: the text is stored as typed. A link with a scheme other than `http`/`https` renders as literal text (§9.10). (untested)
- `preset` must match a configured `key`, else a 400 that says so (§9.2 says what the client does). (test: TestCreateReminderValidation)
- `due_at`: any integer in `946684800 .. 4102444800` (2000-01-01 to 2100-01-01 UTC), the past included. A backdated row never notifies (§4.1). Out of range is a 400 that names both bounds. (test: TestCreateReminderValidation, TestBackdatedCreateCarriesBothStamps)
- Exactly one of `preset`/`due_at` on a create, at most one on a PATCH. Both together, or neither on a create, is a 400. The server has no fallback to `default_preset`: the client always resolves it. (test: TestCreateReminderValidation, TestPatchValidation)
- **PATCH fields:** an absent key is unchanged. `extra_channels: null` or `[]` clears the list. `text: null` or `""` is a 400, and `due_at` cannot be unset. An empty object is a 400 (`"nothing to change"`). (test: TestPatchValidation, TestPatchChannelSemantics, TestUpdateChannelsClearRule)
- An unknown field is a 400 that names the lexically first unknown key. (test: TestPatchValidation, TestCreateReminderValidation)
- `extra_channels`: at most 16 names, de-duplicated, and sorted by name before it is written. (test: TestExtraChannelsCanonicalized, TestCreateReminderValidation)
- Every new name in `extra_channels` must exist in `channels`, else a 400 that names it. A disabled channel is accepted. On a PATCH, a name already stored on the row is accepted even if it no longer exists: you can carry an orphan forward, but not add one. (test: TestPatchChannelSemantics, TestExtraChannelsCanonicalized)
- Unknown `{id}` → 404. Malformed JSON → 400. (test: TestLifecycleUnknownIDsAre404, TestPatchValidation)

### 8.4 Create, edit, and re-snooze are one path

`POST /api/reminders` and `PATCH /api/reminders/{id}` accept the same fields, and the client shows the same control for both (§9.2). There is no `/snooze` endpoint: a chip on a row, a new time from the picker, and a text fix are all `PATCH`, with the re-time rules of §4.1. A `PATCH` with only `text` leaves `done_at` alone. (test: TestPatchReTimeUnclears, TestUpdateTextOnlyLeavesLifecycleAlone)

---

## 9. Frontend

No JS test runner exists, so "(untested)" on a browser rule means that nothing executes `app.js` or `sw.js` to check it.

### 9.1 Visual direction

"Patina structure, Horizon palette": generous spacing, pill-shaped actions, an italic serif for times, a humanist sans for everything else.

- Upcoming items are drawn in quiet ink. Only overdue items carry full contrast. (untested)
- The two token sets use identical names. The light set is declared on bare `:root`. The dark set redefines the same names inside `@media (prefers-color-scheme: dark)`. No rule is scoped to a `data-theme` attribute.

| token | dark | light |
|---|---|---|
| `--surface` | `#212A2E` | `#EFF1F0` |
| `--surface-lift` | `#263034` | `#F7F8F8` |
| `--sheet` | `#1C2428` | `#FFFFFF` |
| `--field` | `#1A2226` | `#FFFFFF` |
| `--ink` | `#E4E9EA` | `#1E282C` |
| `--ink-quiet` | `#8DA0A7` | `#5C6C72` |
| `--ink-faint` | `#5D7078` | `#97A3A7` |
| `--edge` | `#334147` | `#D3DAD9` |
| `--late` | `#E0A33E` | `#96620F` |
| `--late-ink` | `#212A2E` | `#FFFFFF` |
| `--chip` | `#2C383D` | `#E2E6E5` |
| `--chip-ink` | `#C2CED2` | `#3C4A4E` |
| `--scrim` | `rgba(8,12,14,.62)` | `rgba(30,40,44,.42)` |

(test: TestCSSTokensMatchTheSpecTable)

- The theme follows `prefers-color-scheme` only: no toggle, no override, no theme setting in the TOML, no persistence. (partly tested: TestCSSTokensMatchTheSpecTable, which checks the media block but not the absence of a toggle)
- Fonts are these two system stacks only. Nothing is downloaded and nothing is embedded: there is no `@font-face` and no `web/fonts/`. (untested)

```css
--body: "Optima", Candara, "Gill Sans", "Gill Sans MT", sans-serif;   /* UI */
--time: "Iowan Old Style", Palatino, "Palatino Linotype", Georgia, serif;  /* times */
```

- The CSP carries `font-src 'none'` (§10.2). (test: TestSecurityHeadersOnEveryResponse)

### 9.2 Layout

One column, single page. Top to bottom:

1. **Capture bar**: a single-line text input, the preset chips from `/api/config` in file order, then a final "Pick a time" chip. (untested)
   - A chip saves immediately on click or tap: one interaction (Goal 1). (untested)
   - On a successful save, the input and the channel selection clear. If a save fails, the typed text stays and a toast shows the error. (untested)
   - The input is autofocused on desktop only (`min-width: 701px` and `hover: hover`). (untested)
   - Every chip, "Pick a time" included, is `disabled` while the input is empty or whitespace-only. (untested)
   - The input carries `maxlength="1000"`. The server stays the authority on the byte length (§8.3). (partly tested: TestCreateReminderValidation, server half only)
2. **Notification slot** (§9.8), directly under the capture bar. It is absent when push works. (untested)
3. **"Needs you now"** zone: the overdue count in amber, rows sorted by `due_at` ascending, full-contrast text, the lateness in amber, a lifted background, and a 2px amber left spine. (untested)
4. **"Waiting quietly"** zone: quiet ink, sorted by `due_at` ascending. (untested)
5. **Empty state**: "Nothing waiting." / "Type above and pick when it should come back." It replaces both zones when both lists are empty. A zone with an empty list hides its heading. (untested)

**Rows.**

- Row actions are `Clear` and `Edit`. Only "Needs you now" rows also get the re-snooze chips: the presets with `quick = true`, each one a `PATCH` with that `preset` (§8.4). (untested)
- On desktop, the actions show on `:hover, :focus-within`. On mobile, they always show. (untested)
- `Clear` shows a 5-second undo toast that calls `/undone`. There are no confirm dialogs anywhere. (untested)
- A click on the row text, on its time, or on `Edit` expands the row into an inline editor: the capture bar component, prefilled with the raw text (markdown links included), all preset chips, "Pick a time", and the channel selector. It saves with `PATCH` (§8.4). Its keys are in §9.4. (untested)
- A click on a link inside the row text follows the link and does not open the editor. (untested)

**Channels.**

- The capture bar has an "Also send to" toggle that expands the channel chips from `/api/channels` (multi-select). A disabled channel is grayed and cannot be selected. (untested)
- If `/api/channels` is empty, the capture bar does not render the toggle. (untested)
- No chip is preselected, and the selection resets after each save. (untested)
- **The client persists nothing**: no preference cookie, no `localStorage`. The server or the OS holds every piece of state, so the app behaves the same in every browser. (untested)
- The row editor renders its channel chips when `/api/channels` is non-empty or when the row carries any names. An attached disabled channel renders selected, grayed, and deselectable. (untested)
- A name on the row that `/api/channels` does not list (an orphan, §4.1) renders as a grayed chip labeled with the name, selected and deselectable. After a deselect, it cannot be selected again. (untested)
- The editor's `PATCH` always carries `text` and the full `extra_channels` that its chips show selected, orphans included. It adds a timing field (`preset` from a chip, `due_at` from the picker) only if a chip or the picker chose a moment in this editing session. (partly tested: TestPatchChannelSemantics, server side only)
- A row with channels carries a small quiet marker. If `delivery_error` is set, the marker is amber and carries the error as its accessible label, and the expanded editor shows the full error text. (untested)

**Failure states.**

- A failed API call shows the `error` string of §8.3 in a toast. Nothing is optimistic, except the `Clear` undo. (untested)
- A `401` from any call replaces the list with the token field. (untested)
- A failed poll is not a toast. The last good list stays. After two consecutive failures, one quiet line shows under the capture bar: "Not connected — last updated 3 minutes ago", timed from the last successful poll. It clears on the next success. A poll failure never clears the list and never touches the badge. (untested)
- A failed user action shows a toast at once, on the first failure. (untested)
- A `400` that names an unknown `preset` makes the client fetch `/api/config` again and redraw the chip rows in place (the capture bar and any open row editor). Typed text, the open editor, and the picker's pending value survive. (partly tested: TestUnknownPresetErrorKeepsTheClientPrefix checks the server prefix that `app.js` matches. The redraw itself is untested.)
- On a `config_version` change (§5.5), the reload waits until the capture input is empty, no row editor is open, and the picker is closed. (untested)
- The list is rendered keyed by reminder id. A render pass updates the row nodes in place, removes only the rows that left the response, and inserts only the rows that arrived. (untested)
- The one exception: a render pass skips a row with an open editor entirely. If that row left the response, it stays on screen until the editor closes, and the next pass then reconciles it. (untested)
- Toasts live in one `aria-live="polite"` region. The toast's `Undo` is a focusable button. There is no undo shortcut. (untested)
- While a create or save request is in flight, the control that submits it is disabled. (untested)

### 9.3 Badge semantics

- The badge shows the overdue count only, never the total. (partly tested: TestState, server side `overdue_count` only)
- `setBadge(n)` does three things: `navigator.setAppBadge(n)` where it exists, a canvas-drawn favicon with a count dot in `link[rel=icon]`, and the title `(n) Nag`. (untested)
- `setBadge(0)` is a full reset: `clearAppBadge()`, the plain `32.png` favicon, and no title prefix. (untested)
- The base title is the constant `Nag`, never read back from `document.title`. (untested)
- Poll `/api/state` every 45 s while visible, at once on `visibilitychange` to visible, after each successful mutation (§8.2), and on a service-worker message (§9.5). A hidden tab keeps polling every 5 minutes. (untested)
- All `/api/state` fetches share one `AbortController`, and a new fetch aborts the one in flight. An aborted fetch updates nothing: not the list, the badge, the clock offset, or the stored ETag. (untested)
- A `304` updates the clock offset from the `Date` header, re-renders the rows in memory, re-arms the due timer, and redraws the badge. Only a `200` replaces the list. (partly tested: TestStateETagExcludesServerTime, server half only)
- The badge and the lists are never held by the push cooldown (§7.3): a held reminder is in "Needs you now" and in `overdue_count` as soon as it is due, because the count comes from `due_at` and `done_at` only. The cooldown delays only the OS notification. Why: this is what makes a 30-minute cooldown on notifications acceptable. (partly tested: TestState, TestFanOutIsNotHeldByTheCooldown)
- A one-shot timer fires at the earliest `later.due_at` and polls. It is armed regardless of visibility. (untested)
- The timer is re-armed from the first element of `later` (server-sorted, §8.2) on each poll that answers `200` or `304`, so also after each fire. (untested)
- Its delay is clamped: `Math.min((due_at - now()) * 1000, 2_000_000_000)`. (untested)
- `manifest.webmanifest`: `name`, `short_name`, `id: "/"`, `start_url: "/"`, `scope: "/"`, `display: "standalone"`, `background_color` and `theme_color` set to the dark `--surface`, and `icons` at 192px, 512px, and 512px `maskable`. (test: TestManifestPinnedValues, TestManifestContentType)
- `web/icons/` holds exactly four PNGs: `192.png`, `512.png`, `512-maskable.png` (safe zone in the inner 80%), and `32.png`. (test: TestIconsAreEmbeddedAtTheirNamedSizes)
- The notification `icon` is `192.png`, its `badge` is `32.png`. There is no `apple-touch-icon`. (untested)
- The client never computes the overdue/later split. It stores a server clock offset on every poll (`server_time`, or the `Date` header on a `304`) and reads every relative string through one `now()`. The offset corrects skew, not zone: the browser gives the timezone (§6). (partly tested: TestState, which checks that `server_time` is returned)

### 9.4 Keyboard

- `/` or `n` focuses the capture input. They are ignored when focus is in a text field (`input`, `textarea`, `[contenteditable]`) and when the picker or a row editor is open. (untested)
- `Enter` saves on its surface: the capture bar with `default_preset`, a row editor the text without touching the time, the picker `Set reminder` (§9.6). (untested)
- `Esc` acts on one thing per press, innermost first: the picker, then the row editor (it cancels the edit), then a non-empty capture input. With nothing open and an empty input, it does nothing. (untested)
- There are no other shortcuts: no per-preset digits and no `<kbd>` hints. (untested)

### 9.5 Service worker

- The worker is served from `/sw.js`. (test: TestServiceWorkerIsEmbeddedAtTheRoot, TestCachingSplit)
- `app.js` registers it on every load, with no gesture, awaits the registration once, and never calls `register` again. (untested)
- Push support is detected once: `'serviceWorker' in navigator`, `'PushManager' in window`, and `'Notification' in window`. If one fails, or `register` rejects, the app skips push, shows the quiet line in the slot (§9.8), and the lists, the badge, the picker, and every write still work. (untested)
- `push` draws exactly one notification, then posts `{type:'state-changed'}` to every client. It never draws one per `texts` entry. (partly tested: TestPayloadForms, TestPayloadTextsCappedAtThree, payload side only)
- The whole `push` chain is wrapped in `event.waitUntil(...)`. (untested)

  | payload | title | body |
  |---|---|---|
  | `n == 1` | `texts[0]` | `whenText(due_at)` |
  | `n > 1` | `${n} reminders need you` | `texts` joined with ` · `, then ` · +${n - texts.length} more` when `n` exceeds what was sent |

- Options in both cases: `{tag: 'nag', renotify: true, data: {id}, icon, badge}`. `data.id` is present only when the payload carries an `id`. (partly tested: TestSendToOptionsAndLogging, TestPayloadForms, server side only)
- The worker carries its own copy of the time formatter (§9.11). It uses the device clock, with no offset. (test: TestWorkerFormatterMatchesThePage)
- A `push` always draws. If the data is absent, does not parse, or carries no usable `texts`, the title is `Nag reminder`, with no body and the same `tag` and `renotify`. (untested)
- No `fetch` handler and no cache. (untested)
- `install` calls `skipWaiting()`, `activate` calls `clients.claim()`. (untested)
- `notificationclick` closes the notification, then focuses an existing client (`clients.matchAll({type: 'window', includeUncontrolled: true})`) or opens one, all inside `event.waitUntil`. An existing client gets `{type:'focus-reminder', id}` if `data.id` is present, else `{type:'state-changed'}`. (untested)
- A new window opens on `/?focus=<id>`, or on `/` without an id. `app.js` treats `focus` exactly like a `focus-reminder` message, then calls `history.replaceState({}, '', '/')`. (untested)
- The page switches on `type`. `focus-reminder` refreshes state, scrolls the row into view, and flashes it for ~2.4 s (respecting `prefers-reduced-motion`). `state-changed` refreshes state and does nothing else. (untested)
- If the id is in neither list after the refresh, the app is focused and nothing else happens: no scroll, no flash, no toast, no error. (untested)
- `pushsubscriptionchange` re-subscribes with the key from `GET /api/config` and posts it to `POST /api/push/subscribe`. If the fetch fails, it does nothing. (untested)
- Dismissing a notification does nothing: there is no `notificationclose` handler. The reminder stays overdue (Goal 3). (untested)
- Notifications are not synced between devices. A reminder cleared on one device leaves the notification on another until the OS drops it. The list is the truth. (untested)

### 9.6 Picker

The picker is a bottom sheet.

- Its foot has `Cancel` (ghost) and `Set reminder` (filled). The wheels and the calendar move a pending value only. Nothing is written until `Set reminder`: a `POST` with a `due_at` from the capture bar, a `PATCH` from a row editor (§8.4). It closes the sheet on success. `Cancel`, `Esc`, and a scrim click discard. (untested)
- A live summary shows `fullText` and a relative line (§9.11). A past moment is allowed, and the relative line then reads "in the past — it will wait in Needs you now, and nothing will be sent". (untested)
- A month calendar with prev/next month, weeks starting per `week_start`. Today has a thin ring, the selected day is amber-filled, and past days are dimmed (`--ink-faint`) but tappable. "Today" and "past" come from `now()` (§9.3). (untested)
- There is no year stepper and no upper bound on paging. (untested)
- The chosen wall-clock time is read in the browser's timezone: `new Date(y, m, d, hh, mm, 0, 0)`, then `Math.floor(getTime() / 1000)`. There is no DST-gap detection (§6). (untested)
- The hour wheel holds `hour_min..hour_max` inclusive. The range is a hard limit, with no "show all hours" option. The minute wheel holds `0..59` by `minute_step`. (untested)
- Opening state for a new reminder: `default_time` clamped into `[hour_min, hour_max]`, on today if that is still ahead of `now()`, else tomorrow. (untested)
- Opening state when editing: the stored date, the hour clamped into `[hour_min, hour_max]`, and the minute `00`. If that moment is not ahead of `now()`, the new-reminder rule applies. The sheet never opens on the past sentence. (untested)
- The wheels are 24-hour. The summary is locale-formatted. (untested)
- The sheet is a modal: `role="dialog"`, `aria-modal="true"`, `aria-labelledby` the summary line. Focus moves in on open and returns to the opening chip on close. `Tab` is trapped inside. (untested)

### 9.7 Interaction states

- `-webkit-tap-highlight-color: transparent` on interactive elements, and a designed `:active` scale of ~0.96. (untested)
- Focus rings only through `:focus-visible`: `2px solid var(--late)`, offset 2px. (untested)
- All reminder text and error strings are written with `textContent`, links included (§9.10). `app.js` contains no `innerHTML`. (untested)
- `index.html` has no inline `style` attribute and no `<style>` block (§10.2 `style-src 'self'`). (test: TestIndexHasNoInlineStyle)
- CSP does not govern the CSSOM, so JS can set a style property (`el.style.setProperty`). Prefer a class, and use `setProperty` only for a number that CSS cannot know. (untested)

### 9.8 Notification permission

- Nothing about push is requested on load. Permission is asked only from a click. (untested)
- The slot shows the "Turn on notifications" button only when one of these holds: `Notification.permission !== 'granted'`, no local `pushManager.getSubscription()`, `push_subscribed` is false, or this load's re-post answered anything other than 2xx. If push is unsupported (§9.5), the slot shows the quiet line instead. Otherwise the slot is absent. There is no permanent settings control. (untested)
- On click: `requestPermission()`, then `subscribe({userVisibleOnly: true, applicationServerKey})`, then `POST /api/push/subscribe` with `application_server_key` set to the `vapid_public` string that it subscribed under (§7.1). (partly tested: TestSubscribeUpsert, server side only)
- `vapid_public` is decoded from unpadded base64url to 65 bytes for `subscribe`, and encoded back with the same alphabet. The two conversions are exact inverses. (untested)
- If permission is `denied`, one quiet line replaces the button: the browser blocks notifications, and the user must allow them again in site settings. (untested)
- On every load, if a local subscription exists, it is posted to `POST /api/push/subscribe` unconditionally. The slot is decided only after that re-post settles. The lists do not wait. (untested)
- Before the re-post, the local `options.applicationServerKey` is encoded as unpadded base64url and compared to `vapid_public`. On a mismatch, the client unsubscribes and subscribes again with the configured key. The key posted is the key compared. A `null` or unavailable readback is not a mismatch: the subscription is kept and posted with `vapid_public`. (untested)
- Cost of the `null` rule: a subscription truly bound to a retired key then takes the matching branch of §7.1 on its 403. The manual fix is to reset (not block) the site's notification permission, or `POST /api/push/unsubscribe`.
- On iOS, when the page does not run standalone, the slot explains that notifications need the app on the Home Screen first (§7.1). (untested)

### 9.9 Mobile (≤700px)

- Rows wrap: the text on its own full-width line, then the time bottom-left and the actions bottom-right. Container padding 18px, tap targets ≥40px, reminder text 16px, capture input 16px (so iOS does not zoom), and no autofocus (§9.2). (untested)

### 9.10 Reminder text: one markdown link form

There is no `url` column. A link lives inline in the text as `[label](https://example.com/x)`. That one form is the whole grammar: no emphasis, no code spans, no autolinking, no reference links.

- A link is `[label](target)` with a non-empty `label` that has no `[` or `]`, and a `target` with no whitespace and no `)`. Non-greedy, left to right: `\[([^\[\]]+)\]\(([^\s)]*)\)`. (test: TestClientLinkRegexMatchesFlattenLinks, TestFlattenLinks)
- A match renders as a link only if `target` has scheme `http` or `https` and contains no `(`. Anything else is emitted whole, as literal text. The workaround for a URL with parens is to percent-encode them (`%28`, `%29`). (partly tested: TestFlattenLinks, Go side only)
- The renderer appends `document.createTextNode` for the text runs and an `a` per link with `textContent = label`, `href = target`, `target="_blank"`, `rel="noopener noreferrer"`. No library parser. The `innerHTML` rule is in §9.7. (untested)
- Links are `color: var(--ink)` with an underline in `--ink-faint`, never `--late`. (untested)
- The server's one helper, `flattenLinks(text)`, replaces a match with its `label` only when the match renders as a link (same gate). Push payloads (§7.1) and shoutrrr messages (§7.2) use it. (test: TestFlattenLinks, TestPayloadFlattensLinks)
- Both copies, Go and JS, keep the single regex above and must agree on every row of this table:

| input | rendered |
|---|---|
| `[docs](https://x/y)` | one link, label `docs` |
| `see [docs](https://x) now` | text, link, text |
| `[a](https://x) and [b](https://y)` | two links |
| `[a](javascript:alert(1))` | literal text, no anchor |
| `[a](mailto:x@y.z)` | literal text, no anchor |
| `[Foo](https://w/Foo_(bar))` | literal text — unencoded `(` (see above) |
| `[Foo](https://w/Foo_%28bar%29)` | one link |
| `[](https://x)` | literal text — empty label |
| `[a](https://x` / `a](b)` / `[[a]](b)` | literal text |
| `https://x/y` | literal text — no autolinking |

(partly tested: TestFlattenLinks runs the table in Go. The JS side shares only the regex.)

### 9.11 Time strings

- Only `whenText` and `fullText` in `app.js` format a time. The worker's copy is identical. (partly tested: TestWorkerFormatterMatchesThePage)
- `whenText(due_at)` is the short form: the row time (§9.2), the picker's relative line (§9.6), and the notification body (§9.5). `d = due_at - now()`, with `now()` from §9.3 (the worker uses the device clock). The first match wins, top to bottom:

| condition | string |
|---|---|
| `d <= -86400` | `N days late` |
| `d <= -3600` | `N h late` |
| `d <= -60` | `N min late` |
| `-60 < d < 60` | `now` |
| `d < 3600` | `in N minutes` |
| same calendar day | `today at <time>` |
| next calendar day | `tomorrow at <time>` |
| 2 to 6 calendar days ahead | `<weekday> at <time>` |
| otherwise, in the current calendar year | `<day month> at <time>` |
| otherwise | `<day month year> at <time>` |

(untested)

- The top three rows are durations. From `today` down, every row is a calendar comparison in the browser's timezone: the difference in days between two local midnights. The year shows only when it is not the current year. (untested)
- Each count is the floor of its unit (90 minutes late is `1 h late`), and singular at 1 (`1 day late`, `in 1 minute`). (untested)
- `<time>`, `<weekday>`, `<day month>`, and `<day month year>` are formatted with `Intl.DateTimeFormat(undefined, …)`. The connective words (`late`, `at`, `today`, `in`) are English. (untested)
- The picker's relative line uses the future rows unchanged. At `d <= 0`, it shows the fixed past sentence of §9.6, never `late`. (untested)
- `fullText(due_at)` is the long form, used only by the picker summary: `Intl.DateTimeFormat(undefined, {weekday: 'long', day: 'numeric', month: 'long'})` plus the same `<time>`, emphasized: `Saturday 15 August at **09:00**`. The year shows under the same rule as in `whenText`. (untested)

---

## 10. Deployment

The `Dockerfile`, `.dockerignore`, `compose.yaml`, `compose.caddy-docker-proxy.yaml` and `.github/workflows/release.yml` are the reference. ADR-003 records the compose and release design.

**Image**

- The final image is `FROM scratch`. It holds `/etc/ssl/certs/ca-certificates.crt`, because Web Push and shoutrrr make outbound TLS calls. (untested)
- It runs as `USER 65534:65534`, exposes `8080`, and has the entrypoint `/nag` with the default command `serve`. (untested)
- It declares `HEALTHCHECK --interval=60s --timeout=5s --start-period=10s CMD ["/nag", "healthcheck"]`. (untested)
- It declares no `VOLUME`. The mounts are the compose bind mounts (§10.1). (untested)
- `main.version` is the only build-time variable. It defaults to `dev`. `nag version` prints it, and the boot log shows it (§10.4). (partly tested: TestVersionOutput)
- The documented local build is `docker compose build --build-arg VERSION=$(git describe --tags --always --dirty)`. A boot line that reads `dev` means that someone built the image without `VERSION`. (untested)
- `.dockerignore` is required. It excludes at least `nag.env`, `.env`, `data/`, `config/`, `*.db*`, `.git/` and `*.md`. Why: the build context is the project directory, so without it `COPY . .` stores the VAPID private key and the database in the cached builder layer. (partly tested: TestDockerignoreKeepsSecretsOutOfTheBuild)

**Compose**

- `compose.yaml` publishes the port on loopback only (`127.0.0.1:8080:8080`) and assumes no particular reverse proxy. The app is never on plain HTTP from the internet. (untested)
- `compose.yaml` sets `restart: unless-stopped` and `mem_limit: 256m` (a ceiling for a runaway channel reply, §7.2). (untested)
- The host's HTTPS proxy must forward the original `Host` header. If it rewrites `Host`, the `Origin`-against-`Host` fallback of §10.2 returns `403` on every mutating request. (untested)
- caddy-docker-proxy is an opt-in file, `compose.caddy-docker-proxy.yaml`, layered on `compose.yaml`. One line in `.env` (Compose's own project file, not `nag.env`) enables it per host: `COMPOSE_FILE=compose.yaml:compose.caddy-docker-proxy.yaml`. So every documented command stays a plain `docker compose …`. With the add-on, the loopback port stays published and unused. `.gitignore` and `.dockerignore` both name `.env`. (untested)

**Release**

- A push of a `v*` tag runs `release.yml`. It builds `linux/amd64` and `linux/arm64` with `VERSION` set to the tag, and pushes `ghcr.io/kamalf/nag:<version>` and `:latest`. It logs in with its own `GITHUB_TOKEN`. (untested)
- `nag version` prints the tag with its `v` (`v0.1.2`). The image tag has no `v` (`0.1.2`). (untested)
- `compose.yaml` names both `image:` and `build: .`. If the image is missing, Compose pulls it. If the pull fails, Compose builds the checkout. `docker compose build` builds under the same name. To update, run `docker compose pull`, then `up -d`. (untested)

### 10.1 Scratch-image consequences

- **`/tmp`.** The image has a writable `/tmp` (mode `1777`, owned by 65534) and sets `TMPDIR=/tmp`. Without it, SQLite fails on its first temp file with an error that looks like corruption. The `Dockerfile` comment explains why the copy is `/empty` → `/`. (untested)
- **Ownership.** There is no `/etc/passwd`, so the user is a bare numeric uid. Before the first `up`, create `data/` and `config/` on the host and `chown -R 65534:65534` them (README setup steps). The app runs non-root against these bind mounts, not named volumes (the reasons are in `compose.yaml`). A wrong uid makes boot refuse to start and name the path (§4.3). (partly tested: TestUnopenableFileRefusesNamingPath)
- **Healthcheck.** There is no curl. `nag healthcheck` builds its URL from `NAG_ADDR`. It keeps the port and uses the host as given, but dials `127.0.0.1` when the host is empty, `0.0.0.0` or `::`. So `:8080` gives `GET http://127.0.0.1:8080/healthz`. There is no second env var. (test: TestHealthcheckURL)
- The healthcheck timeout is 2 seconds, and the exit code is 0 only on `200`. (partly tested: TestHealthcheckExitCode)
- Nothing acts on the health status. `restart: unless-stopped` does not restart an unhealthy container, and there is no autoheal sidecar. It is a status to read in `docker ps`. (untested)
- **Shutdown.** The binary is PID 1, so it handles `SIGTERM` and `SIGINT` itself, in this order: `http.Server.Shutdown` with a ~5 s timeout, cancel the sweep's context, **wait for the sweep goroutine to return**, then `db.Close()` so that SQLite checkpoints the WAL. Why: a tick can outlast Docker's 10-second stop grace period, and a close under a running tick is a mid-write close. (test: TestServeUntilShutdownOrder, TestServeUntilListenFailure)
- Every outbound call in the sweep uses a context derived from the tick's context. So the cancel ends the in-flight send at once (a shoutrrr send that cannot take a context is abandoned, §7.2), and the tick stops at the next row boundary. The wait stays at about a couple of seconds. (partly tested: TestShutdownMidFanOut)

### 10.2 Response headers

One middleware sets these on every response:

```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self';
                         img-src 'self' data:; font-src 'none'; connect-src 'self';
                         object-src 'none'; base-uri 'none'; form-action 'self';
                         frame-ancestors 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
```

(test: TestSecurityHeadersOnEveryResponse)

- `style-src 'self'` forbids a `style` attribute in the markup. It does not govern style changes that JS makes through CSSOM (§9.7). (test: TestIndexHasNoInlineStyle checks the markup)
- **Caching.** `/api/*` gets `Cache-Control: no-store`. The embedded assets (`index.html`, `app.js`, `app.css`, `sw.js`, `manifest.webmanifest`) and the icons get `Cache-Control: no-cache` plus an `ETag` derived from the embedded content. Never a `max-age`. (partly tested: TestCachingSplit)
- **CSRF, rule 1.** Every mutating browser request must be same-origin. `http.CrossOriginProtection` reads `Sec-Fetch-Site`. If that header is absent, it compares `Origin` with `Host`. A mismatch is a `403`. A request with neither header (a non-browser client with the bearer token) passes. (test: TestCrossOriginMutationsRefused)
- **CSRF, rule 2.** A mutating request with a body and a non-JSON `Content-Type` is refused with `415`. (test: TestJSONContentTypeRule)
- Bodyless mutations are exempt from rule 2: `POST /api/push/test`, `/done`, `/undone`, and `DELETE /api/reminders/{id}`. `/logout` is exempt too. Why: a header requirement only breaks the `curl` commands that these endpoints serve, and rule 1 already refuses a form from another origin. (partly tested: TestJSONContentTypeRule)

There is no CSRF token.

### 10.3 Backups

- **With WAL on, a copy of `nag.db` alone can be corrupt.** Either stop the container and copy `data/nag.db*`, or build a backup on `VACUUM INTO`. A clean stop (§10.1) checkpoints the WAL into `nag.db` and removes `-wal` and `-shm`. (untested)
- Keep `./data` on local disk, never on NFS: WAL misbehaves on network filesystems. (untested)
- With the caddy-docker-proxy add-on, the container must join the network that caddy-docker-proxy watches, or caddy silently ignores the labels. (untested)

### 10.4 What gets logged

Logs are the only observability (§2).

- **Four things are never logged, in any line, at any level:**
  - `NAG_TOKEN` or a submitted token, also on a failed `/login`. (untested)
  - a `channels.url`. (partly tested: TestFanOutDeliveryError, TestChannelAddRefusalHidesTheURL)
  - a push endpoint in full. Log the truncated form of §7.5. (test: TestPushEndpointsLogTruncatedForm, TestSendToOptionsAndLogging, TestPushTestLogsAttemptedAndDeletedCounts)
  - reminder `text`. Lines name the id instead. Why: the log stays safe to paste into an issue. (partly tested: TestCreateLogsTheNewID, TestPushLineNamesIDsAndNeverText)
- **One INFO line per mutating request** (`POST`/`PATCH`/`DELETE`): method, path, status, duration, and the affected reminder id. The id comes from the path, or from the response on a create. Why: after the 5-second undo toast is gone, the logged id is the only record of what to `POST …/undone`. (test: TestMutatingRequestsLogExactlyOneID, TestCreateLogsTheNewID)
- The push endpoints have no reminder id. `/api/push/subscribe` and `/api/push/unsubscribe` log the truncated endpoint form (§7.5), never the received URL. `/api/push/test` logs how many subscriptions it attempted and how many rows it deleted. `/login` and `/logout` log neither an id nor a token. (partly tested: TestPushEndpointsLogTruncatedForm, TestPushTestLogsAttemptedAndDeletedCounts)
- **One line per `4xx`/`5xx`**, whatever the method, including every failed `/login`. (partly tested: TestRequestLogging)
- **Successful `GET`s are not logged.** (test: TestRequestLogging)
- **The sweep logs decisions, not ticks:** rows marked, rows skipped as too late with their ids (§7.3), rows purged, subscriptions deleted, every delivery failure, and rows that phase 3 dropped at its deadline with their ids. (test: TestPushLineNamesIDsAndNeverText, TestPurgeBoundary, TestFanOutDeliveryError, TestFanOutDeadlineDropsRemainingRows)
- **One line per push**, with `n`, the ids it covered, and how many subscriptions it went to. (test: TestPushLineNamesIDsAndNeverText)
- A tick that marks rows but holds them logs one line: the number of held rows and the seconds left in the cooldown window. (test: TestRowDueInsideTheWindowWaitsForIt)
- **Every line in this section is INFO or above, and there is no log level to configure.** Why: a DEBUG line would need a `NAG_LOG_LEVEL` that §5.1 does not have, and a restart, to read about a sweep that already ran. (partly tested: the tests use the default INFO handler)
- At boot, one line each: build version (§10), listen address, resolved DB path, resolved config path (and whether it was just written from the default, §5.3), timezone, preset count, `config_version`, subscription count. The VAPID-mismatch WARNING (§7.1) joins them only when it applies. (untested)

---

## 11. Decisions log

The decisions and their reasons are stated in the sections above and in the code comments. Cross-cutting decisions are recorded as ADRs in `docs/adr/` (see `docs/adr/INDEX.md`).
