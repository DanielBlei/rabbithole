# Configuration

Two files, both under `configs/`:

| File | What it holds |
|---|---|
| `config.yaml` | how to run — model, scoring, storage, paths |
| `feeds.yaml` | the feed list and per-feed tuning |

Copy the `*.example.yaml` templates and edit (`make setup` does this, plus the profile copy,
for any file that's missing). Both are read once at startup: **editing them while the server
is running has no effect** — stop it, edit, start again. The web UI has read-only viewers for
both under the gear menu (View config / View feeds).

## config.yaml

The schema is nested. Every field below is optional unless marked required.

| Field | Meaning | Default |
|---|---|---|
| `user` | Name shown in the web UI's shell prompt | your OS login name |
| `profile` | Path to your interest-profile markdown, injected into the scoring prompt | **required** |
| `inference.provider` | `ollama` \| `vllm` \| `heuristic` | `ollama` |
| `inference.host` | Inference endpoint URL | `http://localhost:11434` |
| `inference.model` | Model name | `qwen3:4b` |
| `inference.api_key` | Optional bearer token (vLLM prod / Ollama Cloud) | `""` |
| `inference.think` | Model reasoning during scoring | `true` |
| `inference.batch_size` | Items sent per scoring request; also multiplies `tokens_per_item` | `5` |
| `inference.max_parallel` | Concurrent scoring requests in flight. Ollama queues rather than parallelizes — use `1` for it | `2` |
| `inference.model_tuning.*` | Decoding limits — see below | see below |
| `ingest.since` | Global lookback window — the outermost fallback for a feed's `since` | `14d` |
| `ingest.digest_dir` | Where `ingest --markdown` writes the digest | none (required by that flag) |
| `ingest.feeds` | Path to the feed list | `feeds.yaml` beside `config.yaml` |
| `store.db_path` | SQLite database path | **required** |

Durations accept a `d` (days) suffix on top of Go's standard units (`h`, `m`, `s`) —
e.g. `14d`, `168h`, `36h`, `1h30m`.

`api_key` is stored in plaintext. The config viewer masks it, but the file itself isn't
protected — keep `configs/config.yaml` out of version control (it's gitignored).

### inference.model_tuning

Decoding limits sent with every scoring request. Omit the block, or any field in it, to
take the defaults. Tune when swapping in a model that is terser or more verbose.

| Field | Meaning | Default |
|---|---|---|
| `num_ctx` | Input window. Left unset, Ollama silently drops the front of an over-long prompt and the model scores articles it never fully saw. Ollama only — vLLM fixes this at startup | server default |
| `max_tokens` | Hard output cap; `0` auto-sizes from the three below | `0` |
| `tokens_per_item` | Allowance per article in a batch | `256` |
| `tokens_overhead` | Allowance for the JSON scaffolding | `256` |
| `tokens_thinking` | Added when `think` is on; reasoning spends the same budget | `2048` |
| `reason_max_chars` | Max characters for the reason in the model's JSON response; schema-enforced | `200` |

The model is held to the response shape by **structured outputs**: the schema is compiled
into a grammar and the sampler may only pick tokens that fit, so scores come back as
integers and unknown fields can't appear. `reason_max_chars` is part of that schema and is
what actually keeps rationales short — the prompt's word count is only a suggestion.

## feeds.yaml

A `defaults:` block plus the feed list. Any RSS/Atom URL works.

```yaml
defaults:
  since: 7d       # lookback window for feeds that don't set their own
  max_items: 25   # cap per feed per run; 0 or omitted = uncapped

feeds:
  - name: Hugging Face blog
    url: https://huggingface.co/blog/feed.xml
    tags: [ai, research]

  - name: Medium — AI          # high volume: tighter window, harder cap
    url: https://medium.com/feed/tag/artificial-intelligence
    since: 2d
    max_items: 10

  - name: Old Newsletter       # parked, not deleted
    url: https://example.com/feed.xml
    enabled: false
```

| Field | Meaning | Default |
|---|---|---|
| `name` | Display name; also the `source` items are stored under | **required, unique** |
| `url` | RSS/Atom feed URL — also the feed's identity | **required** |
| `enabled` | `false` parks the feed — kept in the file, never fetched | `true` |
| `since` | This feed's lookback window | `defaults.since`, then `ingest.since` |
| `max_items` | Cap on the newest in-window items this feed contributes per run | `defaults.max_items`, then uncapped |
| `tags` | Free-form labels, shown in the feeds viewer | `defaults.tags` |

`max_items` matters more than it looks: some feeds return their whole archive.
Hugging Face's blog feed returns ~800 items in a single fetch, and without a cap every
unseen one goes to the model for scoring on the first run.

### How a value is resolved

Each knob falls through until something sets it:

```
feed entry  →  feeds.yaml defaults  →  config.yaml (since only)  →  built-in
```

The feeds viewer marks inherited values with `*` and names their origin on hover, so you can
always see whether a feed set a value itself or picked it up from further out. `tags` are the
exception — a feed's tags are *added to* the defaults' rather than replacing them.

### Identity: what makes a feed "the same feed"

A feed's identity is derived from its **URL**, not its name. Fetch history is keyed on that
identity, so:

- **Renaming a feed keeps its history.** The name is a label; change it freely.
- **Changing a feed's URL starts a new feed.** That's deliberate — a different URL is a
  different source.
- **Two entries with the same URL share one history entry.** Allowed (it's harmless and
  self-inflicted), but a warning is logged at startup so it isn't silent.

Names must still be unique, and that's a *separate* constraint: items are stored under the
feed name, so two feeds sharing one would merge into a single source in the feed list.
Renaming does still start a new source for already-stored items — history follows the URL,
stored items follow the name.

### Feed health

Every ingest run appends one row per feed to a fetch history: outcome, item count, error,
and how long it took. The feeds viewer aggregates it into a status dot, the current failure
streak, when the feed last worked, and a strip of recent attempts so a pattern is visible at
a glance rather than only the latest verdict.

A failing feed never fails the run — it's logged, recorded, and skipped. History is recorded
on `--dry-run` too, so a dead feed surfaces even on a run that persists nothing.

The history is append-only and pruned to the newest 200 attempts per feed. Rows for feeds
you've removed are left alone, so re-adding a feed restores its history rather than starting
blank.

Medium feed URL shapes:

- `https://medium.com/feed/tag/<tag>`
- `https://medium.com/feed/@<username>`

## Interest profile

`configs/profile.example.md` is a starting point — free-form markdown describing what you
care about. It's injected verbatim into the scoring prompt, so the more specific, the better
the ranking.
