# ai-searcher

A personal RSS reading assistant. It fetches your RSS/Atom feeds (Medium, arXiv, blogs,
…), scores each new item against an editable interest profile using an LLM, and writes a
dated markdown digest of **what to read today** — best first, each with a one-line reason.

Built to keep up with AI engineering without manually triaging titles. Pluggable model
backend: **Ollama** by default, any **OpenAI-compatible** endpoint (vLLM) as a drop-in, or
a model-free **heuristic** for offline testing.

## How it works

```
feeds (RSS/Atom) ──▶ fetch + normalize ──▶ drop seen/old ──▶ LLM scores each item
                                                                      │
        markdown digest  ◀── select top-N over threshold ◀───────────┘
```

State (seen items, scores, digest history) lives in a local SQLite file, so re-runs only
score genuinely new items.

## Quick start

```bash
# 1. Configure
cp configs/config.example.yaml configs/config.yaml
cp configs/profile.example.md  configs/profile.md
$EDITOR configs/profile.md      # describe what you care about — this drives ranking

# 2a. With Ollama (default)
ollama serve
ollama pull qwen3:4b
go run . run

# 2b. Offline smoke test (no model needed)
go run . run --provider heuristic

# Preview without writing files or recording items
go run . run --dry-run
```

The digest is written to `data/digests/YYYY-MM-DD.md`.

## Configuration

See `configs/config.example.yaml`. Key fields:

| Field | Meaning |
|-------|---------|
| `profile` | Path to your interest-profile markdown (injected into the scoring prompt) |
| `provider` | `ollama` \| `vllm` \| `heuristic` |
| `chat_host` / `chat_model` | Inference endpoint and model |
| `api_key` | Optional bearer token (vLLM prod / Ollama Cloud) |
| `batch_size` | Items per scoring request |
| `top_n` / `min_score` | Digest size cap and inclusion threshold (0–10) |
| `since` | Lookback window (e.g. `168h`) |
| `feeds` | List of `{ name, url }` RSS/Atom sources |

Medium feeds: `https://medium.com/feed/tag/<tag>`, `.../@<user>`, `.../<publication>`.

## Commands

```
ai-searcher run [--config PATH] [--provider P] [--dry-run] [--think] [--debug]
```

- `--think` enables the model's reasoning mode during scoring. Off by default —
  reasoning models (e.g. qwen3) otherwise spend the whole timeout on chain-of-thought
  before emitting JSON. Leave it off unless you have a capable model and want the extra
  reasoning.
- `--debug` logs every stage: config, per-feed fetches, age/dedup filtering, each scoring
  batch and per-item score, selection, and write — with timings.

## Layout

```
cmd/                 cobra CLI (root + run)
internal/config      YAML config + profile loading
internal/feeds       concurrent RSS/Atom fetch + normalization (gofeed)
internal/rank        Scorer interface, prompt/JSON parsing, batching, selection, heuristic
internal/inference   provider factory (ollama | vllm | heuristic)
internal/ollama      Ollama JSON-mode client
internal/vllm        OpenAI-compatible JSON-mode client
internal/digest      markdown renderer
internal/store       SQLite (seen dedup + digest history + feedback column)
```

## Roadmap

- **Adaptive ranking** — a `rate` command writes the `feedback` column (already in the
  schema); a later scorer feeds liked/disliked examples back into the prompt.
- **Full-text crawl** — fetch article bodies for richer scoring beyond title+summary.
- **Embeddings pre-filter** — cheap shortlist → LLM rerank for large feed sets.
- **Scheduling & delivery** — systemd timer / cron, plus email or chat delivery.

## Testing

```bash
go test ./...
```
