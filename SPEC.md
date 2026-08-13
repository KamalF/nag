# Nag — implementation spec

A personal, self-hosted reminder app that reproduces the Slack "remind me about this" workflow:
capture a note, pick a preset delay, get an OS notification, and see it sit in a list until you
manually clear it.

Single user. Single instance. No multi-tenancy, no accounts, no sync protocol.

---

## 1. Goals

1. **Capture in one interaction** — type text, tap a preset, done. No date picker in the common path.
2. **Notify at the due time** through Firefox/Chrome as a real OS notification, on laptop and phone.
3. **Persist until dismissed** — an item that has fired stays in an "overdue" list until explicitly cleared. This is the core behaviour and the reason existing tools don't fit.
4. **Glanceable from a pinned tab** — the favicon shows the count of items needing attention.
5. **Optional fan-out** to ntfy / IM, per reminder.

### Non-goals (v1)

- Recurring reminders (no rrule). Design leaves room but do not build it.
- Natural-language date parsing. Presets and an explicit picker only.
- Multi-user, sharing, teams.
- Native mobile app or mobile push beyond standard Web Push.
- Full-text search, tags, projects, priorities.
- Keyboard shortcuts for individual presets (see §9.4 and §12).

---

## 2. Stack

| Concern | Choice |
|---|---|
| Language | Go (single static binary, `CGO_ENABLED=0`) |
| Storage | SQLite, WAL |
| SQLite driver | `modernc.org/sqlite` (pure Go, no cgo) |
| Config | TOML via `pelletier/go-toml/v2` |
| Web Push | `github.com/SherClockHolmes/webpush-go` |
| Fan-out | shoutrrr — **nickfedor's maintained fork** (verify the exact module path on pkg.go.dev before pinning; upstream `containrrr/shoutrrr` has slowed). That check covers the **send API** as much as the path: `router.Send(message, params) []error` takes no context, which is why §7.2 wraps it |
| Timezones | `import _ "time/tzdata"` — required, the scratch image has no tzdata |
| Logging | `log/slog`, text handler to stderr. Logs are the only observability — §10.4 says what gets a line |
| Frontend | No build step. Hand-written HTML/CSS/JS, `//go:embed`'d. Every file served as-is — no templating, no server-side rendering |
| Fonts | **System stacks only, no webfonts** — nothing is downloaded, nothing is embedded (§9.1) |
| Router | stdlib `net/http.ServeMux` (Go 1.22+ pattern matching) |
| Reverse proxy | Caddy via caddy-docker-proxy labels |

HTTPS is mandatory — service workers and Web Push do not work over plain HTTP.

---

## 3. Repo layout

```
cmd/nag/main.go             # subcommand dispatch, wiring — no flags, see below
internal/config/            # TOML load, validate, defaults, SIGHUP reload
  nag.default.toml          # embedded here, written out on first boot (see below)
internal/store/             # SQLite open, migrations, queries
internal/presets/           # preset evaluation (tz-aware)
internal/notify/            # web push + shoutrrr fan-out
  sweep.go                  # the sweep goroutine (§7.3) — the only caller of both on the
                            #   schedule path; §7.4's `channel test` and §7.5 share the senders
internal/httpapi/           # handlers, auth middleware
web/                        # embedded frontend
  embed.go                  # package web, holds the //go:embed directive (see below)
  index.html
  app.js
  app.css
  sw.js                     # MUST be served from / (see §9.5)
  manifest.webmanifest      # required for the Badging API and iOS push (§9.3)
  icons/
nag.env.example
Dockerfile
.dockerignore               # not optional — see §10
compose.yaml
README.md
reminder-theme-preview.html    # design reference only — §9.1's tokens and layout
reminder-datetime-picker.html  # design reference only — §9.6's sheet
```

The two `reminder-*.html` files at the root are **reference mockups, not part of the app**: nothing embeds them, nothing serves them, and no route reaches them. They are the source §9.1 and §9.6 are read against, which is why they stay in the repo instead of in a screenshot — and why §10's `.dockerignore` names them.

**`//go:embed` patterns cannot contain `..`**, so both embedded trees live inside the package
that reads them: `nag.default.toml` sits next to the loader in `internal/config/` (it is still
*written out* to `NAG_CONFIG` on first boot, §5.3), and `web/` carries its own one-line
`embed.go` declaring `package web`. Neither can live at the repo root and be embedded from a
subpackage — that is a compile error, not a lint.

**There are no command-line flags anywhere**, on any subcommand. Everything that varies is an
environment variable (§5.1) or a TOML key (§5.3), and a flag would be a third place to look for
the same answer — one that `compose.yaml` would then have to carry in `command:`, out of sight of
both the env file and the config file. `main` dispatches on `os.Args[1]`, prints the usage block
below for a **missing** subcommand as well as for an unknown word or `-h`, and exits `2` — no
argument at all is the unknown-word case, never an index panic, because the default lives in the
Dockerfile's `CMD ["serve"]` (§10) and nowhere else.

Subcommands (§5.2, §7.4, §10):

```
nag serve          # the default, per CMD
nag genkeys        # emit a complete env file to stdout
nag healthcheck    # exit 0 if the local instance is serving; for Docker HEALTHCHECK
nag version        # build version + Go version to stdout, exit 0
nag config check   # validate NAG_CONFIG and exit; 0 if a reload would accept it
nag channel add|list|rm|enable|disable|test
```

---

## 4. Data model

SQLite. All timestamps are **Unix seconds, UTC**. Never store local time.

```sql
CREATE TABLE reminders (
  id             INTEGER PRIMARY KEY,
  text           TEXT    NOT NULL,        -- may contain [label](https://…) links, see §9.10
  due_at         INTEGER NOT NULL,
  notified_at    INTEGER,                 -- NULL = the sweep has not handled it yet
  pushed_at      INTEGER,                 -- NULL = handled, but not yet carried by a push (§7.3)
  done_at        INTEGER,                 -- NULL = still in the list
  created_at     INTEGER NOT NULL,
  extra_channels TEXT,                    -- JSON array of channel names, may be NULL
  delivery_error TEXT                     -- last fan-out failure, classified shape only (§4.1)
);
CREATE INDEX idx_reminders_pending ON reminders(due_at) WHERE done_at IS NULL;

CREATE TABLE push_subscriptions (
  id            INTEGER PRIMARY KEY,
  endpoint      TEXT NOT NULL UNIQUE,
  p256dh        TEXT NOT NULL,
  auth          TEXT NOT NULL,
  vapid_public  TEXT NOT NULL,            -- key this sub was created under, see §7.1
  created_at    INTEGER NOT NULL
);

CREATE TABLE channels (
  id      INTEGER PRIMARY KEY,
  name    TEXT NOT NULL UNIQUE,           -- lowercase slug, see §7.4; used in extra_channels
  url     TEXT NOT NULL,                  -- shoutrrr URL, CONTAINS SECRETS
  enabled INTEGER NOT NULL DEFAULT 1
);
```

### 4.1 Semantics

- **Schedule and lifecycle are independent.** `due_at` is when to ping; `done_at` is whether it's still your problem.
- **Overdue** = `done_at IS NULL AND due_at <= now`.
- **Later** = `done_at IS NULL AND due_at > now`.
- **`notified_at` and `pushed_at` answer two different questions, and both exist solely for the sweep.** `notified_at` is "the sweep has handled this row" — it is what makes phase 1 of §7.3 idempotent, and it is set whether or not anything was ever sent. `pushed_at` is "this row has been carried by a Web Push notification", which is a later and separate event because pushes are rate-limited to one per 30 minutes (§7.3). The set of reminders *held* by that cooldown is therefore a query — `notified_at IS NOT NULL AND pushed_at IS NULL AND done_at IS NULL` — and not in-memory state, so a restart mid-cooldown cannot lose a notification. Neither column carries client-facing meaning: `notified_at` is in the reminder object for `curl` visibility and nothing in `app.js` reads it, and `pushed_at` is **not returned at all** (§8.2) — it is bookkeeping with a 30-minute lifetime, and a field the UI must ignore is a field better not sent.
- **Re-time** = set the new `due_at`, `notified_at = NULL`, `pushed_at = NULL`, clear `delivery_error`, and `done_at = NULL` if it was set. One UPDATE, identical before or after firing and on an already-cleared row. There is no separate snooze operation in the data model or the API — a chip on a row and an edit are the same UPDATE (§8.4).
- **A write that lands in the past never notifies.** On create *and* on re-time, if the resulting `due_at <= now`, the same statement sets **both `notified_at = now` and `pushed_at = now`** instead of `NULL`. The row appears in "Needs you now" immediately, phase 1 of the sweep will never pick it up, and — because `pushed_at` is already stamped — it can never become a candidate for a digest either. This is what makes the picker's promise (§9.6) true unconditionally rather than only outside a 30-minute window, and it leaves §7.3's too-late gate doing the one job it was designed for: catching up after an outage. It also means a moment chosen a few seconds in the past — clock rounding, a slow tap — lands in the list you are already looking at instead of pinging you thirty seconds later.
- **A backdated write sends nothing at all, including to its channels.** Fan-out lives in the sweep and the sweep only ever fans out rows it has just marked (§7.3), so a row that arrives already marked is invisible to that path: no push, no ntfy, no Telegram. That is the intent rather than a side effect — a past `due_at` is a bookkeeping entry, the one shape of write whose whole purpose is to appear in a list rather than to interrupt anybody, and a handler that opened a socket to Telegram on the way to a 201 would put an unbounded third-party timeout on the request path for the one case that by definition is not urgent. `extra_channels` is still stored, so the row carries them and a later re-time into the future fans out normally. §9.6's picker sentence and the §7.6 table both say this out loud, because "with no notification" would otherwise read as though only the OS notification were suppressed.
- **Re-timing with an `offset` preset resolves from `now`, never from the old `due_at`.** "30 min" on an item three days overdue means thirty minutes from now. `clock` and `weekday` presets are absolute anyway, so this only matters for offsets.
- **Clear** = set `done_at = now`. Keep the row.
- **Undone** = set `done_at = NULL`, and in the same statement **`pushed_at = now` when `notified_at IS NOT NULL AND pushed_at IS NULL`**. One UPDATE, and that guard is the whole of what it does: it makes "a row that had already fired comes back without re-notifying" literally true of every such row rather than only of the ones a push had got to. For a row that was already pushed about, both columns were set and stay set — it returns to the overdue list silently, and you decide when, or whether, to re-time it. For a row marked *during* a cooldown, `pushed_at` was still NULL and the row was in the held set (§7.3): clearing it dropped it out of the digest via `done_at IS NULL`, and un-clearing it hours later would put it straight back in, because phase 2 selects on `notified_at IS NOT NULL AND pushed_at IS NULL AND done_at IS NULL` and never re-checks a row's age. That is the too-late gate walked around by a `Clear` and an `Undo`, on a row that had already been handled, so undone stamps the column the cooldown had not reached yet. A new ping then costs an explicit re-time, which is the one operation that nulls both columns again. Undone on a row that isn't done is a successful no-op. **A row cleared *before* it ever fired is the other half**, and the guard is what leaves it alone: a done row is invisible to phase 1 (§7.3), so `notified_at` is still NULL, nothing is stamped, and un-clearing it after its due time hands the sweep a live candidate that will notify if it is inside the too-late gate. That is the intent — you un-cleared it because you wanted it back — and both halves are in the §7.6 table.
- **`delivery_error` never holds remote text.** It is built from a closed set of classifications, one entry per channel that failed, joined with `; `: `<name>: timeout`, `<name>: refused`, `<name>: dns`, `<name>: http <code>`, `<name>: send failed` for anything else. At most three entries, then `; +N more`, which caps the column at a couple of hundred bytes. **A send that phase 3's expiring deadline aborts mid-flight lands in that set as `<name>: timeout`** (§7.3): it was attempted and it did not complete, which is what the word means here, so the fan-out budget adds no sixth shape — while a channel the budget never reached at all records nothing, because this column only ever describes attempts. The reason is the next bullet read three times over: shoutrrr's own error strings routinely embed the URL it was given, and this column is returned raw in the reminder object (§8.2), rendered into a row (§9.2), and logged (§10.4) — so a passed-through error would defeat the masking rule in every one of those places, and there would be no single spot to redact. The classification is what you act on anyway: `http 401` is a bad token, `timeout` is a host that is down, and the URL is one `nag channel list` away.
- **The classification is read from the error's type, and never from its text.** `errors.As` down the returned chain — `net.Error` for `timeout`, `*net.OpError` / `syscall.ECONNREFUSED` for `refused`, `*net.DNSError` for `dns`, a status carried as a value on a typed error for `http <code>` — is the whole of how a shape is chosen, and **anything that cannot be typed is `send failed`**. That is a floor rather than a fallback: shoutrrr's services commonly flatten their failures into strings on the way out (`fmt.Errorf("...: %v", err)`), so the typed chain frequently does not survive the library at all, and `send failed` is the honest answer whenever it hasn't. `http <code>` in particular appears **only** when the library surfaces a status as a value; a code that exists only inside a sentence is not one. **Never parse remote text or an error string to recover a class** — a substring match would put the library's own message, URL and all, back onto the code path this column exists to keep it off, and a classification inferred that way is the same masking hazard read at one remove: it is `send failed`, and the log line and `nag channel list` are where the rest of the story is.
- `delivery_error` is cleared on any re-time, on any subsequent successful fan-out, and **whenever `extra_channels` changes at all** — not only when it is emptied. Any `PATCH` whose resulting list differs from the stored one clears the column in the same statement, whether it removes the last channel, swaps one for another, or adds a fourth. **`extra_channels` is stored canonicalised — de-duplicated and sorted by name (§8.3) — so "differs" is a plain ordered comparison of the two arrays, byte for byte, and needs no set logic.** Without the canonical form the comparison has no honest answer: the editor renders its chips in `/api/channels`'s `ORDER BY name` (§8.2, §9.2) while an as-received array would hold whatever order the names were added in, so re-saving a row without touching its channels at all would compare unequal and clear the column on a no-op. (The comparison matters because the row editor *always* sends `extra_channels`, orphans included, §9.2 — so "the key was present" is not the same question as "the list changed", and only the second one clears.) The reason is one argument that covers every one of those: the string names channels (every shape in the bullet above is `<name>: …`) and the marker that carries it is drawn from the row's channel list (§9.2), so as soon as the list no longer contains the named channel the error is both meaningless and unreachable — an amber marker explaining a failure against a name that is no longer on the row, or, with the list emptied, no marker at all to explain it from. Clearing on *any* change rather than only on an emptying is also the rule with the least to remember: "the channels changed, so the last attempt's verdict is stale" needs no reasoning about which channel failed or whether that particular one survived the edit, and the next tick that fans out writes a current verdict anyway. It only ever describes the most recent attempt — a reminder with three channels where one fails carries that one entry, and a later attempt where a different one fails replaces the string wholesale rather than accumulating. **One consequence is worth saying out loud: a `delivery_error` naming a channel that `nag channel rm` has since removed persists until the row is re-timed or its channel list edited.** Nothing is ever attempted for an orphan (§7.2), so there is no later attempt to clear the column — which is consistent with the sentence above rather than a gap in it: the string describes the most recent attempt, and that attempt did fail. The orphan's own chip is still on the row (§9.2), so taking it off clears the column in the same `PATCH` by the rule at the top of this bullet.
- **`channels.url` never appears in an API response in any form** — not masked, and not as a boolean saying one exists. `GET /api/channels` returns `{name, enabled}` and nothing more (§8.2); the only reader of the URL is `nag channel list`, which masks it (§7.4).
- `extra_channels` holds names, not ids, and has no foreign key. Renaming a channel therefore orphans the reminders pointing at it — `nag channel rm` warns when reminders still reference the name, and there is deliberately no rename command. Delete and re-add under the old name if you need to repoint one.

### 4.2 Migrations

`PRAGMA user_version` plus an ordered `[]string` of DDL statements in `internal/store`. No migration library. Each migration runs in a transaction; bump `user_version` in the same transaction.

If `user_version` is **higher** than the number of known migrations, **refuse to start** and name both numbers. That is a rollback onto a newer database, and the alternative is an old binary quietly writing against a schema it doesn't understand.

### 4.3 Connection

```go
dsn := "file:" + path +
    "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
db.SetMaxOpenConns(1)
db.SetMaxIdleConns(1)
db.SetConnMaxLifetime(0)
```

`MaxOpenConns(1)` is deliberate — it eliminates `SQLITE_BUSY` **within the process** and costs nothing at this scale. It says nothing about a second process on the same file, which is a real case here: `nag channel …` runs against the same `NAG_DB` while the serving container holds the write lock, and `busy_timeout(5000)` is what covers that one (§7.4). The other two matter *because* of it: without them the single connection is eligible to be closed and reopened between statements, which throws away the WAL read state and the per-connection pragmas for no benefit.

`synchronous(NORMAL)` is pinned rather than left to the driver: it is the standard pairing with WAL — a commit is durable against a process crash, and only a host power loss can cost the last few transactions — and the alternative is a full `fsync` per commit on the write path of a sweep that runs every 30 seconds. Pinned, because "the driver's default" is not a thing this file should have to look up in a year.

No `foreign_keys` pragma, because the schema deliberately has none — `extra_channels` holds names on purpose (§4.1).

If the file cannot be opened or the migrations cannot run, **refuse to start** and name the path. A reminder app that starts without its database is worse than one that doesn't start.

### 4.4 Retention

The sweep also purges: `DELETE FROM reminders WHERE done_at IS NOT NULL AND done_at < now - retention_days*86400`. Default 30 days. **`retention_days = 0` means keep cleared reminders forever** — skip the DELETE entirely.

---

## 5. Configuration

Two separate concerns, deliberately:

- **Secrets → environment** (VAPID keys, auth token). Never in the TOML.
- **Behaviour → TOML** in a mounted directory, hand-edited.

### 5.1 Environment

| Var | Required | Notes |
|---|---|---|
| `NAG_DB` | no | default `/data/nag.db` |
| `NAG_CONFIG` | no | default `/config/nag.toml` |
| `NAG_ADDR` | no | default `:8080` |
| `NAG_TOKEN` | **yes** | bearer token; exchanged for a signed cookie at `/login` (§8.1). Also the root of the cookie signing key — there is deliberately no second secret. **Minimum 20 characters**, else refuse to start |
| `NAG_VAPID_PUBLIC` | **yes** | must be **unpadded base64url decoding to exactly 65 bytes** — an uncompressed P-256 point, the same shape check §8.2 applies to a client-supplied `application_server_key` |
| `NAG_VAPID_PRIVATE` | **yes** | must be **unpadded base64url decoding to exactly 32 bytes** — a P-256 scalar |
| `NAG_VAPID_SUBJECT` | **yes** | must parse as an absolute URL with scheme `mailto:` or `https:` (RFC 8292), and **must not be the literal `mailto:you@example.com`** that `genkeys` emits |

**Refuse to start** if any required var is missing or fails its constraint. `NAG_TOKEN` carries a length floor because it is not only the bearer credential but the sole input to the cookie key (§8.1) — a six-character hand-typed token is a six-character MAC key, and the failure is silent. `genkeys` emits 43 characters, so the floor only ever catches a hand-edited file.

**The two VAPID keys are shape-checked at boot, not merely required to be non-empty**, and the check is the same one the server already applies to the `application_server_key` a browser posts (§8.2): unpadded base64url, decoding to 65 bytes for the public key and 32 for the private one. Refuse to start and name the variable, the expected decoded length, and what was found. Without it a mangled paste — a truncated line, a stray space, standard base64 instead of base64url, a `\r` from §5.2 — boots happily and then surfaces as a push failure on every sweep, in a log line, on the one path whose failures are hardest to attribute — and only once a reminder comes due. The same string is either well-formed at boot or it is a startup error naming the variable.

`NAG_VAPID_SUBJECT` rejects the placeholder on purpose, and it is the one required value `genkeys` cannot produce for you. Push services use the subject to reach the operator when a subscription misbehaves, so a placeholder that boots is a placeholder that ships and stays — this turns the §5.2 edit from advisory into the last of the four setup steps. The error says which var, that the placeholder is what it found, and to put your own address there.

Error must print the fix:

```
FATAL: no VAPID keypair configured.
  Generate one:  docker compose run --rm -T nag genkeys >> nag.env
  Then restart.
```

**Only `nag serve` enforces that table.** `genkeys` and `version` require nothing — `genkeys` exists precisely to produce those values; `channel` needs only `NAG_DB`; `healthcheck` needs only `NAG_ADDR`; `config check` needs only `NAG_CONFIG`. A subcommand that refused to run without the keys it was invoked to generate would be a closed loop.

**Every subcommand that opens the database creates and migrates it**, exactly as `serve` does (§4.2, §4.3) — there is one `store.Open`, not two. So `nag channel add ntfy '…'` on a box that has never run `serve` works and leaves a migrated database behind, rather than either failing on a missing file or writing an unmigrated one that `serve` then has to repair. The `chown` from §10.1 is what makes that write land as uid 65534 regardless of which command gets there first, which is why it is step two of four and not a footnote.

### 5.2 `nag genkeys` subcommand

Emits a **complete, pipeable env file** on stdout — nothing else, no banner, no prompts, so
`docker compose run --rm -T nag genkeys >> nag.env` is the documented path:

```
NAG_TOKEN=<32 random bytes, base64url>
NAG_VAPID_PUBLIC=<from webpush.GenerateVAPIDKeys()>
NAG_VAPID_PRIVATE=<from webpush.GenerateVAPIDKeys()>
NAG_VAPID_SUBJECT=mailto:you@example.com
```

`NAG_VAPID_SUBJECT` is emitted with that literal placeholder — the file is one edit away from
working rather than three — and that edit is **required**, not suggested: the placeholder itself
is a boot error (§5.1), so `up -d` fails until you replace it with your address. `genkeys` says
exactly that on stderr. Anything advisory goes to **stderr**, so it survives redirection without
corrupting the output.

