# Architecture

## How it works

```
feeds (RSS/Atom) ──▶ fetch + normalize ──▶ drop seen/old ──▶ LLM scores each item
                                                                      │
                             select top-N over threshold ◀────────────┘
                                         │
                    ┌────────────────────┴────────────────────┐
                    ▼                                         ▼
              SQLite store                            markdown digest
      (web UI, JSON API, items CLI)                (ingest --markdown)
```

`ingest` and `serve` share the same cycle and store: `ingest` runs it once from the CLI and
can write the result as a markdown digest, while `serve` exposes the store over HTTP and can
trigger the same cycle in the background (see [docs/api.md](api.md)). State (seen items,
scores, digest history, user status/notes) lives in a local SQLite file, so re-runs only
score genuinely new items.

## Layout

```
cmd/                  cobra CLI (root, ingest, items, serve)
internal/config       YAML config, feed list and profile loading
internal/ingest       fetch -> score -> record cycle, plus the background run manager
internal/feeds        concurrent RSS/Atom fetch + normalization (gofeed)
internal/rank         Scorer interface, prompt/JSON parsing, batching, selection, heuristic
internal/inference    provider factory (ollama | vllm | heuristic)
internal/ollama       Ollama JSON-mode client
internal/vllm         OpenAI-compatible JSON-mode client
internal/httpclient   shared HTTP transport (bearer auth injection)
internal/retry        exponential backoff for a still-starting-up inference server
internal/digest       markdown renderer
internal/store        SQLite (seen dedup, digest history, user status/notes)
internal/server       composition root for serve: mounts api + web, health endpoints
internal/api          JSON API route set (/api/*)
internal/web          server-rendered htmx UI, templates and static assets
internal/httplog      HTTP access-log middleware
internal/logger       zerolog setup for --debug/--trace
```

## Roadmap

- **Adaptive ranking** — feed `status`/`user_score`/`user_note` history (recorded via
  `items` or the API) back into the scoring prompt as liked/disliked examples.
- **Full-text crawl** — consider fetch article bodies for richer scoring beyond title+summary.
- **Feegs page text search** — FTS5 over the item text; the SQLite driver already includes it.
- **Scheduling & delivery** — systemd timer or cron to run ingest unattended, plus email,
  push or an output feed so the digest reaches you instead of waiting to be opened.
- **Postgres** — one store reachable from more than one of your machines, instead of each
  process owning a local file. Still one profile and one reader (see [store.md](store.md)).
