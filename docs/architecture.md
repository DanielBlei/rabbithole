# Architecture

## How it works

```mermaid
flowchart LR
    F[feeds<br/>RSS/Atom] --> RUN
    CLI["ingest<br/>(CLI)"] --> RUN
    WEB["serve<br/>(ingest button)"] --> RUN
    RUN[one run:<br/>fetch, score, record] --> DB[(SQLite store)]
    RUN -. "--markdown" .-> MD[markdown digest]
    DB --> UI[web UI]
    DB --> API[JSON API]
    DB --> ITEMS[items CLI]
```

`ingest` and `serve` share the same cycle and store: `ingest` runs it once from the CLI and
can write the result as a markdown digest, while `serve` exposes the store over HTTP and can
trigger the same cycle in the background (see [docs/api.md](api.md)). State (seen items,
scores, digest history, user status/notes) lives in a local SQLite file, so re-runs only
score genuinely new items.

## Scoring flow

```mermaid
flowchart TD
    A[RSS feeds] --> B[fetch each feed]
    B --> C{new, and inside<br/>the window?}
    C -- no --> D[skip]
    C -- yes --> E[title + source + summary<br/>summary capped at 1500 chars]
    E --> F[group into batches<br/>batch_size per request]
    F --> G[send with your profile<br/>max_parallel requests in flight]
    G --> H[score 0-10 + a reason,<br/>as JSON of a fixed shape]
    H --> I[(store: score, reason, model)]
```

**What gets sent.** Three things per article: the source name, the title, and the summary the
feed itself published. Not the article. Nothing fetches the page, so a score is a judgement on
what the feed chose to say about a piece. The summary is capped at 1,500 characters, which
most feeds never reach (a typical `<description>` is a few hundred); the cap is there for the
occasional feed that inlines a whole post.

**How it's grouped.** Articles are gathered into batches of `batch_size` and each batch is one
request, with `max_parallel` requests in flight. Both default to 1: a small local model reads
one article better than five, and Ollama queues concurrent requests anyway. Raise them against
a bigger model or an endpoint that genuinely serves in parallel, such as vLLM. Your profile
goes with every request, since the question is always "worth reading for this reader".

**What comes back.** A score from 0 to 10 and a sentence or two of reasoning per article, as
JSON in a fixed shape that the inference server enforces while the model writes rather than
asking it to comply. Score, reason and the model's name are saved against the article; one
that fails to score stays unscored and is retried next run, so a bad reply costs a run rather
than the article.

Each step has a knob, all of them in [configuration.md](configuration.md):

| Step | Knobs |
|---|---|
| What's eligible | `ingest.since`, a feed's own `since` and `max_items` |
| How much text | the 1,500-character cap (fixed), `model_tuning.num_ctx` for how much the model actually reads |
| How it's split | `inference.batch_size`, `inference.max_parallel` |
| How much reply | `model_tuning.tokens_per_item`, `tokens_overhead`, `reason_max_chars`, and `tokens_thinking` when `think` is on |

The limit of all this is the first step: a feed that publishes a two-line teaser gets scored on
a two-line teaser, and a site with no feed can't be read at all. Both are on the roadmap below.

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
- **Full-text crawl** — fetch the article page and score the piece itself, rather than the
  summary a feed chose to publish about it.
- **Pages without a feed** — follow a plain blog that publishes no RSS or Atom at all, reading
  the posts off the page itself. Everything after the fetch already works on a title, a source
  and some text, so the fetch is the only step that assumes a feed exists.
- **Scheduling & delivery** — systemd timer or cron to run ingest unattended, plus email,
  push or an output feed so the digest reaches you instead of waiting to be opened.
- **Postgres** — one store reachable from more than one of your machines, instead of each
  process owning a local file. Still one profile and one reader (see [store.md](store.md)).