**The env file has to exist before that command runs.** `compose.yaml` declares `env_file: ./nag.env`, and Compose errors out on a missing env file — including for `run`, so the very command that creates the file can't start. Together with the ownership step from §10.1, the README opens with exactly four commands:

```
mkdir -p data config
sudo chown -R 65534:65534 data config
cp nag.env.example nag.env
docker compose run --rm -T nag genkeys >> nag.env
```

**`-T` is not optional on that fourth line.** Older Compose versions allocate a TTY for `run` even when stdout is redirected, and a TTY translates every `\n` into `\r\n` — so each appended line lands in `nag.env` with a trailing `\r`, which Compose then reads as part of the value. A carriage return inside `NAG_VAPID_PRIVATE` is the worst of those: it is what §5.1's shape check exists to catch, and without that check it fails much later and much less clearly — as a signing error naming a JWT, about a key that looks correct in every editor. `-T` disables the TTY, the output is bytes, and the file is exactly what `genkeys` printed.

`genkeys` touches neither directory, so the order of the two pairs doesn't matter — but `up -d` needs all four, followed by the one hand edit: replace `mailto:you@example.com` in `nag.env`. Four commands and one edit, and the app names the edit if you skip it.

**Every `docker compose` command in this file needs the `caddy` network to already exist** — the `genkeys` line above, `config check` (§5.5), `channel add` (§7.4), and `up -d` alike. `compose.yaml` declares that network as `external: true` (§10), and `docker compose run` fails hard on a missing external network rather than creating one, so on a box that is not already running caddy-docker-proxy the fourth command fails before it can write a byte. Bring the proxy up first, or `docker network create caddy` if you are setting this up ahead of it. It is the one prerequisite that is not itself one of the four, which is exactly why it is easy to leave out of a README.

**`nag.env.example` is comments only — every line starts with `#`, and it declares no variables at all.** It exists to be copied and then appended to, and a template carrying `NAG_TOKEN=` with an empty value would leave the finished file holding *two* of every key: the empty one from the template and the real one from `genkeys`. Compose resolves that by taking the last occurrence, so it happens to work — and the one hand edit §5.1 makes unmissable then has two candidate lines, of which editing the wrong one silently does nothing. A commented template cannot produce that. It documents each variable, its default, and which of them are required — `NAG_DB` and `NAG_ADDR` are described in there too, and telling those apart from the four that are mandatory is what that line is for; `genkeys` appends the four real lines below it.

(The alternative — `env_file: [{path: ./nag.env, required: false}]` — trades one documented copy for a silently keyless boot, so prefer the `cp`.)

### 5.3 TOML

Embed `internal/config/nag.default.toml` with `//go:embed` (§3 explains why it lives in the package). On boot, if `NAG_CONFIG` does not exist, **write the default file to that path** and log it. Gives working defaults plus a self-documenting file to edit. If the directory is missing or unwritable, **refuse to start** and name the path — never fall back to running configless, because the resulting behaviour would be invisible.

**A config that exists but cannot be read or does not validate is also a refusal to start**, naming the path and the located error from §5.5. Only the *reload* path keeps the old config on a bad file, and it can do that because there is an old config to keep; at boot there is nothing behind it, and the two remaining options are to run on the shipped defaults or to exit. Silently serving someone else's presets because their file had a typo in it is the same invisible-behaviour failure as running configless, so: exit `1`, and the default file is written **only** when the path is genuinely absent — never over a file that is present and broken.

```toml
[general]
timezone       = "Europe/Paris"
default_preset = "tomorrow"     # bare Enter in the capture bar
retention_days = 30             # 0 = keep cleared reminders forever

[picker]
hour_min     = 8
hour_max     = 18
minute_step  = 15               # 60 must be divisible by this
default_time = "09:00"
week_start   = "monday"         # monday | sunday

[[preset]]
key    = "30min"
label  = "30 min"
kind   = "offset"
offset = "30m"
quick  = true                   # also offered as a row re-snooze chip

[[preset]]
key    = "3h"
label  = "3 hours"
kind   = "offset"
offset = "3h"
quick  = true

[[preset]]
key   = "tomorrow"
label = "Tomorrow 9:00"
kind  = "clock"
days  = 1
at    = "09:00"
quick = true

[[preset]]
key         = "next-monday"
label       = "Next Monday"
kind        = "weekday"
weekday     = "monday"
at          = "09:00"
same_day_ok = false             # on a Monday this means the FOLLOWING Monday
```

There is no theme setting — see §9.1.

### 5.4 Rules

- `key` is identity; `label` is display only. Renaming a label must not break stored history.
- **File order is UI order.** No `order` field.
- The config **replaces** the preset list wholesale. Never merge arrays with defaults.
- `kind` is a closed set: `offset` | `clock` | `weekday`. Reject anything else. Do not add free-form parsing.
- **Each kind has an exact field set, and a field belonging to another kind is an error, not noise.** A `weekday` preset carrying `offset =` is a config the author misunderstood; accepting and ignoring it means the chip silently does something other than what the file says. **A key that belongs to no kind at all — a plain typo — is the same error one step further out, and §5.5 rejects it on the same grounds.**

  | `kind` | required | optional | rejected |
  |---|---|---|---|
  | `offset` | `offset` (a positive `time.ParseDuration` string) | — | `at`, `days`, `weekday`, `same_day_ok` |
  | `clock` | `at` | `days` (integer `>= 0`, default `0`) | `offset`, `weekday`, `same_day_ok` |
  | `weekday` | `weekday`, `at` | `same_day_ok` (default `false`) | `offset`, `days` |

  `key`, `label`, and `quick` (default `false`) apply to every kind; `key` and `label` must both be non-empty.
- The "Pick a time" entry is **not** a preset — it's rendered by the client after the configured presets.
- `[picker]` constrains **only the picker sheet**. A preset with `at = "07:00"` under `hour_min = 8` is valid and must not be rejected; the hour range is a UX constraint on manual selection, not a rule about when reminders may fire.

### 5.5 Validation (at boot, and on reload)

Fail loudly on: duplicate `key`; empty `key` or `label`; `default_preset` not matching a key; empty preset list; a field outside its kind's set per the table in §5.4; a missing required field for the kind; unparseable `offset`; **`offset` zero or negative** (`-30m` parses cleanly and would schedule into the past on every tap); `days < 0`; `at` not `HH:MM` in 24-hour form; unknown `weekday`; unknown `timezone`; `week_start` not `monday` or `sunday`; `hour_min` or `hour_max` outside `0..23`; `hour_min > hour_max`; `minute_step` not in `1..60` or `60 % minute_step` non-zero; `retention_days < 0`; `default_time` not `HH:MM`, outside `[hour_min, hour_max]`, or not on a `minute_step` boundary; **an unknown key or an unknown section anywhere in the file**, named in the error.

That last one takes **decoder strict mode** — go-toml/v2's `Decoder.DisallowUnknownFields`, or whatever the pinned version calls the equivalent — because the default is to ignore what it does not recognise, so `quik = true` or a `[pickr]` section parses cleanly and then does nothing at all. That is exactly the silent-misconfiguration failure §5.4's cross-kind-field rule was written against, read one level out: a file stating something the app never reads is a file whose author is wrong about what the app does, and the chip they were configuring behaves as though the line weren't there. §8.3 rejects unknown JSON fields for the same reason, and this is the same rule applied to the one surface a human hand-edits under SIGHUP.

Every message names the offending preset `key` (or the `[section].field`) and the value it rejected. This file is hand-edited by one person under SIGHUP, so "invalid config" without a location is a message that costs more than it saves.

On **SIGHUP**, re-read and re-validate. If invalid, **log the error and keep the old config** — never take a running instance down over a bad edit. `docker compose kill -s HUP nag` is the reload path.

A file that has gone **missing or unreadable** is that same case: log and keep. The default is written on boot only (§5.3); a reload that recreated it would answer a mistyped `mv` by silently replacing your presets with the shipped ones, which is worse than continuing on the config already in memory.

**A reload's outcome exists only in the log** — the UI never learns about it, and `config_version` deliberately doesn't move on failure, so a rejected edit is indistinguishable from a signal that never arrived. That is what `nag config check` is for: it loads and validates `NAG_CONFIG` through the same code path as boot, prints either the resolved preset list in file order or the same located error message, and exits `0` or `1`. `docker compose run --rm nag config check` before the HUP makes a bad edit cost nothing; it needs `NAG_CONFIG` and nothing else (§5.1), and it never writes the default file — an absent config is an error here, not something to fix silently.

Changing `general.timezone` does **not** rewrite existing `due_at` values — they are absolute UTC instants and stay exactly where they are. Only future preset evaluation moves.

A client that loaded before a reload holds a stale `/api/config`. `/api/config` returns a `config_version`; `/api/state` returns the same value, and the client reloads the page when it changes.

**A successful load always replaces the config in memory. The counter moves only when the part the client can see actually differs.** Those are two questions and they need **two hashes**, computed over the same resolved config:

- The **full hash** covers every resolved value: `[general]`, `[picker]`, and every field of every preset in file order — `kind`, `offset`, `at`, `days`, `weekday`, `same_day_ok` included. It decides only what gets logged: identical means one `config unchanged` line, different means one line naming the reload. It never gates the swap; the newly loaded config is installed either way, so "reloaded and nothing happened" is not a state this can produce.
- The **client hash** covers exactly what `/api/config` returns — `default_preset`, `vapid_public`, `picker`, and the presets as `{key,label,quick}` in file order. `config_version` increments when and only when *this* hash changes, because it exists to answer "must every open page reload" and nothing else.

Getting this wrong is not cosmetic, and one hash cannot do both jobs. `/api/config` deliberately does not expose `kind`, `offset`, `at`, `days`, `weekday`, `same_day_ok`, `general.timezone`, or `retention_days` (§6, §8.2) — so a single hash over the client-visible projection is **blind to every edit that changes what a chip actually does**. Changing `offset = "30m"` to `"45m"`, or the timezone the whole schedule resolves in, leaves `{key,label,quick}` byte-identical: a counter gated on that hash would not move, which is correct, but a *swap* gated on it would discard the edit and log `config unchanged` at you while doing it. And in the other direction, bumping `config_version` on every successful load means reverting an edit, or simply firing HUP twice, reloads every open client to hand it the config it already had — and `config check` followed by `kill -s HUP` is precisely the workflow that signals twice.

Combined with the failure rule above, `config_version` means **"the config the client can see has changed this many times"** — not how many signals arrived, not how many parsed, and not how many changed something.

**The config is published to its readers with one `atomic.Pointer[Config]`.** The HTTP handlers (`/api/config`, preset resolution on create and `PATCH`) and the sweep goroutine (`retention_days`) both read it while the signal handler replaces it, so a bare struct field is a data race that `go test -race` will find and a plain map read will not. Load the pointer **once** per request and once per tick and use that snapshot throughout — the same discipline as reading `now` once (§7.3, §8.2), and for the same reason: a request that resolves a preset from one config and reports `config_version` from the next is a client told to reload by the response that already applied the change. A HUP that fails validation never stores a pointer, which is what makes "keep the old config" a single skipped assignment rather than a rollback.

The counter lives in memory and restarts at 1, so it can move **backwards** across a restart. The client compares with `!=`, never `>` — any difference means reload.

---

## 6. Preset evaluation

All arithmetic happens in `general.timezone`, then converts to UTC. Build times with `time.Date(y, m, d, hh, mm, 0, 0, loc)` so DST transitions are handled by the stdlib. Presets always resolve in the server's timezone, including when you're travelling.

**The server schedules; the browser displays.** Every stored instant is UTC seconds and the backend treats it as nothing else. Presets resolve in `general.timezone`. The client formats every instant in the **browser's** timezone and locale, and the picker interprets the wall-clock time you choose as browser-local (§9.6). In the normal case — laptop and server in the same zone — these agree. Pick 09:00 from a phone two zones away and you get 09:00 where you are, while the `tomorrow` chip gives you 09:00 where the server is. **That divergence is accepted**: the alternative is rendering foreign wall-clock times to someone looking at their own watch, and it is a worse trade for a single-user app. Clock *skew* is still corrected against the server's clock (§9.3) — a phone with a wrong clock is a bug worth papering over, a phone in another country is not.

**A preset's `label` is a hand-written config string and is not adjusted for any of this.** From a phone two zones east of the server, the chip reads "Tomorrow 9:00" and the row it creates reads "tomorrow at 10:00". Accepted, for the same reason and one more: the label describes the server-side rule, the row describes your watch, and computing labels client-side would mean shipping `kind`, `at`, `days`, and the server's timezone to a client that otherwise needs none of them (§8.2) — a second evaluator, in another language, to restate something you only see when travelling.

- **`offset`** — `due = now + offset`. No rounding. This is the one kind that is pure duration arithmetic on an absolute instant, so it never touches a calendar and DST cannot affect it: "3 hours" is three hours of wall time even across a transition.
- **`clock`** — take today's `y/m/d` in `loc`, advance it by `days` **calendar days** (`AddDate(0, 0, days)` on the date parts, never `+ days*24h` on the instant), then build the result with `time.Date(y, m, d, at.hh, at.mm, 0, 0, loc)`. If the result is `<= now`, advance the date by one more calendar day and rebuild. Calendar-day arithmetic is the whole point on a DST boundary: "tomorrow 09:00" must be 09:00, not 08:00 or 10:00.
- **`weekday`** — find the next date whose weekday matches, at `at`, built the same way.
  - If today matches and `same_day_ok = true` and `at` is still in the future → today.
  - Otherwise → the next matching weekday strictly after today.
  - Note: with `same_day_ok = false`, "next Monday" on a Monday means +7 days. This is intentional and matches Slack.

A `clock` preset whose `at` falls inside a spring-forward gap (02:30 in a zone that jumps 02:00→03:00) has no such wall-clock time that day. `time.Date` normalises it forward to 03:30 rather than failing; accept that and assert it in the test. Do not add gap detection.

Unit tests are required for: DST spring-forward and fall-back on a `clock` preset, including an `at` inside the gap; a `weekday` preset evaluated *on* that weekday, both `same_day_ok` values; `offset` crossing midnight; `clock` evaluated at 23:50; `clock` with `days = 0` both before and after `at`.

---

## 7. Notifications

### 7.1 Web Push (always on)

The VAPID keypair is the app's permanent identity to push services. Generated once, stored in the env file, **never rotated casually** — every stored subscription is bound to the public key it was created under.

- Store `vapid_public` on each subscription row, and store **the key the client says it subscribed under** — `POST /api/push/subscribe` carries an `application_server_key` field and the server records that, never its own configured value (§8.2). Stamping the configured key would make the column tautological: a browser re-posting a subscription still bound to a retired keypair (which §9.8 tells it to do on every load) would be filed as current, the boot warning below would see zero mismatches, and the 403 handler below would take its "matches, so it's a bug" branch forever. The column is only worth having if it records what the *subscription* is bound to.
- **At boot, if any subscription's `vapid_public` differs from the configured one, log a prominent WARNING** naming the count. This is the difference between finding out at startup and finding out after three weeks of silent failures.
- Rotation is survivable, not casual: the server drops the dead rows on the first 403 (below) and each browser re-subscribes under the new key the next time the page is opened (§9.8). Every device has to open the app once.
- Send with `webpush.SendNotificationWithContext(ctx, payload, sub, &webpush.Options{ TTL: 259200, Subscriber: subject, VAPIDPublicKey: ..., VAPIDPrivateKey: ..., Urgency: webpush.UrgencyHigh, Topic: "nag" })` — the context-carrying form, never the bare `SendNotification`: the per-send deadline arrives that way (below), and so does the cancellation that bounds shutdown (§10.1).
- **`Topic` is the constant string `"nag"`, not the reminder id.** A push service replaces any still-undelivered message carrying the same topic for the same subscription, so a constant topic means **at most one push can ever be pending for a device**. That is the entire fix for the offline device: a laptop shut for two days does not receive a queue of notifications on waking, it receives the most recent one. A per-reminder topic collapsed nothing, because the ids differ by construction — it looked like deduplication and was decoration.
- **The generous TTL (~3 days) is safe *because* of that.** The two are a pair and must be read together: if the browser is closed the push service queues and delivers on next launch, and the constant topic bounds that queue at one. Shortening the TTL was the other way to bound the burst, and it costs the weekend case — close the laptop Friday, open it Monday, and a one-day TTL means the nudge is simply gone. With the topic doing the bounding, three days buys that case for nothing.
- Payload, **one message per push and it may cover several reminders** (§7.3):

  ```json
  {"n": 1,  "id": 123, "due_at": 1786742400, "texts": ["Renew the domain"]}
  {"n": 14,                                  "texts": ["Renew the domain", "Call the plumber", "Move the standup"]}
  ```

  `n` is how many reminders this push covers. `due_at` is present **only when `n == 1`** — with several there is no single moment to time — and `id` **only when `n == 1` and there is a real row to point at**, which is every scheduled push but not the test one (§7.5). So `id` implies `n == 1`, and `n == 1` does not imply `id`: a consumer must check for the field rather than infer it from the count, and §9.5 spells out what a notification carrying no `id` does when clicked. `texts` carries at most **three** entries, in due order, each **link-flattened** (§9.10) so the notification shows `Renew the domain`, not `Renew [the domain](https://…)`; when `n == 1` that one entry is the full text, and when `n > 1` each is truncated to 120 bytes on a rune boundary with a trailing `…`. The worker draws whichever form it gets and derives "+11 more" from `n - texts.length` (§9.5), so there is no fourth field. That caps the payload around 1 KB in the worst case — the `n == 1` form carries the full text, so a 1000-byte reminder (§8.3) is most of it — which is an order of magnitude inside the ceiling, and that is what matters: the encrypted payload limit is around 4 KB and `text` alone is allowed 1000 bytes (§8.3), so a naive "one entry per reminder" digest would silently exceed it on a backlog of five.

  Nothing else goes in. No channel list, no history, no overdue total — the count in the notification is the count of reminders *this push is about*, which is locally true and cannot drift; the badge and the list own the overall number (§9.3). The service worker must be able to draw the notification with **no network call at all**: it is woken with no page, on a device that may have just come back from being offline, under a hard deadline before the browser gives up and shows its own generic notification. (A same-origin `fetch` from a worker *does* carry the session cookie — its default credentials mode is `same-origin` — so the reason is reachability and latency, not auth. §9.5 leans on that fact for `pushsubscriptionchange`.)
- Every send takes a context from its caller, and that context carries the deadline: **10 s** per send from the sweep (§7.3), and **the smaller of 3 s and what is left of the 12-second budget** from `POST /api/push/test` (§7.5). A deadline is the only part of a send that differs between the two callers; every rule below is shared, which is what makes them one helper rather than two ladders. It is a deadline rather than a client timeout because only the first of those two numbers is expressible as one, and because a client timeout cannot be cancelled from the outside (§10.1).
- On `404` or `410`, **delete the subscription row**.
- On `401` or `403`, check the row's `vapid_public`. If it **differs** from the configured key, **delete the row** and log a WARNING naming the endpoint in truncated form (below) — a subscription bound to a retired keypair can never succeed again, so keeping it buys a guaranteed failure on every sweep, forever. If it **matches**, log an ERROR and keep it: that's a bug or a push-service problem, not a stale key.
- On any other error, log and keep.
- **An endpoint is never written to the log in full**, in any of those lines. Use the same truncation `/api/push/test` returns — scheme, host, and the last 8 characters of the path (§7.5) — for the same reason: the full URL is the capability to push to that device, and it is enough to tell two browsers apart without being enough to use. §10.4 carries this as a general rule.
- **Send failures are classified, never stringified into the database.** The status code or transport failure becomes the `delivery_error` shape only for shoutrrr (§4.1); a push failure touches no reminder column at all and lives only in the log line.
- **On success, nothing is recorded.** There is deliberately no `last_ok_at` and no `user_agent` on the row: `FROM scratch` has no `sqlite3` (§7.4) and no endpoint or subcommand exposes a subscription, so both columns would be written every 30 seconds and read by nobody. A column that cannot be read is not observability — the log line is (§10.4).
- **Never send a silent push.** The service worker must always call `showNotification`, or browsers will revoke the subscription.
- **If there are zero subscription rows**, the sweep still marks the rows and the notification goes nowhere. Log a WARNING naming how many reminders the push would have covered and the id of the first — otherwise the first reminders after a fresh deploy vanish with no trace.

**Platform reality**, worth knowing before Goal 2 is tested:

- Desktop Firefox and Chrome: works from a normal tab, no install needed.
- Android Chrome/Firefox: same.
- **iPhone: Web Push only works if the site is added to the Home Screen** (iOS 16.4+). Every iOS browser is WebKit, so this is not a Chrome-vs-Safari question. This is the reason `manifest.webmanifest` is required rather than optional (§9.3).

### 7.2 shoutrrr fan-out (opt-in per reminder)

**Fan-out is not subject to the push cooldown and is never coalesced.** It happens in the same tick that marks the row (phase 3 of §7.3) and it sends one message per reminder. The 30-minute rule exists to keep an OS notification tray from filling up with a nudge you already understood; a channel is an *addressed* output you asked for on that specific reminder, often a different address per reminder, and there is nothing coherent to collapse two of them into. It is also the one output whose whole value is "tell me on Telegram *now*", so deferring it by up to half an hour would remove the reason to have configured it. Phase 1's `LIMIT 50` bounds how many rows reach it per tick, and phase 3's own deadline bounds how long it may spend on them (§7.3).

