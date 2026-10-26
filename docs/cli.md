# CLI reference

The web UI is the primary interface (see the [README](../README.md)). The CLI covers
scripted runs, direct access to the item store, the web UI's login, and inspection of what the
model received.

```
rabbithole [global flags] <command>
```

| Command | What it does |
|---|---|
| [`ingest`](#ingest) | Fetch the feeds, score what is new, record it, and optionally write a markdown digest |
| [`digest`](#digest) | Print a daily or weekly Markdown digest of already-scored articles |
| [`serve`](#serve) | Serve the web UI and the JSON API, behind the login |
| [`ingest`](#ingest) | Fetch the feeds, score what is new, record it, and optionally write a markdown digest |
| [`config`](#config) | Export, import, list and roll back the saved settings |
| [`sources`](#sources) | Import and export the feed set |
| [`store`](#store) | Show where the store lives |
| [`auth`](#auth) | Show, reset or switch off the web UI's login |
| [`items`](#items) | Browse the stored items and record your own read, skip, rating and notes |
| `eval` | Measure how well scoring matches your profile; see [evals.md](evals.md) |

Every command reads its settings from the store. No file is read unless you name one, and a bare
run uses the defaults.

## Global flags

| Flag | Description | Default |
|---|---|---|
| `--db` | Where the store is: a SQLite path or a `postgres://` URL. Also `RABBITHOLE_DB` | a file in your user data dir, see [Store location](configuration.md#store-location) |
| `--config` | A config file to import on start. It is applied when it changes since it was last applied; edits made in Settings are kept until then. A change saves a new version. A `store` block in it only locates the store. Optional | none |
| `--env-file` | The env file read for `RABBITHOLE_*` variables the environment does not set | `./.env` |
| `--db-max-conns` | Postgres only: cap the connection pool | `10` |
| `--db-conn-max-lifetime` | Postgres only: how long one pooled connection may live, e.g. `30m` | `30m` |
| `--debug` | Log each stage with timings: settings, per-feed fetches, filtering, scoring batches and per-item scores, selection, write | off |
| `--trace` | Additionally log raw model prompts and responses; implies `--debug` | off |

## digest

```
rabbithole digest [--period daily|weekly] [--limit N]
```

Prints a Markdown reading digest to stdout from articles already scored and stored. The default
period is the previous complete UTC day (limit 10); `--period weekly` selects the previous
complete Monday-to-Monday UTC week (limit 20). Both periods include their start and exclude their
end. `--limit` must be between 1 and 50. Articles without an LLM score are omitted; user ratings
do not affect relevance order. No feeds are fetched and no model is called.

```
rabbithole digest
rabbithole digest --period weekly
rabbithole digest --period daily --limit 5
```

## ingest

```
rabbithole ingest [--dry-run] [--provider P] [--no-think] [--markdown [--out DIR]]
```

Fetches the feeds in the store, scores new items against the active interest profile, and records
them in the store, using the saved settings. `--provider` and `--no-think` apply to this run only
and are never saved.

| Flag | Description |
|---|---|
| `--dry-run` | Print the digest to stdout without writing files or recording items. Feed health is still recorded. |
| `--provider` | Override the configured provider for this run (`ollama`, `openai`, `vllm`, `heuristic`) |
| `--no-think` | Disable model reasoning for this run, for models without a thinking mode or for a faster pass |
| `--markdown` | Also write `YYYY-MM-DD.md` to `--out`, else `ingest.digest_dir` if a config set it, else `./digests` |
| `--out DIR` | Folder for `--markdown` |

## serve

```
rabbithole serve [--addr ADDR] [--tls-cert FILE --tls-key FILE] [--insecure-http]
                 [--trusted-proxies NETS] [--no-env-write] [--dev]
```

Serves the web UI and the JSON API (see [docs/api.md](api.md)), both behind the login
described in [docs/auth.md](auth.md). `--addr` defaults to `127.0.0.1:8080`, which is
loopback-only; set it explicitly to listen on other interfaces. SIGINT and SIGTERM trigger a
graceful shutdown, allowing up to 5 seconds for in-flight requests and for any running ingest
to finish.

| Flag | Meaning |
|---|---|
| `--tls-cert`, `--tls-key` | Serve HTTPS with this PEM certificate and key; give both or neither |
| `--insecure-http` | Allow plain HTTP on an address other than loopback, for a TLS proxy on another host |
| `--trusted-proxies` | Comma-separated networks or addresses whose `X-Forwarded-For` and `X-Forwarded-Proto` are believed; default `127.0.0.0/8,::1/128`, empty trusts none |
| `--no-env-write` | Settings never writes a pasted API key to `.env`: the paste action is hidden and the endpoint refuses |
| `--dev` | Serve the CSS, scripts and fonts `no-cache`, so an edit shows on the next reload instead of up to 5 minutes later. For working on them; `DEV=1 make serve` passes it |

With `--config`, the log says `config imported from <file>; edits in Settings are kept until the file changes`, and
the settings page shows the same note. A running `serve` reads the active settings at the start of
each ingest run, so `config import` and `config rollback` from another terminal apply to the next
run with no restart. Its environment is fixed at start, so restart it after changing an API key.

Without either, an `--addr` that other machines can reach (`:8080`, `0.0.0.0`, a LAN address)
is refused, since the login would cross the network unencrypted.

## config

```
rabbithole config export [-o FILE] [-f yaml|json] [--defaults]
rabbithole config import <file>
rabbithole config history [--limit N]
rabbithole config rollback <version>
```

The settings live in the store as a numbered history; the highest version is in force, and no
saved version means the defaults. See [configuration.md](configuration.md#import-and-export).

- **`export`** writes the active version to stdout, or to `-o FILE`, with a header comment naming the
  version. It lists only the settings you changed, so it can be `{}`. `--defaults` writes every
  setting with its value in force instead. `-f json` writes JSON (also when `-o` ends in `.json`),
  without a header. It holds only `$RABBITHOLE_*_API_KEY` references, never a key. With no saved
  version it writes a comment only, which is still a valid import file. It never creates a store.
- **`import <file>`** saves the file as the new active version. It never asks: the file wins. A file
  equal to the active config changes nothing. Values equal to a default are not saved and stay
  defaults; the command prints how many. YAML and JSON are both read. If it replaces a version saved elsewhere, it prints
  the undo command. An invalid file saves nothing. A literal `api_key` is moved to `./.env`. The
  file's `store` block locates the store unless `--db` or `RABBITHOLE_DB` is set.
- **`history`** lists versions, newest first, with their source and time. `*` marks the active one.
  `--limit` defaults to 20; `0` shows all.
- **`rollback <version>`** copies an earlier version forward as a new latest version, so the history
  is kept and a rollback can itself be rolled back.

After an import or rollback, a warning names any referenced API key variable that is not set.

```
$ rabbithole config import config.yaml
config version 4 saved from config.yaml, replacing version 3 (settings UI, saved 14:02); undo with: rabbithole config rollback 3
```

## sources

```
rabbithole sources import <file>
rabbithole sources export [-o FILE]
```

Feeds are an explicit import; no feeds file is read at startup.

- **`import`** adds the feeds in a feeds YAML file whose name and URL the store has never held.
  Known feeds, deleted ones included, are skipped, so a second run adds nothing. It prints how many
  were added and skipped, and any warnings. The same import is on the Sources page (file picker or
  drag and drop).
- **`export`** writes the current feed set in the same shape, to stdout or `-o FILE`.

The example list is
[`docs/examples/feeds.yaml`](https://github.com/DanielBlei/rabbithole/blob/main/docs/examples/feeds.yaml)
(download it with `curl`, see [configuration.md](configuration.md#quick-start)), and the file's shape is in
[configuration.md](configuration.md#the-feeds-files-shape).

## store

```
rabbithole store path
```

Prints the store this command line would use and where that choice came from (`--db`,
`RABBITHOLE_DB`, `--config`, `default` or `legacy`), without creating anything. A Postgres store
prints its host and database, never the password.

## auth

```
rabbithole auth status
rabbithole auth reset [--username NAME] [--password-stdin]
rabbithole auth disable
```

The web UI's login, managed from the machine the database lives on. How the login itself
works (first run, sessions, HTTPS) is in [auth.md](auth.md). These commands are the only way to
switch a login off or replace a forgotten password: nothing in the web UI or a config file
can, so reaching the port is never enough to unlock the app.

**`status`** says whether the login is on, and for which user:

```
$ rabbithole auth status
login: on, user daniel
```

The other two answers are `login: not set up yet, the web UI serves only the setup page` on
a fresh install, and `login: off, the web UI is open to anyone who can reach it`.

**`reset`** sets a new password: the way back in after forgetting one, and the way to change
it. It asks twice, without echoing, and keeps the username unless `--username` changes it:

```
$ rabbithole auth reset
New password:
Retype new password:
password set for daniel; every browser has been logged out
```

| Flag | Meaning |
|---|---|
| `--username NAME` | Change the username as well |
| `--password-stdin` | Read the password from the first line of stdin instead of prompting, for scripts |

```
printf '%s\n' "$NEW_PASSWORD" | rabbithole auth reset --password-stdin
```

Usernames need at least 3 characters and passwords at least 8. A reset never reopens the
instance, so there is no moment when someone else could claim it. An instance with no login
yet has no account name to keep, so a reset there needs `--username`.

**`disable`** switches the login off, leaving the web UI and the API open to anyone who can
reach the port. Settings → Account in the web UI then offers **set a password** to lock it
again.

`reset` and `disable` both log every browser out, including those of a `serve` that is
running at the time.

## items

Reads and annotates the item store from the terminal. Items are addressed by id or link,
both of which `items list` prints.

```
rabbithole items list [--status S] [--source NAME] [--limit N] [--since D] [--before D]
                      [--sort score|latest|oldest] [--bookmarked]
rabbithole items sources
rabbithole items read|skip|unread <id|link>...
rabbithole items bookmark|unbookmark <id|link>...
rabbithole items rate <id|link> <0-10>
rabbithole items note <id|link> <text>...
rabbithole items prune [--all | --source NAME [--since D] [--before D]] [--include-saved] [--dry-run]
```

- `list` returns the last three days, highest score first, using the user rating where set
  and the model score otherwise. `--since` and `--before` are durations relative to now
  (`3d`, `12h`); `--before` on its own pages further back without reapplying the three-day
  default. `--limit` defaults to 50.
- `sources` lists source names with item counts, as accepted by `list --source`.
- `read`, `skip`, `unread`, `bookmark` and `unbookmark` accept multiple identifiers. They
  continue past individual failures and report the number that failed.
- `rate` and `note` apply to a single item, as one value cannot meaningfully apply to
  several. `note` requires no quoting; trailing arguments are joined.
- `prune` deletes items, and only items: one source, everything past a certain age, or a
  window combining both. Todos, ideas, feed health and run history are untouched, so it is
  the way to clean up a feed without starting the database over.

At least one of `--source`, `--since` or `--before` is required, so a prune can't select the
whole store by leaving a flag off. Emptying the feed is `--all`, which says so explicitly and
refuses to be combined with the three. `--since` and `--before` mean what they do in `list`
(durations before now, compared against the item's own published date and falling back to
when it was first seen), so `items list` with the same flags previews exactly what `prune`
would delete. `--dry-run` prints the count and the ten newest matches without deleting.

Items you bookmarked, rated or annotated are kept and reported; `--include-saved` removes
those too. Everything else on a row comes back on re-ingest, so that state is the only part
worth protecting.

```
rabbithole items prune --source "Red Hat Emerging Tech" --dry-run
rabbithole items prune --before 90d
rabbithole items prune --all --include-saved       # or: make clean-feeds
```

`make clean-feeds` wraps that last one in a y/N prompt and is the quickest way to reset the
feed while developing. It runs against the store `make` uses (`DB=`, default `./data/rabbithole.db`).

Two things to know. Pruning inside the ingest window costs a re-fetch and a re-score: the
next run sees the link as new, because deduplication keys on links that are still stored.
Items from feeds that publish no date are always re-fetched while they remain in the feed.
And on SQLite the database file does not shrink: SQLite reuses the freed pages, but reclaiming
the space on disk means running `VACUUM` yourself with the server stopped.

These commands use the same store method as the HTTP handlers, so changes made here appear
in the web UI on refresh.
