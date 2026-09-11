# CLI reference

The web UI is the primary interface (see the [README](../README.md)). The CLI covers
scripted runs, direct access to the item store, the web UI's login, and inspection of what the
model received.

```
rabbithole [--config PATH] [--debug] [--trace] <command>
```

| Command | What it does |
|---|---|
| [`ingest`](#ingest) | Fetch the feeds, score what is new, record it, and optionally write a markdown digest |
| [`serve`](#serve) | Serve the web UI and the JSON API, behind the login |
| [`auth`](#auth) | Show, reset or switch off the web UI's login |
| [`items`](#items) | Browse the stored items and record your own read, skip, rating and notes |
| `eval` | Measure how well scoring matches your profile; see [evals.md](evals.md) |

Commands that touch the database take the same `--config` as `serve`, so they work on the same
store.

| Flag | Description | Default |
|---|---|---|
| `--config` | Path to the configuration file | `./configs/config.yaml` |
| `--debug` | Log each stage with timings: configuration, per-feed fetches, filtering, scoring batches and per-item scores, selection, write | off |
| `--trace` | Additionally log raw model prompts and responses; implies `--debug` | off |

## ingest

```
rabbithole ingest [--dry-run] [--provider P] [--no-think] [--markdown]
```

Fetches the configured feeds, scores new items against the interest profile, and records
them in the store.

| Flag | Description |
|---|---|
| `--dry-run` | Print the digest to stdout without writing files or recording items. Feed health is still recorded. |
| `--provider` | Override the configured provider for this run (`ollama`, `vllm`, `heuristic`) |
| `--no-think` | Disable model reasoning for this run, for models without a thinking mode or for a faster pass |
| `--markdown` | Also write `YYYY-MM-DD.md` to `ingest.digest_dir`; fails if that setting is empty |

## serve

```
rabbithole serve [--addr ADDR] [--tls-cert FILE --tls-key FILE] [--insecure-http]
                 [--trusted-proxies NETS]
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

Without either, an `--addr` that other machines can reach (`:8080`, `0.0.0.0`, a LAN address)
is refused, since the login would cross the network unencrypted.

## auth

```
rabbithole auth status
rabbithole auth reset [--username NAME] [--password-stdin]
rabbithole auth disable
```

The web UI's login, managed from the machine the database lives on. How the login itself
works (first run, sessions, HTTPS) is in [auth.md](auth.md). These commands are the only way to
switch a login off or replace a forgotten password: nothing in the web UI or the config file
can, so reaching the port is never enough to unlock the app.

**`status`** says whether the login is on, and for which user:

```
$ rabbithole auth status
login: on, user daniel
```

The other two answers are `login: default (admin / admin), no password set yet` on a fresh
install, and `login: off, the web UI is open to anyone who can reach it`.

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

The password needs at least 8 characters. A reset never goes back to `admin` / `admin`, so
there is no moment when someone else could log in with the default and claim the instance.

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
- `prune` deletes items, and only items — one source, everything past a certain age, or a
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
feed while developing. It names the config it is about to act on, since that is what decides
which database gets emptied.

Two things to know. Pruning inside the ingest window costs a re-fetch and a re-score: the
next run sees the link as new, because deduplication keys on links that are still stored.
Items from feeds that publish no date are always re-fetched while they remain in the feed.
And the database file does not shrink — SQLite reuses the freed pages, but reclaiming the
space on disk means running `VACUUM` yourself with the server stopped.

These commands use the same store method as the HTTP handlers, so changes made here appear
in the web UI on refresh.