If `extra_channels` is non-empty, resolve names against `channels` (skip disabled), build a sender, and send. **A name that resolves to no row is skipped with one INFO line** naming the reminder id and the name. Orphans are defined behaviour (§4.1), not an error, so no `delivery_error` is recorded and §10.4's promise to log "every delivery failure" would never cover them — and a fan-out you deliberately asked for going nowhere in silence is the one outcome this app should never produce. **A *disabled* channel is skipped silently, and the asymmetry with the orphan is intentional**: you muted it yourself and the mute is one `nag channel list` away, whereas an orphan is a name pointing at nothing — the same silence, minus any record that you chose it. The message body is the **link-flattened** text (§9.10), same as the push payload — a channel is an output, not a second renderer. Failures are **classified** into the fixed `delivery_error` shape (§4.1) and logged in that same form, and must **never** block or retry — the overdue list is the real safety net. **The column is written, or cleared on success, per reminder as soon as that row's attempts finish** — not batched to the end of the phase and not carried into the next tick, so a phase 3 that hits its deadline (§7.3) has already recorded every row it did attempt. Same 10 s timeout as push, and every send is bounded by phase 3's context, so the effective per-channel timeout is the smaller of 10 s and what is left of the phase's 20-second budget (§7.3).

**That bound is imposed by a wrapper, because shoutrrr's API cannot carry it.** `router.Send(message, params) []error` takes no `context.Context`, and several of its services build their own `http.Client` with no configurable timeout — so neither the 10 s per-channel timeout nor phase 3's whole-phase deadline is reachable through the library directly. Each channel send therefore runs `router.Send` on **its own goroutine**, writing the result into a buffered channel of one, while the caller `select`s on that channel and on a context derived from the phase's with a 10-second timeout — which is exactly how "the smaller of 10 s and the remaining budget" is obtained. Whichever arrives first is the verdict: a result is classified as above, and a context that expires first means the send is **abandoned** — recorded as `<name>: timeout`, the classification §7.3 already blesses for a send its budget cut short (§4.1) — with its goroutine left to finish in the background and be discarded, result and all. **A send the shutdown cancellation ends the same way (§10.1) lands on the same word**, and deliberately needs no new one: the `select` cannot tell a deadline from a cancel and does not have to, because `<name>: timeout` already means "attempted, did not complete" — which is exactly what happened, and the only shape a column that describes attempts (§4.1) can honestly carry for one the process stopped waiting on. This is what makes phase 3's "really ends at ~20 s" bound (§7.3) true regardless of what the library offers, and an abandoned goroutine holding a dead socket for a few seconds costs nothing at this scale: it has no lock, nothing left to write to, and it ends on its own client's timeout. §2's instruction to verify the module path on pkg.go.dev before pinning **extends to the API**: if the fork has grown a context-aware send or a settable `http.Client`, use it and the wrapper reduces to a plain call — but the bound must never depend on that.

Do not use ntfy's own delayed delivery. This app owns the schedule; ntfy is only an output.

### 7.3 The sweep

One goroutine, 30-second ticker, runs once immediately at boot before the first tick. `now` is read **once per tick** and used for every statement in it.

**The governing rule is one sentence: at most one Web Push notification in any 30-minute window, and it carries every reminder that has come due since the last one.** Nothing else is rate-limited — the badge, the two lists, and the shoutrrr fan-out are all prompt. A tick therefore has four phases, **in this order**, and the order is load-bearing rather than narrative: every column that decides *whether* a notification happens — the lifecycle marks `notified_at` and `pushed_at` — is committed before any socket is opened, and the push goes out before the one phase whose duration a third party controls.

**Phase 1 — mark.** Writes only; no network.

```
tx: SELECT id, text, due_at, extra_channels FROM reminders
    WHERE done_at IS NULL AND notified_at IS NULL AND due_at <= now
    ORDER BY due_at, id LIMIT 50
tx: UPDATE reminders SET notified_at = now WHERE id IN (<all selected>)
tx: UPDATE reminders SET pushed_at   = now WHERE id IN (<selected with due_at < now - 1800>)
```

The selected rows carrying `extra_channels` are held in memory for phase 3, `text` included — the tick fans out exactly what it just marked and never re-queries for it, so the message body that phase sends (§7.2) has to come out of this SELECT. **Rows the too-late gate stamped are excluded from that hand-off**, i.e. only rows with `due_at >= now - 1800` are eligible: the gate means no output at all, not merely no push (below, and the §7.6 rows that say "no push and no fan-out").

**Phase 2 — the digest push.** Runs only when `now - last_push_at >= 1800`:

```
tx: SELECT id, text, due_at FROM reminders
    WHERE done_at IS NULL AND notified_at IS NOT NULL AND pushed_at IS NULL
    ORDER BY due_at, id
tx: UPDATE reminders SET pushed_at = now
    WHERE done_at IS NULL AND notified_at IS NOT NULL AND pushed_at IS NULL
then: if the select returned anything — set last_push_at = now, then send ONE push
      (§7.1) to each subscription in turn
```

**Phase 2's UPDATE repeats the SELECT's predicate instead of listing ids.** There is no `LIMIT` here (below), so an id list is unbounded and a large backlog would walk into SQLite's bound-parameter ceiling. The two forms are equivalent *in this transaction specifically*: `MaxOpenConns(1)` (§4.3) means no handler can write between the two statements, so the predicate cannot have gained a row. Phase 1 still needs its explicit id list, because `LIMIT 50` is exactly what makes its selected set narrower than its predicate.

**Phase 3 — fan out** (§7.2), to the eligible rows phase 1 handed over, sequentially, one message per reminder.

**Phase 4 — purge** old `done` rows (§4.4).

**Fan-out runs after the push, and under a 20-second deadline for the whole phase.** Both halves exist to stop a third party's dead socket from delaying this app's own notifications, and the arithmetic is why: `LIMIT 50` rows × up to 16 channels each (§8.3) × a 10-second timeout is over two hours of a single tick, during which no further tick starts (below), so a batch of unreachable channels would delay every subsequent digest by hours. `LIMIT 50` bounds the *count* and never the *duration* — only a clock can do that.

So phase 3 takes a whole-phase deadline of **20 seconds measured from the moment phase 3 starts**, from a fresh clock read taken for that deadline and nothing else, and stops when it expires: rows not yet attempted are **skipped, logged at INFO with their ids and how many were dropped, and never retried**. No `delivery_error` is written for them, because nothing was attempted and the column describes attempts. That is the same trade the whole sweep is built on — the row is already marked and already in "Needs you now", the list is the safety net, and a fan-out queue that survived across ticks would be durable state for the one output that is explicitly best-effort.

**That deadline is a context threaded into every send, not a check between rows, and the difference is the whole of whether the bound holds.** A single row may carry up to 16 channels (§8.3), each sent with its own 10-second timeout, so a budget consulted only at row boundaries lets a row that enters phase 3 at 19.9 s run for another 160 seconds after the budget expired — the phase would end at roughly three minutes, and every sentence here and in §7.6 that puts its cost at "the next tick and no more" would be false. So the 20 s is the deadline on a context that bounds every send in the phase, and **each individual channel send takes the smaller of 10 seconds and whatever is left of the budget** — exactly the discipline §7.5 pins for its own 12-second budget, and for the same reason: a per-call timeout bounds one call, and only a deadline carried into the calls bounds the set of them. shoutrrr's `Send` cannot be handed that context at all, so the send runs on its own goroutine and the phase `select`s the result against the deadline, abandoning rather than cancelling (§7.2) — from the outside that is the same bound, which is the point of doing it that way. Phase 3 therefore really does end at ~20 s, whatever the shape of the rows inside it and whatever the library exposes. Rows the budget never reached are skipped exactly as described above, and so are the channels of a row it reached partway through — nothing was attempted for either, so neither is retried and neither records anything in `delivery_error`, a column that describes attempts (§4.1). **A send the expiring budget cuts short mid-flight is the one thing here that does record a verdict, and it needs no new vocabulary: it is `<name>: timeout`** — it was attempted and it did not complete, which is exactly what that classification already means (§4.1) — written with the rest of that row's verdict as soon as its attempts finish (§7.2). A sixth classification meaning "we stopped waiting" would be a distinction nobody can act on: a channel that could have answered inside the budget would not be in this position.

**That fresh read is the one place the tick's single `now` is deliberately not used, and it is a correctness fix rather than a nicety.** Phase 2 sends sequentially with a 10-second timeout each (below), so three sleeping devices put roughly 30 seconds between the tick's `now` and the first line of phase 3 — and a deadline of `now + 20s` would already be in the past before a single channel was tried, dropping *every* fan-out on exactly the ticks where a device was unreachable. The semantic `now` is untouched: every statement, every stamped column, and the too-late gate still read the once-per-tick value (above). Only the deadline reads the clock again, and it reads it once.

With the deadline in place the worst case is bounded on both outputs: a fan-out message is either attempted within 20 seconds of the phase starting or dropped, and a push is delayed by at most one tick interval plus the fan-out budget — the previous tick's phase 3 can hold the goroutine for 20 seconds, which costs the next tick and no more. That second half is true only because the deadline is threaded into the sends rather than consulted between them (above).

Ordering phase 3 *after* phase 2 is what keeps that bound to a single tick — the digest of the tick that marks a row is never behind that row's own fan-out — and it costs nothing, because fan-out is not coalesced and has no state to share with the push.

**The held set is a query, not state.** Phase 2 selects on `notified_at IS NOT NULL AND pushed_at IS NULL`, so the reminders waiting out a cooldown live in the database (§4.1). A restart mid-window loses nothing, and a row **cleared** during the window drops out of the digest by itself via the `done_at IS NULL` clause — you dealt with it, so it does not need to arrive as a notification thirty minutes later. Undoing that clear does not put it back either: undone stamps `pushed_at` on a row whose `notified_at` is set (§4.1), so a held row leaves the held set for good the moment it is cleared, and phase 2 — which does not re-check ages — cannot pick it up again at an arbitrary age later.

**`last_push_at` is in memory only**, and it is the single piece of cooldown state that is not durable. It **starts at zero**, so the tick that runs at boot always finds the window open. A restart therefore opens the window early: at worst one extra notification, never a missed one, which is the direction this app should fail in. It is deliberately *not* derived from `MAX(pushed_at)`, because phase 1 stamps that column on rows nothing was ever sent about (below) and the maximum would then start a cooldown for a push that never happened.

**The window is consumed whenever phase 2 selected rows — whatever the sends then did, and even with zero subscriptions stored.** `last_push_at` is set before the first socket is opened, for the same reason `pushed_at` is: the rows have already left the held set and can never be re-selected, so a window preserved on failure would have nothing left to carry. Holding it open would only mean phase 2 re-running on the next tick against an empty select. This keeps the governing sentence literally true — at most one push attempt per 30 minutes — and it is why the §7.6 rows that end in **0** still end a cooldown.

**Sends are sequential**, one subscription at a time, each with the 10-second timeout. Three sleeping devices is a 30-second tick and the next one is skipped, which the no-overlap rule below already tolerates; a personal instance has two to four subscriptions and the alternative buys nothing but a `WaitGroup` and a way to serialise the 404/410 row deletions against the single connection.

**The "too late" gate is applied in phase 1, at mark time, and nowhere else.** A row whose `due_at` is more than **1800 seconds** in the past when the sweep first sees it gets `notified_at` *and* `pushed_at` stamped, with one INFO line naming the id: it is never pushed and never fanned out, it just appears in the list. Deferring to the digest cannot then be undone by this rule — a row marked on time and held for the full window is up to 30 minutes late when phase 2 finds it, and phase 2 does not re-check its age. Putting the gate at push time instead would have made the cooldown eat exactly the notifications it was supposed to batch, and it would have done so silently.

Both constants are 30 minutes, and the spec means that as **one** number the user can hold: *nothing more than half an hour late produces a notification, and no more than one notification per half hour.*

**This gate only ever sees rows that were in the future when they were written** and then went unswept — a stopped container, a suspended host, an outage. A deliberately backdated `due_at` never reaches phase 1 at all: the write path already set both columns (§4.1), so the row is filtered out by the `notified_at IS NULL` clause. The two mechanisms are disjoint on purpose, which is what lets the picker state flatly that a past time will not notify (§9.6) without knowing this constant or being re-worded when it changes.

**Mark before sending, always.** Every column that decides whether a notification happens — `notified_at` and `pushed_at`, and nothing else — is committed in phases 1 and 2, before phase 2's push and phase 3's fan-out open a single socket. A dropped notification still appears in the overdue list; a crash-loop notification storm does not self-heal. This is the deliberate trade, and it is the reason the *lifecycle* writes are ordered ahead of the network rather than interleaved per row.

**Two writes are deliberately not covered by that rule, and both are bookkeeping about an attempt rather than a decision to attempt.** A `404`, `410`, or stale-key `403` **deletes its subscription row inline**, as phase 2 handles that endpoint's response, because the response is the only thing that ever knows the row is dead (§7.1). And `delivery_error` is written — or cleared, on success — **per reminder, immediately after that row's send attempts complete** in phase 3 (§7.2): not accumulated and flushed at the end of the phase, and never deferred to the next tick. Both are consequences of the same principle read the other way round: a column that describes what a socket did cannot be committed before the socket is opened, and neither of these columns can resurrect a notification or suppress one. The rule that matters is that nothing the sweep learns from the network can change whether a row has been handled.

**No `LIMIT` in phase 2.** It sends exactly one push whatever it selects, so a cap would strand rows for another 30 minutes and save nothing — and `texts` is capped at three entries regardless (§7.1), so the payload does not grow with the count either.

**`LIMIT 50` in phase 1 is a deliberate rate limit, not a pagination bug**, and with the digest in place it now bounds exactly one thing: **shoutrrr messages per tick**, which are the only output left that scales with row count (§7.2). The sweep does not loop within a tick, so a backlog of 200 marks over four ticks — about two minutes — and lands in one or two digests.

**No overlapping ticks and no unbounded sends.** The sweep is one goroutine on `for range ticker.C`, so a tick that runs long simply means the next one is skipped rather than queued. Every outbound call — the digest push and every shoutrrr send — uses a client with a **10-second timeout**, and phase 3 carries a deadline for the phase as a whole (above); without both, a single hanging endpoint stalls the goroutine with the columns already committed, which is the one failure mode this design cannot retry its way out of. A per-call timeout alone bounds one send and not a tick, which is exactly the gap the phase deadline closes. The digest also caps the worst case structurally: phase 2 makes one call per *subscription*, not per reminder, so a device count of three is three calls however long the backlog is.

**The deferral is tolerable only because the badge is not subject to it.** A reminder held by the cooldown is in "Needs you now" and in `overdue_count` the moment it comes due, so the pinned tab (Goal 4) is correct within 45 seconds — at most one round trip, given the one-shot due timer (§9.3). What the cooldown delays is the OS notification, which is the nudge; what it never delays is the list, which is the truth. That is the same trade the whole app is built on, applied to a rate limit.

Do not use in-process timers per reminder. Polling survives restarts and requires no rescheduling when a due date is edited.

### 7.4 `nag channel …`

The final image is `FROM scratch` — no shell, no `sqlite3`, no way to hand-edit the table. Channel management is therefore a subcommand, run against the same `NAG_DB`:

```
nag channel add <name> <shoutrrr-url>    # rejects a duplicate name and an unparseable URL
nag channel list                         # URLs masked, shows enabled state
nag channel rm <name>                    # prints how many reminders reference it, then proceeds
nag channel enable <name>
nag channel disable <name>               # mute without orphaning extra_channels
nag channel test <name>                  # send "Nag test notification" now, print the result
```

`add` hands the URL to shoutrrr's router **before** writing the row and refuses one it cannot parse, naming the parse error. Otherwise a typo is stored happily and surfaces weeks later as a `delivery_error` on a reminder you cared about.

**`list`'s mask is pinned: scheme, host, and the last path segment — userinfo and query are *always* elided**, and the elision is a literal `…` written unconditionally, so the form is one shape whether it dropped a password, four path segments, or nothing at all. `ntfy://user:tok_abc123@ntfy.example.com/alerts` prints as `ntfy://ntfy.example.com/…/alerts`, and a URL with no path at all prints as `telegram://telegram/…`, which is the honest rendering of a URL that is essentially all credential.

That is deliberately **not** §7.5's "last 8 characters of the path" form, and the difference is where the secret sits in each. A push endpoint's capability *is* its opaque path, so a short tail of it is the one thing that tells two browsers apart while being far too little to send with. A shoutrrr URL keeps its secret in exactly the parts §7.5's rule preserves, and its path is usually the harmless half: the two URLs above put the token in the userinfo and the addressee in the query, and the last 8 characters of the first one's path is the topic name — the only piece you actually wanted to read. So the rule is inverted for this surface on purpose, because the question `list` answers is which of your channels a name points at, never what the credential is.

**`name` is a lowercase slug: `^[a-z0-9][a-z0-9_-]{0,31}$`.** `add` rejects anything else and prints the pattern. It is not cosmetic — the name is a JSON array element in `extra_channels` (§4.1), a chip label (§9.2), and a CLI argument, and `UNIQUE` on TEXT is case-sensitive in SQLite, so `ntfy` and `NTFY` would coexist as two rows rendering two identical-looking chips. Lowercasing at the door is one rule instead of a `COLLATE NOCASE` index plus a matching fold on every `extra_channels` lookup at send time.

`test` is the fan-out counterpart to `POST /api/push/test` (§7.5) and exists for the same reason: a channel is a URL with a secret in it, and you want the "did that work" answer at the moment you paste it, not at the moment a reminder fires. It sends even to a disabled channel — you are asking about the URL, not the mute switch — and says which one it did. It carries **the sweep's 10-second timeout**, through the same wrapper and the same classification (§7.2), so the word it prints on a dead host is the word a `delivery_error` would have held; there is no shorter budget here because unlike `POST /api/push/test` (§7.5) nothing is holding a `WriteTimeout` open behind it.

Usage is `docker compose run --rm nag channel add ntfy 'ntfy://…'`. The URL contains secrets, which is exactly why channels are not in the TOML (§5) and why there is no API to create them — the write path is a shell you already have to be on the box to use.

`rm` **warns and proceeds** — no `--force` flag. Orphaned names in `extra_channels` are already defined behaviour (§4.1): they resolve to nothing and are skipped at send time. A confirmation prompt would also be dead weight in `compose run`, which is not always a TTY.

The subcommand opens the DB directly through the same `store.Open` as `serve`, so it **creates and migrates the file if it does not exist** (§5.1) — adding your first channel before the first `up -d` is a supported order, not a trap. It also means **the serving container may hold the write lock**. `busy_timeout(5000)` covers it; if it still fails, say so plainly and suggest retrying.

### 7.5 `POST /api/push/test`

Sends a fixed "Nag test notification" push to every stored subscription and returns `200` with per-endpoint results, applying **the same response handling as the sweep for all four codes** — 404/410 delete, and 401/403 delete only when the row's `vapid_public` differs from the configured key (§7.1). One helper does the send-and-handle for both callers, so there is no second copy of that ladder to drift; and `deleted: true` on a `403` is then the fastest available answer to "is this browser still bound to the keypair I rotated away from", which is the question you have when push went quiet after a rotation. **It ignores the 30-minute cooldown and does not touch `last_push_at`** — you are asking whether the transport works, and an endpoint that answers "come back in twenty minutes" would be answering a different question than the one you typed. It sends the `n == 1` payload form with a fixed text and carries the same constant `Topic`, so on a device that is offline it is subject to the same one-pending-message replacement as a real push (§7.1). **The payload is pinned, because the worker has to draw it with no special case:**

```json
{"n": 1, "due_at": 1786742400, "texts": ["Nag test notification"]}
```

`due_at` is the server's `now` at the moment of the call, so the notification body reads `now` through the ordinary formatter (§9.11) rather than needing a second title-and-body shape in `sw.js`. There is **no `id`** — no row exists to point at — which is the whole reason §7.1 states `id` separately from `n == 1`, and it makes the click behave like a digest's (§9.5): the app is focused, and no row is scrolled to or flashed.

**Its per-send timeout is 3 seconds, not the sweep's 10, and the handler as a whole is bounded by a 12-second budget.** Sends are sequential here as in the sweep (§7.3), so on the sweep's numbers two unreachable endpoints outlive the server's 15-second `WriteTimeout` (§8.2) and the `curl` gets no response at all — precisely in the "notifications are silent" case this endpoint exists for, which is also the case where an endpoint that never answers is the likeliest thing to find. A diagnostic probe optimises for answering the caller, not for patient delivery: a subscription that cannot answer in 3 seconds comes back as `timeout`, and that *is* the diagnosis. The sweep keeps its 10 seconds unchanged, because there nobody is waiting on the answer and a slow push is still a delivered one.

The budget is a deadline on the handler, taken from one clock read before the first send, and **each send gets the smaller of 3 seconds and whatever is left of it** — so the sends together can never run past 12 and the response always has three seconds of the write deadline to itself. A per-send timeout alone would not have given that: five dead endpoints at 3 seconds each is exactly the 15 the write deadline already is, and a personal instance can easily hold five rows. Subscriptions the budget did not reach are **reported, never silently omitted**: each one gets an entry with `status: null` and `error: "not attempted"` (§8.2), so the array always holds exactly one result per stored subscription.

The response, whose `status` and `error` vocabulary §8.2 pins to a closed set:

```json
{"results":[{"endpoint":"https://updates.push.services.mozilla.com/…f3Ka9tQ2",
             "status":201,"deleted":false,"error":null},
            {"endpoint":"https://fcm.googleapis.com/…kQ7bT1xz",
             "status":403,"deleted":true,"error":"stale vapid key"},
            {"endpoint":"https://web.push.apple.com/…9pQ4mW7v",
             "status":null,"deleted":false,"error":"not attempted"}]}
```

`endpoint` is truncated to scheme, host, and the **last 8 characters** of the path — enough to tell two browsers on the same push service apart, not enough to send one a notification, which the full URL would be. `deleted` says whether this call dropped the row — necessarily `false` on a `not attempted` entry, since nothing was attempted and therefore nothing was learned about it. A subscription list with zero rows returns `{"results":[]}` and a 200, not an error; that is itself the answer to "is anything subscribed", and it is the only way the array comes back short. **Not surfaced in the UI** — it exists so that `curl` can answer "is push actually working from this box to this browser" without waiting for a real reminder. This is the first thing you will want when notifications are silent.

