# Configuration

Three files under `configs/`:

| File | Contents |
|---|---|
| `config.yaml` | How to run — model, scoring, storage, paths |
| `feeds.yaml` | Feeds to seed the store with on first run — see [Feeds](#feeds) |
| `profile.md` | The interest profile used for ranking |

## Getting started

```sh
make setup   # copies the *.example.* templates for any file that is missing
```

The templates ship pointing at the example profile and feed list so a fresh checkout runs
without further edits. Once you have your own, update `configs/config.yaml` accordingly:

```yaml
profile: ./configs/profile.md
ingest:
  feeds: ./configs/feeds.yaml
```

> **Note:** until those paths are changed, edits to `configs/profile.md` and
> `configs/feeds.yaml` have no effect — the application reads the example files.

`config.yaml` and `profile.md` are read once at startup; changes made while the server is
running are not picked up, so restart it. Feeds are the exception — they live in the
database and are edited from the **Sources** page, taking effect immediately. The loaded
configuration can be inspected from the web UI under the gear menu (View config).

## config.yaml

All fields are optional unless marked required.

| Field | Description | Default |
|---|---|---|
| `user` | Name shown in the web UI's shell prompt | OS login name |
| `profile` | Path to the interest profile | **required** |
| `inference.provider` | `ollama` \| `vllm` \| `heuristic` | `ollama` |
| `inference.host` | Inference server URL | `http://localhost:11434` |
| `inference.model` | Model name | `qwen3:4b` |
| `inference.api_key` | Bearer token, where the endpoint requires one | none |
| `inference.think` | Allow model reasoning before scoring, with fallback | `true` |
| `inference.batch_size` | Articles per scoring request | `5` |
| `inference.max_parallel` | Scoring requests in flight | `2` |
| `inference.model_tuning.*` | Decoding limits — see below | see below |
| `ingest.since` | Lookback window for new items | `14d` |
| `ingest.feeds` | Path to the feed seed file | `feeds.yaml` beside `config.yaml` |
| `ingest.digest_dir` | Output directory for `ingest --markdown` | none — required by that flag |
| `store.db_path` | SQLite database file | **required** |

Durations accept a `d` (days) suffix in addition to the standard `h`, `m` and `s` — for
example `14d`, `168h`, `1h30m`.

**Providers**

- `ollama` — a local Ollama server; the default.
- `vllm` — an OpenAI-compatible vLLM endpoint.
- `heuristic` — keyword matching with no model involved. Suitable for offline use and for
  testing feed and filter changes without inference latency.

**Thinking fallback**

Not every model has a reasoning mode. On Ollama, `think: true` is probed once at startup: a
model that rejects it falls back to `think: false` with a warning, rather than failing every
scoring request. Any other error during the probe leaves thinking on, so a transient problem
is not mistaken for a missing feature. vLLM has no such probe — `think` is passed through as
set.

> **Note:** `api_key` is stored in plaintext. The web UI's config viewer masks it, but the
> file itself is unprotected — keep `configs/config.yaml` out of version control (it is
> gitignored by default).

### inference.model_tuning

Decoding limits sent with every scoring request. Omit the block, or any field within it, to
take the defaults. Worth adjusting when substituting a model that is more or less verbose.

| Field | Description | Default |
|---|---|---|
| `num_ctx` | Input window | server default |
| `max_tokens` | Cap on the whole reply; `0` derives it from the three fields below | `0` |
| `tokens_per_item` | Allowance per article in a batch | `256` |
| `tokens_overhead` | Allowance for the JSON envelope | `256` |
| `tokens_thinking` | Added when `think` is enabled | `2048` |
| `reason_max_chars` | Maximum length of the per-item rationale | `200` |

Responses are constrained to a fixed JSON shape, so scores are always returned as integers
and `reason_max_chars` is enforced rather than merely requested.

> **Recommendations**
>
> - **Set `num_ctx` explicitly.** Left unset, Ollama silently truncates an over-long prompt
>   and the model scores articles it did not fully receive. `8192` is a reasonable starting
>   point. Ollama only; vLLM fixes the equivalent at startup via `--max-model-len`.
> - **Use `max_parallel: 1` with Ollama.** Requests are queued rather than served
>   concurrently, so higher values yield no additional throughput.
> - **Disable `think` for small models** (1B–4B). Reasoning quality is limited at that size
>   and consumes `tokens_thinking` on every request.
> - **Keep `batch_size` moderate.** Larger batches lengthen the prompt and increase the
>   chance of the model losing track of individual items; `5` is a reasonable balance.

## Feeds

Feeds live in the database and are managed from the **Sources** page (side menu → Sources,
or the gear menu → Edit sources). You can add, edit, enable, disable and delete feeds there,
edit the set-wide defaults, and search the list. Changes take effect on the next ingest run —
no restart.

`configs/feeds.yaml` is a **seed file**, not the live configuration. On every `serve`
startup, any feed in it the database has never seen is added; everything else is left as it
is. Nothing ever writes to it. Start with `--debug` to see which file was read and how many
feeds it added.

So both routes work, and neither undoes the other:

- Adding an entry to the file and restarting picks it up.
- Disabling, retuning or deleting a feed on the Sources page survives every restart. That is
  why deleting hides a feed rather than dropping it — the database has to remember it, or
  the next restart would add it back from the file.

The **export** button on the Sources page gives you the current set in this same YAML shape.
Copy it over `configs/feeds.yaml` to reproduce the set on another install.

### Feed URLs

You can leave `https://` off — `example.com/feed.xml` is filled in for you, here and on the
Sources page. Either way it is the same feed, not two.

`http://` is accepted and left as you wrote it, because some feeds only serve it. The Sources
page marks those insecure: the request goes over the network in the clear, so anyone in
between can see which feed you asked for.

### The seed file's shape

A `defaults:` block followed by the feed list. Any RSS or Atom URL is accepted.

```yaml
defaults:
  since: 7d
  max_items: 25

feeds:
  - name: Hugging Face blog
    url: https://huggingface.co/blog/feed.xml
    tags: [AI, Research]

  - name: Medium — AI          # high volume: tighter window, harder cap
    url: https://medium.com/feed/tag/artificial-intelligence
    since: 2d
    max_items: 10

  - name: Old Newsletter       # disabled, not deleted
    url: https://example.com/feed.xml
    enabled: false
```

| Field | Description | Default |
|---|---|---|
| `name` | Display name; also the source items are stored under | **required, unique** |
| `url` | RSS or Atom URL; a missing scheme becomes `https://` | **required, unique** |
| `enabled` | `false` retains the feed but never fetches it | `true` |
| `since` | Lookback window for this feed | defaults, then `ingest.since` |
| `max_items` | Maximum items contributed per run; `0` is uncapped | defaults, then uncapped |
| `tags` | Free-form labels, used for filtering in the UI | defaults' tags |

The defaults accept `since`, `max_items`, `enabled` and `tags`, applying each to any feed
that does not set it. Values resolve through the following chain:

```
feed  →  feed defaults  →  config.yaml ingest.since  →  built-in
```

Tags are the exception: a feed's tags are added to the defaults rather than replacing them.
On the Sources page, leaving `since` or `max items` blank means the feed takes the default —
the greyed value in the empty box is what it will use.

`ingest.feeds` in `config.yaml` names the seed file. It defaults to `feeds.yaml` beside the
config. A missing seed file is not an error — it just means nothing new to import.

> **Recommendations**
>
> - **Cap high-volume feeds.** Some return their entire archive: the Hugging Face blog feed
>   returns roughly 800 items in a single fetch, and without `max_items` every one of them
>   is sent for scoring on the first run.
> - **Disable feeds rather than deleting them.** Disabling keeps the feed in the list with
>   its history; deleting takes it out of the list and out of every run.

### Deleting is reversible

Deleting a feed hides it rather than dropping it, so nothing is lost. Two ways back, neither
involving the database:

- Pick **deleted** in the **state** filter beside the search box, then **restore** on the
  feed you want.
- **Add the same URL again.** That brings the old feed back with whatever you type this
  time, instead of creating a second one.

Either way the feed keeps its fetch history. What you cannot do from the page is erase a
deleted feed for good — deliberately, for the reason below.

### Deleted feeds and the seed file

A deleted feed still counts as one the database has seen, so a feed you remove stays removed
even while it is still listed in `feeds.yaml`. Without that, every restart would bring it
back.

To be rid of a feed for good, delete it on the Sources page *and* take it out of the seed
file. Otherwise adding it again later restores the old feed rather than starting fresh.

### Feed identity

Every feed gets an ID when it is first stored, derived from its URL at that moment and then
frozen. That ID is what fetch history hangs off, so:

- **Renaming keeps the feed's history**, and the Sources page re-files its existing items
  under the new name so they don't split into two sources in the feed list.
- **Changing a URL also keeps its history.** The feed is the row, not the link.
- **Names and URLs are unique.** One feed per name (items are stored under it) and one per
  link.

### Feed health

Each run records, per feed, whether the fetch succeeded, how many items it returned, and how
long it took. The Sources page presents this as a status indicator, the current failure
streak, the time of the last success, and a strip of recent attempts, so trends are visible
rather than only the most recent result.

- A failing feed does not fail the run; it is logged, recorded and skipped.
- History is recorded on `--dry-run` as well, so an unreachable feed is visible even on a
  run that persists nothing.
- The most recent 200 attempts per feed are retained. Removing a feed leaves its history
  intact, so re-adding it restores the record.

### Medium feed URLs

```
https://medium.com/feed/tag/<tag>
https://medium.com/feed/@<username>
```

## Interest profile

Free-form markdown describing the subject matter of interest; `configs/profile.example.md`
is a starting point. It is passed to the model verbatim with every batch of articles and is
the primary influence on how items are scored.

HTML comments (`<!-- ... -->`) are removed before the profile reaches the model, so notes to
yourself can be kept in the file without being read as interests.

> **Recommendation:** be specific, and state exclusions as well as interests. "Kubernetes
> operators, KubeVirt, Go internals — not funding rounds or product launches" ranks
> considerably better than "AI and infrastructure". When results are consistently
> off-target, revise this file before changing the model.

## The Maze weather widget

The Maze page carries a weather and pollen read-out. None of it lives in the config files:
every setting is in the browser, under Settings → Weather.

| Setting | What it does |
|---|---|
| Weather, Pollen | Show or hide each. Weather off means no location prompt and no requests |
| Layout | Sub-bar (the default), inline chip, or either rail; the rails show the full read-out |
| Units | °C or °F |
| Hours | 24-hour or AM/PM, matching the clock |
| Location | Type a city, or press "Use my location" |

Data comes from [Open-Meteo](https://open-meteo.com), fetched by the browser and cached for 30
minutes. You are asked for a location once, on the first visit; decline and nothing is
requested, and the city search does the same job. [SECURITY.md](../SECURITY.md) has what gets
sent.
