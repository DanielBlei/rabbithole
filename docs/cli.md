# CLI reference

The web UI is the primary interface (see the [README](../README.md)). The CLI covers
scripted runs, direct access to the item store, and inspection of what the model received.

```
rabbithole [--config PATH] [--debug] [--trace] <command>
```

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
rabbithole serve [--addr ADDR]
```

Serves the web UI and the JSON API (see [docs/api.md](api.md)). `--addr` defaults to
`127.0.0.1:8080`, which is loopback-only; set it explicitly to listen on other interfaces.
SIGINT and SIGTERM trigger a graceful shutdown, allowing up to 5 seconds for in-flight
requests and for any running ingest to finish.

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