It takes no body, so it is one of the endpoints exempt from the JSON `Content-Type` requirement (§10.2) and the documented invocation stays a bare:

```
curl -X POST -H "Authorization: Bearer $NAG_TOKEN" https://nag.example.com/api/push/test
```

### 7.6 What actually arrives, case by case

The rate limit, the digest, the constant `Topic`, and the too-late gate interact, and the interactions are the whole behaviour. This table is normative — where a sentence elsewhere disagrees with it, this wins. **Each row assumes the cooldown window is open** unless it says otherwise, which is the steady state: nothing has been pushed in the last 30 minutes.

| Situation | Notifications on the device | Why |
|---|---|---|
| One reminder comes due, device online | **1**, at the due time (≤30 s late) | Cooldown was open; phase 2 sends the `n == 1` form, so the title is the reminder text |
| Two reminders due in the same tick | **1**, at the due time | One digest, `n == 2`, both texts in the body |
| A reminder comes due 7 minutes after the last notification | **1**, but at the 30-minute mark — up to 23 minutes late | Held by the cooldown. It is in the list and the badge immediately (§9.3); only the nudge waits |
| Six reminders spread over a 25-minute stretch, all of it inside one cooldown | **1**, when the window opens, `n == 6` | Every one was held, and they ride the same digest. Had the stretch *started* with an open window, the first would have pinged on time and the other five would have arrived as a second digest with `n == 5` |
| Device asleep or offline for 20 minutes | **1**, on wake | Pushes were queued by the push service, and the constant `Topic` replaced each pending message with the next, so exactly one survives |
| Device offline for 2 days, server up throughout | **1**, on reconnect — the most recent digest, possibly hours stale | Same replacement rule. It names the reminders from its own tick, not all of them; the badge you are about to look at names the total |
| Device offline past the 3-day TTL | **0** | The push service dropped it. The list and badge are correct on the next load, which is the fallback the whole design leans on |
| Server down 20 minutes, 14 reminders due in that window | **1**, `n == 14`, within 30 s of restart | All 14 are ≤1800 s late at mark time, so all 14 are pushable and land in one digest |
| Server down 2 days, 40 reminders due | **0** | Every row is >1800 s late at mark time: marked, `pushed_at` stamped, one INFO line each, no push and no fan-out. You come back to a full list |
| Server down 40 minutes, 3 due at the start and 2 near the end | **1**, `n == 2` | The first three are past the gate and are marked silently; the last two are inside it |
| Server down 25 minutes, **60** reminders due and all inside the gate | **1** with `n == 50`, then **1** with `n == 10` half an hour later | Phase 1's `LIMIT 50` bounds a tick, so ten rows are marked on the next tick — by which time this digest has just closed the window. The only case where the two rate limits compound, and it needs a 50-row backlog to reach |
| A held reminder is cleared before the window opens | **0** for that row | Phase 2 filters on `done_at IS NULL`. You already dealt with it |
| The same held row, **undone** an hour later | **0**, until you re-time it | Undone stamps `pushed_at` on a row whose `notified_at` is set (§4.1), so it does not re-enter the digest at an age phase 2 would never have checked. It is back in "Needs you now" silently; the nudge takes an explicit re-time |
| A reminder is cleared while still in "Waiting quietly", then undone 6 minutes after its due time | **1**, within one tick of the undo (≤30 s) | A done row is invisible to phase 1, so this one was never marked and `notified_at` is still NULL. Undone, it is due, unmarked, and inside the gate — so the next tick finds a live candidate and pings. Unlike §4.1's "already fired, comes back without re-notifying", this reminder genuinely never fired |
| The same row, undone **40 minutes** after its due time | **0** | Now past the too-late gate: marked silently with one INFO line, straight into the list |
| A reminder is re-timed while held | **0** now, **1** at the new time | Re-time nulls both columns (§4.1), so it leaves the held set and re-enters phase 1 later |
| A backdated `due_at` (picker or `curl`) | **0**, ever | The write path stamps both columns (§4.1). This is the promise §9.6 makes |
| A backdated `due_at` **with `extra_channels`** | **0**, and no ntfy/IM message either | Fan-out only ever covers rows the sweep just marked, and this row arrived marked (§4.1). Nothing is sent on any output; the channels stay stored, so a later re-time into the future fans out normally |
| Zero subscriptions stored | **0**, plus a WARNING naming the count (§7.1) | Nothing to send to. The rows are still marked, so they are not re-attempted every 30 s — and the window is still consumed, because phase 2 did select rows (§7.3) |
| Every send in a digest fails — push service down, or a dead endpoint | **0**, plus a log line per endpoint | `pushed_at` is committed before the first socket opens, so those rows are gone from the held set; the window is consumed too. The list is the safety net, exactly as with mark-before-send (§7.3) |
| Three devices subscribed | **1 each** | Phase 2 makes one call per subscription with the same payload. `Topic` and `tag` are per-subscription, so collapsing is per-device |
| Reminder with `extra_channels`, held by the cooldown | ntfy/IM message **immediately**; OS notification at the 30-minute mark | Fan-out is not rate-limited, and it runs in the same tick that marks the row (§7.2) |
| A tick whose fan-out targets are unreachable — 30 rows, dead ntfy host | Push notification **on time**; the ntfy messages that fit in 20 s are attempted, the rest are dropped with one INFO line | Phase 3 runs after the push and under a whole-phase deadline that bounds every individual send, by wrapper where the library takes no context (§7.2, §7.3), so the phase ends at ~20 s whatever the row-and-channel shape. A dead channel can cost you channel messages; it can never delay a notification past the next tick |
| `POST /api/push/test` during a cooldown | **1**, immediately | It bypasses the cooldown and does not consume the window (§7.5) |

Two reads of the same fact are worth stating plainly. **The count in a notification is never the badge count.** `n` describes the reminders that push is about; the badge describes everything overdue, including rows you were notified about an hour ago and have not cleared. They differ constantly and neither is wrong. And **a notification is never the record of anything** — dismissing it does nothing (§9.5), losing it to a TTL costs nothing, and every case above that ends in **0** ends in a correct list.

---

## 8. HTTP API

JSON. `/api/*` is the only authenticated surface.

### 8.1 Auth

**Cookie.** `nag_session`, value `b64(exp) + "." + b64(mac)`. No session table, no user id — there is one user. Every piece of that is pinned, because two readings of it are two implementations that cannot verify each other's cookies:

- `exp` is the expiry as a **decimal ASCII string** — `"1786742400"`, not eight big-endian bytes.
- `b64` is **unpadded base64url**, `base64.RawURLEncoding`, matching every other encoding in the app (§9.8).
- `mac = HMAC-SHA256(key: k, message: "v1|" + exp)` over that same decimal string, byte for byte.
- `k = HMAC-SHA256(key: NAG_TOKEN, message: "nag-cookie-v1")` — the token is the key, the constant is the message. No second secret to manage, and **rotating `NAG_TOKEN` invalidates every cookie**, which is the log-out-everywhere lever.
- Attributes: `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`, `Max-Age` 365 days.
- **`Secure` is set unconditionally.** Behind Caddy the app sees plain HTTP, so deriving it from `r.TLS != nil` would silently ship a non-Secure cookie. Never do that.
- `SameSite=Lax`, not `Strict`, so following a notification click into a fresh tab arrives authenticated.
- **Sliding renewal**: a valid cookie older than 30 days is re-issued on the way out, so a pinned tab never logs out. The cookie carries only its expiry, so "older than 30 days" is computed as `expiry - now < maxAge - 30d` — write `maxAge` as the one constant both the issuer and this check read, because hardcoding `335` here breaks silently the day `Max-Age` changes.
- Verify with `hmac.Equal`; reject when `expiry < now`.
- **`/logout` is not revocation.** With no session table it only clears the cookie in that browser; a copied cookie stays valid until it expires. Rotating `NAG_TOKEN` is the only lever that invalidates anything, which is exactly why the cookie key is derived from it.

**Bearer.** `Authorization: Bearer <NAG_TOKEN>` is accepted on all of `/api/*`, permanently — it's the path for `curl`, `/api/push/test`, and the future bookmarklet. Compare with `subtle.ConstantTimeCompare`.

**Login.**

- `GET /` serves `index.html` **unauthenticated**. Otherwise the browser gets a bare 401 with nowhere to type a token. The page calls `/api/state`; on 401 it renders a token field instead of the list.
- `POST /login` `{token}` → `204` + cookie on match, `401` otherwise. On failure, sleep ~250 ms and serialise failures through a mutex. That is the entire rate limit and it is sufficient for one user behind HTTPS.
- **On the `204` the client calls `location.reload()`, and that is the whole of the post-login path.** The pre-login load already ran its init work against a 401 — the first `/api/state`, and §9.8's awaited subscription re-post and notification-slot decision — so the alternative is re-running each of those by hand, in the right order, from the login handler. A reload re-runs them for free under the new cookie, and it costs nothing to do because there is no client-side state to preserve anywhere (§12): one full `/api/state` body before the cheap polls resume (§8.2), which is the same price any reload pays.
- `POST /logout` → clears the cookie.
- Static assets (`/app.js`, `/app.css`, `/sw.js`, `/manifest.webmanifest`, `/icons/*`) are unauthenticated. They carry no data, and `/sw.js` must be fetchable for registration to work.

### 8.2 Endpoints

| Method | Path | Body / notes |
|---|---|---|
| POST | `/login` | `{token}` → 204 + cookie |
| POST | `/logout` | → 204, cookie cleared |
| GET | `/api/config` | `{config_version, vapid_public, default_preset, presets:[{key,label,quick}], picker:{hour_min,hour_max,minute_step,default_time,week_start}}` — client must never hardcode these |
| GET | `/api/state` | `{server_time, config_version, overdue_count, push_subscribed, overdue:[…], later:[…]}` — single source for list, badge, and title. Both lists come **server-sorted** by `due_at` ascending, then `id` ascending; the client never re-sorts. The tiebreak is not decoration — two chips tapped in the same minute produce byte-identical `due_at` values, and `ORDER BY due_at` alone lets SQLite return them in either order, so rows would swap places under your finger between polls. `push_subscribed` is `COUNT(push_subscriptions) > 0` — instance-wide, not per-device (§9.8). **`now` is read once per request** and every part of the response derives from that one value: `server_time`, `overdue_count`, and which list each row lands in. Three separate clock reads can put a row in `later` while the count above it already includes it, and "the list never disagrees with the badge" (§9.3) is an invariant this endpoint either holds or breaks on its own |
| POST | `/api/reminders` | `{text, extra_channels?}` plus exactly one of `preset`/`due_at` → 201 + the reminder |
| POST | `/api/reminders/{id}/done` | → 200 + the reminder. **On a row that is already done this is a successful no-op and `done_at` keeps its original value** — the UPDATE carries `WHERE done_at IS NULL`, the response is the row as it stands, and the status is still 200. Re-stamping would push the retention clock (§4.4) forward on every double-tap, so a row cleared 29 days ago could be kept alive indefinitely by a jittery finger or two tabs polling the same list; and it is the exact mirror of `/undone`, which is also a no-op on a row that isn't done (§4.1) |
| POST | `/api/reminders/{id}/undone` | → 200 + the reminder. Powers the undo toast; a no-op on a row that isn't done (§4.1). It also stamps `pushed_at` on a row that had already been marked but never pushed about (§4.1) — a write the response cannot show, since `pushed_at` is not in the reminder object; `notified_at` is the field that tells you which of the two undone cases you are in |
| PATCH | `/api/reminders/{id}` | `{text?, extra_channels?, preset?, due_at?}` → 200 + the reminder. The only write path for both a correction and a re-snooze (§8.4) |
| DELETE | `/api/reminders/{id}` | hard delete → 204. **No UI surfaces this** — like `/api/push/test` it is a `curl` affordance, for the row you want gone rather than cleared |
| GET | `/api/channels` | `[{name, enabled}]`, **`ORDER BY name`** — it drives chip order in two places (§9.2) and the client never re-sorts, exactly as with `/api/state`. **The URL is not in the response at all**, not masked and not reduced to a boolean: the client has no use for it — a chip is a name and a selected state — `nag channel list` is where you look at a URL (§7.4), and a field the UI must ignore is a field better not sent, which is the same argument that keeps `pushed_at` out of the reminder object (§4.1). Read-only — writes are §7.4 |
| POST | `/api/push/subscribe` | `{endpoint, keys:{p256dh, auth}, application_server_key}` → **204 with no body**, matching `/unsubscribe`: there is no subscription object in the API (§7.1 has no reader for one), so the only thing a body could carry is an echo of the request. Stores `application_server_key` as the row's `vapid_public` (§7.1). **`ON CONFLICT(endpoint) DO UPDATE` must overwrite `p256dh`, `auth`, *and* `vapid_public`**, leaving `created_at` at its original value. An insert-or-ignore would make §9.8's unconditional re-post useless for the one job it exists to do: after a key rotation the browser re-posts the same endpoint under the new key, and a row whose `vapid_public` never updates keeps failing forever while looking healthy. `endpoint` must parse as an absolute `https:` URL and `application_server_key` must be unpadded base64url decoding to 65 bytes — otherwise a padded encoding from a hand-written client makes §7.1's boot mismatch warning fire on every start of a perfectly healthy install. **`keys.p256dh` and `keys.auth` are shape-checked on the same argument as §5.1's VAPID pair, not merely required**: both must be unpadded base64url, decoding to exactly **65 bytes** for `p256dh` (an uncompressed P-256 point, the same shape as the server key) and **16 bytes** for `auth`, rejected with the usual 400 naming the field. A mangled value here is accepted by the table and by nothing after it — it surfaces as an encryption or send failure on every sweep, in a log line, about a row that looks perfectly healthy — so the string is either well-formed at subscribe time or it is a 400 |
| POST | `/api/push/unsubscribe` | `{endpoint}` → 204. **No UI surfaces this either.** The notification slot is absent whenever push is working (§9.2), so the app has no "turn it off" control by design; revoking permission in the browser is the user-facing path, and the row that strands is deleted on the next 404/410 (§7.1). This endpoint exists for `curl` and for a client that wants to unsubscribe cleanly |
| POST | `/api/push/test` | §7.5, not in the UI. **The per-endpoint result vocabulary is closed, both fields.** `status` is the HTTP status code the push service returned, or **`null`** when the attempt produced none at all — a timeout, a DNS failure, a refused connection; a `0` there would be indistinguishable from a code and would sort as one. `error` is `null` on success and otherwise drawn from **the same classification set §4.1 defines for `delivery_error`** — `timeout`, `refused`, `dns`, `http <code>`, `send failed` — plus two values that set has no use for: **`stale vapid key`**, for a 401/403 whose row's `vapid_public` differs from the configured key, which is the case that also sets `deleted: true` (§7.1); and **`not attempted`**, for a subscription the endpoint's 12-second send budget never reached, which always carries `status: null` and `deleted: false` (§7.5). Both extras exist because this endpoint reports per subscription rather than per attempt: every stored row gets an entry, so "no result" is never how a caller learns something. One vocabulary for every delivery failure in the app, so the string you read here is the string you would have read in a `delivery_error` or a log line, and remote text reaches this response as little as it reaches that column |
| GET | `/healthz` | no auth. Runs `SELECT 1` — otherwise it returns 200 over a broken database |

**`/api/` is also registered explicitly, as a catch-all that returns `404`.** `ServeMux` matches the longest registered pattern, and `GET /` serves `index.html` (§8.1) — so without that registration `/api/typo` falls through to the root pattern and answers a misspelled endpoint with the HTML page under a `200` — a `curl` reading markup for a JSON field that was never going to be there, and a client whose bug is invisible because nothing failed. The catch-all sits behind the same auth middleware as the rest of `/api/*` and answers in the ordinary error shape (§8.3), so the API surface fails honestly on a typo instead of pretending the path exists.

**One consequence of that registration is explicit rather than accidental: a wrong method on a real path answers `404`, not `405`.** `ServeMux` synthesises a `405` with an `Allow` header when a request's path matches a pattern but its method does not — and with `/api/` registered for every method, `POST /api/config` matches *that* pattern, so the mux never reaches the method-mismatch branch and the request lands on the catch-all's 404 like any misspelling. Accepted, deliberately, for a surface whose only non-browser client is `curl`: both answers mean "no such request here", the correct method is in the table above rather than in a header nobody reads from a shell, and one honest shape beats two a client would have to tell apart. Registering each method separately to recover the `405` would mean listing every method of every path and then still writing the catch-all for typos.

Reminder object: `{id, text, due_at, notified_at, done_at, extra_channels, delivery_error}` — **`pushed_at` is deliberately absent** (§4.1). `text` is returned **raw**, markdown links and all — the client renders it (§9.10). `extra_channels` is **always a JSON array**, `[]` rather than `null`, whatever the column holds; `notified_at`, `done_at`, and `delivery_error` are `null` when unset. Send raw timestamps; **all human formatting happens client-side** so it follows the browser's locale and timezone (§6, §9.11).

Every mutating endpoint returns the affected reminder (or 204 for a delete), and **the client follows every successful mutation with a `/api/state` fetch** rather than splicing the response into its list. The returned object is what makes the row redraw correct; the refetch is what makes the badge, the title, and the overdue/later split correct, and those are server-computed by definition (§9.3). One extra request per user action, at a rate bounded by how fast a person can tap.

Request bodies are capped at **16 KB** via `http.MaxBytesReader` — `text` is limited to 1000 bytes (§8.3) but nothing else would stop a client from streaming a gigabyte into the decoder first. Over the cap is a 400 like any other malformed body. The server also sets `ReadHeaderTimeout: 10s`, `ReadTimeout: 15s`, `WriteTimeout: 15s`, and `IdleTimeout: 120s`; the defaults are unlimited, and this process is the one that must still be alive in a year. **`WriteTimeout` is also the number `POST /api/push/test`'s send budget is derived from** — 12 seconds of sequential sends at 3 seconds each, deliberately under the 15, because the one handler that legitimately spends seconds on third-party sockets is also the one whose whole value is the answer it prints (§7.5).

`/api/state` returns the full `later` list, unpaginated, and the client polls it every 45 s while visible and every 5 minutes while hidden (§9.3). At personal scale that is a few KB and the simplicity is worth more than the bytes. It carries a **weak** `ETag` — `W/"…"` — and honours `If-None-Match` with `304`, which makes the steady-state poll nearly free — **the ETag hashes the body with `server_time` excluded**, because that field changes every second and hashing the whole body would mean the `304` never once fires. Weak is not a hedge but the accurate label for exactly that exclusion: two responses sharing the tag are equivalent for every purpose this client has and are *not* byte-identical, since `server_time` has moved between them, and a strong validator claims byte equality. On a `304` there is no body at all, so the client reads the clock from the response's standard `Date` header, which is present on both paths and needs no invention (§9.3).

**`app.js` holds that ETag itself, in a variable, and sends it explicitly.** `/api/*` is `no-store` (§10.2), so the browser cache will never revalidate on the client's behalf; and there is no client-side persistence anywhere (§12), so a reload legitimately costs one full body before the cheap polls resume. Both of those are consequences, not oversights.

`/api/*` responses carry `Cache-Control: no-store` (§10.2). Cleared reminders are kept (§4.4) but **not exposed** — there is no history endpoint in v1; undo works from the id the client already holds.

### 8.3 Validation and errors

Every rejection is `{"error":"<lowercase sentence>"}` with a `4xx`. The client shows the string verbatim, so write them for a human.

**Every `5xx` carries that same one-field shape, with a fixed generic sentence** — `{"error":"internal error, see the server log"}` — and **never** an internal error string, a driver message, or a stack fragment. A 500 needs a shape at all because two readers already assume one: §9.2 renders "the error string from §8.3" in a toast on any failed action, and §10.4 logs `4xx`/`5xx` alike. And it needs a *constant* one for the masking argument that governs `delivery_error` (§4.1): a `modernc.org/sqlite` message or a wrapped `*url.Error` reaches a toast, and the same string reaches the log — so there is no single place to redact a path, a DSN, or a channel URL that has already been embedded in it. The real error lives in the log line for that request (§10.4), which is where the id, the path, and the status already are.

