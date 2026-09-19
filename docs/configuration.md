# Configuration

Files under `configs/`:

| File | Contents |
|---|---|
| `config.yaml` | How to run — model, scoring, storage, paths |
| `feeds.yaml` | Feeds to seed the store with on first run — see [Feeds](#feeds) |
| `prompts/profile.example.md` | Optional example/legacy import source; live profiles are in SQLite |
| `prompts/system.md` | Optional [system prompt](#system-prompt) override; omit to use the built-in default |

The system prompt is still file/config owned. Interest profiles are application data managed
under **Settings → Profiles** and persisted in SQLite.

## Getting started

```sh
make setup   # copies the *.example.* templates for any file that is missing
```

The templates ship pointing at the example feed list. A fresh checkout needs no profile file:
it uses the immutable built-in **Default**. Once you have your own feed seed, update
`configs/config.yaml` accordingly:

```yaml
ingest:
  feeds: ./configs/feeds.yaml
```

Create, duplicate, edit and select profiles from **Settings → Profiles**. Those changes take
effect on the next ingest without restarting the server.

`config.yaml` and a system-prompt override are read at startup, so restart after changing
them. Feeds and profiles live in the database and take effect on the next ingest. The loaded
configuration can be inspected from the web UI under the gear menu (View config).

## config.yaml

All fields are optional unless marked required.

| Field | Description | Default                           |
|---|---|-----------------------------------|
| `user` | Name shown in the web UI's shell prompt | OS login name                     |
| `profile` | Optional legacy Markdown profile to import at boot | built-in Default |
| `inference.provider` | `ollama` \| `vllm` \| `heuristic` | `ollama`                          |
| `inference.host` | Inference server URL | `http://localhost:11434`          |
| `inference.model` | Model name | `qwen3.5:4b`                      |
| `inference.api_key` | Bearer token, where the endpoint requires one | none                              |
| `inference.think` | Allow model reasoning before scoring, with fallback | `true`                            |
| `inference.system_prompt` | Path to a system-prompt override, or `false` to send none — see below | built-in                          |
| `inference.batch_size` | Articles per scoring request | `1`                               |
| `inference.max_parallel` | Scoring requests in flight | `1`                               |
| `inference.model_tuning.*` | Decoding limits — see below | see below                         |
| `ingest.since` | Lookback window for new items | `7d`                              |
| `ingest.feeds` | Path to the feed seed file | `feeds.yaml` beside `config.yaml` |
| `ingest.digest_dir` | Output directory for `ingest --markdown` | none — required by that flag      |
| `store.db_path` | SQLite database file | one of `db_path`/`url` required   |
| `store.url` | Postgres connection URL, instead of `db_path` | one of `db_path`/`url` required   |

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
| `tokens_per_item` | Allowance per article in a batch | `1024` |
| `tokens_overhead` | Allowance for the JSON envelope | `512` |
| `tokens_thinking` | Added when `think` is enabled | `2048` |
| `reason_max_chars` | Maximum length of the per-item rationale | `512` |

Responses are constrained to a fixed JSON shape, so scores are always returned as integers
and `reason_max_chars` is enforced rather than merely requested.

> **Recommendations**
>
> - **Set `num_ctx` explicitly.** Left unset, Ollama silently truncates an over-long prompt
>   and the model scores articles it did not fully receive. `8192` is a reasonable starting
>   point. Ollama only; vLLM fixes the equivalent at startup via `--max-model-len`.
> - **Leave `max_parallel` at 1 with Ollama.** Requests are queued rather than served
>   concurrently, so higher values yield no additional throughput. Raise it only against an
>   endpoint that genuinely serves in parallel, such as vLLM.
> - **Disable `think` for small models** (1B–4B). Reasoning quality is limited at that size
>   and consumes `tokens_thinking` on every request. The shipped example config does this.
> - **Raise `batch_size` only on a capable model.** One article per request is the default
>   because a local 4B has the whole context to itself and scores it better; larger batches
>   lengthen the prompt and increase the chance of the model losing track of individual items.

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
- Disabling, retuning or deleting a feed on the Sources popup survives every restart. That is
  why deleting hides a feed rather than dropping it — the database has to remember it, or
  the next restart would add it back from the file.

The **export** button on the Sources popup gives you the current set in this same YAML shape.
Copy it over `configs/feeds.yaml` to reproduce the set on another install.

### Feed URLs

You can leave `https://` off — `example.com/feed.xml` is filled in for you, here and on the
Sources window. Either way it is the same feed, not two.

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
| `type` | Source kind: `rss`, `blog`, `news` or `academic`. RSS and academic are implemented; blog and news are not yet implemented | `rss` |
| `enabled` | `false` retains the feed but never fetches it | `true` |
| `since` | Lookback window for this feed | defaults, then `ingest.since` |
| `max_items` | Maximum items contributed per run; `0` is uncapped | defaults, then uncapped |
| `tags` | Free-form labels, used for filtering in the UI | defaults' tags |

### Academic sources

Set `type: academic` to use an academic provider. The URL host selects arXiv,
Crossref or Semantic Scholar, and its query parameters define the saved search.

Supported search parameters include `q`, `query`, `search_query` and
`query.bibliographic`. Optional filters are `publisher`, `journal`, `issn`,
`open_access` and `min_citations`. Crossref-style `query.publisher-name` and
`query.container-title` are also accepted.

`since` and `max_items` are applied by the common ingest pipeline after provider
results are normalized.

The defaults accept `since`, `max_items`, `enabled` and `tags`, applying each to any feed
that does not set it. Values resolve through the following chain:

```
feed  →  feed defaults  →  config.yaml ingest.since  →  built-in
```

Tags are the exception: a feed's tags are added to the defaults rather than replacing them.
On the Sources window, leaving `since` or `max items` blank means the feed takes the default —
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

To be rid of a feed for good, delete it on the Sources section *and* take it out of the seed
file. Otherwise adding it again later restores the old feed rather than starting fresh.

### Feed identity

Every feed gets an ID when it is first stored, derived from its URL at that moment and then
frozen. That ID is what fetch history hangs off, so:

- **Renaming keeps the feed's history**, and the Sources section re-files its existing items
  under the new name so they don't split into two sources in the feed list.
- **Changing a URL also keeps its history.** The feed is the row, not the link.
- **Names and URLs are unique.** One feed per name (items are stored under it) and one per
  link.

### Feed health

Each run records, per feed, whether the fetch succeeded, how many items it returned, and how
long it took. The Sources section presents this as a status indicator, the current failure
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

## Store

Set exactly one of `store.db_path` or `store.url`. Which one is set picks the engine, so
there is no separate driver setting that could disagree with it. Setting both, or neither,
is rejected at startup.

```yaml
store:
  db_path: ./data/rabbithole.db   # SQLite, the local default
  # url: postgres://rabbithole@db.example.com:5432/rabbithole
```

SQLite is the right answer for one machine and needs nothing installed. Postgres is for
reaching the same store from more than one machine, and is what to use with a hosted
database such as Supabase, RDS or Cloud SQL.

**One process at a time.** Whichever engine you choose, only one `rabbithole serve` may
point at a store at a time. Machines take turns. A second server reaching the same database
marks the first one's running ingest as failed, because interrupted runs are reconciled at
startup (see [store](store.md#one-writer-at-a-time)).

### The password

It comes from the `RABBITHOLE_DB_PASSWORD` environment variable, not the config file, which
keeps it out of anything that copies `config.yaml` around and out of the web UI's config
viewer. `.env.example` is a starting point; nothing reads `.env` automatically, so source it
from your shell or point a systemd unit at it with `EnvironmentFile=`. The `make` targets
that run the binary do pick up `./.env`.

```bash
export RABBITHOLE_DB_PASSWORD=...
```

A URL that already carries a password still works, and the environment variable overrides it
when both are set. Expect a warning at startup in that case: a password in `store.url` lives
in the config file, which is the thing the variable exists to avoid.

### TLS

`sslmode` defaults to **`verify-full`** when the URL does not name one, which both encrypts
the connection and checks that the server is who it claims to be. Weaker modes have to be
asked for explicitly:

| Mode | Meaning |
|---|---|
| `verify-full` | Encrypted, certificate and hostname verified. The default |
| `verify-ca` | Encrypted, certificate verified, hostname not |
| `require` | Encrypted, but nothing is verified, so it can be intercepted |
| `disable` | Not encrypted. Only sensible over a loopback connection |

## Interest profile

Profiles are first-class local application data. The effective profile is free-form Markdown
passed with every scoring batch and is the primary influence on how items are scored.

A fresh database has an application-owned **Default** profile:

- its stable identity and content are built into the binary;
- it can be selected and duplicated;
- it cannot be edited or deleted;
- it does not require a writable `profile.md`.

Local profiles live in SQLite and are managed under **Settings → Profiles**. The friendly
editor has **I'm interested in**, **I'm less interested in**, and **Additional context**
fields. It deterministically renders Markdown. Profiles that do not match that canonical
format open in Advanced/raw mode, which preserves arbitrary Markdown instead of attempting a
lossy conversion.

The active profile is resolved once at the start of each ingest run. Switching or editing a
profile therefore affects the next run immediately, without restarting `serve`; a run already
in progress keeps its original immutable snapshot for every feed and batch. Existing scores
are never automatically invalidated or rescored.

To update existing feed rows intentionally, use **Settings → Profiles → Rescore recent
items**. After confirmation, the application:

- snapshots the currently active profile once;
- loads already-scored items from the last seven days from SQLite, without refetching feeds;
- scores them with the configured provider, model, system prompt, thinking, batch and
  parallelism settings;
- replaces successful LLM scores, explanations, model attribution and profile provenance;
- preserves ratings, notes, read/hidden state, bookmarks, tags and digest dates.

Items that still fail after the normal scoring retries keep their previous valid score. The
runner reports candidates, successful replacements and failures. Rescoring can incur the same
runtime or provider cost as scoring an equivalent number of new articles.

HTML comments (`<!-- ... -->`) are removed before the profile reaches the model, so notes to
yourself can be kept in raw Markdown without being read as interests.

### Backward compatibility: `config.profile`

An existing configuration may still contain:

```yaml
profile: ./configs/prompts/profile.md
```

At startup the file is read, HTML comments are stripped, and unreadable or semantically empty
files remain errors. Its cleaned contents are imported into a deterministic local profile:

- the same configured path does not create duplicates on later starts;
- on a database with no persisted active selection, the imported profile becomes active, so
  an existing file-based setup keeps its old scoring behavior;
- once an active profile has been selected in the UI, later restarts do not overwrite that
  choice merely because `profile:` remains configured;
- the import is a bootstrap, not synchronization. Later edits to the Markdown file do not
  overwrite the local profile. Edit the local copy in Settings, or change/remove `profile:`
  deliberately.

CLI `ingest` uses the same SQLite active profile and the same bootstrap rule as `serve`. With
no configured file and no persisted selection, both use Default.

> **Recommendation:** be specific, and state exclusions as well as interests. "Kubernetes
> operators, KubeVirt, Go internals — not funding rounds or product launches" ranks
> considerably better than "AI and infrastructure". When results are consistently
> off-target, revise the active profile before changing the model.

## System prompt

Where `profile.md` describes what the reader wants, `inference.system_prompt` controls how the
model is instructed to score and respond — the scale it uses, and the JSON shape it must reply
in. Unlike `profile`, it's optional: leave it unset and a built-in default is used.

`inference.system_prompt` takes one of three forms:

```yaml
inference:
  # system_prompt: ./configs/prompts/system.md   # override: this file's contents
  # system_prompt: false                         # send no system message at all
```

- **Unset** (the default) — the built-in prompt is used.
- **A path** — that file's contents are used instead, verbatim. HTML comments (`<!-- ... -->`)
  are stripped first, same as `profile.md`. `configs/prompts/system.example.md` carries the
  built-in prompt's exact text as a starting point to copy from — prompt files live in their own
  `configs/prompts/` folder rather than directly under `configs/`.
- **`false`** — no system message is sent at all. Useful for a model whose own Modelfile or chat
  template already bakes one in: any system message the app sends — the built-in default or an
  override — takes precedence over a model's own, so `false` is how you defer to it instead.
  (Ollama documents this directly for `/api/generate`'s `system` field as overriding what the
  Modelfile sets; `/api/chat` runs through the same templating.)

The built-in prompt already tells the model that article titles/summaries are data, not
instructions — worth keeping if you write your own override. See
[SECURITY.md](../SECURITY.md#untrusted-content) for why that matters.

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
