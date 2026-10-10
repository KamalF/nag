# Nag

A personal, self-hosted reminder app. Type a note, tap when it should come
back, and get an OS notification at that time. The reminder then stays in
"Needs you now" until you clear it — dismissing the notification does not.

One user, one instance. A single Go binary with SQLite, served behind Caddy.
[SPEC.md](SPEC.md) is the full specification.

## Before you start

You need Docker with Compose, and a [caddy-docker-proxy] instance on the same
host. Every `docker compose` command below needs the external `caddy` network
to exist, including the setup commands, because `compose.yaml` declares it
`external: true` and Compose will not create it. If the proxy is not running
yet, create the network first:

```
docker network create caddy
```

Edit the `caddy:` label in `compose.yaml` to your own domain. HTTPS is
mandatory: service workers and Web Push do not work over plain HTTP.

[caddy-docker-proxy]: https://github.com/lucaslorentz/caddy-docker-proxy

## Setup

Run these four commands in the project directory, in this order:

```
mkdir -p data config
sudo chown -R 65534:65534 data config
cp nag.env.example nag.env
docker compose run --rm -T nag genkeys >> nag.env
```

1. `data/` holds the database and `config/` holds `nag.toml`. Both are bind
   mounts.
2. The image runs as uid 65534 and has no shell, so ownership cannot be fixed
   from inside it. Do it once, here.
3. Compose refuses to run anything, `genkeys` included, while `nag.env` is
   missing, so the file must exist first.
4. `genkeys` prints a complete env file on stdout. **Keep `-T`.** Without it
   some Compose versions allocate a TTY, which turns each line ending into
   `\r\n`, and the `\r` becomes part of every key.

Then make the one hand edit: open `nag.env` and replace
`mailto:you@example.com` with your own address. Push services use it to reach
you, and nag refuses to start while the placeholder is there.

Start it:

```
docker compose up -d
```

On first boot nag writes its default config to `config/nag.toml`. Its
timezone is `Europe/Paris`, and every preset resolves in it, so set
`timezone` to yours and reload (see Configuration). Then open the site,
enter the value of `NAG_TOKEN` from `nag.env` once, and press "Turn on
notifications".

### iPhone

On iPhone, Web Push only works from a Home Screen app: open the site in
Safari, then Share → Add to Home Screen. The installed app has its own
cookie storage, separate from Safari's, so you type the token once more
inside it. That is expected, not a broken login.

## Configuration

`config/nag.toml` holds behaviour: timezone, presets, the picker's hours,
and retention. The file documents itself. Secrets stay in `nag.env`.

Check an edit, then reload without a restart:

```
docker compose run --rm nag config check
docker compose kill -s HUP nag
```

`config check` prints the preset list or the exact error. A reload that
fails validation is logged and ignored, and the running config stays in
place — so check first.

## Channels (optional)

A reminder can also go to ntfy, Telegram, Matrix, email and the other
services [shoutrrr] supports. Channel URLs contain secrets, so they live in
the database and are managed from the command line:

```
docker compose run --rm nag channel add ntfy 'ntfy://ntfy.sh/your-topic'
docker compose run --rm nag channel test ntfy
docker compose run --rm nag channel list
docker compose run --rm nag channel disable ntfy
docker compose run --rm nag channel rm ntfy
```

Names are lowercase slugs. `list` masks every URL. Pick channels per
reminder with "Also send to" in the app.

[shoutrrr]: https://github.com/nicholas-fedor/shoutrrr

## When notifications are silent

Ask the server to push a test notification to every subscribed browser:

```
curl -X POST -H "Authorization: Bearer $NAG_TOKEN" https://nag.example.com/api/push/test
```

`$NAG_TOKEN` is the value from `nag.env`; set it in your shell first.

Each subscription gets a status: `201` means the push service accepted it.
Logs are the only other observability: `docker compose logs nag`. They
never contain the token, a channel URL, a full push endpoint, or reminder
text.

## Backups

The database is the only copy of everything. While nag runs, recent writes
live in `nag.db-wal`, so copying `nag.db` alone from a running instance can
produce a corrupt file. Either stop the container first:

```
docker compose stop nag
cp data/nag.db* /your/backup/
docker compose start nag
```

A clean stop folds the WAL back into `nag.db` and removes the `-wal` and
`-shm` files, so the glob copies whatever is there. Or use `VACUUM INTO`
from your own backup tooling. Keep `data/` on a local
disk: WAL does not work reliably on a network filesystem.

## Building

The setup commands build the image implicitly, without a version. To stamp
one, build explicitly:

```
docker compose build --build-arg VERSION=$(git describe --tags --always --dirty)
```

`docker compose run --rm nag version` prints the version, and the boot log
names it. A version of `dev` means the image was built without that
argument.

To update, rebuild before restarting — `up -d` reuses an existing
`nag:latest` and would restart the old binary:

```
git pull
docker compose build --build-arg VERSION=$(git describe --tags --always --dirty)
docker compose up -d
```

Local development uses the Go version pinned in `mise.toml`:
`go test ./...` runs every test. The frontend has no build step and no
test runner.
