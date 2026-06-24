# CLI reference

```
ai-searcher [--config PATH] [--debug] [--trace]
```

`--config` defaults to `./configs/config.yaml`. `--debug` logs every stage — config, per-feed
fetches, age/dedup filtering, each scoring batch and per-item score, selection, and write —
with timings. `--trace` additionally logs raw model prompts/responses and implies `--debug`.

## run

```
ai-searcher run [--provider P] [--dry-run] [--no-think]
```

Fetches feeds, scores new items against the interest profile, and writes today's digest to
`data/digests/YYYY-MM-DD.md`.

- `--provider` overrides the configured provider (`ollama`|`vllm`|`heuristic`) for this run.
- `--dry-run` prints the digest to stdout instead of writing a file or recording items.
- `--no-think` disables the model's reasoning/thinking mode during scoring. Thinking is on by
  default; pass this for models without a capable think mode, or for faster scoring without
  chain-of-thought before the JSON.

## items

Browse and record your own read/skip/rating/notes for digest items, by id or link (both
shown by `items list`).

```
ai-searcher items list [--status S] [--source NAME] [--limit N] [--since D] [--before D] [--sort score|latest|oldest]
ai-searcher items sources
ai-searcher items read|skip|unread <id|link>...
ai-searcher items rate <id|link> <0-10>
ai-searcher items note <id|link> <text>...
```

- `list` shows items best-score-first by default (the highest of `user_score`/`llm_score`,
  whichever is set), optionally filtered by `--status` or `--source`. `--since`/`--before`
  are durations relative to now (e.g. `3d`, `12h`); with neither set, it defaults to the
  config's `list_since` window — `--before` pages older without re-imposing that default.
  `--sort latest` shows newest first instead; `--sort oldest` shows oldest first.
- `sources` lists distinct source names with item counts, for use with `list --source`.
- `read`/`skip`/`unread` accept multiple ids/links at once and set `status`, continuing past
  per-item failures (like `kubectl delete`) and printing a summary of how many failed.
- `rate` sets `user_score` (0–10); `note` sets `user_note` (no quoting needed — all trailing
  words are joined). These two stay single-item since one value can't sensibly apply to many.

These commands call `internal/store.UpdateUserState` directly — the same method `serve`'s
HTTP handlers call (see [docs/api.md](api.md)).

## serve

```
ai-searcher serve [--addr ADDR]
```

Serves the items store over HTTP as JSON. `--addr` defaults to `:8080`. There's no frontend
yet — see [docs/api.md](api.md) for the endpoints. Shuts down gracefully on SIGINT/SIGTERM,
waiting up to 5s for in-flight requests to finish.