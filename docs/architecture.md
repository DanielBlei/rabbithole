# Architecture

## How it works

```
feeds (RSS/Atom) ──▶ fetch + normalize ──▶ drop seen/old ──▶ LLM scores each item
                                                                      │
        markdown digest  ◀── select top-N over threshold ◀───────────┘
```

`run` and `serve` share the same pipeline and store: `run` writes the result as a markdown
digest, `serve` exposes the same store over HTTP instead (see [docs/api.md](api.md)). State
(seen items, scores, digest history, user status/notes) lives in a local SQLite file, so
re-runs only score genuinely new items.

## Layout

```
cmd/                  cobra CLI (root, run, items, serve)
internal/config       YAML config + profile loading
internal/pipeline     fetch -> score -> record cycle, shared by run and serve
internal/feeds        concurrent RSS/Atom fetch + normalization (gofeed)
internal/rank         Scorer interface, prompt/JSON parsing, batching, selection, heuristic
internal/inference    provider factory (ollama | vllm | heuristic)
internal/ollama       Ollama JSON-mode client
internal/vllm         OpenAI-compatible JSON-mode client
internal/httpclient   shared HTTP transport (bearer auth injection)
internal/retry        exponential backoff for a still-starting-up inference server
internal/digest       markdown renderer
internal/store        SQLite (seen dedup, digest history, user status/notes)
internal/server       HTTP handlers for serve (JSON; calls internal/store directly)
internal/logger       zerolog setup for --debug/--trace
```

## Roadmap

- **Adaptive ranking** — feed `status`/`user_score`/`user_note` history (recorded via
  `items` or the API) back into the scoring prompt as liked/disliked examples.
- **HTML frontend** — a thin UI over the existing `serve` JSON API, no client/server split.
- **Full-text crawl** — fetch article bodies for richer scoring beyond title+summary.
- **Embeddings pre-filter** — cheap shortlist → LLM rerank for large feed sets.
- **Scheduling & delivery** — systemd timer / cron, plus email or chat delivery.