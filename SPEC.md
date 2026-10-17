# Nag — implementation spec

A personal, self-hosted reminder app. It reproduces the Slack "remind me about this" workflow:
capture a note, pick a preset delay, get an OS notification. The note then stays in a list until
you clear it manually.

Single user. Single instance. No multi-tenancy, no accounts, no sync protocol.

---

## 1. Goals

1. **Capture in one interaction** — type text, tap a preset, done. No date picker in the common path.
2. **Notify at the due time** through Firefox/Chrome as a real OS notification, on laptop and phone.
3. **Persist until dismissed** — after an item fires, it stays in an "overdue" list until you clear it explicitly. This is the core behavior, and it is the reason why existing tools do not fit.
4. **Glanceable from a pinned tab** — the favicon shows the count of items that need attention.
5. **Optional fan-out** to ntfy / IM, per reminder.

### Non-goals (v1)

- Recurring reminders (no rrule). The design leaves room for them, but do not build them.
- Natural-language date parsing. Presets and an explicit picker only.
- Multi-user, sharing, teams.
- Native mobile app or mobile push beyond standard Web Push.
- Full-text search, tags, projects, priorities.
- Keyboard shortcuts for individual presets (see §9.4 and §11).
- Deferred to a later version: notification action buttons (Snooze and Clear from the notification itself), a bookmarklet or Firefox extension for "remind me about this page", Web Share Target on the phone, recurrence, and a history view of the retained cleared rows. The bookmarklet needs no new field: it posts `[title](url)` as the text, with any parentheses in the URL percent-encoded and `[`/`]` removed from the title (§9.10).

---

## 2. Stack

| Concern | Choice |
|---|---|
| Language | Go (single static binary, `CGO_ENABLED=0`) |
| Storage | SQLite, WAL |
| SQLite driver | `modernc.org/sqlite` (pure Go, no cgo) |
| Config | TOML via `pelletier/go-toml/v2` |
| Web Push | `github.com/SherClockHolmes/webpush-go` |
| Fan-out | shoutrrr — **nickfedor's maintained fork**. Upstream `containrrr/shoutrrr` gets few updates now. Before you pin the fork, check its exact module path on pkg.go.dev. That check covers the **send API** as well as the path: `router.Send(message, params) []error` takes no context, and for this reason §7.2 wraps it |
| Timezones | `import _ "time/tzdata"` — required, because the scratch image has no tzdata |
| Logging | `log/slog`, text handler to stderr. Logs are the only observability. §10.4 says which events get a line |
| Frontend | No build step. Hand-written HTML/CSS/JS, embedded with `//go:embed`. The server sends every file as-is: no templating, no server-side rendering |
| Fonts | **System stacks only, no webfonts** — nothing is downloaded, nothing is embedded (§9.1) |
| Router | stdlib `net/http.ServeMux` (Go 1.22+ pattern matching) |
| Reverse proxy | Any HTTPS proxy on the host, in front of `127.0.0.1:8080`. caddy-docker-proxy labels are in an opt-in Compose file (§10) |

HTTPS is mandatory. Service workers and Web Push do not work over plain HTTP.

**Tests are plain `go test ./...`, and the frontend has none.** There is no build step to attach a JS test runner to, so Go tests cover the §9.10 case table, and a person checks the frontend by hand in the browser. No CI runs the tests, because the project has one person and one box.

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
compose.caddy-docker-proxy.yaml   # opt-in add-on — see §10
.github/workflows/release.yml     # tag → GHCR image — see §10
README.md
```

**`//go:embed` patterns cannot contain `..`**, so each embedded tree lives inside the package
that reads it. `nag.default.toml` sits next to the loader in `internal/config/`. (It is still
*written out* to `NAG_CONFIG` on first boot, §5.3.) `web/` has its own one-line `embed.go` that
declares `package web`. Neither tree can live at the repo root and be embedded from a
subpackage. If you try, the build stops with a compile error, not only a lint warning.

**There are no command-line flags anywhere**, on any subcommand. Each value that varies is an
environment variable (§5.1) or a TOML key (§5.3). A flag adds a third place to look for the same
answer. Also, `compose.yaml` must then carry the flag in `command:`, where neither the env file
nor the config file shows it. `main` dispatches on `os.Args[1]`. If the subcommand is
**missing**, is an unknown word, or is `-h`, `main` prints the usage block below and exits `2`.
A call with no argument at all gets this same unknown-word treatment, never an index panic.
`main` has no default subcommand, because the default lives in the Dockerfile's
`CMD ["serve"]` (§10) and nowhere else.

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

- **Schedule and lifecycle are independent.** `due_at` is when to notify. `done_at` tells whether the item still needs your action.
- **Overdue** = `done_at IS NULL AND due_at <= now`.
- **Later** = `done_at IS NULL AND due_at > now`.
- **`notified_at` and `pushed_at` answer two different questions, and both exist solely for the sweep.** `notified_at` means "the sweep has handled this row". It makes phase 1 of §7.3 idempotent, and the sweep sets it whether or not it sent anything. `pushed_at` means "a Web Push notification carried this row". That is a later, separate event, because pushes are rate-limited to one per 30 minutes (§7.3). Because `pushed_at` is a column, the set of reminders *held* by that cooldown is a query, not in-memory state: `notified_at IS NOT NULL AND pushed_at IS NULL AND done_at IS NULL`. As a result, a restart during a cooldown cannot lose a notification. Neither column has a meaning for the client. `notified_at` is in the reminder object so that a `curl` user can see it, and nothing in `app.js` reads it. `pushed_at` is **not returned at all** (§8.2). It is bookkeeping with a 30-minute lifetime, and a field that the UI must ignore is better not sent.
- **Re-time** = set the new `due_at`, `notified_at = NULL`, `pushed_at = NULL`, clear `delivery_error`, and `done_at = NULL` if it was set. One UPDATE. It is identical before firing, after firing, and on a row that is already cleared. The data model and the API have no separate snooze operation. A chip on a row and an edit are the same UPDATE (§8.4).
- **A write whose `due_at` is in the past never notifies.** On create *and* on re-time, if the resulting `due_at <= now`, the same statement sets **both `notified_at = now` and `pushed_at = now`** instead of `NULL`. The row appears in "Needs you now" immediately, and phase 1 of the sweep never picks it up. Because `pushed_at` is already stamped, the row can never become a candidate for a digest either. This rule makes the picker's promise (§9.6) true in all cases, not only for times more than 30 minutes in the past. It also leaves §7.3's too-late gate with the one job that it was designed for: to catch up after an outage. Also, a time a few seconds in the past (clock rounding, a slow tap) goes straight into the list that you already see. The app does not notify you about it thirty seconds later.
- **A backdated write sends nothing at all, including to its channels.** Only the sweep does fan-out, and the sweep fans out only the rows that it just marked (§7.3). As a result, the sweep sends nothing for a row that arrives already marked: no push, no ntfy, no Telegram. This is the intent, not a side effect. A past `due_at` is a bookkeeping entry. It is the one kind of write whose whole purpose is to appear in a list, not to interrupt anybody. If a handler opens a socket to Telegram before it returns the 201, it puts an unbounded third-party timeout on the request path. It does so for the one case that, by definition, is not urgent. `extra_channels` is still stored, so the row keeps its channels. A later re-time to a future time fans out normally. §9.6's picker sentence and the §7.6 table both state this explicitly. Without these explicit statements, a reader can take "with no notification" to mean that only the OS notification is suppressed.
- **Re-timing with an `offset` preset resolves from `now`, never from the old `due_at`.** "30 min" on an item three days overdue means thirty minutes from now. `clock` and `weekday` presets are absolute anyway, so this rule matters only for offsets.
- **Clear** = set `done_at = now`. Keep the row.
- **Undone** = set `done_at = NULL`, and in the same statement **`pushed_at = now` when `notified_at IS NOT NULL AND pushed_at IS NULL`**. Both changes are one UPDATE. The guard has one purpose: it makes "a row that already fired comes back without a new notification" true of every such row, not only of the rows that a push reached. For a row that a push already carried, both columns were set and stay set. The row returns to the overdue list silently, and you decide when, or whether, to re-time it. A row marked *during* a cooldown still had `pushed_at` NULL, so it was in the held set (§7.3). Clearing it removed it from the digest via `done_at IS NULL`. Without the guard, un-clearing it puts it straight back in the digest. This happens because phase 2 selects on `notified_at IS NOT NULL AND pushed_at IS NULL AND done_at IS NULL`, and its only age check is the lateness bound of §7.3. So a `Clear` followed by an `Undo` bypasses the too-late gate, on a row that the sweep already handled. To prevent this, undone stamps the column that the cooldown did not reach yet. After that, only an explicit re-time can produce a new notification. Re-time is the one operation that sets both columns to NULL again. Undone on a row that is not done is a successful no-op. **A row cleared *before* it ever fired is the other case**, and the guard leaves it alone. A done row is invisible to phase 1 (§7.3), so its `notified_at` is still NULL and nothing is stamped. If you un-clear it after its due time, the sweep gets a live candidate. That candidate notifies if it is inside the too-late gate. This is the intent: you un-cleared it because you wanted it back. The §7.6 table shows both cases.
- **`delivery_error` never holds remote text.** The server builds it from a closed set of classifications, one entry per channel that failed, joined with `; `: `<name>: timeout`, `<name>: refused`, `<name>: dns`, `<name>: http <code>`, `<name>: send failed` for anything else. It holds at most three entries, then `; +N more`. This caps the column at a couple of hundred bytes. **A send that phase 3's expiring deadline aborts mid-flight is recorded in that set as `<name>: timeout`** (§7.3). The send was attempted and did not complete, which is what the word means here. So the fan-out budget adds no sixth shape. A channel that the budget never reached records nothing, because this column describes only attempts. The reason for the closed set is the URL-masking rule (the `channels.url` bullet below), applied in three places. shoutrrr's own error strings often embed the URL that it was given. This column is returned raw in the reminder object (§8.2), rendered into a row (§9.2), and logged (§10.4). So if the column held the raw error text, that text defeats the masking rule in each of those places, and no single place exists to redact it. The classification is what you act on anyway: `http 401` is a bad token, and `timeout` is a host that is down.
- **The classification is read from the error's type, and never from its text.** `errors.As` on the returned error chain is the only way to choose a shape: `net.Error` for `timeout`, `*net.OpError` / `syscall.ECONNREFUSED` for `refused`, `*net.DNSError` for `dns`, and a status carried as a value on a typed error for `http <code>`. **Anything that cannot be typed is `send failed`.** `send failed` is a normal result, not a rare fallback. shoutrrr's services often convert their failures into plain strings before they return them (`fmt.Errorf("...: %v", err)`). So the typed chain frequently does not survive the library at all, and then `send failed` is the honest answer. In particular, `http <code>` appears **only** when the library returns a status as a value. A code that exists only inside an error message does not count. **Never parse remote text or an error string to recover a classification.** A substring match puts the library's own message, URL included, back on the code path. This column exists to keep that message off the code path. A classification inferred that way is an indirect form of the same masking hazard. Such an error is `send failed`, and the log line and `nag channel list` give the other details.
- `delivery_error` is cleared on any re-time, on any later successful fan-out, and **whenever `extra_channels` changes at all**, not only when it is emptied. Any `PATCH` whose resulting list differs from the stored one clears the column in the same statement. This applies whether the `PATCH` removes the last channel, swaps one for another, or adds a fourth. **`extra_channels` is stored canonicalized: de-duplicated and sorted by name (§8.3). Thus "differs" is a plain ordered comparison of the two arrays, byte for byte, and needs no set logic.** Without the canonical form, the comparison has no honest answer. The editor renders its chips in `/api/channels`'s `ORDER BY name` (§8.2, §9.2). An array stored as received holds the names in the order that they were added. In that case, a re-save of a row whose channels did not change compares unequal and clears the column on a no-op. (The comparison matters because the row editor *always* sends `extra_channels`, orphans included, §9.2. Thus "the key was present" is a different question from "the list changed", and only the second one clears.) One reason covers all of these cases. The string names channels: every shape in the bullet above is `<name>: …`. The marker that carries the string is drawn from the row's channel list (§9.2). So when the list no longer contains the named channel, the error is both meaningless and unreachable. If the column is not cleared, the row shows an amber marker that explains a failure for a name that is no longer on the row. If the list is empty, no marker exists at all to show the error. A clear on *any* change, not only on an emptying, is also the rule with the least to remember. "The channels changed, so the verdict of the last attempt is stale" needs no reasoning about which channel failed, or whether that channel survived the edit. The next tick that fans out writes a current verdict anyway. The column only ever describes the most recent attempt. A reminder with three channels where one fails carries that one entry. A later attempt where a different channel fails replaces the whole string. Entries do not accumulate. **One consequence needs to be explicit: a `delivery_error` that names a channel that `nag channel rm` later removed persists until the row is re-timed or its channel list is edited.** Nothing is ever attempted for an orphan (§7.2), so no later attempt clears the column. This is consistent with the rule that the column describes the most recent attempt, not a gap in that rule: the string describes the most recent attempt, and that attempt did fail. The orphan's own chip is still on the row (§9.2). If you remove that chip, the same `PATCH` clears the column, by the rule at the top of this bullet.
- **`channels.url` never appears in an API response in any form**: not masked, and not as a boolean that says a URL exists. `GET /api/channels` returns `{name, enabled}` and nothing more (§8.2). The only reader of the URL is `nag channel list`, which masks it (§7.4).
- `extra_channels` holds names, not ids, and has no foreign key. Thus a channel rename orphans the reminders that point at it. `nag channel rm` warns when reminders still reference the name, and there is deliberately no rename command. To change the URL of a channel, delete the channel and add it again under the same name.

### 4.2 Migrations

`PRAGMA user_version` plus an ordered `[]string` of DDL statements in `internal/store`. No migration library. Each migration runs in a transaction. Bump `user_version` in the same transaction.

If `user_version` is **higher** than the number of known migrations, **refuse to start** and name both numbers. That case is a rollback onto a newer database. The alternative is an old binary that quietly writes against a schema that it does not understand.

### 4.3 Connection

```go
dsn := "file:" + path +
    "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
db.SetMaxOpenConns(1)
db.SetMaxIdleConns(1)
db.SetConnMaxLifetime(0)
```

`MaxOpenConns(1)` is deliberate. It eliminates `SQLITE_BUSY` **within the process** and costs nothing at this scale. It does not cover a second process on the same file, which is a real case here. `nag channel …` runs against the same `NAG_DB` while the serving container holds the write lock, and `busy_timeout(5000)` covers that case (§7.4). The other two settings matter *because* of the first one. Without them, the pool can close and reopen the single connection between statements. That throws away the WAL read state and the per-connection pragmas for no benefit.

`synchronous(NORMAL)` is pinned, not left to the driver. It is the standard pairing with WAL: a commit is durable against a process crash, and only a host power loss can cost the last few transactions. The alternative is a full `fsync` per commit, on the write path of a sweep that runs every 30 seconds. It is pinned so that nobody has to look up "the driver's default" for this file in a year.

No `foreign_keys` pragma, because the schema deliberately has no foreign keys. `extra_channels` holds names on purpose (§4.1).

If the file cannot be opened or the migrations cannot run, **refuse to start** and name the path. A reminder app that starts without its database is worse than one that does not start.

### 4.4 Retention

The sweep also purges: `DELETE FROM reminders WHERE done_at IS NOT NULL AND done_at < now - retention_days*86400`. Default 30 days. **`retention_days = 0` means keep cleared reminders forever**: skip the DELETE entirely.

---

## 5. Configuration

Two separate concerns, deliberately:

- **Secrets → environment** (VAPID keys, auth token). Never in the TOML.
- **Behavior → TOML** in a mounted directory, hand-edited.

### 5.1 Environment

| Var | Required | Notes |
|---|---|---|
| `NAG_DB` | no | default `/data/nag.db` |
| `NAG_CONFIG` | no | default `/config/nag.toml` |
| `NAG_ADDR` | no | default `:8080` |
| `NAG_TOKEN` | **yes** | bearer token, exchanged for a signed cookie at `/login` (§8.1). Also the root of the cookie signing key. There is deliberately no second secret. **Minimum 20 characters**, else refuse to start |
| `NAG_VAPID_PUBLIC` | **yes** | must be **unpadded base64url that decodes to exactly 65 bytes**: an uncompressed P-256 point. §8.2 applies the same shape check to a client-supplied `application_server_key` |
| `NAG_VAPID_PRIVATE` | **yes** | must be **unpadded base64url that decodes to exactly 32 bytes**: a P-256 scalar |
| `NAG_VAPID_SUBJECT` | **yes** | must parse as an absolute URL with scheme `mailto:` or `https:` (RFC 8292), and **must not be the literal `mailto:you@example.com`** that `genkeys` emits |

If any required var is missing or fails its constraint, **refuse to start**. `NAG_TOKEN` has a minimum length because it is both the bearer credential and the sole input to the cookie key (§8.1). A six-character hand-typed token is a six-character MAC key, and that failure is silent. `genkeys` emits 43 characters, so the minimum catches only a hand-edited file.

**The two VAPID keys are shape-checked at boot, not merely required to be non-empty.** The check is the same one that the server already applies to the `application_server_key` that a browser posts (§8.2). A key must be unpadded base64url that decodes to 65 bytes for the public key and 32 for the private one. If a key fails, refuse to start, and name the variable, the expected decoded length, and what was found. Without the check, a mangled paste boots without complaint: a truncated line, a stray space, standard base64 instead of base64url, or a `\r` from §5.2. The bad key then shows only when a reminder comes due, as a push failure on every sweep. That failure is a log line, on the one path whose failures are hardest to attribute. With the check, the same string is either well-formed at boot or a startup error that names the variable.

`NAG_VAPID_SUBJECT` rejects the placeholder on purpose. It is the one required value that `genkeys` cannot produce for you. Push services use the subject to reach the operator when a subscription misbehaves. If the app boots with the placeholder, the placeholder goes into production and stays there. This check makes the §5.2 edit a required setup step, not advice. The error names the var, says that it found the placeholder, and tells you to put your own address there.

The error must print the fix:

```
FATAL: no VAPID keypair configured.
  Generate one:  docker compose run --rm -T nag genkeys >> nag.env
  Then restart.
```

**Only `nag serve` enforces that table.** `genkeys` and `version` require nothing. `genkeys` exists precisely to produce those values. `channel` needs only `NAG_DB`. `healthcheck` needs only `NAG_ADDR`. `config check` needs only `NAG_CONFIG`. If a subcommand refuses to run without the keys that it exists to generate, nobody can ever generate the keys.

**Every subcommand that opens the database creates and migrates it**, exactly as `serve` does (§4.2, §4.3). There is one `store.Open`, not two. Thus `nag channel add ntfy '…'` works on a box that never ran `serve`, and leaves a migrated database behind. It does not fail on a missing file, and it does not write an unmigrated file that `serve` then has to repair. The `chown` from §10.1 lets that write succeed as uid 65534, whichever command creates the file first. For this reason, the `chown` is step two of four and not a footnote.

### 5.2 `nag genkeys` subcommand

`genkeys` emits a **complete, pipeable env file** on stdout and nothing else: no banner, no
prompts. So the documented command is `docker compose run --rm -T nag genkeys >> nag.env`:

```
NAG_TOKEN=<32 random bytes, base64url>
NAG_VAPID_PUBLIC=<from webpush.GenerateVAPIDKeys()>
NAG_VAPID_PRIVATE=<from webpush.GenerateVAPIDKeys()>
NAG_VAPID_SUBJECT=mailto:you@example.com
```

`NAG_VAPID_SUBJECT` is emitted with that literal placeholder, so the file needs one edit to
work, not three. That edit is **required**, not suggested. The placeholder itself is a boot
error (§5.1), so `up -d` fails until you replace it with your address. `genkeys` says exactly
that on stderr. Anything advisory goes to **stderr**, so you still see it when stdout is
redirected, and it does not corrupt the output.

**The env file has to exist before that command runs.** `compose.yaml` declares `env_file: ./nag.env`, and Compose stops with an error on a missing env file. This includes `run`, so the very command that creates the file cannot start. With the ownership step from §10.1, the README opens with exactly four commands:

```
mkdir -p data config
sudo chown -R 65534:65534 data config
cp nag.env.example nag.env
docker compose run --rm -T nag genkeys >> nag.env
```

**`-T` is not optional on that fourth line.** Older Compose versions allocate a TTY for `run` even when stdout is redirected. A TTY translates every `\n` into `\r\n`. Thus each appended line lands in `nag.env` with a trailing `\r`, and Compose reads that `\r` as part of the value. A carriage return inside `NAG_VAPID_PRIVATE` is the worst case. §5.1's shape check exists to catch it. Without that check, the key fails much later and much less clearly: as a signing error that names a JWT, about a key that looks correct in every editor. `-T` disables the TTY. Compose then passes the output through unchanged, and the file is exactly what `genkeys` printed.

`genkeys` touches neither directory, so the order of the two pairs of commands (the two for the directories and the two for the env file) does not matter. But `up -d` needs all four, then the one hand edit: replace `mailto:you@example.com` in `nag.env`. That is four commands and one edit, and if you skip the edit, the boot error names it.

