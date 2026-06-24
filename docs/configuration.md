# Configuration

Copy `configs/config.example.yaml` to `configs/config.yaml` and edit (`make setup` does this,
plus the profile copy, if both files are missing).

| Field | Meaning | Default |
|---|---|---|
| `profile` | Path to your interest-profile markdown, injected into the scoring prompt | required |
| `provider` | `ollama` \| `vllm` \| `heuristic` | `ollama` |
| `chat_host` | Inference endpoint URL | `http://localhost:11434` |
| `chat_model` | Model name | `qwen3:4b` |
| `api_key` | Optional bearer token (vLLM prod / Ollama Cloud) | `""` |
| `batch_size` | Items sent per scoring request | `5` |
| `max_parallel` | Concurrent scoring requests in flight | `4` |
| `min_score` | Inclusion threshold, 0–10 | `6` |
| `since` | Ignore items older than this when fetching | `14d` |
| `list_since` | Default display window for `items list` / `serve`, independent of `since` | `3d` |
| `output_dir` | Digest output directory | `./data/digests` |
| `db_path` | SQLite database path | required |
| `feeds` | List of `{ name, url }` RSS/Atom sources | at least one required |

Durations (`since`, `list_since`) accept a `d` (days) suffix on top of Go's standard units
(`h`, `m`, `s`), e.g. `14d`, `168h`, `36h`.

## Feeds

Any RSS/Atom URL works. Medium feeds:

- `https://medium.com/feed/tag/<tag>`
- `https://medium.com/feed/@<username>`
- `https://medium.com/feed/<publication>`

## Interest profile

`configs/profile.example.md` is a starting point — free-form markdown describing what you
care about. It's injected verbatim into the scoring prompt, so the more specific, the better
the ranking.