- `text` — required, trimmed, non-empty after trimming, max **1000 bytes** (bytes, not runes; the limit exists to bound the push payload, and a markdown link costs ~60 of them). Must be valid UTF-8 and contain **no control characters**, newlines included: the capture control is a single-line `<input>` (§9.2), so a multi-line reminder can only arrive by `curl`, and accepting one would mean specifying how a notification body, a shoutrrr message, and a `textContent` row each fold it. 400 naming the character class, not the byte.
- **Markdown links inside `text` are not validated.** The text is stored exactly as typed, and a `[label](target)` whose scheme isn't `http` or `https` is simply not a link — both renderers emit the whole match as literal text (§9.10). There is deliberately no 400 here: rejecting would make `[ticket](obsidian://…)`, `[doc](file:///…)`, and even plain prose like `see [note](internal)` unenterable with no workaround, in exchange for a guarantee the renderer already provides on its own and that `textContent` provides again behind it (§9.7). `flattenLinks` stays the server's only contact with the text (§9.10).
- `preset` — must match a configured `key`. A client holding a stale `/api/config` can send a key that no longer exists; the 400 says so and §9.2 says what the client does about it.
- `due_at` — any integer in `946684800 .. 4102444800` (2000-01-01 to 2100-01-01 UTC), **including the past**: backdating is sometimes intentional, and it lands in "Needs you now" with `notified_at` and `pushed_at` already set, so it never notifies (§4.1, §9.6). Outside the range is a 400 naming both bounds. Nothing legitimate produces one — the picker cannot, and a preset cannot — so the check exists to stop a `curl` typo or a millisecond timestamp from becoming a row that every formatter downstream (§9.11) has to render as a date tens of thousands of years from now — a current-epoch value in milliseconds read as seconds is around 1.79 × 10¹² of them, somewhere around the year 56 600.
- **Exactly one of `preset`/`due_at`** on create, and at most one on a PATCH. Both together is a 400; neither on create is a 400.
- **PATCH field semantics.** A key **absent** from the object is unchanged. `extra_channels: null` or `[]` clears the list. `text` and the timing fields cannot be cleared — `text: null` or `""` is a 400, and there is no way to unset `due_at`. An empty object is a 400 (`"nothing to change"`). Distinguishing absent from `null` needs presence detection, so decode into `map[string]json.RawMessage` and then decode each known key from it. **Unknown fields are rejected too, and that takes an explicit check rather than falling out of the decoding**: a map accepts every key it is given, so after the known ones have been consumed the handler asserts that nothing is left in it and 400s naming the **lexically first** leftover key — Go randomises map iteration, so "the first one" has to be chosen rather than taken, and an error message that names a different key on every retry is one nobody trusts. Cheap, but it has to be written — `DisallowUnknownFields` belongs to a struct decode, and a struct decode is exactly what presence detection ruled out.
- `extra_channels` — max 16 names, de-duplicated server-side, and **sorted by name before it is written**, so a given set of names has exactly one stored form and §4.1's "the resulting list differs from the stored one" test is an ordered array comparison rather than a set comparison.
- `extra_channels` — every name must exist in `channels`, else 400 naming the unknown one. A *disabled* channel is accepted here and skipped at send time (§7.2).
- **On a `PATCH`, a name already stored on that row is accepted even if it no longer exists in `channels`.** Only names *not* already on the row are checked. Orphans are defined behaviour on the send path (§4.1) and `nag channel rm` deliberately creates them, so the strict rule would mean that removing a channel makes every reminder still referencing it un-editable: the name survives in the editor only as that row's own removable orphan chip (§9.2) and is offered on no other row, yet the editor always sends the full list — so fixing a typo in that row's text would 400 on a name the row was already carrying and the user never introduced. The asymmetry is the point — you can carry an orphan forward, you cannot introduce one. A client that wants to drop it sends the list without it, which works because absence is how the list is cleared.
- **The server has no fallback to `default_preset`.** The client always resolves it — a chip and a bare `Enter` both send `preset`, the picker sends `due_at`. That keeps one place deciding what "Enter" means, and it's the place that knows what the user was looking at.
- Unknown `{id}` → 404. Malformed JSON → 400.

### 8.4 Create, edit, and re-snooze are one path

`POST /api/reminders` and `PATCH /api/reminders/{id}` accept the same fields and the client renders the same control for both (§9.2). **There is no `/snooze` endpoint.** A quick chip on a row, a new time from the picker, and a typo fix are all `PATCH` — one handler, one set of re-time rules (§4.1), nothing to keep in sync.

The dropped `snooze_count` is what made a separate endpoint worth having: `/snooze` and `PATCH` differed by a counter and nothing else, and once the counter goes (§12) the two are the same request. Sending a new time through `PATCH` on a cleared reminder un-clears it; sending only `text` leaves `done_at` alone.

---

## 9. Frontend

### 9.1 Visual direction

"Patina structure, Horizon palette" — see `reminder-theme-preview.html`. Generous spacing, pill-shaped actions, italic serif for times, humanist sans for everything else. Upcoming items recede to quiet ink; only overdue carries full contrast.

**Tokens** — identical names, two value sets. The **light** set is declared on bare `:root`; the dark set redefines the same names inside `@media (prefers-color-scheme: dark)`. Nothing sets a `data-theme` attribute, so no rule may be scoped to one:

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

Amber is **not** a straight inversion — `#E0A33E` measures ~2:1 on light and is unusable. Light quiet ink is `#5C6C72` (4.8:1), not `#67787E` (4.05:1, under floor). `--scrim` appears only in the picker mockup, and only the sheet uses it (§9.6), but it is a themed colour like the rest and belongs here rather than inline.