**When you use the caddy-docker-proxy add-on (§10), every `docker compose` command needs the `caddy` network to already exist.** This applies to the `genkeys` line above, `config check` (§5.5), `channel add` (§7.4), and `up -d` alike. The add-on declares that network as `external: true`. If the external network is missing, `docker compose run` stops with an error and does not create the network. Thus, on a box that does not already run caddy-docker-proxy, the fourth command fails before it can write a byte. Start the proxy first. If you set up Nag before the proxy, run `docker network create caddy`. This network is the one prerequisite that is not one of the four commands, and that is exactly why a README can easily leave it out. The base `compose.yaml` declares no external network. Without the add-on, the four commands need nothing beyond Docker.

**`nag.env.example` is comments only: every line starts with `#`, and it declares no variables at all.** You copy it, and then `genkeys` appends to it. If the template carries `NAG_TOKEN=` with an empty value, the finished file holds *two* of every key: the empty one from the template and the real one from `genkeys`. Compose takes the last occurrence, so this happens to work. But the one hand edit that §5.1 makes mandatory then has two candidate lines, and an edit to the wrong one silently does nothing. A commented template cannot cause this problem. The template documents each variable, its default, and which variables are required. `NAG_DB` and `NAG_ADDR` are described in it too, and the "required" information is what tells them apart from the four mandatory ones. `genkeys` appends the four real lines below it.

(The alternative is `env_file: [{path: ./nag.env, required: false}]`. It removes the documented `cp` step, but in exchange the app can boot silently with no keys. So prefer the `cp`.)

### 5.3 TOML

Embed `internal/config/nag.default.toml` with `//go:embed` (§3 explains why it lives in the package). On boot, if `NAG_CONFIG` does not exist, **write the default file to that path** and log it. This gives working defaults plus a self-documenting file to edit. If the directory is missing or unwritable, **refuse to start** and name the path. Never fall back to running without a config, because the resulting behavior is invisible.

**A config that exists but cannot be read or does not validate also makes Nag refuse to start.** The error names the path and the located error from §5.5. Only the *reload* path keeps the old config on a bad file, because there it has an old config to keep. At boot, no old config exists, and the only two options are to run on the shipped defaults or to exit. If a typo in a file makes Nag silently use the shipped presets instead of yours, that is the same invisible-behavior failure as running without a config. So exit `1`. The default file is written **only** when the path is genuinely absent, never over a file that is present and broken.

```toml
[general]
timezone       = "Europe/Paris"
default_preset = "tomorrow"     # used by a bare Enter in the capture bar
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

There is no theme setting (see §9.1).

### 5.4 Rules

- `key` identifies the preset. `label` is for display only. A label rename must not break stored history.
- **File order is UI order.** No `order` field.
- The config **replaces** the preset list wholesale. Never merge arrays with defaults.
- `kind` is a closed set: `offset` | `clock` | `weekday`. Reject anything else. Do not add free-form parsing.
- **Each kind has an exact field set, and a field that belongs to another kind is an error, not something to ignore.** A `weekday` preset with `offset =` is a config that the author misunderstood. If the loader accepts and ignores that field, the chip silently does something other than what the file says. **A key that belongs to no kind at all (a plain typo) is a more general case of the same error, and §5.5 rejects it on the same grounds.**

  | `kind` | required | optional | rejected |
  |---|---|---|---|
  | `offset` | `offset` (a positive `time.ParseDuration` string) | — | `at`, `days`, `weekday`, `same_day_ok` |
  | `clock` | `at` | `days` (integer `>= 0`, default `0`) | `offset`, `weekday`, `same_day_ok` |
  | `weekday` | `weekday`, `at` | `same_day_ok` (default `false`) | `offset`, `days` |

  `key`, `label`, and `quick` (default `false`) apply to every kind. `key` and `label` must both be non-empty.
- The "Pick a time" entry is **not** a preset. The client renders it after the configured presets.
- `[picker]` constrains **only the picker sheet**. A preset with `at = "07:00"` under `hour_min = 8` is valid, and validation must not reject it. The hour range is a UX constraint on manual selection, not a rule about when reminders can fire.

### 5.5 Validation (at boot, and on reload)

Fail loudly on any of these:

- duplicate `key`
- empty `key` or `label`
- `default_preset` not matching a key
- empty preset list
- a field outside its kind's set per the table in §5.4
- a missing required field for the kind
- unparseable `offset`
- **`offset` zero or negative** (`-30m` parses cleanly and schedules into the past on every tap)
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
- **an unknown key or an unknown section anywhere in the file**, named in the error

That last check needs **decoder strict mode**: go-toml/v2's `Decoder.DisallowUnknownFields`, or the equivalent in the pinned version. By default the decoder ignores what it does not recognize. So `quik = true` or a `[pickr]` section parses cleanly and then does nothing at all. That is exactly the silent-misconfiguration failure that §5.4's cross-kind-field rule was written against, at the level of the whole file. If a file states something that the app never reads, its author is wrong about what the app does. The chip that they configure then behaves as if the line is not there. §8.3 rejects unknown JSON fields for the same reason. This is the same rule, applied to the one file that a person edits by hand and reloads with SIGHUP.

Every message names the offending preset `key` (or the `[section].field`) and the value that it rejected. One person hand-edits this file under SIGHUP. For that person, "invalid config" without a location costs more than it saves.

On **SIGHUP**, re-read and re-validate. If the file is invalid, **log the error and keep the old config**. Never take a running instance down over a bad edit. `docker compose kill -s HUP nag` is the reload path.

A file that is now **missing or unreadable** is the same case: log and keep. The default is written on boot only (§5.3). If a reload recreated it, a mistyped `mv` then silently replaces your presets with the shipped ones. That result is worse than to keep the config that is already in memory.

**A reload's outcome exists only in the log.** The UI never learns about it, and `config_version` deliberately does not move on failure. So in the UI, a rejected edit looks the same as a signal that never arrived. `nag config check` exists for this case. It loads and validates `NAG_CONFIG` through the same code path as boot. It prints either the resolved preset list in file order or the same located error message, and exits `0` or `1`. If you run `docker compose run --rm nag config check` before the HUP, a bad edit costs nothing. The check needs `NAG_CONFIG` and nothing else (§5.1). It never writes the default file: an absent config is an error here, not something to fix silently.

Changing `general.timezone` does **not** rewrite existing `due_at` values. They are absolute UTC instants and stay exactly where they are. Only future preset evaluation moves.

A client that loaded before a reload holds a stale `/api/config`. `/api/config` returns a `config_version`. `/api/state` returns the same value, and the client reloads the page when it changes.

**A successful load always replaces the config in memory. `config_version` moves only when the part that the client can see actually differs.** These are two separate decisions, and they need **two hashes**, computed over the same resolved config:

- The **full hash** covers every resolved value: `[general]`, `[picker]`, and every field of every preset in file order, `kind`, `offset`, `at`, `days`, `weekday`, `same_day_ok` included. It decides only what gets logged. If it is identical, the log gets one `config unchanged` line. If it differs, the log gets one line that names the reload. It never decides whether the new config replaces the old one. The newly loaded config is installed either way, so "reloaded and nothing happened" is not a state that this can produce.
- The **client hash** covers exactly what `/api/config` returns: `default_preset`, `vapid_public`, `picker`, and the presets as `{key,label,quick}` in file order. `config_version` increments when and only when *this* hash changes, because it exists to answer "must every open page reload" and nothing else.

A mistake here is not cosmetic, and one hash cannot do both jobs. `/api/config` deliberately does not expose `kind`, `offset`, `at`, `days`, `weekday`, `same_day_ok`, `general.timezone`, or `retention_days` (§6, §8.2). Thus a single hash over the client-visible projection is **blind to every edit that changes what a chip actually does**. A change of `offset = "30m"` to `"45m"`, or of the timezone that the whole schedule resolves in, leaves `{key,label,quick}` byte-identical. A counter that depends on that hash does not move, which is correct. But if the *swap* depends on that hash, the swap discards the edit and logs `config unchanged`. The other direction fails too. If `config_version` increments on every successful load, a reverted edit or a HUP sent twice reloads every open client. Each client reloads only to get the config that it already had.

With the failure rule above, `config_version` means **"the config the client can see has changed this many times"**. It does not count the signals that arrived, the loads that parsed, or the loads that changed something.

**The config is published to its readers with one `atomic.Pointer[Config]`.** The HTTP handlers (`/api/config`, preset resolution on create and `PATCH`) and the sweep goroutine (`retention_days`) read it while the signal handler replaces it. So a bare struct field that the signal handler replaces is a data race, and `go test -race` reports it. Load the pointer **once** per request and once per tick, and use that snapshot throughout. This is the same discipline as reading `now` once (§7.3, §8.2), for the same reason. Without a snapshot, a request can resolve a preset from one config and report `config_version` from the next. Then the response that already applied the change tells the client to reload. A HUP that fails validation never stores a pointer. So "keep the old config" is a single skipped assignment, not a rollback.

The counter lives in memory and restarts at 1, so it can move **backward** across a restart. The client compares with `!=`, never `>`. Any difference means reload.

---

## 6. Preset evaluation

All arithmetic happens in `general.timezone`, and then the result converts to UTC. Build times with `time.Date(y, m, d, hh, mm, 0, 0, loc)`, so that the stdlib handles DST transitions. Presets always resolve in the server's timezone, also when you travel.

**The server schedules. The browser displays.** Every stored instant is UTC seconds, and the backend treats it as nothing else. Presets resolve in `general.timezone`. The client formats every instant in the **browser's** timezone and locale. The picker interprets the wall-clock time that you choose as browser-local (§9.6). In the normal case (laptop and server in the same zone), these agree. If you pick 09:00 from a phone two zones away, you get 09:00 where you are. The `tomorrow` chip gives you 09:00 where the server is. **That divergence is accepted.** The alternative shows foreign wall-clock times to someone who looks at their own watch, and that is a worse trade for a single-user app. Clock *skew* is still corrected against the server's clock (§9.3). A wrong clock on a phone is a fault that the app corrects. A phone in another country is not a fault.

**A preset's `label` is a hand-written config string and is not adjusted for any of this.** From a phone two zones east of the server, the chip reads "Tomorrow 9:00" and the row that it creates reads "tomorrow at 10:00". This is accepted, for the same reason and one more. The label describes the server-side rule, and the row shows the time on your watch. To compute labels client-side, the server must ship `kind`, `at`, `days`, and its timezone to a client that otherwise needs none of them (§8.2). That is a second evaluator, in another language, to restate something that you see only when you travel.

- **`offset`** — `due = now + offset`. No rounding. This is the one kind that is pure duration arithmetic on an absolute instant. It never touches a calendar, and DST cannot affect it: "3 hours" is three hours of elapsed time, even across a transition.
- **`clock`** — take today's `y/m/d` in `loc`, and advance it by `days` **calendar days** (`AddDate(0, 0, days)` on the date parts, never `+ days*24h` on the instant). Then build the result with `time.Date(y, m, d, at.hh, at.mm, 0, 0, loc)`. If the result is `<= now`, advance the date by one more calendar day and rebuild. Calendar-day arithmetic is necessary on a DST boundary: "tomorrow 09:00" must be 09:00, not 08:00 or 10:00.
- **`weekday`** — find the next date whose weekday matches, at `at`, built the same way.
  - If today matches and `same_day_ok = true` and `at` is still in the future → today.
  - Otherwise → the next matching weekday strictly after today.
  - Note: with `same_day_ok = false`, "next Monday" on a Monday means +7 days. This is intentional and matches Slack.

A `clock` preset whose `at` falls inside a spring-forward gap (02:30 in a zone that jumps 02:00→03:00) has no such wall-clock time that day. `time.Date` normalizes it forward to 03:30 and does not fail. Accept that, and assert it in the test. Do not add gap detection.

Unit tests are required for:

- DST spring-forward and fall-back on a `clock` preset, including an `at` inside the gap
- a `weekday` preset evaluated *on* that weekday, both `same_day_ok` values
- `offset` crossing midnight
- `clock` evaluated at 23:50
- `clock` with `days = 0` both before and after `at`

---

## 7. Notifications

### 7.1 Web Push (always on)

The VAPID keypair is the permanent identity of the app to push services. It is generated once and stored in the env file. It is **never rotated casually**, because every stored subscription is bound to the public key that it was created under.

- **A push send reaches only a public address.** An endpoint is whatever URL a subscribe call handed over. Without a guard, a token holder can use the endpoint to make the server send requests into the server's own network: to a private host, or to a metadata service through a redirect. §7.5's per-endpoint status then says what answered. So the push client checks each address at dial time, after DNS resolution. It refuses any address that is not public unicast (loopback, private, link-local, `100.64.0.0/10`, unspecified), and classifies the failure as `refused`. The push client follows no redirect: a 3xx goes through the response handling below as `http 3xx`. It uses no proxy, because with a proxy the dialed address is the address of the proxy. Every real push service is public, so this rule has no effect on a real browser.
- Store `vapid_public` on each subscription row, and store **the key that the client says it subscribed under**. `POST /api/push/subscribe` carries an `application_server_key` field, and the server records that field, never its own configured value (§8.2). If the server stamps its configured key instead, the column always matches the configured key and gives no information. A browser can re-post a subscription that is still bound to a retired keypair (§9.8 tells it to re-post on every load). With the configured key stamped, that row is recorded as current. Then the boot warning below sees zero mismatches, and the 403 handler below takes its "matches, so it's a bug" branch forever. The column has value only if it records what the *subscription* is bound to.
- **At boot, if any subscription's `vapid_public` differs from the configured one, log a prominent WARNING** that names the count. With this warning, you learn about the mismatch at startup, not after three weeks of silent failures.
- The app can recover from a rotation, but a rotation has a cost. The server deletes the dead rows on the first 403 (below). Each browser re-subscribes under the new key the next time that the user opens the page (§9.8). Every device must open the app once.
- Send with `webpush.SendNotificationWithContext(ctx, payload, sub, &webpush.Options{ TTL: 259200, Subscriber: subject, VAPIDPublicKey: ..., VAPIDPrivateKey: ..., Urgency: webpush.UrgencyHigh, Topic: "nag", RecordSize: 2560 })`. This is the form that carries a context. Never use the bare `SendNotification`: the per-send deadline arrives through the context (below), and so does the cancellation that bounds shutdown (§10.1). **`Subscriber` is the subject without its `mailto:` scheme.** webpush-go adds `mailto:` in front of any value that does not start with `https:`. So if webpush-go gets the full `mailto:` URL that §5.1 requires, it signs `sub = mailto:mailto:…`, which is not an RFC 8292 contact.
- **`RecordSize` is 2560, not the library default.** webpush-go pads every encrypted body to its record size, 4096 bytes by default. Mozilla's push service caps what it forwards at 4096 bytes. For Firefox on Android, Mozilla's service routes the message through FCM. In that case, it measures the *base64* body inside a JSON map. That map also carries a channel id, the encoding and a reliability id. The default is refused with a `413` everywhere. A record size of 3070 (exactly 4096 bytes once encoded) passes for desktop Firefox and is still refused on Android. A 2560-byte record encodes to 3416 bytes, which leaves 680 bytes for the other fields of that map. A 2560-byte record still holds 2457 bytes of plaintext. That is more than the largest payload needs: a 1000-byte text (§8.3) with every byte JSON-escaped. A test encrypts that payload for real.
- **`Topic` is the constant string `"nag"`, not the reminder id.** A push service replaces any message that is still undelivered and carries the same topic for the same subscription. So with a constant topic, **at most one push can ever be pending for a device**. This rule alone solves the problem of the offline device. A laptop that was shut for two days does not receive a queue of notifications when it wakes. It receives the most recent one. A topic per reminder replaces nothing, because the ids are always different. It looks like deduplication, but it has no effect.
- **The generous TTL (~3 days) is safe *because* of the constant topic.** The TTL and the topic are a pair and must be read together. If the browser is closed, the push service queues the message and delivers it on the next launch. The constant topic bounds that queue at one. A shorter TTL is the other way to bound the queue, but it loses the weekend case. If you close the laptop on Friday and open it on Monday, a one-day TTL has already dropped the nudge. Because the topic already bounds the queue, a three-day TTL covers the weekend case at no cost.
- Payload, **one message per push, and it can cover several reminders** (§7.3):

  ```json
  {"n": 1,  "id": 123, "due_at": 1786742400, "texts": ["Renew the domain"]}
  {"n": 14,                                  "texts": ["Renew the domain", "Call the plumber", "Move the standup"]}
  ```

  `n` is how many reminders this push covers. `due_at` is present **only when `n == 1`**, because with several reminders there is no single due time to show. `id` is present **only when `n == 1` and there is a real row to point at**. Every scheduled push with `n == 1` has such a row, but the test push does not (§7.5). So `id` implies `n == 1`, but `n == 1` does not imply `id`. A consumer must check for the field and not infer it from the count. §9.5 gives what a notification with no `id` does when clicked. `texts` carries at most **three** entries, in due order. Each entry is **link-flattened** (§9.10), so the notification shows `Renew the domain`, not `Renew [the domain](https://…)`. When `n == 1`, that one entry is the full text. When `n > 1`, each entry is truncated to 120 bytes on a rune boundary with a trailing `…`. The worker draws whichever form it gets and derives "+11 more" from `n - texts.length` (§9.5), so there is no fourth field. The worst-case payload is then about 1 KB. Most of it is the full text of the `n == 1` form, which can be a 1000-byte reminder (§8.3). That is well inside the ceiling, and that is what matters. The encrypted payload limit is about 4 KB, and `text` alone can have 1000 bytes (§8.3). So a naive digest with one entry per reminder silently goes over the limit on a backlog of five.

  Nothing else goes in: no channel list, no history, no overdue total. The count in the notification is the count of reminders *that this push is about*. That count is exact for this push and cannot go out of date. The badge and the list show the overall number (§9.3). The service worker must be able to draw the notification with **no network call at all**. The browser starts the worker with no open page and under a hard deadline, sometimes on a device that just came back online. If the worker misses that deadline, the browser shows its own generic notification. (A same-origin `fetch` from a worker *does* carry the session cookie, because its default credentials mode is `same-origin`. So the reason for this rule is reachability and latency, not authentication. §9.5 relies on that fact for `pushsubscriptionchange`.)
- Every send takes a context from its caller, and that context carries the deadline. The sweep gives **10 s** per send (§7.3). `POST /api/push/test` gives **the smaller of 3 s and what is left of the 12-second budget** (§7.5). The deadline is the only part of a send that is different between the two callers. Every rule below is shared, and that is why the two callers use one helper and not two copies of the response handling. The limit is a deadline and not a client timeout for two reasons. Only the first of those two numbers, the sweep's 10 s, can be expressed as a client timeout. And a client timeout cannot be canceled from the outside (§10.1).
- On `404` or `410`, **delete the subscription row**.
- On `401` or `403`, check the row's `vapid_public`. If it **differs** from the configured key, **delete the row** and log a WARNING that names the endpoint in truncated form (below). A subscription bound to a retired keypair can never succeed again. If the server keeps the row, the row fails on every sweep, forever. If it **matches**, log an ERROR and keep the row: that is a bug or a push-service problem, not a stale key.
- On any other error, log and keep.
- **An endpoint is never written to the log in full**, in any of those lines. Use the same truncation that `/api/push/test` returns: scheme, host, and the last 8 characters of the path (§7.5). The reason is the same: the full URL is the capability to push to that device. The truncated form is enough to tell two browsers apart, but not enough to send a push. §10.4 states this as a general rule.
- **Send failures are classified, never stringified into the database.** Only for shoutrrr does the status code or transport failure become a `delivery_error` entry (§4.1). A push failure changes no reminder column at all, and only the log line records it.
- **On success, nothing is recorded.** The row deliberately has no `last_ok_at` and no `user_agent`. `FROM scratch` has no `sqlite3` (§7.4), and no endpoint or subcommand exposes a subscription. So if the row had these two columns, the sweep writes them every 30 seconds and nothing reads them. A column that cannot be read is not observability. The log line is the observability (§10.4).
- **Never send a silent push.** The service worker must always call `showNotification`. If it does not, browsers revoke the subscription.
- **If there are zero subscription rows**, the sweep still marks the reminder rows, and the notification goes nowhere. Log a WARNING that names the number of reminders in the unsent push and the id of the first one. Without this WARNING, the first reminders after a fresh deploy vanish with no trace.

**Platform reality**, useful to know before you test Goal 2:

- Desktop Firefox and Chrome: works from a normal tab, no install needed.
- Android Chrome/Firefox: same.
- **iPhone: Web Push only works if the site is added to the Home Screen** (iOS 16.4+). Every iOS browser is WebKit, so this is not a Chrome-vs-Safari question. This is why `manifest.webmanifest` is required and not optional (§9.3).

### 7.2 shoutrrr fan-out (opt-in per reminder)

**Fan-out is not subject to the push cooldown and is never coalesced.** It happens in the same tick that marks the row (phase 3 of §7.3), and it sends one message per reminder. The 30-minute rule exists to keep an OS notification tray from filling up with nudges that you already understood. A channel is different: it is an *addressed* output that you asked for on that specific reminder, often with a different address per reminder. Two channel messages cannot be merged into one coherent message. A channel is also the one output whose whole value is "tell me on Telegram *now*". A deferral of up to half an hour takes away the reason that you configured it. Phase 1's `LIMIT 50` bounds how many rows reach fan-out per tick, and phase 3's own deadline bounds how long fan-out can spend on them (§7.3).

If `extra_channels` is not empty, resolve the names against `channels` (skip disabled ones), build a sender, and send. **A name that resolves to no row is skipped with one INFO line** that names the reminder id and the name. Orphans are defined behavior (§4.1), not an error. So no `delivery_error` is recorded, and §10.4's promise to log "every delivery failure" never covers them. Without the INFO line, a fan-out that you deliberately asked for goes nowhere in silence. That is the one outcome that this app must never produce. **A *disabled* channel is skipped silently, and the asymmetry with the orphan is intentional.** You muted it yourself, and `nag channel list` shows the mute. An orphan is a name that points at nothing: the same silence, but with no record that you chose it. The message body is the **link-flattened** text (§9.10), the same as in the push payload. A channel is an output, not a second renderer. Failures are **classified** into the fixed `delivery_error` shape (§4.1) and logged in that same form. A failed send must **never** block or retry, because the overdue list is the real safety net. **`delivery_error` is written, or cleared on success, per reminder as soon as that row's attempts finish.** It is not batched to the end of the phase and not carried into the next tick. So when phase 3 hits its deadline (§7.3), it has already recorded every row that it did attempt. The per-send timeout is 10 s, the same as push. Every send is also bounded by phase 3's context, so the effective per-channel timeout is the smaller of 10 s and what is left of the phase's 20-second budget (§7.3).

**That bound is imposed by a wrapper, because shoutrrr's API cannot carry it.** `router.Send(message, params) []error` takes no `context.Context`. Several of its services build their own `http.Client` with no configurable timeout. So the library gives no direct way to apply the 10 s per-channel timeout or phase 3's whole-phase deadline. Each channel send therefore runs `router.Send` on **its own goroutine**, which writes the result into a Go channel with a buffer of one. The caller `select`s on that Go channel and on a context derived from the phase's context with a 10-second timeout. That derived context is exactly how "the smaller of 10 s and the remaining budget" is obtained. The first of the two to arrive decides the verdict. A result is classified as above. If the context expires first, the send is **abandoned**. It is recorded as `<name>: timeout`, the classification that §7.3 already gives to a send that its budget cut short (§4.1). Its goroutine is left to finish in the background, and its result is discarded. **A send that the shutdown cancellation ends in the same way (§10.1) gets the same word**, and deliberately needs no new one. The `select` cannot tell a deadline from a cancel, and it does not have to. `<name>: timeout` already means "attempted, did not complete", which is exactly what happened. In a column that describes attempts (§4.1), it is also the only honest shape for a send that the process stopped waiting on. This wrapper makes phase 3's "really ends at ~20 s" bound (§7.3) true, whatever the library offers. An abandoned goroutine that holds a dead socket for a few seconds costs nothing at this scale. It holds no lock, it has nothing left to write to, and it ends on its own client's timeout. §2's instruction to verify the module path on pkg.go.dev before pinning **also applies to the API**. If the fork now has a context-aware send or a settable `http.Client`, use it, and replace the wrapper with a plain call. But the bound must never depend on that.

**A channel endpoint is trusted like any server that you point nag at.** shoutrrr reads each reply whole (`io.ReadAll`) with no size cap. So an endpoint that answers with an endless body grows nag's memory until the service's own client timeout ends the read. nag's 10-second wrapper above only abandons the read. The growth is bounded in time, not in bytes. To cap the size in nag, nag must replace the `http.Client` of each service. That replacement removes the TLS and auth settings that the services put on their own clients (only `generic` takes an injected client cleanly). So the bound is one level outside nag instead: `compose.yaml`'s `mem_limit` (§10) changes the worst case from a VPS that runs out of memory into a container restart. When that happens, the database loses nothing. Every row of the tick was marked before its fan-out (§7.3), and no row is re-sent. What is lost is the fan-out that was still in flight when the container died. That is the message to the channel with the endless body, plus any later sends and `delivery_error` writes of the same tick. Those are lost too, because the abandoned read can continue after the send that started it has ended.

Do not use ntfy's own delayed delivery. This app owns the schedule. ntfy is only an output.

### 7.3 The sweep

The sweep is one goroutine with a 30-second ticker. It runs once immediately at boot, before the first tick. `now` is read **once per tick** and used for every statement in that tick.

**The governing rule is one sentence: at most one Web Push notification in any 30-minute window, and it carries every reminder that has come due since the last one.** Nothing else is rate-limited: the badge, the two lists, and the shoutrrr fan-out are all prompt. So a tick has four phases, **in this order**. The order is a requirement, not only a way to describe the steps. Every column that decides *whether* a notification happens (the lifecycle marks `notified_at` and `pushed_at`) is committed before any socket is opened. And the push goes out before the one phase whose duration a third party controls.

**Phase 1 — mark.** Writes only, no network.

```
tx: SELECT id, text, due_at, extra_channels FROM reminders
    WHERE done_at IS NULL AND notified_at IS NULL AND due_at <= now
    ORDER BY due_at, id LIMIT 50
tx: UPDATE reminders SET notified_at = now WHERE id IN (<all selected>)
tx: UPDATE reminders SET pushed_at   = now WHERE id IN (<selected with due_at < now - 1800>)
```

The selected rows that carry `extra_channels` are held in memory for phase 3, `text` included. The tick fans out exactly what it just marked and never re-queries for it. So the message body that phase 3 sends (§7.2) must come out of this SELECT. **Rows that the too-late gate stamped are excluded from that hand-off**: only rows with `due_at >= now - 1800` are eligible. The gate means no output at all, not only no push (below, and the §7.6 row "Server down 2 days").

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

**Phase 2's UPDATE repeats the SELECT's predicate instead of listing ids.** There is no `LIMIT` here (below), so an id list has no bound, and a large backlog can hit SQLite's limit on the number of bound parameters. The two forms are equivalent *in this transaction specifically*. `MaxOpenConns(1)` (§4.3) means that no handler can write between the two statements, so the predicate cannot have gained a row. Phase 1 still needs its explicit id list, because `LIMIT 50` is exactly what makes its selected set narrower than its predicate.

**Phase 3 — fan out** (§7.2), to the eligible rows phase 1 handed over, sequentially, one message per reminder.

**Phase 4 — purge** old `done` rows (§4.4).

**Fan-out runs after the push, and under a 20-second deadline for the whole phase.** Both rules, the order and the deadline, exist to stop a dead socket of a third party from delaying this app's own notifications. The arithmetic shows why. `LIMIT 50` rows × up to 16 channels each (§8.3) × a 10-second timeout is more than two hours of a single tick. During that time no further tick starts (below), so a batch of unreachable channels delays every later digest by hours. `LIMIT 50` bounds the *count* and never the *duration*. Only a clock can bound the duration.

So phase 3 takes a whole-phase deadline of **20 seconds measured from the moment phase 3 starts**. The deadline comes from a fresh clock read that is taken for that deadline and nothing else. Phase 3 stops when the deadline expires. Rows not yet attempted are **skipped, logged at INFO with their ids and how many were dropped, and never retried**. No `delivery_error` is written for them, because nothing was attempted and the column describes attempts. That is the same trade that the whole sweep is built on. The row is already marked and already in "Needs you now", and the list is the safety net. A fan-out queue that survives across ticks adds durable state for the one output that is explicitly best-effort.

**That deadline is a context threaded into every send, not a check between rows. This difference alone decides whether the bound holds.** A single row can carry up to 16 channels (§8.3), each sent with its own 10-second timeout. If the budget is consulted only at row boundaries, a row that enters phase 3 at 19.9 s runs for another 160 seconds after the budget expired. The phase then ends at about three minutes. Every sentence here and in §7.6 that puts its cost at "the next tick and no more" is then false. So the 20 s is the deadline on a context that bounds every send in the phase. **Each individual channel send takes the smaller of 10 seconds and whatever is left of the budget.** §7.5 pins exactly the same discipline for its own 12-second budget, for the same reason: a per-call timeout bounds one call, and only a deadline carried into the calls bounds the set of them. shoutrrr's `Send` cannot take that context at all. So the send runs on its own goroutine, and the phase `select`s the result against the deadline. It abandons the send and does not cancel it (§7.2). From the outside, an abandoned send gives the same bound as a canceled one, and that is the purpose of the §7.2 design. So phase 3 really does end at ~20 s, whatever the shape of the rows inside it and whatever the library exposes. Rows that the budget never reached are skipped exactly as described above. The remaining channels of a row that the deadline interrupted partway through are skipped in the same way. Nothing was attempted for either, so neither is retried, and neither records anything in `delivery_error`, a column that describes attempts (§4.1). **A send the expiring budget cuts short mid-flight is the one thing here that does record a verdict, and it needs no new vocabulary: it is `<name>: timeout`.** It was attempted and it did not complete, which is exactly what that classification already means (§4.1). It is written with the rest of that row's verdict as soon as its attempts finish (§7.2). A sixth classification that means "we stopped waiting" is a distinction that nobody can act on: a channel that was able to answer inside the budget is never in this position.

**The fresh clock read for the phase 3 deadline is the one place where the tick's single `now` is deliberately not used. It is a correctness fix, not a matter of style.** Phase 2 sends sequentially with a 10-second timeout each (below). So three sleeping devices put about 30 seconds between the tick's `now` and the first line of phase 3. A deadline of `now + 20s` is then already in the past before phase 3 tries a single channel. That drops *every* fan-out on exactly the ticks where a device was unreachable. The tick's `now` does not change: every statement, every stamped column, and the too-late gate still read the once-per-tick value (above). Only the deadline reads the clock again, and it reads it once.

With the deadline in place, the worst case is bounded on both outputs. A fan-out message is either attempted within 20 seconds of the phase start, or dropped. A push is delayed by at most one tick interval plus the fan-out budget: phase 3 of the previous tick can hold the goroutine for 20 seconds, which costs the next tick and no more. That second half is true only because the deadline is threaded into the sends and not consulted between them (above).

Phase 3 runs *after* phase 2, and that order keeps the bound to a single tick: the digest of the tick that marks a row is never behind the fan-out of that row. The order costs nothing, because fan-out is not coalesced and shares no state with the push.

**The held set is a query, not state.** Phase 2 selects on `notified_at IS NOT NULL AND pushed_at IS NULL`, so the reminders that wait out a cooldown live in the database (§4.1). A restart mid-window loses nothing. A row **cleared** during the window drops out of the digest by itself through the `done_at IS NULL` clause. You dealt with it, so it does not need to arrive as a notification thirty minutes later. An undo of that clear does not put it back either. Undone stamps `pushed_at` on a row whose `notified_at` is set (§4.1). So a held row leaves the held set for good the moment it is cleared, and a later undo cannot put it in a digest.

**`last_push_at` is in memory only**, and it is the single piece of cooldown state that is not durable. It **starts at zero**, so the tick that runs at boot always finds the window open. As a result, a restart opens the window early. At worst that gives one extra notification, never a missed one, which is the direction in which this app must fail. It is deliberately *not* derived from `MAX(pushed_at)`. Phase 1 stamps that column on rows that nothing was ever sent about (below), so the maximum can start a cooldown for a push that never happened. **A `now` earlier than `last_push_at` also counts as an open window.** After a backward clock step (NTP, a snapshot restore), the plain difference `now - last_push_at` shows the window as closed for the whole length of the jump. The closed window then holds every reminder that came due during the jump. This is the same direction as the restart: at worst one extra notification.

**The window is consumed whenever phase 2 selected rows — whatever the sends then did, and even with zero subscriptions stored.** `last_push_at` is set before the first socket is opened, for the same reason as `pushed_at`. The rows have already left the held set and can never be re-selected, so a window kept open on failure has nothing left to carry. To keep it open only makes phase 2 re-run on the next tick against an empty select. This rule keeps the governing sentence literally true: at most one push attempt per 30 minutes. It is also why the §7.6 rows that end in **0** still end a cooldown.

**Sends are sequential**, one subscription at a time, each with the 10-second timeout. Three sleeping devices make a 30-second tick, and the next tick is skipped, which the no-overlap rule below already tolerates. A personal instance has two to four subscriptions. A parallel send gives no benefit. It only adds a `WaitGroup` and the need to serialize the 404/410 row deletions against the single connection.

**Phase 2 refuses a held row more than 3900 seconds past due, and stamps it anyway.** The phase-1 gate cannot cover this case, because it runs at mark time and this row was fresh when it was marked. Take a reminder that was marked on time, held by a closed window, and still held when the server comes back from a two-day outage. Without this rule, it is pushed as though it had just come due. The bound is not a third constant to remember. It is the sum of the two 30-minute constants, plus a margin. A row can be up to 1800 s late when phase 1 marks it (the gate's own edge). It then waits out a window that the previous tick consumed. So the worst case that the two constants legitimately produce is **3570 s**: a `LIMIT 50` backlog whose leftovers are marked on the following tick. The extra five minutes stop that from being a one-tick margin. Without them, two sweep ticks lost to a failing query or a forward clock step silently drop reminders that the constants allow. This rule must fail the way the rest of §7.3 fails: at worst one extra notification, never a missed one. A row past the bound was not batched by the cooldown. An outage stranded it.

Refused rows are stamped exactly like the rows that are sent. The split decides what is *sent*, never what is *stamped*, because a row that leaves the held set must never become selectable again at an even greater age later. Each refused row gets one INFO line that names its id, the same shape as the gate's line (§10.4). **The window is not consumed when every held row was refused.** Nothing was sent, so the cooldown has nothing to protect. This matches the tick where the phase-1 gate stamps every candidate. Together with the gate, the promise now reads: *nothing more than half an hour late is ever notified about, and nothing waits much more than an hour to be.*

**The "too late" gate — the 1800-second one — is applied in phase 1, at mark time, and nowhere else.** When the sweep first sees a row whose `due_at` is more than **1800 seconds** in the past, it stamps `notified_at` *and* `pushed_at` and writes one INFO line that names the id. That row is never pushed and never fanned out. It only appears in the list. So the gate cannot cancel a notification that the cooldown deferred to the digest. A row marked on time and held for the full window is up to 30 minutes late when phase 2 finds it, and phase 2 never re-applies the gate's own number. A gate at push time makes the cooldown drop exactly the notifications that it exists to batch, and it does so silently. Phase 2's hour-long bound (above) is the deliberate exception, and it does not have that effect, precisely because it is the gate's number *plus a whole window*. Every row that the cooldown legitimately delayed is inside it. Only a row that an outage stranded is not.

Both constants are 30 minutes, and the spec means them as **one** number that the user can hold: *nothing more than half an hour late produces a notification, and no more than one notification per half hour.*

**This gate only ever sees rows that were in the future when they were written** and then went unswept: a stopped container, a suspended host, an outage. A deliberately backdated `due_at` never reaches phase 1 at all. The write path already set both columns (§4.1), so the `notified_at IS NULL` clause filters the row out. The two mechanisms never apply to the same row, on purpose. That is what lets the picker state without any condition that a past time will not notify (§9.6), without knowing this constant and without new wording when the constant changes.

**Mark before sending, always.** Every column that decides whether a notification happens (`notified_at` and `pushed_at`, and nothing else) is committed in phases 1 and 2, before phase 2's push and phase 3's fan-out open a single socket. A dropped notification still appears in the overdue list. A storm of repeated notifications from a crash loop does not stop by itself. This is the deliberate trade, and it is the reason that the *lifecycle* writes come before the network and are not interleaved per row.

**Two writes are deliberately not covered by that rule, and both are bookkeeping about an attempt rather than a decision to attempt.** A `404`, `410`, or stale-key `403` **deletes its subscription row inline**, while phase 2 handles the response of that endpoint. Only the response shows that the row is dead (§7.1). And `delivery_error` is written, or cleared on success, **per reminder, immediately after that row's send attempts complete** in phase 3 (§7.2). It is not accumulated and flushed at the end of the phase, and never deferred to the next tick. Both writes follow from the same principle, read in the other direction. A column that describes what a socket did cannot be committed before the socket is opened. Neither of these columns can bring back a notification or suppress one. The rule that matters is this: nothing that the sweep learns from the network can change whether a row has been handled.

**No `LIMIT` in phase 2.** It sends exactly one push, whatever it selects. So a cap strands rows for another 30 minutes and saves nothing. And `texts` is capped at three entries anyway (§7.1), so the payload does not grow with the count either.

**`LIMIT 50` in phase 1 is a deliberate rate limit, not a pagination bug.** With the digest in place, it now bounds exactly one thing: **shoutrrr messages per tick**, the only output left that scales with row count (§7.2). The sweep does not loop within a tick. So a backlog of 200 is marked over four ticks, about two minutes, and lands in one or two digests.

**No overlapping ticks and no unbounded sends.** The sweep is one goroutine on `for range ticker.C`, so when a tick runs long, the next tick is skipped, not queued. Every outbound call (the digest push and every shoutrrr send) uses a client with a **10-second timeout**. Phase 3 also carries a deadline for the whole phase (above). Without both, a single hanging endpoint stalls the goroutine with the columns already committed. That is the one failure mode that this design cannot recover from with a retry. A per-call timeout alone bounds one send and not a tick, and the phase deadline closes exactly that gap. The digest also caps the worst case structurally. Phase 2 makes one call per *subscription*, not per reminder, so three devices make three calls, however long the backlog is.

**The deferral is tolerable only because the badge is not subject to it.** A reminder held by the cooldown is in "Needs you now" and in `overdue_count` the moment it comes due. So the pinned tab (Goal 4) is correct within 45 seconds: at most one round trip, given the one-shot due timer (§9.3). The cooldown delays the OS notification, which is the nudge. It never delays the list, which is the truth. That is the same trade that the whole app is built on, applied to a rate limit.

Do not use in-process timers per reminder. Polling survives restarts, and it needs no rescheduling when a due date is edited.

### 7.4 `nag channel …`

The final image is `FROM scratch`: no shell, no `sqlite3`, no way to hand-edit the table. So channel management is a subcommand, run against the same `NAG_DB`:

```
nag channel add <name> <shoutrrr-url>    # rejects a duplicate name and an unparseable URL
nag channel list                         # URLs masked, shows enabled state
nag channel rm <name>                    # prints how many reminders reference it, then proceeds
nag channel enable <name>
nag channel disable <name>               # mute without orphaning extra_channels
nag channel test <name>                  # send "Nag test notification" now, print the result
```

`add` hands the URL to shoutrrr's router **before** it writes the row. It refuses a URL that the router cannot parse, and names the parse error. Without this check, a typo is stored without complaint and surfaces weeks later as a `delivery_error` on a reminder that you cared about.

**`list`'s mask is pinned: the scheme, then a literal `…` for everything else** (userinfo, host, path and query alike), written unconditionally. So `ntfy://user:tok_abc123@ntfy.example.com/alerts` prints as `ntfy://…`, and so does every other ntfy channel. Services disagree about where the secret goes. It can be in the userinfo or query (ntfy, telegram), or in the last path segment (gotify's token, a webhook's, a topic on public ntfy.sh). It can also be the host itself: ifttt's and wecom's keys, and pushbullet's `o.…` token. That token has a dot, so not even the rule "a host with a dot is a server name" holds. So no part past the scheme is safe to print without a per-service table, and such a table goes stale on every library bump. The name of the channel says which channel it is. The mask confirms only the service. The same mask applies wherever `nag channel` otherwise echoes a URL, including a URL typed where a name belongs.

That is deliberately **not** §7.5's "last 8 characters of the path" form. The difference is what each surface must tell apart. The capability of a push endpoint *is* its opaque path, and its host is one of four push services. So a short tail of the path is the one thing that tells two browsers apart, and it is far too little to send with. A channel already has a name that tells it apart. Across shoutrrr's services, the secret can sit in any part of the URL, so no tail of it is safe to print. The question that `list` answers is which service a named channel uses, never what the credential is.

**`name` is a lowercase slug: `^[a-z0-9][a-z0-9_-]{0,31}$`.** `add` rejects anything else and prints the pattern. The rule is not cosmetic. The name is a JSON array element in `extra_channels` (§4.1), a chip label (§9.2), and a CLI argument. `UNIQUE` on TEXT is case-sensitive in SQLite, so `ntfy` and `NTFY` can coexist as two rows that render two identical-looking chips. To accept only lowercase names at `add` is one rule. The alternative is a `COLLATE NOCASE` index plus a matching fold on every `extra_channels` lookup at send time.

`test` is the fan-out counterpart to `POST /api/push/test` (§7.5), and it exists for the same reason. A channel is a URL with a secret in it, and you want the "did that work" answer at the moment you paste it, not at the moment a reminder fires. It sends even to a disabled channel, because you are asking about the URL, not the mute switch. Its output says when the channel is disabled. It carries **the sweep's 10-second timeout**, through the same wrapper and the same classification (§7.2). So the word that it prints on a dead host is the word that a `delivery_error` holds for the same failure. There is no shorter budget here because, unlike `POST /api/push/test` (§7.5), no server `WriteTimeout` applies to it.

Usage is `docker compose run --rm nag channel add ntfy 'ntfy://…'`. The URL contains secrets. That is exactly why channels are not in the TOML (§5) and why no API creates them. The only way to write a channel is a shell, and you must already be on the host to use it.

`rm` **warns and proceeds**. There is no `--force` flag. Orphaned names in `extra_channels` are already defined behavior (§4.1): they resolve to nothing and are skipped at send time. A confirmation prompt is also useless in `compose run`, which is not always a TTY.

The subcommand opens the DB directly through the same `store.Open` as `serve`. So it **creates and migrates the file if it does not exist** (§5.1). To add your first channel before the first `up -d` is a supported order, not a trap. It also means that **the serving container can hold the write lock**. `busy_timeout(5000)` covers that. If it still fails, say so plainly and suggest a retry.

### 7.5 `POST /api/push/test`

This endpoint sends a fixed "Nag test notification" push to every stored subscription and returns `200` with per-endpoint results. It applies **the same response handling as the sweep for all four codes**: 404/410 delete, and 401/403 delete only when the row's `vapid_public` differs from the configured key (§7.1). One helper does the send and the response handling for both callers, so no second copy of the response handling can drift from the first. And `deleted: true` on a `403` is then the fastest answer to "is this browser still bound to the keypair I rotated away from". That is the question you have when push went quiet after a rotation. **It ignores the 30-minute cooldown and does not touch `last_push_at`.** You are asking whether the transport works. An endpoint that answers "come back in twenty minutes" answers a different question than the one you typed. It sends the `n == 1` payload form with a fixed text and carries the same constant `Topic`. So on a device that is offline, it is subject to the same one-pending-message replacement as a real push (§7.1). **The payload is pinned, because the worker has to draw it with no special case:**

```json
{"n": 1, "due_at": 1786742400, "texts": ["Nag test notification"]}
```

`due_at` is the server's `now` at the moment of the call. So the notification body reads `now` through the ordinary formatter (§9.11), and `sw.js` needs no second title-and-body shape. There is **no `id`**, because no row exists to point at. That is the whole reason that §7.1 states `id` separately from `n == 1`. It also makes the click behave like a digest's click (§9.5): the app is focused, and no row is scrolled to or flashed.

**Its per-send timeout is 3 seconds, not the sweep's 10, and the handler as a whole is bounded by a 12-second budget.** Sends are sequential here, as in the sweep (§7.3). With the sweep's numbers, two unreachable endpoints take longer than the server's 15-second `WriteTimeout` (§8.2), and the `curl` gets no response at all. That happens precisely in the "notifications are silent" case that this endpoint exists for. In that case, an endpoint that never answers is also the likeliest thing to find. A diagnostic probe optimizes for an answer to the caller, not for patient delivery. A subscription that cannot answer in 3 seconds comes back as `timeout`, and that *is* the diagnosis. The sweep keeps its 10 seconds unchanged, because there nobody waits for the answer, and a slow push is still a delivered push.

The budget is a deadline on the handler, taken from one clock read before the first send. **Each send gets the smaller of 3 seconds and whatever is left of the budget.** So the sends together can never run past 12 seconds, and the response always has three seconds of the write deadline to itself. A per-send timeout alone does not give that: five dead endpoints at 3 seconds each make exactly the 15 seconds of the write deadline, and a personal instance can easily hold five rows. Subscriptions that the budget did not reach are **reported, never silently omitted**. Each one gets an entry with `status: null` and `error: "not attempted"` (§8.2), so the array always holds exactly one result per stored subscription.

The response, whose `status` and `error` vocabulary §8.2 pins to a closed set:

```json
{"results":[{"endpoint":"https://updates.push.services.mozilla.com/…f3Ka9tQ2",
             "status":201,"deleted":false,"error":null},
            {"endpoint":"https://fcm.googleapis.com/…kQ7bT1xz",
             "status":403,"deleted":true,"error":"stale vapid key"},
            {"endpoint":"https://web.push.apple.com/…9pQ4mW7v",
             "status":null,"deleted":false,"error":"not attempted"}]}
```

`endpoint` is truncated to scheme, host, and the **last 8 characters** of the path. That is enough to tell two browsers on the same push service apart, and not enough to send one a notification. The full URL is enough for that. `deleted` says whether this call dropped the row. It is necessarily `false` on a `not attempted` entry, because nothing was attempted, so nothing was learned about the row. A subscription list with zero rows returns `{"results":[]}` and a 200, not an error. That is itself the answer to "is anything subscribed", and it is the only way the array comes back short. **The UI does not show this endpoint.** It exists so that `curl` can answer "is push actually working from this box to this browser" without a wait for a real reminder. This is the first thing that you will want when notifications are silent.

It takes no body. So it is one of the endpoints exempt from the JSON `Content-Type` requirement (§10.2), and the documented invocation stays a bare:

```
curl -X POST -H "Authorization: Bearer $NAG_TOKEN" https://nag.example.com/api/push/test
```

### 7.6 What actually arrives, case by case

The rate limit, the digest, the constant `Topic`, and the too-late gate interact, and the interactions are the whole behavior. This table is normative: where a sentence elsewhere disagrees with it, the table wins. **Each row assumes the cooldown window is open** unless it says otherwise. That is the steady state: nothing was pushed in the last 30 minutes.

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
| A tick whose fan-out targets are unreachable: 30 rows, dead ntfy host | Push notification **on time**. The ntfy messages that fit in 20 s are attempted, the rest are dropped with one INFO line | Phase 3 runs after the push, under a whole-phase deadline that bounds every individual send. A wrapper applies it where the library takes no context (§7.2, §7.3). So the phase ends at ~20 s, whatever the row-and-channel shape. A dead channel can cost you channel messages. It can never delay a notification past the next tick |
| `POST /api/push/test` during a cooldown | **1**, immediately | It bypasses the cooldown and does not consume the window (§7.5) |

Two facts about these cases need a plain statement. **The count in a notification is never the badge count.** `n` describes the reminders that the push is about. The badge describes everything overdue, including rows that you were notified about an hour ago and have not cleared. They differ constantly, and neither is wrong. And **a notification is never the record of anything**. A dismissal does nothing (§9.5), a loss to the TTL costs nothing, and every case above that ends in **0** ends in a correct list.

---

## 8. HTTP API

The API uses JSON. `/api/*` is the only authenticated surface.

### 8.1 Auth

**Cookie.** `nag_session`, value `b64(exp) + "." + b64(mac)`. No session table, no user id: there is one user. Every piece of that value is pinned, because two readings of it make two implementations that cannot verify each other's cookies:

- `exp` is the expiry as a **decimal ASCII string**: `"1786742400"`, not eight big-endian bytes.
- `b64` is **unpadded base64url**, `base64.RawURLEncoding`, which matches every other encoding in the app (§9.8).
- `mac = HMAC-SHA256(key: k, message: "v1|" + exp)` over that same decimal string, byte for byte.
- `k = HMAC-SHA256(key: NAG_TOKEN, message: "nag-cookie-v1")`. The token is the key, and the constant is the message. There is no second secret to manage, and **rotating `NAG_TOKEN` invalidates every cookie**. That is how you log out every browser at once.
- Attributes: `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`, `Max-Age` 365 days.
- **`Secure` is set unconditionally.** Behind the reverse proxy, the app sees plain HTTP. So if it derives `Secure` from `r.TLS != nil`, it silently ships a non-Secure cookie. Never do that.
- `SameSite=Lax`, not `Strict`, so that a notification click that opens a fresh tab arrives authenticated.
- **Sliding renewal**: a valid cookie older than 30 days is re-issued in the response, so a pinned tab never logs out. The cookie carries only its expiry, so "older than 30 days" is computed as `expiry - now < maxAge - 30d`. Write `maxAge` as the one constant that both the issuer and this check read. If you hardcode `335` here, the check breaks silently on the day that `Max-Age` changes.
- Verify with `hmac.Equal`. Reject when `expiry < now`.
- **`/logout` is not revocation.** With no session table, it only clears the cookie in that browser. A copied cookie stays valid until it expires. Rotating `NAG_TOKEN` is the only action that invalidates anything, and that is exactly why the cookie key is derived from it.

**Bearer.** `Authorization: Bearer <NAG_TOKEN>` is accepted on all of `/api/*`, permanently. It is the path for `curl`, `/api/push/test`, and the future bookmarklet. Compare with `subtle.ConstantTimeCompare`.

**Login.**

- `GET /` serves `index.html` **unauthenticated**. Otherwise the browser gets a bare 401 with nowhere to type a token. The page calls `/api/state`. On 401, it renders a token field instead of the list.
- `POST /login` `{token}` → `204` + cookie on match, `401` otherwise. On failure, sleep ~250 ms and serialize failures through a mutex. That is the entire rate limit, and it is sufficient for one user behind HTTPS.
- **On the `204` the client calls `location.reload()`, and that is the whole of the post-login path.** The pre-login load already ran its init work against a 401: the first `/api/state`, and §9.8's awaited subscription re-post and notification-slot decision. The alternative is to re-run each of those by hand, in the right order, from the login handler. A reload re-runs all of them under the new cookie, with no extra code. It costs nothing beyond what any reload pays, because there is no client-side state to preserve anywhere (§11): one full `/api/state` body before the cheap polls resume (§8.2).
- `POST /logout` → clears the cookie.
- Static assets (`/app.js`, `/app.css`, `/sw.js`, `/manifest.webmanifest`, `/icons/*`) are unauthenticated. They carry no data, and `/sw.js` must be fetchable for registration to work.

### 8.2 Endpoints

| Method | Path | Body / notes |
|---|---|---|
| POST | `/login` | `{token}` → 204 + cookie |
| POST | `/logout` | → 204, cookie cleared |
| GET | `/api/config` | `{config_version, vapid_public, default_preset, presets:[{key,label,quick}], picker:{hour_min,hour_max,minute_step,default_time,week_start}}`. The client must never hardcode these |
| GET | `/api/state` | `{server_time, config_version, overdue_count, push_subscribed, overdue:[…], later:[…]}`. This is the single source for list, badge, and title. Both lists come **server-sorted** by `due_at` ascending, then `id` ascending. The client never re-sorts. The tiebreak is necessary. Two chips tapped in the same minute produce byte-identical `due_at` values, and with `ORDER BY due_at` alone SQLite can return them in either order. Rows then swap places under your finger between polls. `push_subscribed` is `COUNT(push_subscriptions) > 0`: instance-wide, not per-device (§9.8). **`now` is read once per request**, and every part of the response derives from that one value: `server_time`, `overdue_count`, and which list each row lands in. With three separate clock reads, a row can land in `later` while the count above it already includes it. "The list never disagrees with the badge" (§9.3) is an invariant that this endpoint either holds or breaks on its own |
| POST | `/api/reminders` | `{text, extra_channels?}` plus exactly one of `preset`/`due_at` → 201 + the reminder |
| POST | `/api/reminders/{id}/done` | → 200 + the reminder. **On a row that is already done, this is a successful no-op, and `done_at` keeps its original value.** The UPDATE carries `WHERE done_at IS NULL`, the response is the row as it stands, and the status is still 200. The handler does not re-stamp, because a re-stamp pushes the retention clock (§4.4) forward on every double-tap. Then an accidental double tap, or two tabs that poll the same list, can keep a row cleared 29 days ago alive indefinitely. It is also the exact mirror of `/undone`, which is also a no-op on a row that is not done (§4.1) |
| POST | `/api/reminders/{id}/undone` | → 200 + the reminder. Powers the undo toast. A no-op on a row that is not done (§4.1). It also stamps `pushed_at` on a row that was already marked but never pushed about (§4.1). The response cannot show that write, because `pushed_at` is not in the reminder object. `notified_at` is the field that tells you which of the two undone cases you are in |
| PATCH | `/api/reminders/{id}` | `{text?, extra_channels?, preset?, due_at?}` → 200 + the reminder. The only write path for both a correction and a re-snooze (§8.4) |
| DELETE | `/api/reminders/{id}` | hard delete → 204. **No UI surfaces this.** Like `/api/push/test`, it is a `curl` affordance, for the row that you want gone and not cleared |
| GET | `/api/channels` | `[{name, enabled}]`, **`ORDER BY name`**. It drives chip order in two places (§9.2), and the client never re-sorts, exactly as with `/api/state`. **The URL is not in the response at all**: not masked, and not reduced to a boolean. The client has no use for it, because a chip is a name and a selected state. `nag channel list` is where you look at a URL (§7.4). A field that the UI must ignore is better not sent. The same argument keeps `pushed_at` out of the reminder object (§4.1). Read-only. Writes are §7.4 |
| POST | `/api/push/subscribe` | `{endpoint, keys:{p256dh, auth}, application_server_key}` → **204 with no body**, matching `/unsubscribe`. There is no subscription object in the API (§7.1 has no reader for one), so the only thing that a body can carry is an echo of the request. Stores `application_server_key` as the row's `vapid_public` (§7.1). **`ON CONFLICT(endpoint) DO UPDATE` must overwrite `p256dh`, `auth`, *and* `vapid_public`**, and leave `created_at` at its original value. An insert-or-ignore makes §9.8's unconditional re-post useless for the one job that it exists to do. After a key rotation, the browser re-posts the same endpoint under the new key. A row whose `vapid_public` never updates keeps failing forever while it looks healthy. `endpoint` must parse as an absolute `https:` URL. `application_server_key` must be unpadded base64url that decodes to 65 bytes. Otherwise a padded encoding from a hand-written client makes §7.1's boot mismatch warning fire on every start of a perfectly healthy install. **`keys.p256dh` and `keys.auth` are shape-checked for the same reason as §5.1's VAPID pair, not merely required.** Both must be unpadded base64url. `p256dh` must decode to exactly **65 bytes** (an uncompressed P-256 point, the same shape as the server key), and `auth` to exactly **16 bytes**. A bad value gets the usual 400 that names the field. The table accepts a mangled value, but nothing after the table does. It surfaces as an encryption or send failure on every sweep, in a log line, about a row that looks perfectly healthy. So the string is either well-formed at subscribe time, or it is a 400 |
| POST | `/api/push/unsubscribe` | `{endpoint}` → 204. **No UI surfaces this either.** The notification slot is absent whenever push works (§9.2), so by design the app has no "turn it off" control. The user-facing path is to revoke permission in the browser, and the next 404/410 deletes the stranded row (§7.1). This endpoint exists for `curl` and for a client that wants to unsubscribe cleanly |
| POST | `/api/push/test` | §7.5, not in the UI. **The per-endpoint result vocabulary is closed, both fields.** `status` is the HTTP status code that the push service returned. It is **`null`** when the attempt produced no code at all: a timeout, a DNS failure, a refused connection. A `0` in that field cannot be told apart from a code, and it sorts as one. `error` is `null` on success. Otherwise it comes from **the same classification set that §4.1 defines for `delivery_error`**: `timeout`, `refused`, `dns`, `http <code>`, `send failed`. It adds two values that this set has no use for. **`stale vapid key`** is for a 401/403 whose row's `vapid_public` differs from the configured key. That is the case that also sets `deleted: true` (§7.1). **`not attempted`** is for a subscription that the 12-second send budget of the endpoint never reached. It always carries `status: null` and `deleted: false` (§7.5). Both extras exist because this endpoint reports per subscription, not per attempt. Every stored row gets an entry, so a caller never learns something from a missing result. There is one vocabulary for every delivery failure in the app. So the string that you read here is the string that a `delivery_error` or a log line holds for that failure, and remote text never reaches this response, just as it never reaches that column |
| GET | `/healthz` | no auth. Runs `SELECT 1`. Without it, the endpoint returns 200 over a broken database |

**`/api/` is also registered explicitly, as a catch-all that returns `404`.** `ServeMux` matches the longest registered pattern, and `GET /` serves `index.html` (§8.1). Without that registration, `/api/typo` falls through to the root pattern and answers a misspelled endpoint with the HTML page under a `200`. A `curl` then reads markup for a JSON field that was never going to be there, and a client bug is invisible because nothing failed. The catch-all sits behind the same auth middleware as the rest of `/api/*` and answers in the ordinary error shape (§8.3). So the API surface fails honestly on a typo and does not pretend that the path exists.

**One consequence of that registration is explicit rather than accidental: a wrong method on a real path answers `404`, not `405`.** When the path of a request matches a pattern but its method does not, `ServeMux` synthesizes a `405` with an `Allow` header. But `/api/` is registered for every method, so `POST /api/config` matches *that* pattern. The mux never reaches the method-mismatch branch, and the request lands on the 404 of the catch-all like any misspelling. This is accepted deliberately, for a surface whose only non-browser client is `curl`. Both answers mean "no such request here". The correct method is in the table above, not in a header that nobody reads from a shell. One honest shape is better than two shapes that a client must tell apart. To recover the `405`, every method of every path must be registered separately, and the catch-all for typos is still necessary.

Reminder object: `{id, text, due_at, notified_at, done_at, extra_channels, delivery_error}`. **`pushed_at` is deliberately absent** (§4.1). `text` is returned **raw**, markdown links and all, and the client renders it (§9.10). `extra_channels` is **always a JSON array**, `[]` and not `null`, whatever the column holds. `notified_at`, `done_at`, and `delivery_error` are `null` when unset. Send raw timestamps. **All human formatting happens client-side**, so it follows the browser's locale and timezone (§6, §9.11).

Every mutating endpoint returns the affected reminder (or 204 for a delete). **The client follows every successful mutation with a `/api/state` fetch**, and does not splice the response into its list. The returned object makes the row redraw correct. The refetch makes the badge, the title, and the overdue/later split correct, and those are server-computed by definition (§9.3). This costs one extra request per user action, at a rate bounded by how fast a person can tap.

Request bodies are capped at **16 KB** via `http.MaxBytesReader`. `text` is limited to 1000 bytes (§8.3), but nothing else stops a client from streaming a gigabyte into the decoder first. A body over the cap is a 400, like any other malformed body. The server also sets `ReadHeaderTimeout: 10s`, `ReadTimeout: 15s`, `WriteTimeout: 15s`, and `IdleTimeout: 120s`. The defaults are unlimited, and this process must still be alive in a year. **`WriteTimeout` is also the number that `POST /api/push/test`'s send budget is derived from.** The budget is 12 seconds of sequential sends at 3 seconds each, deliberately under the 15. The one handler that legitimately spends seconds on third-party sockets is also the one whose whole value is the answer that it prints (§7.5).

`/api/state` returns the full `later` list, unpaginated. The client polls it every 45 s while visible and every 5 minutes while hidden (§9.3). At personal scale that is a few KB, and the simplicity is worth more than the bytes. It carries a **weak** `ETag` (`W/"…"`) and honors `If-None-Match` with `304`, which makes the steady-state poll nearly free. **The ETag hashes the body with `server_time` excluded**, because that field changes every second. A hash of the whole body never matches twice, so the `304` never fires. The weak form is not a precaution. It is the accurate label for exactly that exclusion. Two responses that share the tag are equivalent for every purpose of this client, but they are *not* byte-identical, because `server_time` has moved between them. A strong validator claims byte equality. On a `304` there is no body at all, so the client reads the clock from the standard `Date` header of the response. That header is present on both paths and needs no new mechanism (§9.3).

**`app.js` holds that ETag itself, in a variable, and sends it explicitly.** `/api/*` is `no-store` (§10.2), so the browser cache never revalidates on the client's behalf. There is no client-side persistence anywhere (§11), so a reload legitimately costs one full body before the cheap polls resume. Both of those are consequences, not oversights.

`/api/*` responses carry `Cache-Control: no-store` (§10.2). Cleared reminders are kept (§4.4) but **not exposed**: there is no history endpoint in v1. Undo works from the id that the client already holds.

### 8.3 Validation and errors

Every rejection is `{"error":"<lowercase sentence>"}` with a `4xx`. The client shows the string verbatim, so write it for a human.

**Every `5xx` carries that same one-field shape, with a fixed generic sentence**: `{"error":"internal error, see the server log"}`. It **never** carries an internal error string, a driver message, or a stack fragment. A 500 needs a defined shape, because two readers already expect one. §9.2 renders "the error string from §8.3" in a toast on any failed action, and §10.4 logs `4xx` and `5xx` alike. And the shape must be *constant* for the masking argument that governs `delivery_error` (§4.1). If a 500 carries the real error, a `modernc.org/sqlite` message or a wrapped `*url.Error` reaches a toast, and the same string reaches the log. So there is no single place to redact a path, a DSN, or a channel URL that is already embedded in it. The real error lives in the log line for that request (§10.4), where the id, the path, and the status already are.

- `text` — required, trimmed, non-empty after trimming, max **1000 bytes** (bytes, not runes: the limit exists to bound the push payload, and a markdown link costs ~60 of them). It must be valid UTF-8 and contain **no control characters**, newlines included. The capture control is a single-line `<input>` (§9.2), so a multi-line reminder can only arrive by `curl`. To accept one, the spec must define how a notification body, a shoutrrr message, and a `textContent` row each fold it. The 400 names the character class, not the byte.
- **Markdown links inside `text` are not validated.** The text is stored exactly as typed. A `[label](target)` whose scheme is not `http` or `https` is not a link: both renderers emit the whole match as literal text (§9.10). There is deliberately no 400 here. A 400 makes `[ticket](obsidian://…)`, `[doc](file:///…)`, and even plain prose like `see [note](internal)` impossible to enter, with no workaround. In exchange, it gives a guarantee that the renderer already provides on its own, and that `textContent` provides again behind it (§9.7). `flattenLinks` stays the server's only contact with the text (§9.10).
- `preset` — must match a configured `key`. A client that holds a stale `/api/config` can send a key that no longer exists. The 400 says so, and §9.2 says what the client does about it.
- `due_at` — any integer in `946684800 .. 4102444800` (2000-01-01 to 2100-01-01 UTC), **including the past**. Backdating is sometimes intentional. A backdated row lands in "Needs you now" with `notified_at` and `pushed_at` already set, so it never notifies (§4.1, §9.6). A value outside the range is a 400 that names both bounds. Nothing legitimate produces one: the picker cannot, and a preset cannot. The check stops a `curl` typo or a millisecond timestamp from becoming a row that every formatter downstream (§9.11) must render as a date tens of thousands of years from now. A current-epoch value in milliseconds, read as seconds, is around 1.79 × 10¹² of them, somewhere around the year 56 600.
- **Exactly one of `preset`/`due_at`** on create, and at most one on a PATCH. Both together is a 400. Neither on create is a 400.
- **PATCH field semantics.** A key **absent** from the object is unchanged. `extra_channels: null` or `[]` clears the list. `text` and the timing fields cannot be cleared: `text: null` or `""` is a 400, and there is no way to unset `due_at`. An empty object is a 400 (`"nothing to change"`). To tell absent from `null`, the handler needs presence detection, so decode into `map[string]json.RawMessage` and then decode each known key from it. **Unknown fields are rejected too, and that needs an explicit check, because the decoding does not reject them.** A map accepts every key that it is given. So after the known keys are consumed, the handler asserts that nothing is left in the map. If a key is left, it returns a 400 that names the **lexically first** leftover key. Go randomizes map iteration, so "the first one" must be chosen, not taken. Nobody trusts an error message that names a different key on every retry. This check is cheap, but it must be written: `DisallowUnknownFields` belongs to a struct decode, and presence detection ruled out a struct decode.
- `extra_channels` — max 16 names, de-duplicated server-side, and **sorted by name before it is written**. So a given set of names has exactly one stored form, and §4.1's test "the resulting list differs from the stored one" is an ordered array comparison, not a set comparison.
- `extra_channels` — every name must exist in `channels`, else 400 naming the unknown one. A *disabled* channel is accepted here and skipped at send time (§7.2).
- **On a `PATCH`, a name already stored on that row is accepted even if it no longer exists in `channels`.** Only names *not* already on the row are checked. Orphans are defined behavior on the send path (§4.1), and `nag channel rm` deliberately creates them. With the strict rule, the removal of a channel makes every reminder that still references it impossible to edit. The name survives in the editor only as the removable orphan chip of that row (§9.2), and no other row offers it. But the editor always sends the full list. So a typo fix in the text of that row gets a 400 on a name that the row already carried and that the user never introduced. The asymmetry is the point: you can carry an orphan forward, but you cannot introduce one. A client that wants to drop the orphan sends the list without it. That works because absence is how the list is cleared.
- **The server has no fallback to `default_preset`.** The client always resolves it: a chip and a bare `Enter` both send `preset`, and the picker sends `due_at`. So one place decides what "Enter" means, and it is the place that knows what the user was looking at.
- Unknown `{id}` → 404. Malformed JSON → 400.

### 8.4 Create, edit, and re-snooze are one path

`POST /api/reminders` and `PATCH /api/reminders/{id}` accept the same fields, and the client renders the same control for both (§9.2). **There is no `/snooze` endpoint.** A quick chip on a row, a new time from the picker, and a typo fix are all `PATCH`: one handler, one set of re-time rules (§4.1), nothing to keep in sync.

The `snooze_count` counter, now removed, was the only reason for a separate endpoint. `/snooze` and `PATCH` differed by a counter and nothing else, and once the counter is gone (§11), the two are the same request. A new time sent through `PATCH` on a cleared reminder un-clears it. A `PATCH` with only `text` leaves `done_at` alone.

---

## 9. Frontend

### 9.1 Visual direction

"Patina structure, Horizon palette". Generous spacing, pill-shaped actions, italic serif for times, humanist sans for everything else. Upcoming items are drawn in quiet ink. Only overdue items carry full contrast.

**Tokens.** The two value sets use identical names. The **light** set is declared on bare `:root`. The dark set redefines the same names inside `@media (prefers-color-scheme: dark)`. Nothing sets a `data-theme` attribute, so a rule must never be scoped to a `data-theme` attribute. The two sets:

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

Amber is **not** a straight inversion: `#E0A33E` has a contrast of ~2:1 on the light surface, so it is unusable there. Light quiet ink is `#5C6C72` (4.8:1), not `#67787E` (4.05:1, under the contrast floor). Only the sheet uses `--scrim` (§9.6). But `--scrim` is a themed color like the rest, so it belongs in this table and not as a literal value in a rule.

**The table above is the complete set of color tokens.** `web/app.css` also declares four non-color tokens. They do not change with the theme, so the table leaves them out: `--body` and `--time` (fonts, below), `--pad` (26px, 18px under 700px — §9.9), and `--item` (44px, the height of a wheel row in the picker — §9.6).

**Theme follows `prefers-color-scheme`. There is no override, no toggle, and no theme setting in the TOML.** A media query is the entire implementation. There is no persistence, no flash of the wrong theme on first paint, no server-side templating of `index.html`, and nothing to get out of sync between devices. The OS already knows whether the user wants a dark theme.

**Fonts are system stacks. Nothing is downloaded and nothing is embedded.** `web/app.css` declares exactly these two stacks:

```css
--body: "Optima", Candara, "Gill Sans", "Gill Sans MT", sans-serif;   /* UI */
--time: "Iowan Old Style", Palatino, "Palatino Linotype", Georgia, serif;  /* times */
```

So there is no `@font-face` and no `web/fonts/`, and the CSP says `font-src 'none'` (§10.2). The faces that define the look are licensed OS faces: Optima for the humanist sans, Iowan Old Style for the old-style serif. They cannot legally be self-hosted. A substitute pair shipped as woff2 makes the app look *less* like its design on the Mac and iPhone, where those faces exist. On Linux and Windows, the stacks fall back to Gill Sans and Georgia. The design stays the same, with slightly different letterforms. This variation across platforms is accepted and deliberate, and it is the only one in the app.

### 9.2 Layout

One column, single page. Top to bottom:

1. **Capture bar** — a single-line text input plus preset chips from `/api/config` in file order, then a final "Pick a time" chip. **A chip saves immediately on click or tap.** That tap is the one interaction of Goal 1. **On a successful save, the input is cleared**, and so is the channel selection (below). The bar is then ready for the next capture. The client is also idle again, which is the ordinary state in which a held reload (below) takes place. If a save fails, the text that you typed stays in the input, and a toast shows the error. The input is **autofocused on desktop only** (`min-width: 701px` and a `hover: hover` check). On a phone, autofocus opens the keyboard over the list on every load. That works against Goal 4 on the device where a quick glance matters most.
   **Every chip, including "Pick a time", is `disabled` while the input is empty or whitespace-only.** The chips enable on the first non-space character. Without this rule, a tap on a chip sends a request to the server, and the answer is a red toast that says `text is required`. A chip that is visibly not ready is a better way to say "type something first".
   The input carries `maxlength="1000"`, so the browser stops you at the limit while you type, not with a toast on save. `maxlength` is a hint, not the rule. `maxlength` counts UTF-16 code units and §8.3 counts bytes. So the server stays the authority, and the server can still reject an emoji-dense text of 1000 characters.
2. **Notification slot** — the single "Turn on notifications" control, or the iOS Home-Screen line, or the `denied` explanation (§9.8). It sits directly under the capture bar. **It is absent entirely when push works**, which is its steady state. It is above the lists because broken push is the one fault that you cannot notice by yourself: no notification arrives, and nothing else tells you.
3. **"Needs you now"** zone, with the overdue count in amber. Sorted by `due_at` ascending (longest overdue first). Each row has full-contrast text, the lateness in amber, a lifted background, and a 2px amber left spine.
4. **"Waiting quietly"** zone. Quiet ink, sorted by `due_at` ascending.
5. **Empty state** when both are empty: "Nothing waiting." / "Type above and pick when it should come back." This is the most-seen screen. Treat it as the reward, not as an error.

When the list of a zone is empty, the heading of that zone is hidden entirely. When both lists are empty, the empty state in point 5 replaces both zones.

**Row actions**: `Clear` (primary) and `Edit`. **On "Needs you now" rows only**, there are also re-snooze chips for presets with `quick = true`, each one a `PATCH` with that `preset` (§8.4). On desktop, the actions appear on `:hover, :focus-within`. On mobile, they always show. `Clear` shows a 5-second undo toast that calls `/undone`. **No confirm dialogs anywhere.**

`:focus-within` is necessary in that reveal. With `:hover` alone, a tab into a row reveals nothing. Every action behind the reveal is then focusable but invisible, and the keyboard cannot reach the chips. `Edit` is in the group because the row *text* cannot be a `<button>`. The text contains anchors (§9.10), and nesting interactive elements is invalid. So pointer users get the click target described below, and the keyboard gets a real button. Without `Edit`, the row editor has no keyboard path at all.

The re-snooze chips are for reminders that are already late, and this is why the zone split matters here. An `offset` preset resolves from `now` (§4.1). So "30 min" on a reminder that waits for next Monday quietly pulls it *forward* by six days. That is correct per the re-time rules, but nobody wants it. A "Waiting quietly" row gets `Clear` and the row editor. The editor offers the same chips in a context where the row's current time is visible next to them.

**Editing a row is the capture bar again.** A click on the row's text, on its time, or on the `Edit` action expands the row into the same control as the capture bar, prefilled. It has a text input that holds the **raw** text including any markdown links, the full preset chip row, the "Pick a time" chip, and the channel selector. It is the same component, with the same keys, the same behavior, and the same endpoint (`PATCH`, §8.4). `Esc` cancels. `Enter` saves the text and does not touch the time. A chip or the picker sets a new time. Nothing else is necessary to satisfy "rename it and move it".

A click on a **link** inside the row text follows the link and does not open the editor.

**Channels.** The capture bar has an "Also send to" affordance. It expands a row of channel chips from `/api/channels` (multi-select toggles, with disabled channels shown grayed and unselectable). **When `/api/channels` is empty, the capture bar does not render the affordance at all.** Fan-out is opt-in per §7.2 and a fresh install has no channels, so the default state of this control is "gone". By default no chip is selected, and **the selection resets after each save**. The client remembers nothing (§11). The same chips appear in the edit form and show the reminder's current `extra_channels`.

**The row editor hides its own channel affordance on a different condition, and this is deliberate. The editor renders its channel affordance when `/api/channels` is non-empty *or* when the row itself carries any names.** If the catalog is empty and the row carries names, the affordance shows only that row's orphan chips. You can remove each orphan chip with a tap, as described below. This looks like a special case, but it protects the general rule that a save never removes a name that you did not remove. Without this rule, `nag channel rm` on the last channel makes every later save of a row that references that channel send `[]`. The editor always sends the full list (below), and an affordance that is not rendered has nothing selected. The save then silently wipes the orphan names that §8.3, §4.1 and the orphan-chip row of §11 exist to preserve. Because the empty-list suppression applies to the capture bar only, both rules stay true at the same time: the editor always sends the full list of what its chips show, and an orphan survives until you take it off the row.

**The edit form has two asymmetries, and both mean "you can always take a name off a row".** "Grayed and unselectable" is a rule about *adding*, never about removing. The row editor is the only place where the difference shows:

- **A disabled channel that is already attached renders selected, grayed, and deselectable.** A chip that is unselectable in both directions lets `nag channel disable` silently make a name impossible to remove from the reminders that carry it. That is a mute switch that the app cannot undo.
- **A name on the row that `/api/channels` does not list renders as a grayed chip labeled with the name itself.** The chip is selected and deselectable. After you deselect it, you cannot select it again. These are the orphans that `nag channel rm` creates on purpose (§4.1). By definition they are invisible in the API's channel list. Without a chip of their own, the editor sends a selection that silently omits them, and every save silently removes them. The chip makes §8.3's rule ("you can carry an orphan forward, you cannot introduce one") reachable from a browser, not only from `curl`. If you leave the chip alone, the name survives the `PATCH`. If you tap it once, the name is gone.

So the editor **always sends `extra_channels`**, as the full list of what its chips show selected, orphans included. It never omits the key to mean "unchanged", because with orphans rendered, nothing remains that the client cannot see. More generally, **the editor's `PATCH` always carries every field that the editor shows.** It sends `text` and `extra_channels` on every save. It adds a timing field (`preset` from a chip, `due_at` from the picker) only when a chip or the picker chose a moment during this editing session. So "`Enter` saves the text" and "a chip commits a time" have the same request shape, and differ only in which of its fields changed. There is one body to build, one handler to read it (§8.4), and no branch that decides which keys to include. The absent-means-unchanged rule (§8.3) makes it harmless to send the unchanged fields. Because the editor sends an identical `extra_channels` again, that rule compares the resulting list against the stored one, and does not test whether the key was present (§4.1). A row with channels attached carries a small quiet marker. When `delivery_error` is non-NULL, the marker turns amber and carries the error as its accessible label. The row also **shows the error text in full when the row is expanded for editing**. A `title` tooltip alone is unreachable on the phone, and the phone is where you are when you notice the amber.

**Failure states.** A failed API call shows the `error` string from §8.3 in a toast. Nothing is applied optimistically, except the `Clear` undo path. A `401` from any call replaces the list with the token field, because a cookie can expire while a tab stays pinned for a year.

- **A failed poll is not a toast.** `/api/state` runs every 45 s unattended. A toast for each failure means a toast every 45 seconds for the whole short network outage, on a tab that nobody looks at. Instead, keep the last good list on screen. After **two consecutive** failures, show one quiet persistent line under the capture bar: "Not connected — last updated 3 minutes ago". The timestamp is the time of the last successful poll. The line clears itself on the next success. A poll failure never clears the list and never touches the badge. A stale list with a label is better than an empty one.
- **A failed user action *is* a toast**, immediately and on the first failure. The difference is that someone waits for the result.
- **A `400` that names an unknown `preset`** means that the server reloaded its config after this client loaded it (§5.5). It is the one error that the user cannot act on and that the app can fix itself. So on this specific 400, the client **fetches `/api/config` again and redraws the chip rows in place**: the chip row of the capture bar and of any open row editor. Typed text, the open editor, and the picker's pending value all survive. The full page reload stays under the ordinary quiet-moment rule below. A client that treats this 400 as a `config_version` change defers the repair exactly when the repair is necessary. That 400 can only arrive *from* a state that holds the reload: text is typed in the capture bar, or a row editor is open. The page then holds a reload that it was told to take, keeps the same stale chips, and gets a 400 on every tap. Because the client redraws the chips in place, "the app can fix itself" is true from inside the states that hold the reload.

**On a `config_version` change, the reload waits for a quiet moment.** The rule from §5.5 is unchanged: any difference means reload. But a reload in mid-sentence discards whatever is typed in the capture bar or half-chosen in the picker. So if the capture input is non-empty, a row editor is open, or the picker sheet is up, hold the reload. Take the reload on the next poll that finds the input empty, no row editor open, and the sheet closed. A stale client is a client whose chips can get a 400 (handled above). A client that discards your typed text is a client that you stop trusting.

**A poll must never do to a row what a reload does to the page.** This is the same rule at the level of a row. In practice, the rule applies to the 45-second poll, not to the config reload. The row editor *is* the row, expanded in place (above). So a naive "replace the list on every 200, redraw every row on every 304" (§9.3) discards half-typed text, drops focus, and can move the scroll position. It does this every 45 seconds, unattended, and also on the 304 path, which exists exactly to keep the time strings current. So the list is rendered **keyed by reminder id**, and a render pass:

- **skips any row with an open editor entirely.** No attribute is touched. If that row is no longer in the response, it stays on screen until the editor closes, and the next pass then reconciles it. A row cleared on another device while you edit it here costs one stale row for a few seconds, and that is acceptable.
- **updates each row node in place** and does not replace it. For an unchanged row, a redraw changes the time string and the lateness class, and nothing else. Reuse of the node preserves focus inside the row's action group, the `:hover` and `:focus-within` reveal (above), and the browser's own scroll anchoring.
- **removes only the rows that left the response, and inserts only the rows that arrived.** No other row is rebuilt, so the row that you are about to tap moves only when another row leaves or arrives. The server-sorted order with its `id` tiebreak (§8.2) makes this stable.

None of this needs a framework or a diffing library. It is a `Map` from id to node. It is also the only reason that `app.js` can hold a list, a poll, and an inline editor at once without a state manager.

Toasts live in one `aria-live="polite"` region, so both errors and the undo offer are announced. The toast's `Undo` is a real focusable button that `Tab` can reach. There is deliberately **no undo shortcut**: a 5-second `Ctrl+Z` window adds a fourth key binding, and the only one that works only for a limited time (§9.4).

While a create or save request is in flight, the control that submits it is disabled. So a double-tap on a chip cannot produce two reminders.

### 9.3 Badge semantics

**The badge shows the overdue count only, never the total.** Upcoming items must never light it up. That is exactly what makes Slack's icon meaningful.

Implement one `setBadge(n)`:
1. `navigator.setAppBadge(n)` if available (installed PWA, in Chrome/Edge/Safari). Firefox does not support the Badging API on any platform.
2. Always also: a canvas-drawn favicon with a count dot, swapped in through `link[rel=icon]`.
3. Always also: `document.title` prefixed `(n) `.

`setBadge(0)` is a full reset, not a no-op: `clearAppBadge()` rather than `setAppBadge(0)`, the plain `32.png` back in `link[rel=icon]`, and the `(n) ` prefix removed from the title.

**The drawn favicon is also specified exactly, because a tab strip is not part of the page and no rule of §9.1 applies to it.** It is a 32×32 canvas. `32.png` is drawn at full size, then a filled circle in the bottom-right quadrant: radius ~11px, `#E0A33E` (the dark-set `--late`) with a `#212A2E` numeral centered in it. Both colors are hardcoded, not read from a custom property. **The circle is drawn as a `#212A2E` disc of that radius, with the amber fill 1.5px inside it.** The dot then reads as one object over any part of the icon that it lands on. The app icon carries an amber row of its own (`32.png`, below). Without the dark ring, the two ambers merge into one blob at the size that a tab strip actually shows. The dot must stay legible on a light *and* a dark browser chrome. `prefers-color-scheme` describes the page, not the chrome, and no media query reports the color that Firefox painted its tab bar. So the pair is chosen once to work against both, and the theme rule does not apply here. The numeral is the count up to **9**, and the literal `9+` for a count above 9. At that point the dot means "several", which is all that a dot this small can show. The exact number is in the title and the list. The document's base title is the bare word `Nag`, and `setBadge(n)` writes `(n) Nag`. Read the base from a constant, never from `document.title`. Otherwise the prefix repeats into `(3) (2) Nag` on the second call.

Poll `/api/state` every 45s while the document is visible, immediately on `visibilitychange` → visible, immediately after every successful mutation (§8.2), and immediately on a service-worker message.

**These triggers overlap by design, so every `/api/state` fetch goes through one `AbortController`. Before you issue a fetch, abort the fetch in flight.** A mutation's immediate refetch often starts while a 45-second poll is still in flight. The trigger "immediately after every successful mutation" makes this overlap normal. Two responses from the same endpoint can arrive in any order. So a slow poll can answer *after* the refetch that overtook it, and re-render the list as it stood before the mutation. A row that you just cleared then reappears, and stays there for up to 45 seconds until the next poll corrects it. With one controller, replaced on each issue, the newest request is the only one that can write anything: **an aborted fetch updates nothing at all — not the list, not the badge, not the clock offset, and not the stored ETag.** The last item is the most important one. The ETag is held in a variable in `app.js` (§8.2). A stale response that overwrites the ETag with an older value does harm that lasts longer than the wrong render, and every later poll then revalidates against a body that the client no longer has on screen.

**A `304` re-renders too.** The ETag excludes `server_time` so that the steady-state poll is cheap (§8.2). So the common case is a poll that returns no body, while every row's time string has moved: `in 4 minutes` became `in 3 minutes`, and `now` became `2 min late`. The obvious `if (res.status === 304) return` freezes every time string on screen until the data changes, which on a quiet afternoon takes hours. So a 304 updates the clock offset from the `Date` header, re-renders the existing rows from the list already in memory, and re-arms the due timer. Only a 200 replaces the list. Both paths go through the keyed render of §9.2, so neither disturbs an open row editor or the focus inside a row. The same pass draws the badge, the title and the favicon, so these are also correct after a 304.

**A hidden tab keeps polling, at 5 minutes.** A suspended poll breaks Goal 4 outright. A pinned tab whose favicon you read is `visibilityState: "hidden"` by definition. So with a suspended poll, the count refreshes only when you focus the tab, which is exactly when the favicon stops mattering. 45 s is for the tab in front of you. 5 minutes keeps the icon correct without polling for a machine that nobody uses.

**`sw.js` also posts `{type:'state-changed'}` to every client on `push` (§9.5).** So a reminder that fires shows on the favicon immediately, not up to five minutes later. The message uses the "poll on a service-worker message" trigger above. Without this message, only `notificationclick` fires that trigger, that is, only after you already saw the notification.

**The badge is never subject to the push cooldown (§7.3).** The cooldown can hold a reminder for up to 30 minutes before its notification, but the reminder is in `overdue_count` the moment it comes due. The one-shot due timer below puts it on the favicon within one round trip. This property is essential, not incidental: it is the entire reason that a 30-minute rate limit on notifications is acceptable in a reminder app. Nothing here needs to know that the cooldown exists. The count is computed from `due_at` and `done_at`, and `pushed_at` is invisible to the API (§4.1).

The one-shot due timer below is armed regardless of visibility, and its poll runs even when the tab is hidden. Browsers throttle background timers to roughly one per minute. So the timer degrades into a slightly late poll, not a missed one. The 5-minute poll and the worker message are the fallbacks when the timer is late.

**Also set a one-shot timer for the earliest `later.due_at`**, and poll when it fires. The server computes the overdue/later split, and the client never computes it again. Without this timer, a row that comes due stays in "Waiting quietly" and reads `now` for up to 45 seconds, while the badge disagrees with it. One `setTimeout`, re-armed on each poll from the first element of `later` (the list is server-sorted, §8.2), turns the worst case from 45 seconds into one round trip. It is the cheapest possible fix, and it keeps "the list never disagrees with the badge" literally true.

**Clamp that delay to ~23 days**: `Math.min((due_at - now()) * 1000, 2_000_000_000)`, re-armed on each fire. `setTimeout` truncates its delay to a signed 32-bit integer. So any delay past 2 147 483 647 ms wraps and **fires immediately**. Without the clamp, one reminder six months out fires the timer at once. The timer polls, re-arms from the same first element, and fires again. The result is a tight loop against the server for as long as the tab is open. The clamp turns it into a harmless extra poll every few weeks.

**`manifest.webmanifest` is required**, not optional. The Badging API only works in an installed PWA, and on iPhone, Web Push only works from the Home Screen (§7.1). Minimum viable content: `name`, `short_name`, `id: "/"`, `start_url: "/"`, `scope: "/"`, `display: "standalone"`, `background_color` and `theme_color` from `--surface`, and `icons` at 192px and 512px plus a 512px `maskable`. `theme_color` can only hold one value, so it takes the dark `--surface`. So the manifest does not follow the OS theme.

`web/icons/` therefore holds exactly four PNGs: `192.png`, `512.png`, `512-maskable.png` (safe zone inside the inner 80%, per the maskable spec), and `32.png`. `32.png` is the base image that the canvas favicon draws its count dot onto. It is also the `badge` that the service worker names in `showNotification` (§9.5). **The notification's `icon` is `192.png`, not `32.png`.** Platforms render `icon` at roughly 64–192 device pixels, and they upscale a 32-pixel source into a blurry square. The `badge` is the small, almost monochrome glyph in the status bar, and 32 is the right size for exactly that slot. These are two different assets for two different slots. If you name the same file for both, the code looks tidier, but every Android phone shows a blurry notification icon. There is no `apple-touch-icon`: iOS 16.4+ takes the manifest icons for a Home Screen install, which is the only iOS path that the app supports.

**The server clock is the only reference for *when*, never for *where*.** `/api/state` returns `server_time` (and the `Date` header carries it on a `304`, §8.2). The client stores an offset on every poll and renders every relative string through a single `now()`. The server computes the overdue/later split, and the client never computes it again. So a phone with a wrong clock still shows the right lateness, and the list never disagrees with the badge. Timezone is a separate question, and the browser answers it (§6). The offset corrects skew, not zone.

**`now()` returns Unix seconds, and this is specified as exactly as the cookie encoding (§8.1).** Every timestamp that crosses the wire is Unix seconds (§4), and `Date.now()` is milliseconds. If you mix them, the symptom is that every row reads `19163 days late`:

```js
let offset = 0;                                     // seconds
const clockNow = () => Math.floor(Date.now() / 1000);
// on every 200: offset = server_time - clockNow()
// on every 304: offset = Math.floor(Date.parse(res.headers.get('Date')) / 1000) - clockNow()
const now = () => clockNow() + offset;              // seconds
```

So `whenText`'s `d = due_at - now()` is a difference of seconds (§9.11). The **two places that need milliseconds convert at the call site**: the one-shot timer below (`(due_at - now()) * 1000`), and the picker, which builds a local `Date` and sends `Math.floor(d.getTime() / 1000)` (§9.6). Nothing else in `app.js` touches `Date.now()` directly.

### 9.4 Keyboard

- `/` or `n` — focus the capture input
- `Enter` — save with `default_preset`
- `Esc` — clear the input / close the sheet / cancel a row edit

That is the whole list. **`Enter` does whatever "save" means on the surface where you press it**: the capture bar saves with `default_preset` as above, a row editor saves the text and does not touch the time (§9.2), and inside the picker sheet `Enter` activates `Set reminder` (§9.6).

**`Esc` acts on exactly one thing per press, innermost first: picker sheet, then row editor, then the capture input.** The three can be open at the same time, because you often open the picker *from* a row editor. One press that closes all of them discards an edit that you are still in the middle of. Closing the sheet returns focus to the chip that opened it (§9.6). So the second press acts on the editor and cancels it, and if the capture bar contains text, a third press clears it. With nothing open and an empty input, `Esc` does nothing at all.

**`/` and `n` are ignored when focus is already in a text field**: check `event.target` against `input`/`textarea`/`[contenteditable]` and return. **They are also ignored when the picker sheet or a row editor is open**, because nobody asked to focus the capture bar behind a scrim (§9.6). Without the first rule, the two keys are unusable in the capture bar, which is the one place where you are certain to type. The two keys exist for the moment after the input lost focus: you clicked a link in a row, or pressed `Esc`, and you want to return to the input without the mouse. `Esc` is the exception and is handled globally, because "get me out of this" must work from inside the field.

**There are no per-preset digit shortcuts**, and the chips carry no `<kbd>` hints. The capture input is autofocused, so a bare digit cannot both type and fire a preset. "Renew the domain in 2 years" is a reminder that you must be able to type. Each alternative costs more than it returns. `Tab` into the chip row adds a keystroke and a focus concept. Arming the row with `Enter` makes `Enter` stop meaning "save". `Alt`+digit already switches tabs in Firefox and Chrome on Linux and Windows. Chips stay reachable with `Tab` because they are real buttons, and that is enough.

### 9.5 Service worker

The worker must be served from **`/sw.js`**, not `/static/sw.js`. The scope of a worker comes from its path, and a worker under `/static/` cannot control the page at `/`.

**`app.js` registers it on every load, unconditionally and without waiting for a gesture.** `navigator.serviceWorker.register('/sw.js')` needs no permission. `Notification.requestPermission()` and `pushManager.subscribe()` do (§9.8). Early registration is what makes the rest of §9.8 possible: `registration.pushManager.getSubscription()` is how the notification slot decides whether the slot is necessary. Without the registration, the unconditional re-post (§9.8) has nothing to post. Registration is **idempotent**. A call on every load costs one no-op against the browser's registration store. It is also the only way that a `skipWaiting` deploy (below) reaches a tab pinned for months. Await the registration and reuse that object. Never call `register` a second time to "get" it.

**Everything push-related is feature-detected, in one place, once.** `'serviceWorker' in navigator`, `'PushManager' in window`, and `'Notification' in window` must all be true before any of this runs. A Firefox private window, an older desktop Safari, and any non-secure origin fail at least one of them. There, the naive `navigator.serviceWorker.register(...)` throws a `TypeError` that stops the rest of the load, the list included. Compute one boolean at startup. When it is false, skip registration entirely. Render the notification slot as the same quiet single line that §9.8 uses for `denied`, worded for the browser and not for the permission. **The lists, the badge, the picker, and every write path work without a service worker.** When push is not available, only push is lost, and nothing else. This is also why the list render never awaits registration (§9.8).

If `register` rejects (a syntax error in `sw.js`, a hard-refresh race, a browser that disabled workers by policy), log it to the console, set that same boolean false, and continue. There is nothing to retry and nothing the user can do. A rejected promise on the load path must not become an unhandled rejection that stops the poll from starting.

- `push` → parse the payload (§7.1), draw **exactly one** notification, then `postMessage({type:'state-changed'})` to every client, so an open-but-hidden tab updates its badge at once (§9.3). One push is one notification, whatever `n` says. The worker never loops over `texts` to call `showNotification` per entry, because that undoes the batching that the server just did.

  **The whole chain is wrapped in `event.waitUntil(...)`**, exactly as `notificationclick` is (below), and this is not optional. `showNotification` returns a promise. If a `push` handler returns before that promise settles, the browser treats the event as finished, and is then free to kill the worker mid-draw. The visible result is a push that arrives and shows nothing. *That* is the silent push that §7.1 warns about, the one that browsers answer by revoking the subscription. So `event.waitUntil` covers the `showNotification` call and the `postMessage` fan-out together, and the handler is `async` only inside that promise.

  | payload | title | body |
  |---|---|---|
  | `n == 1` | `texts[0]` | `whenText(due_at)` |
  | `n > 1` | `${n} reminders need you` | `texts` joined with ` · `, then ` · +${n - texts.length} more` when `n` exceeds what was sent |

  Options in both cases: `{tag: 'nag', renotify: true, data: {id}, icon, badge}`. `data.id` is present only when the payload itself carried an `id`. A sweep payload with `n == 1` carries an `id`. The test endpoint also sends `n == 1`, but without an `id` (§7.1, §7.5). Read the field, never the count.
- **`tag` is the constant `'nag'`, matching the server's `Topic` (§7.1), and it needs `renotify: true`.** With a constant tag, the tray holds at most one Nag notification, and a new one replaces the previous one. That is correct for an app where the tray is a nudge and the count lives on the badge (§9.3). Without a constant tag, three ignored notifications stay stacked, and each one points at the same list. `renotify` is not optional decoration. In Chrome, a replacement under an existing `tag` is **silent** by default. Without `renotify`, the second digest of the morning appears with no sound, no vibration, and no banner, and looks exactly like a bug.
- The relative string comes from the same formatter as the rows (§9.11). The worker carries its own copy, because `sw.js` cannot import from `app.js`. The worker also has no server offset, because there is no page to hold one. So its `now()` is `Math.floor(Date.now() / 1000)`: the same seconds convention as §9.3, without the correction. This is accepted: the notification is a nudge, and the list it points at is correct. The server already replaces each link with its label and caps the length of every `texts` entry (§7.1), so the worker needs no markdown code and no truncation logic at all.
- **A `push` event must never draw nothing.** If `event.data` is absent, does not parse, or carries no usable `texts`, call `showNotification` anyway, with the fixed title `Nag reminder`, no body, and the same `tag: 'nag'` and `renotify: true`. An early return gets the subscription revoked (§7.1). It also gives the user the browser's own generic "site updated in the background" notification, which is strictly worse than a vague one of ours. Wrap the parse in a `try`, and never skip the `showNotification` call.
- **No `fetch` handler, and no cache.** The worker exists for `push`, `notificationclick`, and `pushsubscriptionchange`. It never intercepts navigation. Offline support is not a goal, and a cache here is the fastest way to serve a year-old `app.js` to a pinned tab.
- `install` → `skipWaiting()`, `activate` → `clients.claim()`. A new worker takes over on the next load instead of waiting for every tab to close.
- `notificationclick` → `notification.close()`. Then, if a client exists, focus it, else open a new one. Then, **if `data.id` is present**, `postMessage({type:'focus-reminder', id})`. Without a `data.id`, the message is `{type:'state-changed'}` instead, and that covers two payloads. The first is a digest: several reminders mean there is no single row to scroll to. A pick of the first row flashes one arbitrary row while the other five sit below it without a highlight. The second is the test push, which is `n == 1` but points at no row at all (§7.5). In both cases, the whole answer is to focus the tab on a freshly refreshed "Needs you now" list. Wrap the whole handler in `event.waitUntil`. Find clients with `clients.matchAll({type: 'window', includeUncontrolled: true})`. Without `includeUncontrolled`, the search cannot see a tab loaded before this worker took over, and the worker opens a second window next to it.

  **The id travels in the URL when there is no client to talk to.** A `postMessage` to a client that `clients.openWindow` just returned is a race that the worker always loses. The page has not parsed `app.js` yet, so no `message` listener exists, and the highlight silently never happens. This is the *common* case, because a notification that you click usually arrived with the tab closed. So there are two paths, and they are not symmetric. An existing client gets `postMessage`: it already listens, and the worker must not navigate it away from whatever you were doing. A new client is opened as `clients.openWindow('/?focus=<id>')`. On load, `app.js` reads `focus` from `location.search` and treats it exactly like a `focus-reminder` message. It then immediately calls `history.replaceState({}, '', '/')`. So a refresh does not flash again a row that you dealt with an hour ago, and the URL never becomes something that you can bookmark. Anything with no `data.id` (a digest or a test push) opens the bare `/`.
- The page that receives a message: **switch on `type`.** `focus-reminder` → refresh state, scroll the row into view, and apply a ~2.4s highlight flash (respect `prefers-reduced-motion`). `state-changed` → refresh state and nothing else. It arrives on a tab that nobody looks at, so a scroll or a flash moves the page under whatever you return to.

  **If the id is in neither list after that refresh, the app is focused and nothing else happens**: no scroll, no flash, no toast, no error. Either the row was cleared on another device while the notification sat in the tray, or it was cleared here and purged after the retention window (§4.4). Both are ordinary: a notification is never the record of anything (§7.6). This applies identically to the `?focus=<id>` path, which is treated as the same message (above). So a bookmarked or re-opened URL that names a long-gone id lands on a normal list, not on a dead end. Do not fall back to a flash of the first row. A digest does not pick one arbitrary row, and that reason is even stronger when the correct row is gone.
- **`pushsubscriptionchange`** → re-subscribe with the configured VAPID key and `POST /api/push/subscribe` the new endpoint. Browsers rotate subscriptions on their own schedule. A missing handler is the single most common cause of "push worked for a month and then stopped".

  **The key comes from `GET /api/config`.** A worker fetch is same-origin and carries the session cookie by default (§7.1), so the authenticated read works. `event.oldSubscription?.options.applicationServerKey` is tempting, but browsers do not populate it reliably, so do not depend on it. A fetch also gets a *rotated* key, so the handler does not re-subscribe under the retired one. If the fetch fails (offline, or a cookie that finally expired), do nothing. §9.8's unconditional re-post then heals it the next time the page opens. That is the same recovery path, one page-open later, and a worker with no credentials can do nothing useful.

  This is the one handler that legitimately talks to the server. It is not on the notification-drawing path, so §7.1's "no network call" rule applies to `push` only.

These non-behaviors are deliberate, because they are the whole point of the app:

- **Dismissing a notification does nothing.** There is no `notificationclose` handler. If you swipe a notification away, the reminder stays overdue. That is Goal 3, and it is what every other tool gets wrong.
- **Notifications are not synced between devices.** If you clear a reminder on the laptop, the phone's notification stays on screen until the OS drops it. `tag` only collapses within one browser. The list is the truth, and notifications are a nudge toward it.

### 9.6 Picker

The picker is a bottom sheet that contains:

- **Two controls at the foot of the sheet: `Cancel` and `Set reminder`**, a ghost button and a filled one. The wheels and the calendar only move a pending value. **Nothing is written until `Set reminder`.** This is the one place in the app where a two-step commit is right. A chip is one interaction because it carries its own answer (§9.2, Goal 1). The picker exists because you choose between many values, and a save on every scroll-snap fires a `PATCH` per notch of the minute wheel. `Set reminder` is the whole write: a `POST` with a `due_at` from the capture bar, a `PATCH` from a row editor (§8.4). It closes the sheet on success. When focus is inside the sheet, `Enter` activates it. `Cancel`, `Esc`, and a scrim click all discard. The summary line above it is the confirmation that you read before you press it.
- **Live summary**: full date and time, and a relative line ("in 3 days"), both from §9.11's formatters. The live summary is non-negotiable, because it is what makes this picker better than a native control. If the chosen moment is in the past, say **"in the past — it'll be waiting in Needs you now, and nothing will be sent"** and **allow it**. The sentence says "nothing", not "no notification", because a backdated write skips its channels too (§4.1). A sentence that names only the OS notification reads as a promise that ntfy still fires. Backdating is sometimes intentional. The write path stamps both `notified_at` and `pushed_at` on anything that lands at or before `now` (§4.1). So the sentence is as true for a moment one second ago as for last Tuesday. It deliberately mentions no window and no cooldown. The client knows neither constant (§7.3), and because of §4.1, neither applies to anything a person can pick here.
- **Month calendar**: prev/next month, week starting per `week_start`. Today gets a thin ring, the selected day is amber-filled, and past days are dimmed (`--ink-faint`) but still tappable. "Today" and "past" come from `now()` (§9.3), not `Date.now()`.
- **No year stepper, and no upper bound on paging.** Next year is a dozen taps of `next month` away. That friction is a fair match for how rarely a personal reminder belongs a year out, and there is no preset past "next Monday" either. This choice needs one thing from another section: the summary line shows a year as soon as you leave the current one (§9.11). So you can verify the paging, and you do not have to guess how many taps you made.
- The chosen wall-clock time is interpreted in the **browser's** timezone and sent as an absolute `due_at`: `new Date(y, m, d, hh, mm, 0, 0)`, then `Math.floor(getTime() / 1000)` (§6, §9.3). A date and time inside a spring-forward gap has no such local instant. `new Date` normalizes it forward, exactly as Go's `time.Date` does on the server, so 02:30 becomes 03:30. The summary line shows the moment that will actually be stored, before you can save it. Accept that, as §6 does, and add no gap detection here either.
- **Hour wheel**: values `hour_min..hour_max` inclusive, from config. **The range is a hard constraint. There is no "show all hours" escape hatch.** This is a deliberate decision. Because the wheel cannot produce an hour outside the range, there is no "outside your usual hours" hint either.
- **Minute wheel** — `0..59` step `minute_step`.
- **Opening state, for a new reminder:** the time is `default_time` clamped into `[hour_min, hour_max]`. The date is **today if that time is still ahead of `now()`, otherwise tomorrow.** A sheet that always opens on today greets you every afternoon with the in-the-past warning before you touch anything. That reads as a broken default, not a considered one.
- **Opening state, when editing: the stored moment if it is still ahead of `now()`, else the new-reminder rule.** Take the reminder's `due_at`, clamp its hour into `[hour_min, hour_max]`, and set the minute to `00` (below). If the resulting moment is still in the future, the sheet opens on it. If not, the sheet opens exactly as for a new reminder: `default_time` clamped, today or tomorrow. **One rule covers both modes, and the sheet never opens on the in-the-past warning.** The rows that you re-time are usually the overdue ones, because the quick chips and the row editor are for them (§9.2). A sheet that opens on their stored date shows the warning before you touch anything. That is exactly the broken default that the create-mode rule avoids, on the one sheet whose whole job is to choose a moment still to come. The "still ahead of `now()`" check uses the value that the sheet *shows*, not the stored one. The minute reset can move that value back past `now()` even when the stored moment is ahead of it: a row due today at 14:30 reads back as 14:00. Nothing is lost when the old date is not shown. The row's current due time stays on the row itself, next to the open editor (§9.2). The picker was never the place to read it.
- When the sheet does open on the stored date, its hour is clamped into `[hour_min, hour_max]` and **the minute is always `00`**. It is never the row's actual minute, and never snapped to the nearest `minute_step`. If a `30min` chip left a row at 14:23 next Thursday, the picker opens on 14:00. Often the wheel cannot show the stored minute at all (`23` is not on a 15-minute step), and both alternatives are worse. A snap to the nearest step is a silent 7-minute move that looks like the stored value. A wheel that shows every minute, only in the edit case, means two different minute wheels. `00` is always a valid value, because `60 % minute_step == 0` is enforced (§5.5). It is visibly a *reset*, not a near-miss, which is the honest signal when you are about to re-time the row anyway. The hour clamp can *silently move* the time. A preset can legitimately produce 07:00 while `hour_min = 8` (§5.4), and that reminder opens on 08:00. This is accepted, and the summary line shows it before you can save. The wheel cannot show 07:00. The alternatives are to widen the range that the whole app is built around, or to refuse to open the picker on a row that a chip created. If you edit only the *date* of such a row, its time does move into the range. That is the cost of the hour range as a hard constraint (§11). An overdue row never takes this path. It opens on the new-reminder default above, whose minute comes from `default_time` and is on a `minute_step` boundary by §5.5.
- Wheels are plain scroll containers: `scroll-snap-type: y mandatory`, item height 44px, vertical padding equal to one item so that the first and last values can be centered, a centered selection band and edge fade in CSS, and the selection read back from `scrollTop` on a ~90ms debounce. Each value is also a real `<button>` that scrolls itself to center, so tap and keyboard keep working. No drag handling, no library.
- Sheet: `max-height: 94vh`, `overflow-y: auto`, `padding-bottom: calc(16px + env(safe-area-inset-bottom))`, closes on scrim click and `Escape`.
- **It is a real modal**: `role="dialog"`, `aria-modal="true"`, `aria-labelledby` the summary line, focus moved into the sheet on open and **returned to the chip that opened it** on close, and `Tab` trapped inside while it is up. It covers the page under a scrim, so anything focusable behind it is a trap in the other direction: you tab into a list that you cannot see. This is also why `/` and `n` are inert while it is open (§9.4).
- The wheels are 24-hour, while the summary line is locale-formatted. That inconsistency is accepted. The wheels are a constrained work-hours range where 24-hour is unambiguous, and the summary is the thing that you actually read back.

### 9.7 Interaction states

- `-webkit-tap-highlight-color: transparent` on all interactive elements — the default gray rectangle ignores border-radius.
- Designed `:active` (slight scale, ~0.96) so taps feel acknowledged.
- Focus rings via `:focus-visible` only, `2px solid var(--late)`, offset 2px. Keyboard users keep them. Touch users never see them.
- All reminder text and all error strings are written with `textContent`, links included (§9.10). **`innerHTML` appears nowhere in `app.js`.**
- **No inline `style` attributes in the markup.** `style-src 'self'` (§10.2) blocks a `style="…"` attribute parsed from HTML exactly as it blocks a `<style>` block. So nothing in `index.html` carries one.
- **CSSOM is a different question, and CSP does not govern it.** `el.style.setProperty('--offset', v)` and `el.classList.toggle(…)` from JS are unaffected by any `style-src` value. That is deliberate in CSP3, not a loophole. So `setProperty` is **available** if a value is truly dynamic. This point must be exact, because the opposite belief leads to a wrong rule: "use custom properties instead of inline styles" is advice to use the same API through a different property name. As specified, nothing actually needs it. The highlight flash and the sheet transform are class toggles with the values in `app.css`. The wheels are positioned with `scrollTop` (§9.6), which is not CSS at all. So the rule is: **prefer a class, and use `setProperty` only for a number that CSS cannot know.** The only dynamic attribute in the app remains `link[rel=icon].href` (§9.3).

### 9.8 Notification permission

Nothing about push is requested on load. Browsers require a gesture, and Firefox rejects a request without one outright.

- The app shows a single **"Turn on notifications"** button, and **only when it is necessary**: push is unsupported by this browser (§9.5, and then the slot shows the quiet line, not the button), or `Notification.permission !== 'granted'`, or permission is granted but `pushManager.getSubscription()` returns nothing, or `push_subscribed` is false, or this load's re-post below failed. When notifications work, the control is absent. There is no permanent settings control.
- **The `push_subscribed` condition is a backup for the two local checks. It stays deliberately, even though it is almost always redundant.** The re-post below is awaited before the slot is decided. So on a healthy device whose row the server dropped, the re-post already restored the row, the flag reads true, and the condition contributes nothing. That is the reason to await it. The condition still catches the case that the local checks cannot see: this browser holds a `PushSubscription` that no longer exists on the server and cannot be re-created. It is cheap, and the failure it guards against is silence. **It cannot, however, catch a failed re-post while another device's row is healthy.** The flag is instance-wide (§8.2), so it reads `true` on a laptop that receives no push, whenever the phone is subscribed. Only the re-post's own result shows that. So if `POST /api/push/subscribe` answers anything other than 2xx (not only if it throws), this load remembers it and shows the button. That is a fifth condition, next to the four above.
- On click: `requestPermission()`, then `subscribe({userVisibleOnly: true, applicationServerKey})`, then `POST /api/push/subscribe`, with `application_server_key` set to the `vapid_public` string that it just subscribed under. That is the value that the server records (§7.1).
- **`applicationServerKey` is bytes, not the string.** `subscribe` needs a `Uint8Array`. So `/api/config`'s `vapid_public` is decoded on the way in, from unpadded base64url to 65 bytes, and encoded again with the same alphabet on the way out (below). There are two conversions and one alphabet. They must be exact inverses, or the key check below compares a value against its own re-encoding and fails on a healthy install. `atob` accepts the string only after `-`/`_` are mapped back to `+`/`/` and the padding is restored. That is the whole decoder, six lines, and it is the only encoding code in `app.js`.
- If permission is `denied`, replace the button with one quiet line. The line explains that the browser blocks notifications, and that the user must allow them again in site settings. Script cannot prompt again for a `denied` permission, and to pretend otherwise is worse than to say so.
- **On every load, if a local subscription exists, `POST /api/push/subscribe` it unconditionally.** It is an upsert. So it costs one request, and it needs no way to ask the server "do you know *this* endpoint". `push_subscribed` is instance-wide (§8.2) and can never answer that. This re-post restores a row that the server deleted on a 410 or a 403 (§7.1), so the app does not silently stay without push.
- **That re-post is awaited before the notification slot is decided.** The slot's condition reads `push_subscribed`. Take a healthy install whose row the server dropped. If a `/api/state` lands first, it reports `false`, and the slot flashes "Turn on notifications" for a moment before it disappears. That is an invitation to fix something that is already fixing itself, on the one control whose whole design is to be absent when things work (§9.2). So resolve the local subscription and its re-post first, then render the slot from the state that follows. The lists render immediately either way. Only the slot waits.
- **Compare keys first, and post the key that you compared.** Encode `subscription.options.applicationServerKey` as **unpadded base64url**, and compare it to `/api/config`'s `vapid_public`. `base64.RawURLEncoding` is what `webpush.GenerateVAPIDKeys` produced, so padding or standard base64 here turns every comparison into a false mismatch. On a mismatch, `unsubscribe()` and subscribe again with the configured key before you post. Whatever survives that check goes in `application_server_key`. A re-post of a subscription still bound to a retired keypair fixes nothing, because the push service keeps rejecting it. This is the client half of making key rotation recoverable (§7.1).
- **A readback that is `null` or unavailable is not a mismatch.** Keep the subscription, and re-post it with the configured `vapid_public` as its `application_server_key`. That is the value that the comparison checks against. Every browser new enough to reach this line populates `options.applicationServerKey` (Chrome 42+, Firefox 46+, Safari 16.4+). So a null is a theoretical corner case, and the failure actually worth avoiding is the naive reading. If a null counts as a mismatch, the client runs `unsubscribe()` and `subscribe()` on a **working** subscription on every page load. That is the one thing that this whole re-post exists to prevent. **This costs the automatic recovery, and the cost must be stated, because the obvious sentence to write here is false.** A post of the configured key makes the row's `vapid_public` equal to it by construction. So a subscription that truly *is* bound to a retired keypair takes §7.1's **matching** branch on its 403: an ERROR line every sweep, and the row kept. From the server's side, a matching key means a bug or a push-service problem, not a stale key. The boot mismatch WARNING is blind in the same way, for the same reason. So in this case the fix is manual. **Reset** the site's notification permission in the browser. Reset it, do not block it, or you get the `denied` line instead of the button. The reset drops the local subscription and puts the button back, and the next `404`/`410` removes the stranded row (§7.1, §8.2). `POST /api/push/unsubscribe` with `curl` does the same thing from the other end. That is the right trade: the alternative unsubscribes and re-subscribes a working subscription on every load of every device, to rescue a case that no shipping browser produces. (§9.5's warning is about whether `pushsubscriptionchange` populates `event.oldSubscription` at all. That is a different question from what a live subscription's `options` holds.)
- On iOS, when the page does not run standalone, the same slot explains that notifications need the app added to the Home Screen first (§7.1).

### 9.9 Mobile (≤700px)

Rows wrap: the text takes the full width on its own line, then the time goes bottom-left and the actions bottom-right (`margin-left: auto`). Container padding 18px, tap targets ≥40px, reminder text 16px. The capture input **must stay at 16px**, so that iOS does not zoom on focus. The capture input is **not** autofocused here (§9.2).

### 9.10 Reminder text: one markdown link form

There is no `url` column. A reminder that needs a link carries it inline in the text, as `[label](https://example.com/x)`. That is also exactly the output of the deferred bookmarklet, with no extra field to design.

**The whole grammar** is that one form. No emphasis, no code spans, no bare-URL autolinking, no reference links. Anything else is literal text.

- Match `[label](target)`, where `label` is **non-empty** and contains no `[` or `]`, and `target` contains no whitespace and no `)`. Non-greedy, left to right: `\[([^\[\]]+)\]\(([^\s)]*)\)`. An empty label is not a link. Rendered as a link, an empty label gives an unclickable empty anchor and flattens to an empty push body.
- **A match is only rendered as a link if `target` has scheme `http` or `https` and contains no `(`.** Anything else is emitted as the whole match, literally, brackets and all. There is no server-side rejection to pair with this (§8.3). The renderer is the only side where a bad scheme can become an exploit, and it is the side that refuses.
- The `(` clause keeps a paren-bearing URL from failing *silently*. `[Foo](https://en.wikipedia.org/wiki/Foo_(bar))` matches with `target` truncated at the first `)`: a link that looks fine and returns 404. A refusal of any target that contains `(` shows visible raw markdown instead. That tells you to **percent-encode the parens** (`%28`, `%29`), the one documented workaround. The deferred bookmarklet (§1) encodes them before it builds its `[title](url)`.
- Render by walking the matches and appending nodes: `document.createTextNode` for the runs between, and for each link an `a` with `textContent = label`, `href = target`, `target="_blank"`, `rel="noopener noreferrer"`. **No `innerHTML`, no parser reuse from a library** (§9.7).
- Links do not use `--late`: `color: var(--ink)`, with an underline in `--ink-faint`. Amber means **something needs you**: lateness, and a delivery failure on a row that already fired (§9.2). It never means "this is clickable" (§9.1).
- On the server there is exactly one helper, `flattenLinks(text)`. It replaces a match with its `label` **only when that match renders as a link**, with the same gate as above: scheme `http` or `https`, no `(` in the target. Anything else stays exactly as written, brackets and all. Both halves of the gate are necessary. A bare "replace every match with its label" flattens `[a](mailto:x@y.z)` to `a` while the row renders it literally, and turns `[a](javascript:alert(1))` into `a)`. Push payloads (§7.1) and shoutrrr messages (§7.2) use it, so neither shows raw brackets. It is the only place where the server touches text.

The one shared risk is that the parser exists twice: in Go (`flattenLinks`) and in JS (the renderer). **`sw.js` needs no copy of it.** The push payload's `text` arrives already flattened (§7.1), so the worker never sees a bracket. The only thing that the worker carries its own copy of is the time formatter (§9.5, §9.11). Keep both copies to the single regex above, and put the same table of cases behind each:

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

`flattenLinks` runs the same table and must agree on every row. Whatever renders as literal text stays byte-identical in a push payload and in a shoutrrr message. Only a real link collapses to its label.

### 9.11 Time strings

Two functions in `app.js` format time strings, and nothing formats a time outside them.

**`whenText(due_at)`** is the short form. Three places read it: a row's time (§9.2), the picker's relative line (§9.6), and the notification body (§9.5, through the worker's own copy, because `sw.js` cannot import from `app.js`).

`d = due_at - now()`, where `now()` is the skew-corrected clock from §9.3. The worker has no offset, so its copy uses the device clock. **First match wins, top to bottom.** The conditions overlap by design, so the order is the specification.

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

"Calendar day" and "calendar year" are the **browser's**, consistent with §6. **From `today` downward, every condition is a calendar comparison, never a duration.** A calendar comparison is the difference in days between the two local midnights. So the weekday row is a difference of 2 through 6, and the row above it is exactly 1. A mix of the two makes a boundary drift by up to a day, depending on when you look. `d < 6*86400` on a Monday evening reaches Sunday, but the same arithmetic on a Monday morning stops at Saturday. The same reminder then reads `Sunday at 9:00` or `12 April at 9:00`, depending on when you glanced at it. The three rows at the top (`late`, `now`, `in N minutes`) are durations, because that is what they mean. The year is a comparison against the current year, not a distance. A date 8 days out that falls in January reads `2 January 2027`. A date eleven months out in the same year carries no year that it does not need. Without this rule, a row paged fourteen months ahead looks the same as one two months ahead: the same string, a year apart. Each threshold uses the floor of the larger unit, so 90 minutes late is `1 h late`. Every count is singular at 1 (`1 day late`, `in 1 minute`).

**`<time>`, `<weekday>`, `<day month>`, and `<day month year>` are locale-formatted. The surrounding words are not.** `Intl.DateTimeFormat` with an `undefined` locale renders the clock the way the device wants it (`09:00` or `9:00 AM`), and gives the weekday and month names in the device's language. The connective copy (`late`, `at`, `today`, `in`) is English, exactly like every other string in the app. §6's rule was always about *format*, never about a translation of the UI. This is the one place where that distinction is visible enough to state.

The picker's relative line uses the future rows above verbatim. As soon as `d <= 0`, it replaces the whole string with the fixed past sentence from §9.6. It never says `late` about a time that you have not saved yet.

**`fullText(due_at)`** is the long form. Only the picker's summary line uses it (§9.6). That line has room for it, and it is the one place where you commit to a specific moment, not glance at one. It gives weekday, day, month, and time, all locale-formatted, with the time emphasized: `Saturday 15 August at **09:00**`. It uses `Intl.DateTimeFormat(undefined, {weekday: 'long', day: 'numeric', month: 'long'})` plus the same `<time>` as above. The word `at` is the only English in it. **The year appears under the same rule as in `whenText`**: only when it is not the current year. The picker *can* page into another year (§9.6). At exactly that moment, you want the summary line to confirm the year before you save.

---

## 10. Deployment

Multi-stage build, with a `FROM scratch` final image. Copy `ca-certificates.crt` from the builder, because both Web Push and shoutrrr make outbound TLS calls.

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
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

`main.version` is the only build-time variable. `nag version` prints it, and the boot log shows it (§10.4). It defaults to `dev`, so the documented build is `docker compose build --build-arg VERSION=$(git describe --tags --always --dirty)`. So a boot line that reads `dev` gives information by itself: someone built this image by hand.

The Dockerfile has no `VOLUME` declaration. The mount points are bind mounts declared in compose, and `VOLUME` only adds anonymous volumes for anyone who forgot the bind mounts.

**`.dockerignore` is required. It is not only good practice.** The build context is the project directory. §10.1 tells you to put `data/` there, and §5.2 tells you to put `nag.env` there. So a bare `COPY . .` sends your VAPID private key and your database into the build, and stores them in the cached builder layer. The final image is `FROM scratch` and contains neither the key nor the database. That is exactly why this leak is easy to miss. Exclude at least `data/`, `config/`, `nag.env`, `.git/`, and `*.md`. The build needs `go.mod`, `go.sum`, `cmd/`, `internal/`, and `web/`.

The builder stage runs on the build host's platform and cross-compiles to the target's platform. So a multi-arch build needs no emulation: the final stage has no `RUN`, and the CA bundle and `/empty` that it copies are the same bytes on every architecture.

```yaml
services:
  nag:
    image: ghcr.io/kamalf/nag:latest
    build: .
    volumes:
      - ./data:/data
      - ./config:/config
    env_file: ./nag.env
    ports:
      - "127.0.0.1:8080:8080"
    restart: unless-stopped
    mem_limit: 256m   # a ceiling for a runaway channel reply (§7.2)
```

**`compose.yaml` assumes no particular reverse proxy.** It publishes the port on loopback only. So the host's own HTTPS proxy can reach the app, and nothing else can. The app is never on plain HTTP from the internet. That proxy must forward the original `Host` header. For a browser that sends no `Sec-Fetch-Site`, §10.2's same-origin rule falls back to a comparison of `Origin` with `Host`. A proxy that rewrites `Host` to `127.0.0.1:8080` makes that fallback return a `403` on every mutating request.

**caddy-docker-proxy is an opt-in second file**, `compose.caddy-docker-proxy.yaml`, that Compose layers on top of `compose.yaml`:

```yaml
services:
  nag:
    networks: [caddy]
    labels:
      caddy: nag.example.com
      caddy.reverse_proxy: "{{upstreams 8080}}"

networks:
  caddy:
    external: true
```

One line in `.env` enables the add-on, once per host: `COMPOSE_FILE=compose.yaml:compose.caddy-docker-proxy.yaml`. `.env` is Compose's own project file, not `nag.env`. So every documented command stays a plain `docker compose …`, on a host with the add-on and on a host without it. The add-on is a layered file, not a full second copy. So the service is defined once, and an edit cannot make two copies of it differ. With the add-on, the loopback port stays published. It is unused on that host, and it does no harm. `.env` is per-host, so `.gitignore` and `.dockerignore` both name it.

**The image is published to GHCR** as `ghcr.io/kamalf/nag`. A push of a `v*` tag runs `.github/workflows/release.yml`. The workflow builds `linux/amd64` and `linux/arm64` with `VERSION` set to the tag, and pushes `:<version>` and `:latest`. It logs in with its own `GITHUB_TOKEN`, so there is no secret to create. `compose.yaml` names both `image:` and `build: .`. When the image is missing, Compose pulls it. When the pull fails, Compose builds the checkout instead. `docker compose build` builds the image locally under the same name. To update, run `docker compose pull`, then `up -d`. `up -d` alone reuses the image that is already on the host.

### 10.1 Scratch-image consequences

- **No `/tmp`.** SQLite needs a temp directory for some operations, so the image copies one in and sets `TMPDIR=/tmp`. Without `/tmp`, you get a failure that looks like corruption. **The copy is `/empty` → `/`, not the obvious `/empty/tmp` → `/tmp`, and this choice is deliberate.** A `COPY` of a directory copies its *contents*, not the directory. So in the obvious form, `/tmp` is a destination path that the builder creates implicitly. Its mode and ownership are then whatever the builder decides, not the `1777` and the uid that you asked for. When you copy the parent, `tmp` itself is one of the copied entries and carries its own metadata. So the mode of `tmp` is the one set in the builder, and `--chown` applies to `tmp`, because `tmp` is a copied file and not a created path. If `/tmp` ends up root-owned and `0755`, the first temp file that SQLite needs fails with a permission error under uid 65534. That is the same failure that looks like corruption, and its cause is one layer further from where you expect to look.
- **No `/etc/passwd`**, so `USER 65534:65534` is a bare numeric uid. With no shell in the image, there is no way to fix ownership from inside it. **The app therefore runs non-root against host bind mounts that you `chown` once, before the first `up`:**

  ```
  mkdir -p data config
  sudo chown -R 65534:65534 data config
  ```

  These are two README steps, and they come before the two in §5.2. Bind mounts are used instead of named volumes, for three reasons. First, Docker creates a named volume root-owned on first use. The same `chown` then becomes a `docker run --rm -v <project>_nag-data:/data alpine chown …` command that breaks when the compose project is renamed. Second, the §10.3 backup procedure becomes `cp data/nag.db*` on files that you can see. Third, a wrong uid fails visibly at boot (`refuse to start`, naming the path — §4.3), not after the first write. A fix after that point means a change of ownership under a running container. That is why this is a decision and not a note.
- **You cannot reach `/healthz` with curl**, because there is no curl. `nag healthcheck` builds its own URL from `NAG_ADDR`. It keeps the port. It uses the host as given, unless the host is empty, `0.0.0.0`, or `::`. In those cases it dials `127.0.0.1`. So `:8080` → `GET http://127.0.0.1:8080/healthz`, and `127.0.0.1:9000` → that host and port. The timeout is 2 seconds, and the exit code is 0 only on `200`. There is no second env var, and the check stays correct if `NAG_ADDR` is later narrowed to one interface.

  **Nothing acts on the result.** `restart: unless-stopped` does not restart an unhealthy container, and the stack has no autoheal sidecar. So this is a status that you read in `docker ps`, not a self-heal. That is deliberate. The check catches a database that became unopenable (§4.3). A restart loop against a bad disk or a wrong `chown` replaces one visible `unhealthy` status in `docker ps` with a log that nobody reads.
- **The binary is PID 1**, so it gets no default signal handling. Handle `SIGTERM` and `SIGINT`: `http.Server.Shutdown` with a ~5s timeout, cancel the sweep's context and **wait for the sweep goroutine to return**, then `db.Close()` so that SQLite checkpoints the WAL. Without this, every `docker stop` waits the full 10s and then sends SIGKILL mid-write.

  **The sweep runs under a context that the signal cancels, and the wait for it is not optional.** A tick alone can legitimately run longer than Docker's 10-second stop grace period: phase 2 sends sequentially at 10 s each, and phase 3 carries a 20-second budget (§7.3). So with "stop the ticker, close the database", `db.Close()` runs while a tick still writes `delivery_error` and still deletes subscription rows (§7.3). That is a mid-write close that comes from the inside, not from SIGKILL. A cancel, not only a stop, also keeps the wait short. **Every outbound call in the sweep is bounded by a context derived from the tick's.** So cancellation ends the in-flight send at once, and the tick then stops at the next row boundary, without working through the rest of its list. A shoutrrr send is *abandoned*, not aborted (§7.2). The wait returns immediately either way, and the abandoned goroutine holds no lock and dies with the process. So the wait is bounded at roughly a couple of seconds: one ended send plus one row boundary. It is bounded *because* this app never issues a send outside a context (§7.1, §7.2, §7.3). `db.Close()` and its WAL checkpoint run after the goroutine returns, and that is the whole reason the order is written down.

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

Nothing is loaded from another origin (§2, §9.1), so a strict CSP costs nothing and blocks every injected script. `data:` on `img-src` is for the canvas favicon. `font-src 'none'` is not a typo and not laziness. Fonts are system stacks (§9.1). Locally installed faces are not fetches, and CSP never sees them. So *no* font request is legitimate, and the directive says exactly that. `style-src 'self'` also forbids a `style` attribute written into the HTML, and §9.7 makes that a rule, not a surprise. The directive leaves JS style changes through CSSOM untouched, and §9.7 also states that explicitly, so that nobody mistakes the rule for a technical impossibility.

**Caching.** `/api/*` gets `Cache-Control: no-store`. The embedded assets (`index.html`, `app.js`, `app.css`, `sw.js`, `manifest.webmanifest`) get `Cache-Control: no-cache` plus an `ETag` derived from the embedded content. So each load is one cheap revalidation, and a deploy takes effect immediately. With `max-age` on `app.js`, the tab pinned for a year (§8.1) runs last year's code, and a `config_version` reload loads the same stale bundle again. Icon URLs do not contain a content hash, so give them `no-cache` too. At this scale, the revalidation costs almost nothing.

CSRF needs no token. Two rules apply, both on every mutating request. **First, a browser request must be same-origin.** Go's `http.CrossOriginProtection` reads `Sec-Fetch-Site` (every browser since 2023, iOS 16.4 included). If that header is absent, it compares `Origin` with `Host`, and a mismatch is a `403`. A request with neither header comes from a non-browser client (`curl` with the bearer token), and passes. `SameSite=Lax` alone is not enough, because it is about the *site*. A page on a sibling subdomain (`blog.example.com` next to `nag.example.com`) is same-site, so the browser sends the cookie with an empty form POST. **Second, reject any mutating request that has a body and a non-JSON `Content-Type`.** An HTML form cannot send a JSON `Content-Type`.

**Bodyless mutations are exempt from the JSON rule**, and this exemption is a design decision, not a gap. `POST /api/push/test`, `/done`, `/undone`, and `DELETE /api/reminders/{id}` take no body. A header requirement on these endpoints only breaks the `curl` commands that the endpoints exist to serve (§7.5). In a browser, the same-origin rule covers them: a form on another origin, sibling subdomain included, is refused before the cookie is read. Also, a cross-origin form cannot carry the `Authorization` header at all. `/logout` is exempt from the JSON rule too. The worst that a forged form can do there is log you out of your own browser, and from another origin the same-origin rule refuses it anyway.

### 10.3 Backups

The database is the only copy of all your data. **With WAL on, a copy of `nag.db` alone can be a corrupt file**, because the recent writes live in `-wal`. Either stop the container and copy `data/nag.db*`, or build your own backup procedure on `VACUUM INTO`. A clean stop (§10.1) checkpoints the WAL into `nag.db` and removes `-wal` and `-shm`. After a clean stop, `nag.db` alone is complete, and the glob copies whatever exists. Keep `./data` on local disk. WAL misbehaves on network filesystems, so a bind mount that points at NFS is the one way to break this setup.

One more trap: with the caddy-docker-proxy add-on, the container must join the network that caddy-docker-proxy watches, or caddy silently ignores the labels.

### 10.4 What gets logged

Logs are the only observability (§2). So they must stay readable, and with a 45-second poll, a naive access log buries everything that matters.

- **Four things are never logged, in any line, at any level**: `NAG_TOKEN` (or a submitted token, also on a failed `/login`), a `channels.url`, a push endpoint in full (log the truncated form from §7.5 instead), and **reminder `text`**. The first three are secrets or capabilities. `text` stays out so that you can paste the log into an issue or give it to someone who helps you debug. Every line that has a reason to show the text shows the id instead. The per-push line names ids and `n`, not the words of the push. If you need the text, the line gives the id, and the database has the text.
- **One line per mutating request** (`POST`/`PATCH`/`DELETE`) at INFO: method, path, status, duration, and the affected reminder id. The id comes from the path when the path has one, and from the response on a create. **The push endpoints have no reminder id and must not print what they do have.** `/api/push/subscribe` and `/api/push/unsubscribe` log the truncated endpoint form from §7.5 in that field, never the URL that they received. `/api/push/test` takes no body at all (§7.5), so it has no single endpoint to truncate. Its line carries **how many subscriptions it attempted and how many rows it deleted**. These two counts are all that it knows, and together they answer the question that a reader of that line has. `/login` and `/logout` log neither an id nor a token (above). The id makes an accidental `Clear` recoverable after the 5-second undo toast is gone. There is no history view in v1 (§8.2), so `POST /api/reminders/417/done` in the log is the only surviving record of which id to `POST …/undone`. This fact is worth knowing before you need it.
- **One line per `4xx`/`5xx`**, whatever the method — including every failed `/login`.
- **Successful `GET`s are not logged.** Otherwise `/api/state` and `/healthz` fill almost the whole file.
- The sweep logs its decisions, not its ticks: rows marked, rows skipped as too late with their ids (§7.3), rows purged, subscriptions deleted, and every delivery failure. Because a notification is now a digest, the sweep logs **one line per push**, with `n`, the ids it covered, and how many subscriptions it went to. That line is the only record of which reminders a given notification was about. You will ask "why did it tell me about six things". It names ids, never texts (above), and that answers the question just as well. A tick that marks rows but holds them logs that too: the number of held rows, and how many seconds remain in the cooldown window. Rows that phase 3 dropped at its deadline are logged too, with their ids (§7.3).

**Every line in this section is INFO or above, and there is no log level to configure.** `slog`'s text handler defaults to INFO. So a line at DEBUG is a line that nobody ever sees. Such a line needs a `NAG_LOG_LEVEL` that §5.1 does not have, set on a container that you must restart, only to read a line about a sweep that already ran. The held-rows line in particular lets you tell a 20-minute silence between a due time and a notification from a broken sweep. A broken sweep is the first thing that you suspect. So it is INFO, kept to one short line per tick that actually holds something. A quiet instance stays quiet, because the sweep logs decisions, not ticks.
- At boot, one line each: **build version** (§10), listen address, resolved DB path, resolved config path (and whether it was just written from the default, §5.3), timezone, preset count, `config_version`, subscription count. These eight lines answer "what is this instance actually running". The first four are the ones that you use most: a version, an address, and the two paths. The VAPID-mismatch WARNING (§7.1) appears with them only when it applies, and that is exactly when you need it.

---

## 11. Decisions log

| Decision | Rationale |
|---|---|
| TOML, not YAML | Typed decoding in Go, no indentation or implicit-typing traps, hand-edited by one person |
| shoutrrr in-process, not an Apprise sidecar | Apprise adds the only Python to the stack. shoutrrr is a Go library that covers ntfy, Telegram, Matrix, Discord, Gotify, SMTP, and generic webhook |
| Closed set of preset kinds | Free-form natural-language parsing is a permanent maintenance cost. Chips are valuable *because* they are predictable |
| Poll every 30s, not per-reminder timers | Survives restarts, no rescheduling on edit, easy to debug |
| Mark before sending — every lifecycle write lands before any socket opens | A missed notification is visible in the list. A notification storm is not recoverable |
| At most one push per 30 min, carrying everything since the last one | Several reminders that come due around the same time are one message, not five. The badge and the list are never rate-limited (§9.3). So the cooldown delays the notification and never the information in the list, and that is the only reason a reminder app can rate-limit reminders at all |
| Do not notify for anything over 30 min late | After an outage, the list is the recovery mechanism. The number is the same as the cooldown, deliberately, so there is one rule to remember: nothing older than half an hour pings, and no more than one ping per half hour |
| The too-late gate is checked at mark time, never at push time | A check in phase 2 lets the cooldown discard exactly the notifications that it exists to batch, because a legitimately held row is by definition up to 30 minutes late by then. That bug is silent |
| `pushed_at` is a column, `last_push_at` is memory | The *held set* must survive a restart, or the deferral loses notifications. So it is a query on durable columns. The *window* can reset on restart: that costs one extra notification, never a missing one |
| A constant push `Topic` and a constant notification `tag` | With a constant `Topic`, the push service keeps at most one pending message per device. That bounds the burst that a phone gets after a day offline. The old per-reminder topic collapsed nothing, because ids differ by construction. The matching `tag` keeps the tray at one notification. The `tag` needs `renotify: true`, or the replacement is silent |
| The notification's count is not the badge's count | `n` is what this push is about. The badge is everything overdue. They differ constantly and neither is wrong, so the notification never tries to state the total |
| Fan-out is not rate-limited or coalesced | A channel is an addressed output that you asked for on one specific reminder, usually with a different address per reminder, so there is nothing coherent to merge. Its value is "tell me on Telegram *now*", and a 30-minute deferral removes that |
| Fan-out runs after the push, under a 20-second whole-phase deadline | `LIMIT 50` bounds the row count and a 10-second client timeout bounds one send, but neither bounds a *tick*. 50 rows × 16 dead channels × 10 s is over two hours in one goroutine, and the digest push waited behind it. With the fan-out after the push and a cap on the phase, an unreachable channel can cost channel messages but can never delay a notification past the next tick. The 20 s runs from a clock read taken when the phase starts, not from the tick's `now`. By that time, sequential sends to sleeping devices can put that `now` 30 seconds in the past. The budget bounds every send: each send takes the smaller of 10 s and the remainder. A budget checked only between rows lets one 16-channel row run three minutes past it. shoutrrr's `Send` takes no context, so the bound is a goroutine and a `select` that abandons the send (§7.2). The bound is the app's, not the library's |
| `POST /api/push/test` sends at 3 seconds each under a 12-second budget, not the sweep's 10 | With the sweep's timeout, sequential sends put the handler past the 15-second `WriteTimeout` after two dead endpoints. The `curl` then got no answer at all, in exactly the silent-notifications case that the endpoint exists for. A probe optimizes for an answer to the caller: a subscription that cannot reply in 3 s reports `timeout`, and that is the diagnosis. Each send takes the smaller of 3 s and the budget's remainder, because five sends at 3 s each reach the write deadline again. Rows that the budget never reached report `not attempted` and are not omitted, so the array always has one entry per subscription |
| A backdated write sends nothing at all, channels included | Fan-out only covers rows that the sweep just marked, and a backdated row arrives marked. So the alternative was a Telegram call on the request path, for the one write that by definition is not urgent. §4.1, §7.6 and the picker's own sentence state that channels are included, because "no notification" can read as though ntfy still fires |
| Two config hashes: one for the log, one for `config_version` | `/api/config` exposes presets as `{key,label,quick}`, so a single hash over it is blind to `offset`, `at`, `days`, `weekday`, `timezone`. A *swap* gated on that hash discards a real edit and logs `config unchanged` while it does so. With nothing gated on it, every client reloads whenever HUP fires twice. Two questions, two hashes, one load |
| Rows render keyed by id, and a row with an open editor is never touched | The row editor is a row, and the 45-second poll redraws rows, also on the 304 path that keeps time strings moving. A full replace destroys half-typed text, focus, and scroll position every 45 seconds, unattended. §9.2 already protects typing from a config reload. This is the same rule one level down |
| Orphaned channel names get their own grayed chip | When the editor did not show orphan names, it sent a selection that silently omitted them. So every save removed an orphan that `channel rm` created on purpose, and §8.3's "carry an orphan forward" was then reachable only from `curl`. A chip that you can deselect but not select again makes the whole rule visible |
| The empty-channel-list suppression belongs to the capture bar only | The row editor renders its channel affordance when the catalog is non-empty *or* the row carries names, because it always sends the full list of what its chips show. An editor also suppressed on an empty catalog lets `channel rm` on the last channel make every later save of a row that references it send `[]`. That wipes the orphan that the orphan chip (previous row) exists to keep, from the one surface that is there to preserve it |
| A past `due_at` is stamped `notified_at` **and** `pushed_at` at write time | This puts the "backdating never pings" promise in the write path, not in a 1800-second threshold that the client then needs to know. The too-late gate then does only its real job: outage catch-up. The row stays out of the digest as well as out of phase 1, and the picker's sentence is true for a moment one second in the past and for a moment one week in the past |
| Undone stamps `pushed_at` on a row that was marked but never pushed about | "Comes back without re-notifying" was true only of rows that a push reached. Take a row marked during a cooldown, cleared inside the window, and undone hours later. It still matched phase 2's held-rows predicate. Phase 2 does not check ages again, so the row entered the next digest at any age, and went around the too-late gate through a `Clear` and an `Undo`. One guarded column in the same UPDATE makes the promise literal for every already-fired row. The never-fired row, whose `notified_at` is still NULL, is deliberately left live |
| `LIMIT 50` per tick, no inner loop | A deliberate ceiling on how fast a pathological backlog can turn into notifications |
| Badge counts overdue only | The badge means "something needs you now", not "you own tasks" |
| No per-preset keyboard shortcuts | The input is autofocused, so bare digits cannot coexist with typing "in 2 years". Every alternative costs a keystroke, a mode, or a conflict with the browser's own `Alt`+digit. `Enter` for the default preset is the only shortcut that is worth its cost |
| Chips save on click, not select-then-confirm | Goal 1 is one interaction. A two-step chip is a worse date picker |
| `same_day_ok` explicit per preset | "Next Monday" on a Monday is genuinely ambiguous. Make it a config decision, not a hidden convention |
| Hour range is a hard constraint, and picker-only | Chosen over an in-sheet "show all hours" toggle, because the constraint is the feature. It bounds manual selection, never what a preset can compute |
| Keep `done` rows for 30 days, `0` = forever | Cheap, enables undo and future history, purged by the same sweep |
| Secrets in env, behavior in TOML | The TOML lives in a mounted directory, and you edit it by hand. Keys must not live in such a file |
| Channels via subcommand, not API or TOML | Their URLs are secrets, and `FROM scratch` leaves no shell to hand-edit the table with |
| Cookie key derived from `NAG_TOKEN` | One secret to manage instead of two, and a change of the token also logs out every session, with no extra code |
| `Secure` cookie unconditionally | Behind the reverse proxy, the app sees plain HTTP. A flag derived from `r.TLS` ships an insecure cookie and looks correct |
| Theme follows the OS, with no override | A media query and nothing else: no persistence, no first-paint flash, no difference between devices. The OS already knows |
| No client-side persistence at all | No cookie for preferences, no localStorage. The server or the OS holds every piece of state, so behavior is identical in every context |
| Server clock for skew, browser for timezone | To correct a wrong clock is cheap, and it keeps the badge count correct. To re-render the server's wall clock for someone in another country is worse than to show them their own |
| Edit reuses the capture bar | Rename and re-time are the same two fields as creation. A separate edit dialog is a second implementation of the same thing |
| No `/snooze` endpoint | After `snooze_count` was removed, `/snooze` did nothing that `PATCH` did not do. One handler holds the re-time rules |
| Dropped `source`, `url`, and `snooze_count` | Nothing set or read `source`. `snooze_count` was counted and never displayed. `url` was a whole column and API field for a link that v1 neither showed nor let you type. A markdown link in the text does the job with no schema change |
| Links live in the text, as markdown | One field instead of two, and the deferred bookmarklet needs no API change. The cost is a tiny parser written twice (§9.10) |
| A bad link scheme is literal text, not a 400 | The renderer must refuse it anyway, and `textContent` refuses it again behind that. So the server's copy of the rule bought nothing, and it made `[note](internal)` and `[doc](obsidian://…)` impossible to enter. `flattenLinks` stays the server's only contact with text |
| An unencoded `(` in a link target refuses to render | With one non-backtracking regex, a paren-bearing URL truncates at the first `)`: a link that looks right and returns 404. A refusal shows raw markdown instead. That is a failure that you can see, and fix by percent-encoding |
| 403 with a stale key deletes the subscription | It can never succeed again, so to keep it is a guaranteed failure every 30s, and a silent one. Together with the client's key check, this makes rotation recoverable |
| The client sends the key it subscribed under | When the server stamped its configured key, `vapid_public` became a copy of the config. Then the boot warning and the 403 branch had no way to fire on the case that they were written for |
| System font stacks, no webfonts | The faces that the design is built on are licensed OS faces that cannot be self-hosted. Substitutes shipped as woff2 look *less* like the design where the real faces exist. No `web/fonts/`, and `font-src 'none'` |
| Bind mounts, chown'd once, non-root | A scratch image has no shell to fix ownership from inside, so ownership is a host step either way. A bind mount is also a visible directory, so a WAL backup is a `cp` and not a command that reaches into a volume |
| Shutdown cancels the sweep, waits for it, and only then closes the database | A tick alone can run longer than Docker's 10-second grace period: sequential 10 s pushes, then phase 3's 20 s budget. So "stop the ticker and close" runs `db.Close()` under a tick that still writes `delivery_error` and still deletes subscription rows. That is a mid-write close that comes from the inside. A cancel, not only a stop, also keeps the wait to a couple of seconds, because every send is a child of the tick's context and ends at once |
| The service worker is registered on every load, and everything push is feature-detected once | `getSubscription()` is how the notification slot decides whether it exists. So registration cannot wait for the gesture that grants permission. Also, an undetected `navigator.serviceWorker` throws on the load path and takes the list with it. If push is unavailable, only push must stop working |
| A clicked notification carries its id in the URL when it opens a window | A `postMessage` to a client that `openWindow` just returned is a race that the worker always loses, because the page has no listener yet. That is the *common* case, because you click notifications that arrived with the tab closed. `/?focus=<id>` plus `history.replaceState` is three lines and cannot race |
| The picker commits on `Set reminder`, not on scroll | A chip is one interaction because it carries its own answer. The picker exists because you choose among many values, and a save per scroll-snap sends a `PATCH` once per notch of the minute wheel. This is the one place where a two-step commit is right |
| No log level, so nothing is logged below INFO | A DEBUG line needs an env var that §5.1 does not have, and a restart, to read about a sweep that already ran. The held-rows line is the one that distinguishes a 20-minute cooldown from a broken sweep, so it is INFO |
| No command-line flags on any subcommand | Env for secrets, TOML for behavior. A flag is a third place to look, and it lives in `compose.yaml`'s `command:`, out of sight of both files |
| Two formatters own every time string | Rows, picker, and notification body had three sets of hand-written examples and no rule. Thresholds and units are exactly what drifts silently between copies (§9.11) |
| Bodyless mutations skip the JSON `Content-Type` check | The same-origin rule already refuses a form from any other origin, sibling subdomain included, and a form cannot send the `Authorization` header. A header requirement on them only breaks the `curl` paths that those endpoints exist for |
| English copy, locale-formatted clocks | "Follows the browser's locale" was always about how a time is *rendered*, not about a translation of `late`. This statement stops the two readings from conflicting in every string |
| A failed poll is a quiet line, a failed action is a toast | A 45-second unattended poll that toasts on failure produces a toast every 45 seconds during a blip. The difference is whether someone waits for the answer |
| Hidden tabs still poll, at 5 min, and the worker pings them | Goal 4 is a favicon read from *another* tab, and that tab is `hidden` by definition. With "do not poll a hidden tab", the count refreshes only when you already look at it |
| No `last_ok_at` or `user_agent` on subscriptions | Nothing can read them: `FROM scratch` has no `sqlite3` (§7.4), and no endpoint or subcommand exposes a subscription. A column written every 30s and read by nobody is not observability. It only looks like it |
| `flattenLinks` shares the renderer's eligibility gate | When it was defined as "replace every match with its label", it disagreed with the renderer on exactly the cases that §9.10's table calls out: `mailto:` flattened to a bare label, `javascript:` to `a)` |
| The placeholder VAPID subject is a boot error | It is the one required value that `genkeys` cannot generate. Push services use it to reach the operator. A placeholder that boots is a placeholder that is still there in a year |
| Editing opens on the stored moment only when it is still in the future, and with the minute wheel at `00` | The stored minute is usually not on a `minute_step` boundary, so the wheel cannot show it. A snap to the nearest step is a silent move of a few minutes that looks like the value you saved. A visible reset is not. A row that you re-time is usually overdue, so the stored date opens the sheet on the in-the-past warning, which is the first screen that the create-mode default was written to avoid. So one rule covers both modes, tested on the value that the sheet shows, because the minute reset can itself land a future row in the past |
| Subcommands migrate the database too | There is one `store.Open`. So `nag channel add` before the first `up -d` is a supported order, and does not leave a missing file or an unmigrated one for `serve` to repair |
| `nag.env.example` is comments only | A template with empty values leaves two of every key after `genkeys >>`. The one required hand edit then has two candidate lines, and the wrong one silently does nothing |
| An already-attached unknown channel name survives a `PATCH` | `channel rm` creates orphans on purpose, and the API's channel list does not show them. The strict rule made every reminder that references a removed channel un-editable, through a 400 on a name that the row already carried. You can carry an orphan forward, but never introduce one |
| CSP does not govern CSSOM, and the spec says so | "No inline styles because `style-src` blocks them" is half true. It leads to a rule that prescribes custom properties set through the same API that it claims is blocked. The markup rule is real. The JS rule is a preference. When the spec names which is which, a future reader does not work around a restriction that does not exist |
| The year appears only when it is not the current one | The picker can page into another year. So with "no year anywhere", a row 14 months out rendered identically to one 2 months out. A year-aware format costs one condition. A year stepper serves reminders a year or more away, and a personal reminder rarely belongs that far out (§9.6) |
| `delivery_error` holds a classification, never the library's error text | shoutrrr's errors embed the URL that it received. This column is returned by the API, drawn into a row, and logged. So a passed-through string breaks the URL-masking rule in three places at once, with no single place to redact. `http 401` and `timeout` are what you act on anyway |
| `config_version` moves only when the resolved config differs | When it moved on every successful load, a reverted edit or a HUP sent twice reloaded every open client to give it the config it already had. And `config check` then HUP is the documented workflow, and it signals twice |
| The cooldown window is consumed even when the push fails or there is nobody to push to | `pushed_at` is already committed, so the rows can never be selected again. A window held open on failure has nothing left to carry, and only runs an empty select again every 30 s. "One attempt per half hour" stays literally true |
| Client time is Unix seconds, with one `now()` | `server_time` is seconds and `Date.now()` is milliseconds. A single subtraction across that boundary renders every row as tens of thousands of days late. Two call sites need ms (the due timer, the picker), and they convert there |
| The due timer's delay is clamped to ~23 days | `setTimeout` truncates to int32. So the timer for a reminder six months out fires immediately, polls, re-arms from the same row, and fires again. That is a tight loop for as long as the tab is open. The clamp costs one extra poll every few weeks |
| Channel names are lowercase slugs | The name is a chip label, a CLI argument, and a JSON array element, and SQLite's `UNIQUE` on TEXT is case-sensitive. So `ntfy` and `NTFY` can be two rows that render two identical chips. One rule at the door is better than `COLLATE NOCASE` plus a matching fold on every send-time lookup |
| No secrets and no reminder text in the log | Tokens, channel URLs and full push endpoints are credentials or capabilities. `text` is left out so that the log stays safe to paste into an issue. Every line that can use the text works from the id, and the id is what makes an accidental `Clear` recoverable |