**That table is the complete set of colour tokens** — the union of both reference files, and authoritative where either disagrees with it. The mockups also declare four non-colour tokens that are layout, not theme, and stay where they are used: `--body` and `--time` (fonts, below), `--pad` (26px, 18px under 700px — §9.9), and `--item` (44px, the picker's wheel row — §9.6).

**Theme follows `prefers-color-scheme`. There is no override, no toggle, and no theme setting in the TOML.** A media query is the entire implementation: no persistence, no first-paint flash, no server-side templating of `index.html`, nothing to get out of sync between devices. The OS already knows whether it's night.

**What the mockups are, and are not.** Both files scope their tokens to `[data-theme="dark|light"]` and ship a theme switcher purely so one page can show both palettes side by side. That is a preview affordance and **must not be carried into `app.css`** — the shipped rule is `:root` plus the media query above. The same applies to anything else in them that exists to make a static page demonstrable rather than to describe the app.

**Fonts are system stacks. Nothing is downloaded and nothing is embedded** — exactly the two declarations both mockups already ship:

```css
--body: "Optima", Candara, "Gill Sans", "Gill Sans MT", sans-serif;   /* UI */
--time: "Iowan Old Style", Palatino, "Palatino Linotype", Georgia, serif;  /* times */
```

So there is no `@font-face` and no `web/fonts/`, and the CSP says `font-src 'none'` (§10.2). The faces that define the look — Optima for the humanist sans, Iowan Old Style for the old-style serif — are licensed OS faces that cannot legally be self-hosted, and shipping a substitute pair as woff2 would mean the app looks *less* like the mockups on the Mac and iPhone where those faces exist. The stacks fall through to Gill Sans and Georgia on Linux and Windows, which is the same design read in a slightly different voice. That is an accepted, deliberate variation across platforms, and it is the only one in the app.

### 9.2 Layout

One column, single page. Top to bottom:

1. **Capture bar** — a single-line text input plus preset chips from `/api/config` in file order, then a final "Pick a time" chip. **A chip saves immediately on click or tap** — that is Goal 1's one interaction. **On a successful save the input is cleared**, along with the channel selection (below), so the bar is ready for the next capture and an idle client is the ordinary state a held reload can land in (below); a save that fails leaves what you typed exactly where it is, next to its toast. The input is **autofocused on desktop only** (`min-width: 701px` / a `hover: hover` check): autofocus on a phone opens the keyboard over the list on every single load, which fights Goal 4 on the device where glanceability matters most.
   **Every chip, including "Pick a time", is `disabled` while the input is empty or whitespace-only**, and enables on the first non-space character. The alternative is a chip tap that round-trips to the server and comes back as a red toast saying `text is required`, which is a worse way to say "type something first" than a chip that visibly isn't ready.
   The input carries `maxlength="1000"` so the limit is felt while typing instead of arriving as a toast on save. It is a hint, not the rule: `maxlength` counts UTF-16 code units and §8.3 counts bytes, so the server stays the authority and an emoji-dense 1000 characters can still be rejected.
2. **Notification slot** — the single "Turn on notifications" control, or the iOS Home-Screen line, or the `denied` explanation (§9.8). It sits directly under the capture bar and **is absent entirely when push is working**, which is its steady state; it is above the lists because it is the one thing whose absence you cannot notice.
3. **"Needs you now"** zone with the overdue count in amber. Sorted by `due_at` ascending (longest overdue first). Each row: full-contrast text, amber lateness, lifted background, 2px amber left spine.
4. **"Waiting quietly"** zone. Quiet ink, sorted by `due_at` ascending.
5. **Empty state** when both are empty: "Nothing waiting." / "Type above and pick when it should come back." This is the most-seen screen — treat it as the reward, not an error.

A zone heading is hidden entirely when its list is empty; the empty state in point 5 replaces both.

**Row actions**: `Clear` primary, `Edit`, plus — **on "Needs you now" rows only** — re-snooze chips for presets with `quick = true`, each one a `PATCH` with that `preset` (§8.4). Desktop reveals them on `:hover, :focus-within`; mobile shows them always. `Clear` shows a 5-second undo toast calling `/undone`. **No confirm dialogs anywhere.**

`:focus-within` is not a nicety there: with `:hover` alone, tabbing into a row reveals nothing, so every action behind the reveal is focusable but invisible and the chips are unreachable by keyboard. And `Edit` is in the group because the row *text* cannot be a `<button>` — it contains anchors (§9.10), and nesting interactive elements is invalid — so pointer users get the click target described below while the keyboard gets a real button. Without it the row editor has no keyboard path at all.

The chips are for the thing that is already late, which is why the zone split matters here: an `offset` preset resolves from `now` (§4.1), so "30 min" on something waiting for next Monday would quietly pull it *forward* by six days. Correct per the re-time rules, and nobody means it. A "Waiting quietly" row gets `Clear` and the row editor, which offers the same chips in a context where the row's current time is visible next to them.

**Editing a row is the capture bar again.** Clicking the row's text — or its time, or the `Edit` action — expands the row into the same control as the capture bar, prefilled: text input holding the **raw** text including any markdown links, the full preset chip row, the "Pick a time" chip, and the channel selector. Same component, same keys, same behaviour, same endpoint (`PATCH`, §8.4). `Esc` cancels, `Enter` saves the text without touching the time, a chip or the picker sets a new time. Nothing else needs to exist to satisfy "rename it and move it".

Clicking a **link** inside the row text follows the link and does not open the editor.

**Channels.** The capture bar has an "Also send to" affordance that expands a row of channel chips from `/api/channels` (multi-select toggles, disabled channels shown greyed and unselectable). **When `/api/channels` is empty the capture bar's affordance is not rendered at all** — fan-out is opt-in per §7.2 and a fresh install has no channels, so the default state of this control is "gone". Default is none selected, and **the selection resets after each save** — nothing is remembered client-side (§12). The same chips appear in the edit form, reflecting the reminder's current `extra_channels`.

**The row editor's copy is suppressed on a different condition, and deliberately so: it renders its channel affordance whenever `/api/channels` is non-empty *or* the row itself carries any names.** With an empty catalogue and a row that carries names, the affordance shows nothing but that row's orphan chips, each one removable by a tap exactly as below. That reads like a special case and is the opposite of one: `nag channel rm` on the last channel would otherwise leave every subsequent save of a row referencing it sending `[]`, because the editor always sends the full list (below) and an unrendered affordance has nothing selected — silently wiping precisely the orphan names §8.3, §4.1 and the orphan-chip row of §12 exist to preserve. Scoping the empty-list suppression to the capture bar is what keeps both of those rules true at the same time: the editor always sends the full list of what its chips show, and an orphan survives until you take it off the row.

**Two asymmetries in the edit form, and both are "you can always take a name off a row".** "Greyed and unselectable" is a rule about *adding*, never about removing, and the row editor is the only place the difference shows:

- **A disabled channel that is already attached renders selected, greyed, and deselectable.** Unselectable in both directions would mean `nag channel disable` silently makes a name impossible to remove from the reminders carrying it — a mute switch that cannot be undone from the app.
- **A name on the row that `/api/channels` does not list renders as a greyed chip labelled with the name itself**, selected and deselectable, and once deselected it cannot be re-selected. These are the orphans `nag channel rm` creates on purpose (§4.1); they are invisible in the API's channel list by definition, so without a chip of their own the editor would send a selection that silently omits them and every save would quietly clean one out. The chip is what makes §8.3's rule ("you may carry an orphan forward, you may not introduce one") reachable from a browser instead of only from `curl`: leave it alone and it survives the `PATCH`, tap it once and it's gone.

The editor therefore **always sends `extra_channels`**, as the full list of what its chips show selected — orphans included. It never omits the key to mean "unchanged", because with orphans rendered there is nothing left that the client cannot see. More generally, **the editor's `PATCH` always carries every field the editor shows**: `text` and `extra_channels` on every save, plus a timing field — `preset` from a chip, `due_at` from the picker — only when one of those chose a moment during this editing session. So "`Enter` saves the text" and "a chip commits a time" are the same request shape, differing only in which of its fields changed: one body to build, one handler to read it (§8.4), and no branch deciding which keys to include. The absent-means-unchanged rule (§8.3) is what makes sending the unchanged ones harmless, and re-sending an identical `extra_channels` is why that rule compares the resulting list against the stored one rather than testing whether the key was present (§4.1). A row with channels attached carries a small quiet marker; when `delivery_error` is non-NULL the marker turns amber, carries the error as its accessible label, and **shows the error text in full when the row is expanded for editing** — a `title` tooltip alone is unreachable on the phone, which is where you are when you notice the amber.

**Failure states.** A failed API call shows the `error` string from §8.3 in a toast; nothing is optimistically applied except the `Clear` undo path. A `401` from any call swaps the list for the token field — a cookie can expire while a tab is pinned for a year.

- **A failed poll is not a toast.** `/api/state` runs every 45 s unattended, so surfacing each failure would mean a toast every 45 seconds for the whole duration of a network blip, on a tab nobody is looking at. Instead: keep the last good list on screen, and after **two consecutive** failures show one quiet persistent line under the capture bar — "Not connected — last updated 3 minutes ago", the timestamp being the last successful poll. It clears itself on the next success. A poll failure never clears the list and never touches the badge; stale-but-labelled beats empty.
- **A failed user action *is* a toast**, immediately and on the first failure. The difference is that someone is waiting for it.
- **A `400` naming an unknown `preset`** means this client is running against a config that has since been reloaded (§5.5). It is the one error the user cannot act on and the app can fix itself — so on this specific 400 the client **re-fetches `/api/config` and redraws the chip rows in place**, the capture bar's and any open row editor's: typed text, the open editor, and the picker's pending value all survive, and the full page reload stays governed by the ordinary quiet-moment rule below. Treating it as a `config_version` change instead would have deferred the repair precisely when it is needed. That 400 can only arrive *from* a state the reload is held in — something is typed in the capture bar, or a row editor is open — so the page would sit there holding a reload it was told to take, with the same stale chips, 400ing on every tap. Redrawing the chips in place is what makes "the app can fix itself" true from inside the very states that hold the reload.

**Reloading on a `config_version` change waits for a quiet moment.** The rule from §5.5 is unchanged — any difference means reload — but reloading mid-sentence would discard whatever is typed in the capture bar or half-chosen in the picker. So: if the capture input is non-empty, a row editor is open, or the picker sheet is up, hold the reload and take it on the next poll that finds all of them idle. A stale client is a client whose chips might 400 (handled above); a client that eats your typing is a client you stop trusting.

**A poll must never do to a row what a reload would do to the page.** That is the same rule one level down, and it is the one the 45-second poll walks into rather than the config reload: the row editor *is* a row (above), so a naive "replace the list on every 200, redraw every row on every 304" (§9.3) discards half-typed text, drops focus, and can move the scroll position — every 45 seconds, unattended, including on the 304 path that exists precisely to keep time strings moving. So the list is rendered **keyed by reminder id**, and a render pass:

- **skips any row with an open editor entirely** — no attribute is touched, and if that row is no longer in the response it stays on screen until the editor closes, at which point the next pass reconciles it. A row cleared on another device while you are editing it here is worth one stale row for a few seconds;
- **updates in place** rather than replacing: for an unchanged row a redraw is the time string and the lateness class, nothing else. Reusing the node is what preserves focus inside the row's action group, the `:hover` and `:focus-within` reveal (below), and the browser's own scroll anchoring;
- **removes and inserts only what actually left or arrived**, so nothing under your finger moves except for the reason it moved. The server-sorted order with its `id` tiebreak (§8.2) is what makes that stable.

None of this needs a framework or a diffing library — it is a `Map` from id to node, and it is the only reason `app.js` can hold a list, a poll, and an inline editor at once without a state manager.

Toasts live in one `aria-live="polite"` region so both errors and the undo offer are announced. The toast's `Undo` is a real focusable button reachable by `Tab`; there is deliberately **no undo shortcut** — a 5-second `Ctrl+Z` window would be the fourth key binding and the only modal one (§9.4).

The control that submits is disabled while a create or save request is in flight, so a double-tap on a chip cannot produce two reminders.

### 9.3 Badge semantics

**The badge is the overdue count only. Never the total.** Upcoming items must never light it up — that's exactly what makes Slack's icon meaningful.

Implement one `setBadge(n)`:
1. `navigator.setAppBadge(n)` if available (installed PWA; Chrome/Edge/Safari). Firefox does not support the Badging API on any platform.
2. Always also: canvas-drawn favicon with a count dot, swapped via `link[rel=icon]`.
3. Always also: `document.title` prefixed `(n) `.

`setBadge(0)` is a full reset, not a no-op: `clearAppBadge()` rather than `setAppBadge(0)`, the plain `32.png` back in `link[rel=icon]`, and the `(n) ` prefix removed from the title.

**The drawn favicon is pinned too, because a tab strip is not a page and none of §9.1 reaches it.** A 32×32 canvas, `32.png` drawn at full size, then a filled circle in the bottom-right quadrant — radius ~11px, `#E0A33E` (the dark-set `--late`) with a `#212A2E` numeral centred in it, both hardcoded rather than read from a custom property. The dot has to stay legible on a light *and* a dark browser chrome, `prefers-color-scheme` describes the page and not the chrome, and there is no media query that reports what colour Firefox painted its tab bar — so the pair is chosen once to work against both and the theme rule simply does not apply here. The numeral is the count up to **9** and the literal `9+` above it, at which point it is a dot that means "several", which is all a 12-pixel circle can ever mean; the exact number is in the title and the list. Title: the document's base title is the bare word `Nag`, and `setBadge(n)` writes `(n) Nag` — read the base from a constant, never from `document.title`, or the prefix compounds into `(3) (2) Nag` on the second call.

Poll `/api/state` every 45s while the document is visible, immediately on `visibilitychange` → visible, immediately after every successful mutation (§8.2), and immediately on a service-worker message.

**Those triggers overlap by construction, so every `/api/state` fetch goes through one `AbortController`: before issuing any of them, abort the in-flight one.** A mutation's immediate refetch lands on top of a 45-second poll routinely — that is what "immediately after every successful mutation" means — and nothing orders two responses to the same endpoint, so a slow poll that answers *after* the refetch which overtook it re-renders the list as it stood before the mutation — a row you just cleared reappears, and stays there for up to 45 seconds until the next poll happens to correct it. One controller, replaced on each issue, makes the newest request the only one that can write anything: **an aborted fetch updates nothing at all — not the list, not the badge, not the clock offset, and not the stored ETag.** That last one is why the rule is cheaper than it looks: the ETag is held in a variable in `app.js` (§8.2), so a stale response overwriting it with an older value would outlive the wrong render entirely, and every later poll would revalidate against a body the client no longer has on screen.

**A `304` re-renders too.** The ETag excludes `server_time` so that the steady-state poll is cheap (§8.2), which means the common case is a poll that returns no body while every row's time string has nevertheless moved — `in 4 minutes` became `in 3 minutes`, and `now` became `2 min late`. The obvious `if (res.status === 304) return` freezes every time string on screen until the data happens to change, which on a quiet afternoon is hours. So: a 304 updates the clock offset from the `Date` header, re-renders the existing rows from the list already in memory, and re-arms the due timer; only a 200 replaces the list. Both paths go through the keyed render of §9.2, so neither disturbs an open row editor or the focus inside a row. That is also what makes the badge, the title and the favicon correct after a 304, since they are drawn by the same pass.

**A hidden tab keeps polling, at 5 minutes.** Suspending the poll entirely would break Goal 4 outright: a pinned tab you are *reading the favicon of* is `visibilityState: "hidden"` by definition, so the count would only ever refresh once you focused the tab — precisely when the favicon has stopped mattering. 45 s is for the tab in front of you; 5 minutes is what keeps the icon honest without polling on behalf of a machine nobody is using.

**`sw.js` also posts `{type:'state-changed'}` to every client on `push` (§9.5)**, which is what makes the firing that actually matters land on the favicon immediately rather than up to five minutes later. It rides the "poll on a service-worker message" trigger above, which is otherwise fired only by `notificationclick` — i.e. only once you have already seen the notification.

**The badge is never subject to the push cooldown (§7.3).** A reminder held for up to 30 minutes before its notification is in `overdue_count` the moment it comes due, and the one-shot due timer below is what puts it on the favicon within one round trip. This is load-bearing rather than incidental: it is the entire reason a 30-minute rate limit on notifications is acceptable in a reminder app. Nothing here needs to know that the cooldown exists — the count is computed from `due_at` and `done_at`, and `pushed_at` is invisible to the API (§4.1).

The one-shot due timer below is armed regardless of visibility, and its poll runs even when hidden. Browsers throttle background timers to roughly one per minute, so it degrades into a slightly-late poll rather than a missed one — the 5-minute poll and the worker message are what it degrades *onto*.

**Also set a one-shot timer for the earliest `later.due_at`** and poll when it fires. The overdue/later split is server-computed and never recomputed client-side, so without this a row that comes due sits in "Waiting quietly" reading `now` for up to 45 seconds while the badge disagrees with it. One `setTimeout`, re-armed on each poll from the first element of `later` (the list is server-sorted, §8.2), turns the worst case from 45 seconds into one round trip. It is the cheapest possible fix and it is what keeps "the list never disagrees with the badge" literally true.

**Clamp that delay to ~23 days** — `Math.min((due_at - now()) * 1000, 2_000_000_000)`, re-arming on each fire. `setTimeout` truncates its delay to a signed 32-bit integer, so anything past 2 147 483 647 ms wraps and **fires immediately**: one reminder six months out would otherwise fire the timer at once, poll, re-arm from the same first element, and fire again — a tight loop against the server for as long as the tab is open. The clamp turns it into a harmless extra poll every few weeks.

**`manifest.webmanifest` is required**, not a nice-to-have: the Badging API only works in an installed PWA, and on iPhone Web Push only works from the Home Screen (§7.1). Minimum viable content — `name`, `short_name`, `id: "/"`, `start_url: "/"`, `scope: "/"`, `display: "standalone"`, `background_color` and `theme_color` from `--surface`, and `icons` at 192px and 512px plus a 512px `maskable`. `theme_color` can only hold one value, so it takes the dark `--surface`; that is the single place the OS-follows-theme rule doesn't reach.

`web/icons/` therefore holds exactly four PNGs: `192.png`, `512.png`, `512-maskable.png` (safe zone inside the inner 80%, per the maskable spec), and `32.png` — the last one being the base image the canvas favicon draws its count dot onto, and the `badge` the service worker names in `showNotification` (§9.5). **The notification's `icon` is `192.png`, not `32.png`**: platforms render `icon` at roughly 64–192 device pixels and will happily upscale a 32-pixel source into a blurry square, while `badge` is the small monochrome-ish glyph in the status bar and 32 is the right size for exactly that one. Two different assets for two different slots, and the only reason to notice is that naming the same file for both looks tidier and ships a soft notification icon on every Android phone. No `apple-touch-icon`: iOS 16.4+ takes the manifest icons for a Home Screen install, which is the only iOS path the app supports anyway.

**The server clock is the only reference for *when*, never for *where*.** `/api/state` returns `server_time` (and the `Date` header carries it on a `304`, §8.2); the client stores an offset on every poll and renders every relative string through a single `now()`. The overdue/later split is server-computed and never recomputed client-side. A phone with a wrong clock then still shows the right lateness, and the list never disagrees with the badge. Timezone is a separate question and the answer is the browser's (§6) — the offset corrects skew, not zone.

**`now()` returns Unix seconds, and that is pinned as tightly as the cookie encoding is (§8.1)** — every timestamp that crosses the wire is Unix seconds (§4), `Date.now()` is milliseconds, and mixing them is a bug whose symptom is every row reading `19163 days late`:

```js
let offset = 0;                                     // seconds
const clockNow = () => Math.floor(Date.now() / 1000);
// on every 200: offset = server_time - clockNow()
// on every 304: offset = Math.floor(Date.parse(res.headers.get('Date')) / 1000) - clockNow()
const now = () => clockNow() + offset;              // seconds
```

So `whenText`'s `d = due_at - now()` is a difference of seconds (§9.11), and the **two places that need milliseconds convert at the call site**: the one-shot timer below (`(due_at - now()) * 1000`) and the picker, which builds a local `Date` and sends `Math.floor(d.getTime() / 1000)` (§9.6). Nothing else in `app.js` touches `Date.now()` directly.

### 9.4 Keyboard

- `/` or `n` — focus the capture input
- `Enter` — save with `default_preset`
- `Esc` — clear the input / close the sheet / cancel a row edit

That is the whole list. **`Enter` means whatever the surface it is pressed on saves**: the capture bar saves with `default_preset` as above, a row editor saves the text without touching the time (§9.2), and inside the picker sheet it activates `Set reminder` (§9.6).

**`Esc` acts on exactly one thing per press, innermost first: picker sheet, then row editor, then the capture input.** The three can be stacked — the picker is routinely opened *from* a row editor — and one press that closed all of them would throw away an edit you were still in the middle of. Closing the sheet returns focus to the chip that opened it (§9.6), so the second press lands on the editor and cancels that, and a third clears the capture bar if there is anything in it. `Esc` with nothing open and an empty input does nothing at all.

**`/` and `n` are ignored whenever focus is already in a text field** — check `event.target` against `input`/`textarea`/`[contenteditable]` and bail — **and whenever the picker sheet or a row editor is open**, because focusing the capture bar behind a scrim is not a state anyone asked to be in (§9.6). Otherwise the two of them are unusable in the capture bar, which is the one place you are guaranteed to be typing. They exist for after a blur: you clicked a link in a row, or `Esc`'d out, and want back in without reaching for the mouse. `Esc` is the exception and is handled globally, because "get me out of this" has to work from inside the field.

**There are no per-preset digit shortcuts** and the chips carry no `<kbd>` hints. The capture input is autofocused, so a bare digit cannot both type and fire a preset — "Renew the domain in 2 years" is a reminder you must be able to type. The alternatives all cost more than they return: `Tab` into the chip row adds a keystroke and a focus concept, arming the row with `Enter` makes `Enter` stop meaning "save", and `Alt`+digit is already tab-switching in Firefox and Chrome on Linux and Windows. Chips stay reachable with `Tab` because they are real buttons; that is enough.

### 9.5 Service worker

Must be served from **`/sw.js`**, not `/static/sw.js` — scope is path-based and a nested worker cannot control the page.

**`app.js` registers it on every load, unconditionally and without waiting for a gesture.** `navigator.serviceWorker.register('/sw.js')` needs no permission — it is `Notification.requestPermission()` and `pushManager.subscribe()` that do (§9.8) — and registering early is what makes the rest of §9.8 possible at all: `registration.pushManager.getSubscription()` is how the notification slot decides whether it is needed, and the unconditional re-post has nothing to post without it. Registration is **idempotent**, so calling it on every load costs one no-op against the browser's registration store and is the only way a `skipWaiting` deploy (below) reaches a tab that has been pinned for months. Await the registration and reuse that object; never call `register` a second time to "get" it.

**Everything push-related is feature-detected, in one place, once.** `'serviceWorker' in navigator`, `'PushManager' in window`, and `'Notification' in window` must all hold before any of this runs — a Firefox private window, an older desktop Safari, and any non-secure origin fail at least one of them, and the naive `navigator.serviceWorker.register(...)` throws a `TypeError` that takes the rest of the load with it, list included. Compute one boolean at startup; when it is false, skip registration entirely and render the notification slot as the same quiet single line §9.8 uses for `denied`, worded for the browser rather than the permission. **The lists, the badge, the picker, and every write path work without a service worker** — losing push must cost push and nothing else, which is also why registration is never awaited before the list renders (§9.8).

If `register` rejects — a syntax error in `sw.js`, a hard-refresh race, a browser that has disabled workers by policy — log it to the console, set that same boolean false, and carry on. There is nothing to retry and nothing the user can do, and a rejected promise on the load path must not become an unhandled rejection that stops the poll from starting.

- `push` → parse the payload (§7.1), draw **exactly one** notification, then `postMessage({type:'state-changed'})` to every client so an open-but-hidden tab updates its badge at once (§9.3). One push is one notification whatever `n` says — the worker never loops over `texts` calling `showNotification` per entry, which would undo the batching the server just did.

  **The whole chain is wrapped in `event.waitUntil(...)`**, exactly as `notificationclick` is (below), and it is not optional: `showNotification` returns a promise, and a `push` handler that returns before it settles tells the browser the event is finished, which frees it to kill the worker mid-draw. The visible outcome is a push that arrives and shows nothing — and *that* is the silent push §7.1 warns about, the one browsers answer by revoking the subscription. So `event.waitUntil` covers the `showNotification` call and the `postMessage` fan-out together, and the handler is `async` only inside that promise.

  | payload | title | body |
  |---|---|---|
  | `n == 1` | `texts[0]` | `whenText(due_at)` |
  | `n > 1` | `${n} reminders need you` | `texts` joined with ` · `, then ` · +${n - texts.length} more` when `n` exceeds what was sent |

  Options in both cases: `{tag: 'nag', renotify: true, data: {id}, icon, badge}`, with `data.id` present only when the payload itself carried an `id` — which is `n == 1` from the sweep, but not the `n == 1` the test endpoint sends (§7.1, §7.5). Read the field, never the count.
- **`tag` is the constant `'nag'`, matching the server's `Topic` (§7.1), and it needs `renotify: true`.** A constant tag means the tray holds at most one Nag notification: a new one replaces the previous, which is right for an app where the tray is a nudge and the count lives on the badge (§9.3) — otherwise ignoring three notifications leaves three stacked, each pointing at the same list. `renotify` is not optional decoration: replacing a notification under an existing `tag` is **silent** by default in Chrome, so without it the second digest of the morning would appear with no sound, no vibration, and no banner, and would look exactly like a bug.
- The relative string comes from the same formatter as the rows (§9.11), which the worker carries its own copy of — `sw.js` cannot import from `app.js`. The worker has no server offset available either — there is no page to hold one — so its `now()` is `Math.floor(Date.now() / 1000)`, the same seconds convention as §9.3 minus the correction. Accepted: it is a nudge, and the list it points at is correct. Every `texts` entry is already link-flattened and length-capped by the server (§7.1), so the worker needs no markdown code and no truncation logic at all.
- **A `push` event must never draw nothing.** If `event.data` is absent, does not parse, or carries no usable `texts`, `showNotification` is still called — fixed title `Nag reminder`, no body, same `tag: 'nag'` and `renotify: true`. Returning early is what gets a subscription revoked (§7.1) and it also hands the user the browser's own generic "site updated in the background" notification, which is strictly worse than a vague one of ours. Wrap the parse, don't guard the call.
- **No `fetch` handler, and no cache.** The worker exists for `push`, `notificationclick`, and `pushsubscriptionchange`; it never intercepts navigation. Offline support is not a goal, and a cache here is the fastest way to serve a year-old `app.js` to a pinned tab.
- `install` → `skipWaiting()`, `activate` → `clients.claim()`. A new worker takes over on the next load instead of waiting for every tab to close.
- `notificationclick` → `notification.close()`, then focus an existing client if one exists, else open a new one; then, **if `data.id` is present**, `postMessage({type:'focus-reminder', id})`. Without a `data.id` the message is `{type:'state-changed'}` instead, and that covers two payloads rather than one: a digest, where several reminders mean there is no single row to scroll to and picking the first would flash one arbitrary row while the other five sit unhighlighted below it, and the test push, which is `n == 1` but points at no row at all (§7.5). Focusing the tab on a freshly-refreshed "Needs you now" list is the whole answer in both cases. Wrap the whole handler in `event.waitUntil`, and find clients with `clients.matchAll({type: 'window', includeUncontrolled: true})` — without `includeUncontrolled` a tab loaded before this worker took over is invisible to the search and gets a second window opened next to it.

  **The id travels in the URL when there is no client to talk to.** `postMessage` to a client that `clients.openWindow` just returned is a race the worker always loses: the page has not parsed `app.js` yet, so no `message` listener exists and the highlight silently never happens — which is the *common* case, since a notification you click is usually one that arrived with the tab closed. So there are two paths and they are not symmetric: an existing client gets `postMessage` (it is already listening, and it must not be navigated out from under whatever you were doing), while a new one is opened as `clients.openWindow('/?focus=<id>')`. On load, `app.js` reads `focus` from `location.search`, treats it exactly like a `focus-reminder` message, and immediately calls `history.replaceState({}, '', '/')` so a refresh does not re-flash a row you dealt with an hour ago and the URL never becomes something you might bookmark. Anything with no `data.id` — a digest, or a test push — opens the bare `/`.
- Page receiving a message: **switch on `type`.** `focus-reminder` → refresh state, scroll the row into view, apply a ~2.4s highlight flash (respect `prefers-reduced-motion`). `state-changed` → refresh state and nothing else; it arrives on a tab nobody is looking at, so scrolling or flashing would move the page under whatever you return to.

  **If the id is in neither list after that refresh, the app is focused and nothing else happens** — no scroll, no flash, no toast, no error. The row was cleared on another device while the notification sat in the tray, or it was cleared here and purged after the retention window (§4.4), and both are ordinary: a notification is never the record of anything (§7.6). This applies identically to the `?focus=<id>` path, which is treated as the same message (above), so a bookmarked or re-opened URL naming a long-gone id lands on a normal list rather than on a dead end. Do not fall back to flashing the first row — the reason a digest doesn't pick one arbitrary row applies twice over when the right one is gone.
- **`pushsubscriptionchange`** → re-subscribe with the configured VAPID key and `POST /api/push/subscribe` the new endpoint. Browsers rotate subscriptions on their own schedule, and skipping this handler is the single most common cause of "push worked for a month and then stopped".

  **The key comes from `GET /api/config`.** A worker fetch is same-origin and carries the session cookie by default (§7.1), so the authenticated read works; `event.oldSubscription?.options.applicationServerKey` is tempting and is not reliably populated across browsers, so do not depend on it. Fetching also means the handler picks up a *rotated* key rather than re-subscribing under the dead one. If the fetch fails — offline, or a cookie that finally expired — do nothing and let §9.8's unconditional re-post heal it the next time the page opens. That is the same recovery path, one page-open later, and there is nothing useful to do in a worker with no credentials.

  This is the one handler that legitimately talks to the server, and it is not on the notification-drawing path, which is why §7.1's "no network call" rule is scoped to `push`.

Deliberate non-behaviours, because they are the whole point of the app:

- **Dismissing a notification does nothing.** No `notificationclose` handler. Swiping it away leaves the reminder overdue — that is Goal 3, and it is what every other tool gets wrong.
- **Notifications are not synced between devices.** Clearing on the laptop leaves the phone's notification on screen until the OS drops it. `tag` only collapses within one browser. The list is the truth; notifications are a nudge toward it.

### 9.6 Picker

See `reminder-datetime-picker.html` for the reference implementation. Bottom sheet containing:

- **Two controls at the foot of the sheet: `Cancel` and `Set reminder`** — a ghost button and a filled one, as in the mockup. The wheels and the calendar only ever move a pending value; **nothing is written until `Set reminder`**, which is the one place in the app where a two-step commit is right rather than wrong. A chip is one interaction because it carries its own answer (§9.2, Goal 1); the picker exists precisely because you are choosing between many, and saving on every scroll-snap would fire a `PATCH` per notch of the minute wheel. `Set reminder` is the whole write — `POST` with a `due_at` from the capture bar, `PATCH` from a row editor (§8.4) — and it closes the sheet on success. `Enter` while focus is inside the sheet activates it, `Cancel` and `Esc` and a scrim click all discard, and the summary line above it is the confirmation you read before pressing it.
- **Live summary** — full date + time, and a relative line ("in 3 days"), both from §9.11's formatters. Non-negotiable: it's what makes this better than a native control. If the chosen moment is in the past, say **"in the past — it'll be waiting in Needs you now, and nothing will be sent"** and **allow it**. "Nothing" rather than "no notification": a backdated write skips its channels too (§4.1), and a sentence naming only the OS notification would read as a promise that ntfy still fires. Backdating is sometimes intentional, and the write path stamps both `notified_at` and `pushed_at` on anything landing at or before `now` (§4.1), so that sentence is true for a moment one second ago as much as for last Tuesday. It deliberately mentions no window and no cooldown: the client knows neither constant (§7.3), and after §4.1 neither applies to anything a person can pick here.
- **Month calendar** — prev/next month, week starting per `week_start`, today gets a thin ring, selected is amber-filled, past days dimmed (`--ink-faint`) but still tappable. "Today" and "past" are derived from `now()` (§9.3), not `Date.now()`.
- **No year stepper, and no upper bound on paging.** Reaching next year is a dozen taps of `next month`, and that friction is a fair match for how rarely a personal reminder belongs a year out — there is no preset past "next Monday" either. The one thing this needs from elsewhere is that the summary line grows a year as soon as you leave the current one (§9.11), so the paging is verifiable rather than a guess about how many taps you just made.
- The chosen wall-clock time is interpreted in the **browser's** timezone and sent as an absolute `due_at` — `new Date(y, m, d, hh, mm, 0, 0)` then `Math.floor(getTime() / 1000)` (§6, §9.3). A date and time straddling a spring-forward gap has no such local instant; `new Date` normalises it forward exactly as Go's `time.Date` does server-side, so 02:30 becomes 03:30 and the summary line shows the moment that will actually be stored before you can save it. Accept that, same as §6 does, and add no gap detection here either.
- **Hour wheel** — values `hour_min..hour_max` inclusive from config. **The range is a hard constraint; there is no "show all hours" escape hatch.** Deliberate decision — and since the wheel cannot produce an hour outside the range, there is no "outside your usual hours" hint either.
- **Minute wheel** — `0..59` step `minute_step`.
- **Opening state, for a new reminder:** time is `default_time` clamped into `[hour_min, hour_max]`; the date is **today if that time is still ahead of `now()`, otherwise tomorrow.** Opening on today unconditionally means that every afternoon the sheet greets you with the in-the-past warning before you have touched anything, which reads as a broken default rather than a considered one.
- **Opening state, when editing: the stored moment when it is still ahead of `now()`, and the new-reminder rule when it is not.** Take the reminder's `due_at`, clamp its hour into `[hour_min, hour_max]`, and set the minute to `00` (below); if the moment that produces is still in the future, the sheet opens on it. Otherwise it opens exactly as it does for a new reminder — `default_time` clamped, today or tomorrow. **One rule covers both modes, and the sheet never opens showing the in-the-past warning.** Rows you re-time are usually the overdue ones — that is what the quick chips and the row editor are for (§9.2) — so opening on their stored date would greet you with the warning before you had touched anything, which is the exact broken-default reading the create-mode rule exists to avoid, on the one sheet whose whole job is choosing a moment that is still to come. The test is on the value the sheet would *show* rather than on the stored one, because the minute reset can move that value backwards past `now()` even when the stored moment is ahead of it: a row due today at 14:30 reads back as 14:00. Nothing is lost by not showing the old date, because the row's current due time stays on the row itself, next to the open editor (§9.2) — the picker was never where you read it.
- When the sheet does open on the stored date, its hour is clamped into `[hour_min, hour_max]` and **the minute is always `00`** — never the row's actual minute, and never snapped to the nearest `minute_step`. A row a `30min` chip left at 14:23 next Thursday opens the picker showing 14:00. The wheel frequently cannot represent the stored minute at all (`23` is not on a 15-minute step), and the two alternatives are both worse: snapping to the nearest step is a silent 7-minute move that looks like the value you stored, and widening the wheel to every minute for the edit case only would mean two different minute wheels. `00` is always a valid value — `60 % minute_step == 0` is enforced (§5.5) — and it is visibly a *reset* rather than a near-miss, which is the honest signal when you are about to re-time something anyway. Clamping the hour can *silently move* the time — a preset may legitimately produce 07:00 while `hour_min = 8` (§5.4), and that reminder opens showing 08:00. Accepted, and the summary line makes it visible before you can save: the wheel cannot represent 07:00, and the alternative is either widening the range the whole app is built around or refusing to open the picker on a row that a chip created. Editing only the *date* of such a row does move its time into the range; that is the cost of the hour range being a hard constraint (§12). An overdue row never reaches any of this — it opens on the new-reminder default above, whose minute is `default_time`'s and is on a `minute_step` boundary by §5.5.
- Wheels are plain scroll containers: `scroll-snap-type: y mandatory`, item height 44px, vertical padding equal to one item so first/last can center, centered selection band and edge fade in CSS, selection read back from `scrollTop` on a ~90ms debounce. Each value is also a real `<button>` that scrolls itself to center — keeps tap and keyboard working. No drag handling, no library.
- Sheet: `max-height: 94vh`, `overflow-y: auto`, `padding-bottom: calc(16px + env(safe-area-inset-bottom))`, closes on scrim click and `Escape`.
- **It is a real modal**: `role="dialog"`, `aria-modal="true"`, `aria-labelledby` the summary line, focus moved into the sheet on open and **returned to the chip that opened it** on close, and `Tab` trapped inside while it is up. It covers the page under a scrim, so anything focusable behind it is a trap in the other direction — you tab into a list you cannot see. This is also why `/` and `n` are inert while it's open (§9.4).
- The wheels are 24-hour while the summary line is locale-formatted. That inconsistency is accepted: the wheels are a constrained work-hours range where 24-hour is unambiguous, and the summary is the thing you actually read back.

### 9.7 Interaction states

- `-webkit-tap-highlight-color: transparent` on all interactive elements — the default grey rectangle ignores border-radius.
- Designed `:active` (slight scale, ~0.96) so taps feel acknowledged.
- Focus rings via `:focus-visible` only, `2px solid var(--late)`, offset 2px. Keyboard keeps them; thumbs never see them.
- All reminder text and all error strings are written with `textContent`, links included (§9.10). **`innerHTML` appears nowhere in `app.js`.**
- **No inline `style` attributes in the markup.** `style-src 'self'` (§10.2) blocks a `style="…"` attribute parsed from HTML exactly as it blocks a `<style>` block, so nothing in `index.html` carries one.
- **CSSOM is a different question, and CSP does not govern it.** `el.style.setProperty('--offset', v)` and `el.classList.toggle(…)` from JS are unaffected by any `style-src` value — that is deliberate in CSP3, not a loophole — so `setProperty` is **available** if a value is genuinely dynamic. It is worth being exact about this because the opposite belief leads somewhere silly: "use custom properties instead of inline styles" would be advice to use the same API through a different property name. As specified, nothing actually needs it: the highlight flash and the sheet transform are class toggles with the values in `app.css`, and the wheels are positioned with `scrollTop` (§9.6), which is not CSS at all. So the rule is **prefer a class; reach for `setProperty` only for a number CSS cannot know**, and the only dynamic attribute in the app remains `link[rel=icon].href` (§9.3).

### 9.8 Notification permission

Nothing about push is requested on load — browsers require a gesture and Firefox rejects it outright.

- The app shows a single **"Turn on notifications"** button, and **only when it's needed**: push is unsupported by this browser (§9.5, and then it is the quiet line rather than the button), or `Notification.permission !== 'granted'`, or granted but `pushManager.getSubscription()` returns nothing, or `push_subscribed` is false. When notifications are working, the control is absent — no permanent settings furniture.
- **The `push_subscribed` condition is the belt to the other two braces, and it is deliberately kept even though it is almost always redundant.** With the re-post below awaited before the slot is decided, a healthy device whose row the server had dropped has already restored it, so the flag reads true and the condition contributes nothing — that is the point of awaiting it. What it still catches is the case the local checks cannot see: the re-post itself failed, or this browser holds a `PushSubscription` that no longer exists server-side and cannot be re-created. Cheap, and the failure it guards against is silence.
- On click: `requestPermission()`, then `subscribe({userVisibleOnly: true, applicationServerKey})`, then `POST /api/push/subscribe` — with `application_server_key` set to the `vapid_public` string it just subscribed under, which is what the server records (§7.1).
- **`applicationServerKey` is bytes, not the string.** `subscribe` needs a `Uint8Array`, so `/api/config`'s `vapid_public` is decoded on the way in — unpadded base64url to 65 bytes — and re-encoded with the same alphabet on the way back out (below). Two conversions, one alphabet, and they must be exact inverses or the key check below compares a value against its own re-encoding and fails on a healthy install. `atob` needs the `-`/`_` mapped back to `+`/`/` and the padding restored before it will accept the string; that is the whole decoder, six lines, and it is the only encoding code in `app.js`.
- If permission is `denied`, replace the button with one quiet line explaining that the browser is blocking it and that it has to be re-allowed in site settings — a `denied` permission cannot be re-prompted from script, and pretending otherwise is worse than saying so.
- **On every load, if a local subscription exists, `POST /api/push/subscribe` it unconditionally.** It's an upsert, so it costs one request and needs no way to ask the server "do you know *this* endpoint" — `push_subscribed` is instance-wide (§8.2) and could never answer that. This is what restores a row the server deleted on a 410 or a 403 (§7.1) instead of leaving the app silently deaf.
- **That re-post is awaited before the notification slot is decided.** The slot's condition reads `push_subscribed`, so on a healthy install whose row the server dropped, a `/api/state` that lands first reports `false` and the slot flashes "Turn on notifications" for a moment before vanishing — an invitation to fix something that is in the middle of fixing itself, on the one control whose whole design is to be absent when things work (§9.2). So: resolve the local subscription and its re-post first, then render the slot from the state that follows. The lists render immediately either way; it is only the slot that waits.
- **Compare keys first, and post the key you compared.** Encode `subscription.options.applicationServerKey` as **unpadded base64url** — `base64.RawURLEncoding` is what `webpush.GenerateVAPIDKeys` produced, so padding or standard base64 here turns every comparison into a false mismatch — and compare it to `/api/config`'s `vapid_public`. On a mismatch, `unsubscribe()` and subscribe again with the configured key before posting. Whatever survives that check is what goes in `application_server_key`. Re-posting a subscription still bound to a retired keypair heals nothing — the push service keeps rejecting it — and this is the client half of making key rotation recoverable (§7.1).
- **A readback that is `null` or unavailable is not a mismatch.** Keep the subscription, and re-post it carrying the configured `vapid_public` as its `application_server_key` — the value the comparison would have been against. Every browser new enough to have reached this line populates `options.applicationServerKey` (Chrome 42+, Firefox 46+, Safari 16.4+), so a null is a theoretical corner, and the failure actually worth avoiding is the naive reading: treating it as a mismatch churns `unsubscribe()` and `subscribe()` on a **working** subscription on every single page load, which is the one thing this whole re-post exists to prevent. **What that costs, said plainly, is the automatic recovery** — and it is worth naming rather than glossing, because the obvious sentence to write here is false. Posting the configured key makes the row's `vapid_public` equal to it by construction, so a subscription that genuinely *is* bound to a retired keypair takes §7.1's **matching** branch on its 403: an ERROR line every sweep, and the row kept, because from the server's side a matching key means a bug or a push-service problem rather than a stale one. The boot mismatch WARNING is blinded the same way, for the same reason. So in this corner the fix is manual, and it is the ordinary one from the row above: **reset** the site's notification permission in the browser — reset rather than block, or you get the `denied` line instead of the button — which drops the local subscription, puts the button back, and leaves the stranded row to the next `404`/`410` (§7.1, §8.2). `POST /api/push/unsubscribe` by `curl` does the same thing from the other end. That is the right trade against churning a working subscription on every load of every device to rescue a case no shipping browser produces. (§9.5's warning is about whether `pushsubscriptionchange` populates `event.oldSubscription` at all, which is a different question from what a live subscription's `options` holds.)
- On iOS, when the page isn't running standalone, the same slot explains that notifications need the app added to the Home Screen first (§7.1).

### 9.9 Mobile (≤700px)

Rows wrap: text takes the full width on its own line, then time bottom-left and actions bottom-right (`margin-left: auto`). Container padding 18px, tap targets ≥40px, reminder text 16px, and the capture input **must stay at 16px** to stop iOS zooming on focus. The capture input is **not** autofocused here (§9.2).

### 9.10 Reminder text: one markdown link form

There is no `url` column. A reminder that needs a link carries it inline, in the text, as `[label](https://example.com/x)` — which is also exactly what the deferred bookmarklet would send, with no extra field to design.

**The whole grammar** is that one form. No emphasis, no code spans, no bare-URL autolinking, no reference links. Anything else is literal text.

- Match `[label](target)` where `label` is **non-empty** and contains no `[` or `]`, and `target` contains no whitespace and no `)`. Non-greedy, left to right: `\[([^\[\]]+)\]\(([^\s)]*)\)`. An empty label is not a link — it would render an unclickable empty anchor and flatten to an empty push body.
- **A match is only rendered as a link if `target` has scheme `http` or `https` and contains no `(`.** Anything else is emitted as the whole match, literally, brackets and all. There is no server-side rejection to pair with this (§8.3): the renderer is the only side where a bad scheme could become an exploit, and it is the side that refuses.
- The `(` clause is what keeps a paren-bearing URL from failing *silently*. `[Foo](https://en.wikipedia.org/wiki/Foo_(bar))` matches with `target` truncated at the first `)` — a link that looks fine and 404s. Refusing any target containing `(` turns that into visible raw markdown instead, which tells you to **percent-encode the parens** (`%28`, `%29`), the one documented workaround. The deferred bookmarklet (§11) encodes them before building its `[title](url)`.
- Render by walking the matches and appending nodes: `document.createTextNode` for the runs between, and for each link an `a` with `textContent = label`, `href = target`, `target="_blank"`, `rel="noopener noreferrer"`. **No `innerHTML`, no parser reuse from a library** (§9.7).
- Links get `--late`-free styling: `color: var(--ink)` with an underline in `--ink-faint`. Amber means **something needs you** — lateness, and a delivery failure on a row that already fired (§9.2) — and it never means "this is clickable" (§9.1).
- Server-side there is exactly one helper, `flattenLinks(text)`, which replaces a match with its `label` **only when that match would render as a link** — same gate as above: scheme `http` or `https`, no `(` in the target. Anything else is left exactly as written, brackets and all. Both halves of the gate are load-bearing: a bare "replace every match with its label" would flatten `[a](mailto:x@y.z)` to `a` while the row renders it literally, and would turn `[a](javascript:alert(1))` into `a)`. Push payloads (§7.1) and shoutrrr messages (§7.2) use it, so neither shows raw brackets. It is the only place the server touches text.

The one shared risk is that the parser exists twice, in Go (`flattenLinks`) and in JS (the renderer). **`sw.js` needs no copy of it**: the push payload's `text` arrives already flattened (§7.1), so the worker never sees a bracket — the only thing it carries its own copy of is the time formatter (§9.5, §9.11). Keep both copies to the single regex above, and put the same table of cases behind each:

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

`flattenLinks` runs the same table and must agree on every row: whatever renders as literal text stays byte-identical in a push payload and a shoutrrr message, and only a real link collapses to its label.

### 9.11 Time strings

Two functions, in `app.js`, and nothing formats a time outside them. The mockups show hand-written samples like `4 days late` and `Monday at 9`; they are illustrations, not formatters, and these are the formatters.

**`whenText(due_at)`** — the short form. Three places read it: a row's time (§9.2), the picker's relative line (§9.6), and the notification body (§9.5, via the worker's own copy — `sw.js` cannot import from `app.js`).

`d = due_at - now()`, where `now()` is the skew-corrected clock from §9.3 (the worker has no offset, so its copy uses the device clock). **First match wins, top to bottom** — the conditions overlap by design, so the order is the specification.

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

"Calendar day" and "calendar year" are the **browser's**, consistent with §6. **From `today` downwards every condition is a calendar comparison, never a duration** — the difference in days between the two local midnights, so the weekday row is a difference of 2 through 6 and the row above it is exactly 1. Mixing the two is what makes a boundary drift by up to a day depending on the time you look: `d < 6*86400` on a Monday evening reaches Sunday, and the same arithmetic on a Monday morning stops at Saturday, so the same reminder reads `Sunday at 9:00` or `12 April at 9:00` according to when you glanced at it. The three duration rows at the top (`late`, `now`, `in N minutes`) are durations because that is what they mean. The year is a comparison against the current one, not a distance: a date 8 days out that happens to fall in January reads `2 January 2027`, and one eleven months out in the same year does not carry a year it doesn't need. Without this rule a row paged fourteen months ahead is indistinguishable from one two months ahead — the same string, a year apart. Each threshold uses the floor of the larger unit, so 90 minutes late is `1 h late`, and every count is singular at 1 (`1 day late`, `in 1 minute`).

**`<time>`, `<weekday>`, `<day month>`, and `<day month year>` are locale-formatted; the surrounding words are not.** `Intl.DateTimeFormat` with an `undefined` locale renders the clock the way the device wants it — `09:00` or `9:00 AM` — and gives the weekday and month names in the device's language. The connective copy (`late`, `at`, `today`, `in`) is English, exactly like every other string in the app. §6's rule was always about *format*, never about translating the UI, and this is the one place that distinction is visible enough to be worth stating.

The picker's relative line uses the future rows above verbatim, and replaces the whole string with the fixed past sentence from §9.6 as soon as `d <= 0` — it never says `late` about a time you have not saved yet.

**`fullText(due_at)`** — the long form, used only by the picker's summary line (§9.6), which has room for it and is the one place you are committing to a specific moment rather than glancing at one. Weekday, day, month, and time, all locale-formatted, with the time emphasised: `Saturday 15 August at **09:00**`. `Intl.DateTimeFormat(undefined, {weekday: 'long', day: 'numeric', month: 'long'})` plus the same `<time>` as above; the word `at` is the only English in it. **The year appears under the same rule as `whenText`** — only when it isn't the current one. The picker *can* be paged into another year (§9.6), and the moment it has been is exactly the moment you want that confirmed in the summary line before saving.

---

## 10. Deployment

Multi-stage build, `FROM scratch` final image. Copy `ca-certificates.crt` from the builder — both Web Push and shoutrrr make outbound TLS calls.

```dockerfile
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" -o /nag ./cmd/nag
RUN mkdir -p /empty/tmp && chmod 1777 /empty/tmp

FROM scratch
COPY --from=build /nag /nag
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build --chown=65534:65534 /empty /
ENV TMPDIR=/tmp
EXPOSE 8080
USER 65534:65534
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s CMD ["/nag", "healthcheck"]
ENTRYPOINT ["/nag"]
CMD ["serve"]
```

`main.version` is the only build-time variable, printed by `nag version` and logged at boot (§10.4). It defaults to `dev`, so the documented build is `docker compose build --build-arg VERSION=$(git describe --tags --always --dirty)` — a boot line reading `dev` is then itself informative: someone built this by hand.

No `VOLUME` declaration: the mount points are bind mounts declared in compose, and `VOLUME` would only add anonymous volumes for anyone who forgot them.

**`.dockerignore` is required, not hygiene.** The build context is the project directory, which is also where §10.1 tells you to put `data/` and §5.2 tells you to put `nag.env` — so a bare `COPY . .` sends your VAPID private key and your database into the build and bakes them into the cached builder layer. The final image is `FROM scratch` and carries neither, which is exactly what makes it easy to miss. Exclude at least `data/`, `config/`, `nag.env`, `.git/`, `*.md`, and the two root `reminder-*.html` mockups — they are design references that nothing embeds (§3), so they are context the build has no use for; the build needs `go.mod`, `go.sum`, `cmd/`, `internal/`, and `web/`.

```yaml
services:
  nag:
    build: .
    image: nag:latest
    volumes:
      - ./data:/data
      - ./config:/config
    env_file: ./nag.env
    networks: [caddy]
    restart: unless-stopped
    labels:
      caddy: nag.example.com
      caddy.reverse_proxy: "{{upstreams 8080}}"

networks:
  caddy:
    external: true
```

### 10.1 Scratch-image consequences

- **No `/tmp`.** SQLite wants a temp directory for some operations, so one is copied in and `TMPDIR=/tmp` is set. Without it you get a failure that reads like corruption. **The copy is `/empty` → `/` rather than the obvious `/emptytmp` → `/tmp`, and that shape is the point:** `COPY` of a directory copies its *contents*, not the directory, so the obvious form makes `/tmp` a destination path the builder creates implicitly — and its mode and ownership are then whatever the builder decides, not the `1777` and the uid you asked for. Copying the parent makes `tmp` itself one of the copied entries, carrying its own metadata, so the mode is the one set in the builder and `--chown` lands on it because it is a file being copied rather than a path being made. A `/tmp` that ends up root-owned and `0755` is a scratch image where the first temp file SQLite reaches for fails as a permission error under uid 65534, which is the same failure that reads like corruption, one layer further from where you would look.
- **No `/etc/passwd`**, so `USER 65534:65534` is a bare numeric uid — and with no shell in the image there is no way to fix ownership from inside it. **The app therefore runs non-root against host bind mounts that you `chown` once, before the first `up`:**

  ```
  mkdir -p data config
  sudo chown -R 65534:65534 data config
  ```

  Two README steps, and they come before the two in §5.2. Bind mounts rather than named volumes for three reasons: a named volume is created root-owned on first use, so the same `chown` becomes a `docker run --rm -v <project>_nag-data:/data alpine chown …` incantation that breaks when the compose project is renamed; the §10.3 backup story turns into `cp data/nag.db*` on files you can see; and a wrong uid fails visibly at boot (`refuse to start`, naming the path — §4.3) instead of after the first write. Getting this wrong later means fixing ownership under a running container, which is why it is a decision and not a note.
- **`/healthz` cannot be curl'd** — there is no curl. `nag healthcheck` builds its own URL from `NAG_ADDR`: keep the port, and use the host as given unless it is empty, `0.0.0.0`, or `::`, in which case dial `127.0.0.1`. So `:8080` → `GET http://127.0.0.1:8080/healthz`, and `127.0.0.1:9000` → that host and port. 2-second timeout, exit 0 only on `200`. No second env var, and it stays correct if `NAG_ADDR` is later narrowed to one interface.

  **Nothing acts on the result.** `restart: unless-stopped` does not restart an unhealthy container, and no autoheal sidecar is in the stack, so this is a status you read in `docker ps`, not a self-heal. That is deliberate: what it catches is a database that has become unopenable (§4.3), and a restart loop against a bad disk or a wrong `chown` would replace one visible red word with a log nobody reads.
- **The binary is PID 1**, so it gets no default signal handling. Handle `SIGTERM` and `SIGINT`: `http.Server.Shutdown` with a ~5s timeout, cancel the sweep's context and **wait for the sweep goroutine to return**, then `db.Close()` so WAL checkpoints. Without this, every `docker stop` waits the full 10s and then SIGKILLs mid-write.

  **The sweep runs under a context the signal cancels, and the wait for it is not optional.** A tick can legitimately outlive Docker's 10-second stop grace all on its own — phase 2 sends sequentially at 10 s each and phase 3 carries a 20-second budget (§7.3) — so "stop the ticker, close the database" would call `db.Close()` under a tick that is still writing `delivery_error` and still deleting subscription rows (§7.3), which is a mid-write close arriving from the inside instead of from SIGKILL. Cancelling rather than merely stopping is also what keeps the wait short: **every outbound call in the sweep is bounded by a context derived from the tick's**, so cancellation ends the in-flight send at once and the tick then stops at the next row boundary rather than working through its remaining list. A shoutrrr send is *abandoned* rather than aborted (§7.2) — the wait returns immediately either way, and the orphaned goroutine holds no lock and dies with the process. The wait is therefore bounded at roughly a couple of seconds — one ended send plus one row boundary — and it is bounded *because* no send in this app is ever issued outside a context (§7.1, §7.2, §7.3). `db.Close()` and its WAL checkpoint run after the goroutine has returned, which is the whole reason the order is written down.

### 10.2 Response headers

Set on every response, from one middleware:

```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self';
                         img-src 'self' data:; font-src 'none'; connect-src 'self';
                         object-src 'none'; base-uri 'none'; form-action 'self';
                         frame-ancestors 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
```

Nothing is loaded from another origin (§2, §9.1), so a strict CSP costs nothing and closes the whole injected-script class. `data:` on `img-src` is for the canvas favicon. `font-src 'none'` is not a typo and not laziness: fonts are system stacks (§9.1), locally installed faces are not fetches and CSP never sees them, so *no* font request is legitimate and the directive says exactly that. Note that `style-src 'self'` also forbids a `style` attribute written into the HTML, which §9.7 makes a rule rather than a surprise — while leaving JS style mutation through CSSOM untouched, which §9.7 also says out loud so that the rule is not mistaken for a technical impossibility.

**Caching.** `/api/*` gets `Cache-Control: no-store`. The embedded assets — `index.html`, `app.js`, `app.css`, `sw.js`, `manifest.webmanifest` — get `Cache-Control: no-cache` plus an `ETag` derived from the embedded content, so each load is one cheap revalidation and a deploy takes effect immediately. `max-age` on `app.js` is how the tab pinned for a year (§8.1) ends up running last year's code while `config_version` politely reloads it into the same stale bundle. Icons are content-addressed by nothing, so give them `no-cache` too; at this scale the revalidation is free.

CSRF needs no token: a mutating request must carry `Content-Type: application/json`, which an HTML form cannot produce cross-origin, and the cookie is `SameSite=Lax`. Reject any mutating request that has a body and a non-JSON `Content-Type`.

**Bodyless mutations are exempt**, and that exemption is a design decision, not a hole: `POST /api/push/test`, `/done`, `/undone`, and `DELETE /api/reminders/{id}` take no body, so demanding a header from them would only break the `curl` invocations they exist to serve (§7.5). They are safe for the same reason the rule works at all — a cross-origin form POST cannot carry the `Authorization` header, and `SameSite=Lax` withholds the cookie from it. `/logout` is exempt too; the worst a cross-origin form achieves there is logging you out of your own browser, which you can undo by typing the token.

### 10.3 Backups

The DB is the only copy of everything. **With WAL on, copying `nag.db` alone can yield a corrupt file** — the recent writes live in `-wal`. Either stop the container and copy all three of `data/nag.db{,-wal,-shm}`, or add whatever backup path you prefer around `VACUUM INTO`. Keep `./data` on local disk: WAL misbehaves on network filesystems, so a bind mount pointing at NFS is the one way to break this.

Other gotchas: the container must join the network caddy-docker-proxy watches or the labels are silently ignored.

### 10.4 What gets logged

Logs are the only observability (§2), which cuts both ways: a 45-second poll means a naive access log buries everything that matters.

- **Four things are never logged, in any line, at any level**: `NAG_TOKEN` (or a submitted token, including on a failed `/login`), a `channels.url`, a push endpoint in full — the truncated form from §7.5 instead — and **reminder `text`**. The first three are secrets or capabilities; `text` is left out so that the log stays a thing you can paste into an issue or hand to someone debugging with you, and because every line that might have wanted it works from the id instead. The per-push line names ids and `n`, not what it said; if you need the wording, the id is right there and so is the database.
- **One line per mutating request** (`POST`/`PATCH`/`DELETE`) at INFO: method, path, status, duration, and the affected reminder id — from the path where there is one, from the response on a create. **The push endpoints have no reminder id and must not print what they do have**: `/api/push/subscribe` and `/api/push/unsubscribe` log the truncated endpoint form from §7.5 in that field, never the URL they were handed. `/api/push/test` takes no body at all (§7.5), so it has no single endpoint to truncate: its line carries **how many subscriptions it attempted and how many rows it deleted** — the only two things it knows, and between them the question anybody reads that line for. `/login` and `/logout` log neither an id nor a token (below). That id is what makes an accidental `Clear` recoverable once the 5-second undo toast is gone: there is no history view in v1 (§8.2), so `POST /api/reminders/417/done` in the log is the only surviving record of which id to `POST …/undone`. Worth knowing before you need it.
- **One line per `4xx`/`5xx`**, whatever the method — including every failed `/login`.
- **Successful `GET`s are not logged.** `/api/state` and `/healthz` would otherwise account for essentially the whole file.
- The sweep logs its decisions, not its ticks: rows marked, rows skipped as too late with their ids (§7.3), rows purged, subscriptions deleted, and every delivery failure. Because a notification is now a digest, **one line per push** naming `n`, the ids it covered, and how many subscriptions it went to — that line is the only record of which reminders a given notification was about, and "why did it tell me about six things" is a question you will have. It names ids, never texts (above), which answers that question exactly as well. A tick that marks rows but holds them logs that too — the count held and how many seconds remain in the window — and rows phase 3 dropped at its deadline, with their ids (§7.3).

**Every line in this section is INFO or above, and there is no log level to configure.** `slog`'s text handler defaults to INFO, so a line worth writing at DEBUG is a line nobody will ever see: it would need a `NAG_LOG_LEVEL` that §5.1 does not have, set on a container you would have to restart, to read a line about a sweep that already ran. The held-rows line in particular is the one that turns a 20-minute silence between a due time and a notification into something distinguishable from a broken sweep, which is the first thing you would suspect — so it is INFO, kept to one short line per tick that actually holds something, and a quiet instance stays quiet because the sweep logs decisions rather than ticks.
- At boot, one line each: **build version** (§10), listen address, resolved DB path, resolved config path (and whether it was just written from the default, §5.3), timezone, preset count, `config_version`, subscription count. Those eight lines are the answer to "what is this instance actually running", and the first four are the ones you actually reach for — a version, an address, and the two paths. The VAPID-mismatch WARNING (§7.1) joins them when it applies, which is exactly when you need it to.

---

## 11. Milestones

Every milestone's tests are plain `go test ./...` and the frontend has none — there is no build step to hang a JS test runner off (§2), so the §9.10 case table is verified in Go and checked by hand in the browser. No CI; this is one person and one box.

**M1 — core**
Schema + migrations, config load/validate/defaults including the §5.4 per-kind field rules, preset evaluation with the tests in §6, API §8 including auth, `genkeys`, `config check`, sweep loop logging instead of sending, minimal unstyled UI (capture + two zones + clear). `/api/channels` is in §8 and returns `[]` until M4.
*Done when:* a reminder created via the UI appears in the right zone, and the sweep logs it **within 30 s of its due time**. Also: sweep idempotency has a test (a marked row never fires twice), a backdated create is stamped in **both** `notified_at` and `pushed_at` and never logged as notified (§4.1), and `nag genkeys >> nag.env` produces a file that boots **once `NAG_VAPID_SUBJECT` is edited** — and names exactly that when it isn't (§5.1).

**M2 — notifications**
`manifest.webmanifest`, subscribe/unsubscribe, service worker, real push, **phases 1 and 2 of the sweep** — mark, then the digest push — with the 30-minute cooldown and the digest payload (§7.1, §7.3), constant `Topic` and `tag`, `pushsubscriptionchange`, 404/410 cleanup with a test, `vapid_public` recorded from the client's key plus the mismatch warning, the §9.8 permission flow, `POST /api/push/test`, notification click → focus + flash, badge/favicon/title including the hidden-tab poll and the worker's `state-changed` message (§9.3). Phase 3 and phase 4 are M4's, with the fan-out and the purge they exist for.
*Done when:* laptop Firefox fires an OS notification with the tab closed but the browser running, clicking it focuses and highlights the row, and `curl -X POST -H "Authorization: Bearer $NAG_TOKEN" …/api/push/test` reports a live endpoint. On phone, confirm the platform path from §7.1 before calling it done.
The cooldown needs tests at the sweep level, against a fake clock and a fake sender, and they are the §7.6 table read as a checklist — at minimum: two rows due in one tick produce one send with `n == 2`; a row due 7 minutes into a cooldown is not sent until the window opens and is then sent with the correct `n`; a held row that is cleared or re-timed first is not sent at all; a row 31 minutes late at mark time is stamped and never sent; a 60-row backlog produces `n == 50` and then `n == 10` one window later; a row cleared before its due time and undone after it is sent, while the same row undone 40 minutes late is not, and a row cleared *while held by the cooldown* is not sent whenever it is undone (§4.1); a digest whose every send fails still consumes the window; and `/api/push/test` sends during a cooldown without consuming the window. Every row of that table whose outcome names a channel waits for M4, because phase 3 is where it lives — the checklist spans the two milestones and the fan-out half is written out below.

**M3 — design**
Both themes with the §9.1 tokens and system font stacks, Patina layout, mobile rules, picker per §9.6, inline row edit, markdown-link rendering per §9.10 with the case table on both sides, the §9.11 time strings, channel selector, undo toast, empty state, failure toasts and the quiet not-connected line.
*Done when:* it matches the two reference mockups on desktop and phone, and a reminder containing `[label](https://…)` renders one clickable link in the row, plain text in the notification, and raw markdown in the edit field.

**M4 — ship**
`nag channel` subcommand including `test` and the slug rule, shoutrrr fan-out, `delivery_error` classification and surfacing — with a test that a failure against a URL carrying a token stores neither the URL nor the token — retention purge, SIGHUP reload, security headers, graceful shutdown, `nag healthcheck`, `nag version`, Dockerfile + compose + caddy labels, `nag.env.example`, README including the ownership and backup notes, the caddy-network prerequisite (§5.2), and **one line about the iOS install**: a Home Screen web app has its own storage and cookie partition, separate from Safari's, so the `nag_session` cookie earned in Safari does not travel into the installed app and the token has to be typed once more inside it (§8.1). That is expected rather than a bug, and it is worth a line because the installed app is not optional on iPhone — it is the only path to Web Push there (§7.1), so every iOS user hits it exactly once and would otherwise read it as a broken login.
*Done when:* on the VPS, from an empty checkout, the four setup commands of §5.2 followed by `docker compose up -d` produce an instance that writes a default config, serves over HTTPS, and refuses to start with the keys removed; and `docker compose kill -s HUP nag` reloads an edited preset list without dropping a request.
Phase 3 finishes the §7.6 checklist M2 started, at the same sweep level and against the same fake clock, now with a fake sender standing in for the router: a row 31 minutes late at mark time is **never fanned out** either, not merely never pushed; fan-out fires in the same tick even when that tick's push is held; a phase 3 that exceeds its deadline drops the remaining rows, logs their ids, and does not delay the next tick's push; a phase 3 whose deadline expires *inside* a row carrying many unreachable channels still returns at ~20 s, because every send is bounded by the budget rather than by its own full 10 s — including the shoutrrr sends the wrapper has to abandon to achieve that (§7.2, §7.3); and a tick whose phase 2 spent 30 seconds on unreachable subscriptions still attempts its fan-out, because the deadline is measured from the start of phase 3 (§7.3).
The two config hashes need tests of their own (§5.5), because the failure mode is silent in both directions: a reload that changes only a preset's `offset` **is applied** — the next tap of that chip lands at the new duration — while `config_version` does **not** move; a reload that renames a `label` moves it; a reload of a byte-identical file moves nothing and logs `config unchanged`; and a reload that fails validation leaves both the running config and the counter untouched. Also: a config that is present but invalid at boot exits `1` and does not overwrite the file (§5.3).

Later, explicitly deferred: notification action buttons (Snooze / Clear from the notification itself), bookmarklet / Firefox extension for "remind me about this page" (which now needs no new field — it posts `[title](url)` as the text, percent-encoding any parens in the URL and stripping `[`/`]` from the title, §9.10), Web Share Target for the phone, recurrence, a history view over the retained `done` rows.

---

## 12. Decisions log

| Decision | Rationale |
|---|---|
| TOML, not YAML | Typed decoding in Go, no indentation or implicit-typing footguns, hand-edited by one person |
| shoutrrr in-process, not an Apprise sidecar | Apprise would be the only Python in the stack; shoutrrr is a Go library covering ntfy, Telegram, Matrix, Discord, Gotify, SMTP, generic webhook |
| Closed set of preset kinds | Free-form natural-language parsing is a permanent maintenance cost; chips are valuable *because* they're predictable |
| Poll every 30s, not per-reminder timers | Survives restarts, no rescheduling on edit, trivially debuggable |
| Mark before sending — every lifecycle write lands before any socket opens | A missed notification is visible in the list; a notification storm is not recoverable |
| At most one push per 30 min, carrying everything since the last one | Several reminders coming due around the same time is one thing to be told, not five. The badge and the list are never rate-limited (§9.3), so what the cooldown delays is the nudge and never the truth — which is the only reason a reminder app may rate-limit reminders at all |
| Don't notify for anything over 30 min late | After an outage the list is the recovery mechanism. Same number as the cooldown, deliberately: one rule to remember — nothing older than half an hour pings, and no more than one ping per half hour |
| The too-late gate is checked at mark time, never at push time | Checking it in phase 2 would let the cooldown discard exactly the notifications it exists to batch, since a legitimately held row is by definition up to 30 minutes late by then. The bug would have been silent |
| `pushed_at` is a column; `last_push_at` is memory | The *held set* must survive a restart or the deferral loses notifications, so it is a query on durable columns. The *window* may reset on restart: that costs one extra notification, never a missing one |
| A constant push `Topic` and a constant notification `tag` | `Topic` makes the push service keep at most one pending message per device, which is what bounds the burst a phone gets after a day offline — the old per-reminder topic collapsed nothing, because ids differ by construction. The matching `tag` keeps the tray at one, and it needs `renotify: true` or the replacement is silent |
| The notification's count is not the badge's count | `n` is what this push is about; the badge is everything overdue. They differ constantly and neither is wrong, so the notification never tries to state the total |
| Fan-out is not rate-limited or coalesced | A channel is an addressed output asked for on one specific reminder, usually a different address per reminder — there is nothing coherent to merge. And its value is "tell me on Telegram *now*", which a 30-minute deferral would remove |
| Fan-out runs after the push, under a 20-second whole-phase deadline | `LIMIT 50` bounds the row count and a 10-second client timeout bounds one send, but neither bounds a *tick*: 50 rows × 16 dead channels × 10 s is over two hours in one goroutine, and the digest push sat behind it. Ordering it after the push and capping the phase means an unreachable channel can cost channel messages and can never delay a notification past the next tick. The 20 s runs from a clock read taken when the phase starts, not from the tick's `now`, which sequential sends to sleeping devices can already have left 30 seconds behind — and it bounds every send, each taking the smaller of 10 s and the remainder, because a budget checked only between rows would let one 16-channel row run three minutes past it. shoutrrr's `Send` takes no context, so that bound is a goroutine and a `select` that abandons the send (§7.2); the bound is the app's, not the library's |
| `POST /api/push/test` sends at 3 seconds each under a 12-second budget, not the sweep's 10 | Sequential sends on the sweep's timeout put the handler past the 15-second `WriteTimeout` after two dead endpoints, so the `curl` got no answer at all in exactly the silent-notifications case the endpoint exists for. A probe optimises for answering the caller: a subscription that cannot reply in 3 s reports `timeout`, which is the diagnosis. Each send takes the smaller of 3 s and the budget's remainder, because five at 3 s is the write deadline again — and rows the budget never reached report `not attempted` rather than being omitted, so the array always has one entry per subscription |
| A backdated write sends nothing at all, channels included | Fan-out only covers rows the sweep just marked, and a backdated row arrives marked — so the alternative was a Telegram call on the request path for the one write that by definition is not urgent. Named in §4.1, §7.6 and the picker's own sentence, because "no notification" reads as though ntfy still fires |
| Two config hashes: one for the log, one for `config_version` | `/api/config` exposes presets as `{key,label,quick}`, so a single hash over it is blind to `offset`, `at`, `days`, `weekday`, `timezone` — gating the *swap* on it would discard a real edit and log `config unchanged` while doing it, and gating nothing on it reloads every client whenever HUP fires twice. Two questions, two hashes, one load |
| Rows render keyed by id, and a row with an open editor is never touched | The row editor is a row, and the 45-second poll redraws rows — including on the 304 path built to keep time strings moving. A full replace eats half-typed text, focus, and scroll position every 45 seconds, unattended. §9.2 already protects typing from a config reload; this is the same rule one level down |
| Orphaned channel names get their own greyed chip | Invisible names meant the editor sent a selection that silently omitted them, so every save cleaned out an orphan `channel rm` created on purpose — and §8.3's "carry an orphan forward" was then reachable only from `curl`. A chip you can deselect but not re-select is the whole rule made visible |
| The empty-channel-list suppression belongs to the capture bar only | The row editor renders its channel affordance whenever the catalogue is non-empty *or* the row carries names, because it always sends the full list of what its chips show. Were it suppressed on an empty catalogue as well, `channel rm` on the last channel would make every later save of a row referencing it send `[]` — wiping the orphan the chip above exists to keep, and from the one surface that was supposed to preserve it |
| A past `due_at` is stamped `notified_at` **and** `pushed_at` at write time | Puts the "backdating never pings" promise in the write path instead of in a 1800-second threshold the client would have to know about. Leaves the too-late gate doing only its real job — outage catch-up — keeps the row out of the digest as well as out of phase 1, and makes the picker's sentence true one second past as well as one week |
| Undone stamps `pushed_at` on a row that was marked but never pushed about | "Comes back without re-notifying" was true only of rows a push had reached. A row marked during a cooldown, cleared inside the window, and undone hours later still matched phase 2's held-rows predicate — and phase 2 does not re-check ages, so it re-entered the next digest at any age, walking around the too-late gate via a `Clear` and an `Undo`. One guarded column in the same UPDATE makes the promise literal for every already-fired row; the never-fired row, whose `notified_at` is still NULL, is deliberately left live |
| `LIMIT 50` per tick, no inner loop | A deliberate ceiling on how fast a pathological backlog can turn into notifications |
| Badge counts overdue only | The badge means "something needs you now", not "you own tasks" |
| No per-preset keyboard shortcuts | The input is autofocused, so bare digits can't coexist with typing "in 2 years"; every alternative costs a keystroke, a mode, or a fight with the browser's own `Alt`+digit. `Enter` for the default preset is the only shortcut that pays for itself |
| Chips save on click, not select-then-confirm | Goal 1 is one interaction; a two-step chip is a worse date picker |
| `same_day_ok` explicit per preset | "Next Monday" on a Monday is genuinely ambiguous; make it a config decision, not a hidden convention |
| Hour range is a hard constraint, and picker-only | Chosen over an in-sheet "show all hours" toggle — the constraint is the feature. It bounds manual selection, never what a preset may compute |
| Keep `done` rows for 30 days, `0` = forever | Cheap, enables undo and future history, purged by the same sweep |
| Secrets in env, behaviour in TOML | The TOML lives in a mounted directory and gets edited; keys must not |
| Channels via subcommand, not API or TOML | Their URLs are secrets, and `FROM scratch` leaves no shell to hand-edit the table with |
| Cookie key derived from `NAG_TOKEN` | One secret to manage instead of two, and rotating the token becomes log-out-everywhere for free |
| `Secure` cookie unconditionally | Behind Caddy the app sees plain HTTP; deriving it from `r.TLS` ships an insecure cookie and looks correct |
| Theme follows the OS, with no override | A media query and nothing else — no persistence, no first-paint flash, no per-device drift. The OS already knows |
| No client-side persistence at all | No cookie for preferences, no localStorage; the server or the OS holds every piece of state, so behaviour is identical in every context |
| Server clock for skew, browser for timezone | Correcting a wrong clock is cheap and keeps the badge honest; re-rendering the server's wall clock to someone in another country is worse than showing them their own |
| Edit reuses the capture bar | Rename and re-time are the same two fields as creation; a separate edit dialog would be a second implementation of the same thing |
| No `/snooze` endpoint | Once `snooze_count` was gone it differed from `PATCH` by nothing at all. One handler holds the re-time rules |
| Dropped `source`, `url`, and `snooze_count` | Nothing set or read `source`. `snooze_count` was counted and never displayed. `url` was a whole column and API field for a link that v1 neither showed nor let you type — a markdown link in the text does the job with no schema at all |
| Links live in the text, as markdown | One field instead of two, and the deferred bookmarklet needs no API change. Cost is a tiny parser written twice (§9.10) |
| A bad link scheme is literal text, not a 400 | The renderer must refuse it anyway, and `textContent` refuses again behind that — so the server's copy of the rule bought nothing and made `[note](internal)` and `[doc](obsidian://…)` unenterable. `flattenLinks` stays the server's only contact with text |
| An unencoded `(` in a link target refuses to render | With one non-backtracking regex, a paren-bearing URL truncates at the first `)` — a link that looks right and 404s. Refusing it shows raw markdown instead, which is a failure you can see and fix by percent-encoding |
| 403 with a stale key deletes the subscription | It can never succeed again, so keeping it is a guaranteed failure every 30s and a silent one. Paired with the client's key check, rotation becomes recoverable |
| The client sends the key it subscribed under | Stamping the server's configured key made `vapid_public` a copy of the config, so the boot warning and the 403 branch could never fire on the case they were written for |
| System font stacks, no webfonts | The faces the design is built on are licensed OS faces that cannot be self-hosted; shipping substitutes as woff2 would look *less* like the mockups where the real ones exist. No `web/fonts/`, and `font-src 'none'` |
| Bind mounts, chown'd once, non-root | A scratch image has no shell to fix ownership from inside, so ownership is a host step either way — and as a visible directory it also makes the WAL backup story a `cp` instead of a volume incantation |
| Shutdown cancels the sweep, waits for it, and only then closes the database | A tick can outlive Docker's 10-second grace unaided — sequential 10 s pushes, then phase 3's 20 s budget — so "stop the ticker and close" runs `db.Close()` under a tick still writing `delivery_error` and still deleting subscription rows: a mid-write close arriving from the inside. Cancelling rather than stopping is also what keeps the wait to a couple of seconds, since every send is a child of the tick's context and aborts at once |
| The service worker is registered on every load, and everything push is feature-detected once | `getSubscription()` is how the notification slot decides whether it exists, so registration cannot wait for the gesture that grants permission — and an undetected `navigator.serviceWorker` throws on the load path and takes the list with it. Losing push must cost push and nothing else |
| A clicked notification carries its id in the URL when it opens a window | `postMessage` to a client `openWindow` just returned is a race the worker always loses — the page has no listener yet — and that is the *common* case, since you click notifications that arrived with the tab closed. `/?focus=<id>` plus `history.replaceState` is three lines and cannot race |
| The picker commits on `Set reminder`, not on scroll | A chip is one interaction because it carries its own answer; the picker exists because you are choosing among many, and saving per scroll-snap would `PATCH` once per notch of the minute wheel. The one place a two-step commit is right |
| No log level, so nothing is logged below INFO | A DEBUG line needs an env var §5.1 doesn't have and a restart, to read about a sweep that already ran. The held-rows line is the one that distinguishes a 20-minute cooldown from a broken sweep, so it is INFO |
| No command-line flags on any subcommand | Env for secrets, TOML for behaviour, and a flag would be a third place to look — one that lives in `compose.yaml`'s `command:`, out of sight of both files |
| Two formatters own every time string | Rows, picker, and notification body had three sets of hand-written examples and no rule; thresholds and units are exactly what drifts silently between copies (§9.11) |
| Bodyless mutations skip the JSON `Content-Type` check | The rule exists to stop a cross-origin HTML form, and a form cannot send the `Authorization` header or, under `SameSite=Lax`, the cookie. Demanding the header anyway would only break the `curl` paths those endpoints exist for |
| English copy, locale-formatted clocks | "Follows the browser's locale" was always about how a time is *rendered*, not about translating `late`. Stating it stops the two readings from fighting in every string |
| A failed poll is a quiet line, a failed action is a toast | A 45-second unattended poll that toasts on failure produces a toast every 45 seconds during a blip. The distinction is whether someone is waiting for the answer |
| Hidden tabs still poll, at 5 min, and the worker pings them | Goal 4 is a favicon read from *another* tab, and that tab is `hidden` by definition — "don't poll a hidden tab" would have refreshed the count only when you were already looking at it |
| No `last_ok_at` or `user_agent` on subscriptions | Nothing could read them: `FROM scratch` has no `sqlite3` (§7.4) and no endpoint or subcommand exposes a subscription. A column written every 30s and read by nobody is not observability, it just looks like it |
| `flattenLinks` shares the renderer's eligibility gate | Defined as "replace every match with its label" it disagreed with the renderer on exactly the cases §9.10's table calls out — `mailto:` flattening to a bare label, `javascript:` to `a)` |
| The placeholder VAPID subject is a boot error | It is the one required value `genkeys` cannot generate, push services use it to reach the operator, and a placeholder that boots is a placeholder that is still there in a year |
| Editing opens on the stored moment only when it is still in the future, and with the minute wheel at `00` | The stored minute usually isn't on a `minute_step` boundary, so it cannot be shown. Snapping to the nearest step is a silent few-minute move dressed as the value you saved; a visible reset isn't. And a row being re-timed is usually overdue, so the stored date would have opened the sheet on the in-the-past warning — the greeting the create-mode default was written to avoid. One rule for both modes instead, tested on the value the sheet would show, since the minute reset can itself land a future row in the past |
| Subcommands migrate the database too | One `store.Open`, so `nag channel add` before the first `up -d` is a supported order instead of a missing file or an unmigrated one for `serve` to repair |
| `nag.env.example` is comments only | A template with empty values leaves two of every key after `genkeys >>`, and the one required hand edit then has two candidate lines — of which the wrong one silently does nothing |
| An already-attached unknown channel name survives a `PATCH` | `channel rm` creates orphans on purpose, and they're invisible in the editor. The strict rule made every reminder referencing a removed channel un-editable via a 400 on a value you can't see. You may carry an orphan forward, never introduce one |
| CSP does not govern CSSOM, and the spec says so | "No inline styles because `style-src` blocks them" is half true and leads to prescribing custom properties set through the very API it claims is blocked. The markup rule is real; the JS rule is a preference, and naming which is which stops a future reader from working around nothing |
| The year appears only when it isn't the current one | The picker can be paged into another year, so "no year anywhere" made a row 14 months out render identically to one 2 months out. A year-aware format costs one condition; a year stepper would be a feature for reminders I don't want |
| `delivery_error` holds a classification, never the library's error text | shoutrrr's errors embed the URL it was handed, and this column is returned by the API, drawn into a row, and logged — so a passed-through string would break the URL-masking rule in three places at once, with no single place to redact. `http 401` and `timeout` are what you act on anyway |
| `config_version` moves only when the resolved config differs | Bumping on every successful load meant a reverted edit, or a HUP sent twice, reloaded every open client to hand it the config it already had — and `config check` then HUP is the documented workflow that signals twice |
| The cooldown window is consumed even when the push fails or there is nobody to push to | `pushed_at` is already committed, so the rows can never be re-selected; a window held open on failure would have nothing left to carry and would only re-run an empty select every 30 s. "One attempt per half hour" stays literally true |
| Client time is Unix seconds, with one `now()` | `server_time` is seconds and `Date.now()` is milliseconds; a single subtraction across that boundary renders every row as tens of thousands of days late. Two call sites need ms (the due timer, the picker) and convert there |
| The due timer's delay is clamped to ~23 days | `setTimeout` truncates to int32, so a reminder six months out fires the timer immediately, which polls, re-arms from the same row, and fires again — a tight loop for as long as the tab is open. The clamp costs one extra poll every few weeks |
| Channel names are lowercase slugs | The name is a chip label, a CLI argument, and a JSON array element, and SQLite's `UNIQUE` on TEXT is case-sensitive — so `ntfy` and `NTFY` would be two rows rendering two identical chips. One rule at the door beats `COLLATE NOCASE` plus a matching fold on every send-time lookup |
| No secrets and no reminder text in the log | Tokens, channel URLs and full push endpoints are credentials or capabilities; `text` is omitted so the log stays safe to paste into an issue. Every line that could have wanted the text works from the id, and the id is what makes an accidental `Clear` recoverable |
