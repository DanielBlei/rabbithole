# Configuration

You do not need a config file. Run `rabbithole serve` and it starts on sensible defaults. Settings
you change are saved in the app's store as numbered versions that you can list and roll back. You
change them on the Settings page (the gear in the top bar), or from the command line (export, edit, import).

## Quick start

```sh
rabbithole serve
```

On a terminal, the first start asks `Create a new SQLite store at ...? [Y/n]`. Answer `Y`, then
open <http://localhost:8080>.

**Add sources.** Use the Sources page, or start from the example list. Download it, then import it
with the CLI or drop the file on the Sources page:

```sh
curl -fsSLO https://raw.githubusercontent.com/DanielBlei/rabbithole/main/docs/examples/feeds.yaml
rabbithole sources import feeds.yaml
```

**Change settings.** Click the gear, see [The Settings page](#the-settings-page).
Or from the command line, export what is saved, edit it, and import it back:

```sh
rabbithole config export -o my.yaml
$EDITOR my.yaml
rabbithole config import my.yaml
```

The export holds only what you changed, so it can be short or just `{}` when everything is at its
default. To see every setting with its value, add `--defaults`.

Keep only the lines you want to change in `my.yaml`. A setting you leave out keeps its default,
now and in later releases. A running `serve` uses the new version on its next ingest run, with no
restart.

[`docs/examples/config.yaml`](https://github.com/DanielBlei/rabbithole/blob/main/docs/examples/config.yaml)
lists every setting with its default, to copy lines from.

## Where things live

| What | Where | Changed with |
|---|---|---|
| Run settings: model, scoring, your name | The store | The Settings page, `config import`, or `serve --config` |
| Feeds (sources) | The store | The Sources page, `sources import` |
| The source library | GitHub, fetched on a Sources visit | `RABBITHOLE_LIBRARY_URL`, see [The source library](#the-source-library) |
| Interest profiles | The store | Settings → Profiles |
| Where the store is, the listen address, TLS | Flags and the environment | `--db` or `RABBITHOLE_DB`, `--addr`, the `--tls-*` flags |
| Secrets (API keys, the Postgres password) | The environment or `./.env` | Your shell profile, or editing `.env` |
| System prompt text | The store, with the run settings | The Settings page; see [System prompt](#system-prompt) |

No file is read unless you name it.

## The Settings page

Click the gear in the top bar. The tabs are General, Profiles, Scoring, Appearance, Widgets, Account
(only when sign-in is on) and Advanced. General, Scoring and Advanced hold the run settings in the
table below; Profiles holds your interest profiles, and Appearance and Widgets are saved in this
browser only (local storage), not in the store, so they do not follow you to another browser or
device.

**Saving.** Edits are staged. A bar at the bottom counts the unsaved changes, with Discard and
Review and save. Review shows each change old to new and which version saving creates; confirming
writes one version. Saving with nothing changed writes nothing. Leave a field blank to use its
default.

- **Another change landed first.** If the settings changed elsewhere (another tab, `config import`)
  while you were editing, saving says "Settings changed elsewhere". Reload to see the latest.
- **Leaving with unsaved edits** asks first. Switching tabs does not.
- **Moving the host.** If you change the inference host while an API key is set, the review asks
  before the key is sent to the new host.

**API key.** Paste a key in the page and it is written to `./.env` (never echoed back), and the
settings store only the `$RABBITHOLE_INFERENCE_API_KEY` reference. Start `serve` with
`--no-env-write` to turn this off. A key already set in the environment cannot be overwritten from
the page. Restart `serve` after a key changes, see [Known limitations](#known-limitations).

**Test connection** builds the scorer from the values on screen, saved or not, and checks the
model. It writes nothing.

**System prompt.** Choose Built-in, Custom text or None, see [System prompt](#system-prompt).

**Advanced.** Saved versions: compare any version with the active one and roll back (a rollback
that moves the inference host while an API key is set asks first, like a host change). Import: choose a config file, read the
preview (what will change, plus notes for deprecated keys, see [Deprecated keys](#deprecated-keys)),
then apply. Export downloads the saved settings as an import file. A value in an imported file that equals its default is not saved, so it stays a default.

## Settings

Every field is optional. A setting you leave out takes its default, now and in later releases.

| Field | Description | Default |
|---|---|---|
| `user` | Name shown in the web UI's shell prompt | OS login name |
| `inference.provider` | `ollama` \| `vllm` \| `openai` \| `heuristic` | `ollama` |
| `inference.host` | Inference server URL | `http://localhost:11434` |
| `inference.model` | Model name | `qwen3.5:4b` |
| `inference.api_key` | A `$RABBITHOLE_*_API_KEY` reference, see [Secrets](#secrets) | none |
| `inference.think` | Allow model reasoning before scoring, with fallback | `true` |
| `inference.structured_output` | Send the verdict schema for the server to enforce. Keep it on; set `false` only for an endpoint that returns scores with reasons that make no sense, see [troubleshooting](troubleshooting.md#scores-of-0-with-reasons-that-make-no-sense) | `true` |
| `inference.effort` | `openai` only: `reasoning_effort` while thinking, `low` \| `medium` \| `high` \| `xhigh` | `medium` |
| `inference.system_prompt` | The prompt text, or `false` to send none, see [System prompt](#system-prompt) | built-in |
| `inference.batch_size` | Articles per scoring request | `1` |
| `inference.max_parallel` | Scoring requests in flight | `1` |
| `inference.model_tuning.*` | Decoding limits, see [model_tuning](#inferencemodel_tuning) | see below |
| `ingest.digest_dir` | Deprecated, removed in v0.4.0. Output directory for `ingest --markdown`; use `--out <dir>` instead | none |

A file may also carry `ingest.since`, which is not a stored setting. It is an import-only alias for the
[feed defaults](#feeds) `since`: importing the file sets it there. See [Deprecated keys](#deprecated-keys).

Durations accept a `d` (days) suffix in addition to the standard `h`, `m` and `s`, for example
`14d`, `168h`, `1h30m`.

### Fields by provider

Each provider reads only some fields. A field the chosen provider ignores is dropped when you
import, with one warning that lists what was dropped (`ignored for provider ollama: inference.effort`).
It is never an error, so an old file keeps importing after you switch provider.

| Field | ollama | vllm | openai | heuristic |
|---|---|---|---|---|
| `host`, `model` | yes | yes | yes | |
| `api_key` | yes | yes | yes | |
| `think` | yes | yes | yes | |
| `effort` | | | yes | |
| `structured_output`, `system_prompt` | yes | yes | yes | |
| `batch_size`, `max_parallel` | yes | yes | yes | yes |
| `model_tuning.num_ctx` | yes | | | |
| other `model_tuning.*` | yes | yes | yes | |

`user` and `ingest.digest_dir` apply whatever the provider.

### Providers

- `ollama`: a local Ollama server. The default.
- `openai`: OpenAI, or any hosted gateway that speaks its chat-completions API.
- `vllm`: a vLLM server.
- `heuristic`: keyword matching with no model. Good for offline use and for testing feed
  changes without waiting on a model.

### Thinking

`think: true` lets the model reason before it scores. That is slower but usually better. Not
every model has a reasoning mode, so rabbithole checks once at startup and falls back to
`think: false` with a warning instead of failing every request. Small models (1B to 4B) do
better with `think: false`.

On `openai`, `effort` sets how hard it thinks. Raise `model_tuning.tokens_thinking` for `high`
and `xhigh`, because reasoning tokens count against the reply budget.

If `think: false` seems to be ignored, see
[troubleshooting](troubleshooting.md#think-false-does-not-reduce-thinking). How the startup
checks work is in [architecture](architecture.md#provider-startup-checks).

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
and `reason_max_chars` is enforced rather than merely requested. With
`inference.structured_output: false` the shape comes from the system prompt instead, and
`reason_max_chars` is only asked for. The built-in prompt and `docs/examples/prompts/system.md` both spell
the shape out; a custom prompt has to as well, and `system_prompt: false` with structured output
off logs a warning at startup.

> **Recommendations**
>
> - **Set `num_ctx` explicitly.** Left unset, Ollama silently truncates an over-long prompt
>   and the model scores articles it did not fully receive. `8192` is a reasonable starting
>   point. Ollama only; vLLM fixes the equivalent at startup via `--max-model-len`.
> - **Leave `max_parallel` at 1 with Ollama.** Requests are queued rather than served
>   concurrently, so higher values yield no additional throughput. Raise it only against an
>   endpoint that genuinely serves in parallel, such as vLLM.
> - **Disable `think` for small models** (1B to 4B). Reasoning quality is limited at that size
>   and consumes `tokens_thinking` on every request. Set `think: false` in your config.
> - **Raise `batch_size` only on a capable model.** One article per request is the default
>   because a local 4B has the whole context to itself and scores it better; larger batches
>   lengthen the prompt and increase the chance of the model losing track of individual items.


## Secrets

The store never holds a secret. It holds a reference to an environment variable, and the
variable is resolved when a run needs it.

```yaml
inference:
  api_key: $RABBITHOLE_INFERENCE_API_KEY
```

The reference must be a whole `$NAME` or `${NAME}` where `NAME` starts with `RABBITHOLE_` and ends
with `_API_KEY`. That list is deliberately narrow. Any other name is refused on import, so a
config file cannot point at `$AWS_SECRET_ACCESS_KEY` and a changed `host` cannot carry an
unrelated variable to another server. `RABBITHOLE_DB` and `RABBITHOLE_DB_PASSWORD` can never be
config references. The error suggests the fix: `export RABBITHOLE_INFERENCE_API_KEY=$YOUR_VAR`.

- The reference is shown in Settings, Advanced, in `config export` and in `config history`. It is a
  name, not a secret. The value is never shown or logged.
- An unset variable is a warning after `config import`, naming the variable, and a clear error when
  a run needs the key. It is never an unexplained 401 later.

### Setting the value

Put it in your shell profile (`~/.zshrc`, `~/.bashrc`) or in `./.env`. Either works, and the
environment wins when both are set.

```sh
# ./.env
RABBITHOLE_INFERENCE_API_KEY=sk-...
```

The binary reads `./.env` itself on every command (`--env-file` reads another path). It loads only
`RABBITHOLE_*` variables and never overrides one the real environment already sets. `.env` is
gitignored. `.env.example` is a starting point, and `make setup` creates `.env` from it.

The format is `KEY=value`, one per line. A leading `export ` is accepted, and one matching pair of
single or double quotes around the value is removed. Nothing is expanded, and a `#` inside a value
is kept. Lines starting with `#` are comments.

### A literal key in an import

If an imported file has a literal `api_key: sk-...`, rabbithole moves it to `./.env` under
`RABBITHOLE_INFERENCE_API_KEY`, stores only
the reference, and warns with the variable name. The file is created `0600`, and your other lines
in it are kept as they are. If the real environment already sets that variable, it wins and the
file's value is ignored with a warning. A key that is not a safe single-line value is refused.

## Import and export

Four commands manage the saved settings. [cli.md](cli.md#config) has the details of each.

```sh
rabbithole config export -o my.yaml   # the saved settings, as an import file
rabbithole config export --defaults   # every setting with the value in force
rabbithole config export -f json      # the same as JSON (also when -o ends in .json)
rabbithole config import my.yaml      # save a file as the new active version
rabbithole config history             # versions, newest first
rabbithole config rollback 3          # go back to version 3
```

A plain export lists only what you changed. `--defaults` adds every other setting at its default for
your provider, and leaves out the ones your provider does not read. API keys stay `$RABBITHOLE_*_API_KEY`
references. JSON has no comment header; both formats import the same way.

Import saves only what differs from the defaults. A value in the file that equals its default (a full
`--defaults` export, or a line copied from the example) is not saved and stays a default, and the command
says how many it skipped. Later releases can still improve those defaults for you.

Import never asks: the file wins, a file equal to the active settings changes nothing, and an
invalid file saves nothing. Every change is a new version, so nothing is lost and a rollback can
itself be rolled back.

### The --config file

If the file is your source of truth (a container, a systemd unit, a cron job), pass
`--config <file>` to any command. The file is applied when it changes since it was last applied.
Edits made in the app or with `config import` are kept until then. In detail:

- If the file has not changed since it was last applied, nothing happens on start.
- When it has changed, it replaces the whole settings document. There is no per-field merge: a
  field you set in the app and left out of the file goes back to its default.
- If the file equals the active settings, no version is saved. Otherwise it is saved as a new
  version, and if that replaces a version saved somewhere else, a warning names that version and
  the command to undo it.
- A feeds file, a profile or `ingest.since` named in the file are applied again when the file or
  the files it names change. The feeds file only adds feeds the store has not seen.
- There is no prompt and no force flag. The last writer wins, and the history keeps everything.
- With a shared Postgres store, pass `--config` on one instance only. The record of what was
  applied is kept per file path.

`serve` says so in its log, and the settings page shows the same note:

```
INF config imported from ./config.yaml; edits in Settings are kept until the file changes
WRN config version 3 (from import of ./other.yaml, saved 14:02) replaced by ./config.yaml; undo with: rabbithole config rollback 3
```

If you edit with export and import, leave `--config` off. The `store` block of a `--config` file
only locates the store, see [Store location](#store-location). It is never saved.
`eval benchmark` reads `--config` without importing it.

## Feeds

A source (a "feed" throughout the UI and this page) is anything ingest reads: an RSS or Atom
feed (`type: rss`, the default), or a saved search on arXiv, Crossref or Semantic Scholar
(`type: academic`, see [Academic sources](#academic-sources)). `blog` and `news` are reserved
for source types still to come and are skipped by ingest for now.

Feeds live in the store and are managed from the **Sources** page (side menu → Sources). You can add, edit, enable, disable and delete feeds there, pick each
one's type, edit the set-wide defaults, and search the list. Changes take effect on the next
ingest run, with no restart.

### Importing and exporting feeds

Import a feeds file from the Sources page (the import button opens a file picker, or drop a file on
the page) or with the CLI:

```sh
rabbithole sources import feeds.yaml
```

The example list is
[`docs/examples/feeds.yaml`](https://github.com/DanielBlei/rabbithole/blob/main/docs/examples/feeds.yaml)
(download it with the `curl` command in the [Quick start](#quick-start)). An import adds the feeds
the store has never held. Known feeds, deleted ones included, are skipped, so importing the same
file twice adds nothing the second time. The page shows how many were added and skipped, or the
parse error inline.

The **export** button on the Sources page and `rabbithole sources export [-o file]` write the
current set in the same shape, so you can move it to another install. Nothing reads a feeds file at
startup. See [cli.md](cli.md#sources).

An old config whose `ingest.feeds` names a feeds file is honoured only when you import that config
(see [Upgrading from v0.2.x](#upgrading-from-v02x)).

### The source library

The **Library** in the Sources side menu lists ready-made feeds grouped by topic, and the Overview
suggests some that match the tags of your sources. The lists are fetched from the project's GitHub
`main` branch the first time you open Sources, never at startup, and cached for an hour. A fix to
the library reaches every install within that hour. If GitHub is unreachable, the Library page says
so and offers **Try again**; nothing else on the page waits for it.

- `RABBITHOLE_LIBRARY_URL` loads the library from another directory URL (a mirror or a fork that holds
  `rss.yaml` and `academic.yaml`), on any port. `off` turns the Library off.
- The usual `HTTPS_PROXY` and `NO_PROXY` variables are honoured. With a proxy, the proxy resolves
  hostnames, so they are not checked here; IP addresses and `localhost` in a URL still are.
- The fetch refuses private and loopback addresses. For a mirror on your own network set
  `RABBITHOLE_ALLOW_PRIVATE_FETCH=1`. Link-local addresses, cloud metadata included, stay blocked.
  A bad `RABBITHOLE_LIBRARY_URL` (not `off`, not an http or https URL with a host) is logged at
  startup and the default is used. A redirect from https to http is refused.

Both variables can be set in `./.env` like the other `RABBITHOLE_*` ones. Contributing a source:
[`library/README.md`](https://github.com/DanielBlei/rabbithole/blob/main/library/README.md).

### Feed URLs

You can leave `https://` off: `example.com/feed.xml` is filled in for you, here and on the
Sources window. Either way it is the same feed, not two.

`http://` is accepted and left as you wrote it, because some feeds only serve it. The Sources
page marks those insecure: the request goes over the network in the clear, so anyone in
between can see which feed you asked for.

### The feeds file's shape

A `defaults:` block followed by the feed list. Any RSS or Atom URL works as `type: rss`;
academic searches are described under [Academic sources](#academic-sources).

The list key can be `feeds:` or `sources:`. Import a file from the Sources page and it shows
what would change (new, already subscribed, removed, clashing) before anything is added.

```yaml
defaults:
  since: 7d
  max_items: 25

feeds:
  - name: Hugging Face blog
    url: https://huggingface.co/blog/feed.xml
    tags: [AI, Research]

  - name: "Medium: AI"          # high volume: tighter window, harder cap
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
| `url` | RSS or Atom URL, or an academic search URL for `type: academic`; a missing scheme becomes `https://` | **required, unique** |
| `type` | Source kind: `rss`, `blog`, `news` or `academic`. RSS and academic are implemented; blog and news are not yet implemented | `rss` |
| `enabled` | `false` retains the feed but never fetches it | `true` |
| `since` | Lookback window for this feed | defaults, then `7d` |
| `max_items` | Maximum items contributed per run; `0` is uncapped | defaults, then uncapped |
| `tags` | Free-form labels, used for filtering in the UI | defaults' tags |

### Academic sources

Set `type: academic` in the feeds file, or pick **academic** as the type when adding a feed
on the Sources page, to use an academic provider. The URL host selects arXiv, Crossref or
Semantic Scholar, and its query parameters define the saved search. The page offers only the
types ingest can fetch, `rss` and `academic`.

| Provider | Example URL |
|---|---|
| arXiv | `https://arxiv.org/search?q=agentic+coding` |
| Crossref | `https://api.crossref.org/works?q=llm&publisher=ACM` |
| Semantic Scholar | `https://www.semanticscholar.org/search?q=code+agents&min_citations=5` |

```yaml
  - name: arXiv RAG research
    type: academic
    url: https://arxiv.org/search?q=retrieval+augmented+generation
    tags: [Research]
```

Supported search parameters include `q`, `query`, `search_query` and
`query.bibliographic`. Optional filters are `publisher`, `journal`, `issn`,
`open_access` and `min_citations`. Crossref-style `query.publisher-name` and
`query.container-title` are also accepted.

`since` and `max_items` are applied by the common ingest pipeline after provider
results are normalized.

The defaults accept `since`, `max_items`, `enabled` and `tags`, applying each to any feed
that does not set it. Values resolve through the following chain:

```
feed  →  feed defaults  →  built-in (7d for `since`)
```

Tags are the exception: a feed's tags are added to the defaults rather than replacing them.
On the Sources window, leaving `since` or `max items` blank means the feed takes the default:
the greyed value in the empty box is what it will use.

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
deleted feed for good, on purpose: its history stays with it.

### Deleted feeds and imports

A deleted feed still counts as one the store has seen, so importing a file that lists it does not
bring it back. To start a deleted feed fresh instead of restoring it, there is no shortcut by
design: restore it, or add the same URL again, and it returns with its history.

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

## Store

Which engine you use is picked by the location: a file path means SQLite, a `postgres://` URL means
Postgres. There is no separate driver setting that could disagree with it.

SQLite is the right answer for one machine and needs nothing installed. Postgres is for reaching
the same store from more than one machine, and is what to use with a hosted database such as
Supabase, RDS or Cloud SQL. The subsections on the password, TLS, pooling and privileges apply
only to Postgres.

### Store location

The first of these wins:

1. `--db <path or url>`
2. the `RABBITHOLE_DB` environment variable (also read from `./.env`)
3. the `store` block of the `--config` file, if you pass one (`db_path` or `url`, never both,
   plus `max_conns` and `conn_max_lifetime` for Postgres). It only locates the store and is never
   saved.
4. the default

`rabbithole store path` prints which store this command line would use and why, without creating
anything.

**The default** is a file in your per-user data directory:

| OS | Path |
|---|---|
| Linux | `$XDG_DATA_HOME/rabbithole/rabbithole.db`, or `~/.local/share/rabbithole/rabbithole.db` |
| macOS | `~/Library/Application Support/rabbithole/rabbithole.db` |

The directory is created `0700` and the database `0600`, because it holds the login hash and the
session signing key.

**Legacy locations.** If there is no store in the per-user directory, `./data/rabbithole.db` and then
`./rabbithole/data/rabbithole.db` (where earlier releases kept it) are still used, with a
deprecation warning. They are supported until v0.4.0. An existing per-user store wins over both.

**First start.** When no store exists yet and a terminal is attached, rabbithole shows where it
looked and asks `Create a new SQLite store at ...? [Y/n]`. `n` creates nothing and exits 0, so you
can set `RABBITHOLE_DB` first. Without a terminal (a service, a script) it creates the store and
logs a hint on how to choose another location. Read-only commands such as `items list` and
`config history` never create a store.

### Using Postgres

Choose it before the first start, in `.env` or your shell profile:

```sh
RABBITHOLE_DB=postgres://rabbithole@db.example.com:5432/rabbithole
RABBITHOLE_DB_PASSWORD=...
```

The database must already exist. rabbithole creates its tables, not the database. Postgres is never
a fallback: if it cannot be reached, the command fails instead of quietly using a local file.
`--db-max-conns` and `--db-conn-max-lifetime` (or the same two fields in a `--config` file's
`store` block) tune the pool and are an error beside a SQLite path.

**One process at a time.** Whichever engine you choose, only one `rabbithole serve` may point at
a store at a time; machines take turns. On Postgres the second server is refused at startup
(`another rabbithole is already serving this store`) instead of being left to mark the first
one's running ingest as failed; on SQLite nothing enforces it. One-shot commands (`items`,
`auth`, `ingest`) place no claim, so they still run while the server is up. See
[store](store.md#one-writer-at-a-time).

### The password

Set `RABBITHOLE_DB_PASSWORD` in `./.env` or your shell profile. It is the password for the
`RABBITHOLE_DB` URL, so the URL itself carries none. Nothing is stored, and the log and the
`store path` output show the host and database, never the password.

A password in the URL (`postgres://user:pw@host/db` or `?password=pw`) also works, but it is not
recommended and you get a warning at startup: the URL then lives in your shell history, the
process list and any config file. A `$RABBITHOLE_DB_PASSWORD` reference inside the URL is
expanded from the environment. Connection errors name the database without the password, even
when the URL is too malformed to parse.

A password is not required: a server that trusts this client can be reached without one. The
error says so when a connection is refused and no password was set.

### TLS

If the URL names no `sslmode`, the connection is encrypted with `require`, but the server's
certificate is not checked. That is what makes Supabase and RDS work on the first try. For anything
beyond loopback, ask for verification:

| Mode | Encrypted | Server verified | Notes |
|---|---|---|---|
| `verify-full` | yes | certificate **and** hostname | The right answer; needs your provider's CA unless it has a public one |
| `verify-ca` | yes | certificate | Hostname unchecked, so any valid cert from that CA passes |
| `require` | yes | **no** | The default. Anyone on the path can impersonate the database |
| `prefer` | not guaranteed | no | Asks for TLS and falls back to plain text when the server declines it |
| `allow` | no, then yes | no | Starts in plain text |
| `disable` | no | no | Only sensible over loopback |

Why the default is `require`, and what that costs, is in [store.md](store.md#postgres-tls-background).
`serve` logs the `sslmode` and whether the connection was encrypted and verified at every start.

**To verify the server,** point the client at your provider's CA bundle (Supabase, RDS and Cloud
SQL each publish theirs):

```sh
RABBITHOLE_DB='postgres://you@db.example.com:5432/rabbithole?sslmode=verify-full&sslrootcert=/etc/ssl/certs/provider-ca.pem'
```

Some providers also want `search_path` when you keep the tables somewhere other than `public`
(`?search_path=rabbithole`), which the store passes through to the session untouched.

### Connection pool

A hosted database caps connections, and a free tier shares that cap, so these two flags exist to
go **down**, not up.

| Flag | Default | What it is |
|---|---|---|
| `--db-max-conns` | `10` | Connections the queries may hold at once (`serve` adds one for its guard) |
| `--db-conn-max-lifetime` | `30m` | Age at which a pooled connection is retired; lower it if something in front recycles connections |

SQLite needs neither.

### Behind a pooler

PgBouncer and the poolers hosted databases ship with can run in **transaction** mode, which
hands each query a different server connection. Prepared statements do not survive that, and the
driver caches them by default, so add one parameter and it stops using them:

```sh
RABBITHOLE_DB='postgres://you@db.example.com:5432/rabbithole?default_query_exec_mode=simple_protocol'
```

This mode is part of the store test suite, so it is supported.

### Privileges

rabbithole needs more access the first time than afterwards.

- **Once, with a role that can create:** the tables and indexes, on the first start against an
  empty database, or when a new release adds a table. Tables in a schema of your own need
  `CREATE` **and** `USAGE` on that schema too.
- **Thereafter, with a role that can only read and write:** `SELECT`, `INSERT`, `UPDATE` and
  `DELETE` on the tables, plus `USAGE` on the schema holding them.

Forgetting `USAGE` on a schema other than `public` shows up as `relation "items" does not exist`,
not as permission denied; see [store.md](store.md#postgres-privileges-background).

Running one role that can do both is fine, and is what a single-machine install does.

## Interest profile

Your profile is plain Markdown that says what you want to read. It is sent with every scoring
batch and is the biggest influence on how items rank.

> **Tip:** be specific, and say what you do not want as well as what you do. "Kubernetes operators,
> KubeVirt, Go internals, not funding rounds or product launches" ranks far better than "AI and
> infrastructure". If results are off-target, revise the profile before changing the model.

A fresh store has a built-in **Default** profile. You can select it or duplicate it, but not edit
or delete it. Manage your own under **Settings → Profiles**. The friendly editor has **I'm
interested in**, **I'm less interested in** and **Additional context** fields. A profile written
some other way opens in the raw Markdown editor, unchanged.

A profile change applies to the next ingest run, with no restart. A run already going keeps the
profile it started with, and existing scores stay as they are until you ask for a rescore:
**Settings → Profiles → Rescore recent items** re-scores the last seven days from the stored
titles and summaries, without refetching feeds. It keeps your ratings, notes, bookmarks and read
state, and items that fail keep their old score. It costs as much model time (or provider money)
as scoring the same number of new articles.

HTML comments (`<!-- ... -->`) are removed before the profile reaches the model, so you can leave
notes to yourself in it.

### Importing a profile file

A v0.2.x config could name a profile file with `profile:`. rabbithole reads that file only when you
import that config (with `--config` or `config import`), and only when the import saves a new
version. Its text becomes a local profile, selected if none is active yet. After that the file is
not read again, so edit the local copy under Settings → Profiles.

[`docs/examples/prompts/profile.md`](https://github.com/DanielBlei/rabbithole/blob/main/docs/examples/prompts/profile.md)
is the built-in Default as a file, for pasting into a new profile.

## Deprecated keys

An old config file still imports. Each deprecated key logs one warning naming its new home, and is
removed in v0.4.0.

| Key | Now |
|---|---|
| `ingest.since` | Written to the feed defaults `since` when the file is imported. A feeds file's `defaults.since` wins over it. Change it later on the Sources page |
| `ingest.digest_dir` | `rabbithole ingest --markdown --out <dir>`. Still honoured when `--out` is not given |
| `inference.summary.*` | Not used. Ignored |

A feed's lookback (`since`) is the feed's own value, else the feed defaults, else the built-in 7
days.

## System prompt

Where the profile describes what the reader wants, `inference.system_prompt` controls how the
model is instructed to score and respond (the scale it uses, and the JSON shape it must reply
in). Unlike the profile, it is optional: leave it unset and a built-in default is used.

`inference.system_prompt` takes one of three forms:

```yaml
inference:
  # system_prompt: ./prompts/system.md   # your own prompt (a file path works on a CLI import)
  # system_prompt: false                 # send no system message at all
```

- **Unset** (the default): the built-in prompt is used.
- **Your own text**: the prompt is stored with the settings and used verbatim. Choose Custom text
  on the Settings page and write it there. In a config file you may give a path instead: `rabbithole config import` and
  `--config` read the file once (a relative path is taken from the config file's folder), strip HTML
  comments (`<!-- ... -->`) and store its text, so editing the file later changes nothing until you
  import again. A file uploaded in the web UI must hold the text itself, never a path.
  [`docs/examples/prompts/system.md`](https://github.com/DanielBlei/rabbithole/blob/main/docs/examples/prompts/system.md) carries the
  built-in prompt's exact text as a starting point to copy from.
- **`false`**: no system message is sent at all. Useful for a model whose own Modelfile or chat
  template already bakes one in: any system message the app sends, the built-in default or an
  override, takes precedence over a model's own, so `false` is how you defer to it instead.
  (Ollama documents this directly for `/api/generate`'s `system` field as overriding what the
  Modelfile sets; `/api/chat` runs through the same templating.)

The built-in prompt already tells the model that article titles/summaries are data, not
instructions, worth keeping if you write your own override. See
[SECURITY.md](../SECURITY.md#untrusted-content) for why that matters.

## The weather widget

The Feed and Maze pages carry a weather and pollen read-out, on by default. None of it lives in
the config files: every setting is in the browser, under Settings → Widgets.

| Setting | What it does |
|---|---|
| Weather, Pollen | Show or hide each. Weather off means no location prompt and no requests |
| Pages | Show it on the Feed, the Maze, or both (the default) |
| Layout | Sub-bar (the default), inline, or full |
| Side menu | Off (the default), or a read-out at the top or bottom of the side menu |
| Units | °C or °F |
| Hours | 24-hour or AM/PM, matching the clock |
| Location | Type a city, or press "Use my location" |

Data comes from [Open-Meteo](https://open-meteo.com), fetched by the browser and cached for 30
minutes. You are asked for a location once, on the first visit; decline and nothing is
requested, and the city search does the same job. [SECURITY.md](../SECURITY.md) has what gets
sent.


## Upgrading from v0.2.x

Earlier releases read `config.yaml` at every start. Settings now live in the store, and no file
is read unless you name it.

1. **Import your old config once.** Pass it on a start, or import it by itself:

   ```sh
   rabbithole serve --config ./rabbithole/config.yaml
   # or
   rabbithole config import ./rabbithole/config.yaml
   ```

   The settings are saved as version 1. A literal `api_key` moves to `./.env` and the store keeps
   only the reference. Fields your provider does not read are dropped with a warning. A
   `ingest.feeds` or `profile` written in the file is imported once, with that import. If the file
   also has a `store` block, it keeps locating your store.
2. **Decide whether to keep passing `--config`.** If you do, the file is applied again whenever it
   changes (see [The --config file](#the---config-file)). If you do not, drop it after the first import
   and edit with `config export` and `config import`.
3. **Your data.** The store is found in this order: the per-user directory, then `./data/rabbithole.db`,
   then `./rabbithole/data/rabbithole.db`. A legacy store is still used, with a deprecation warning,
   until v0.4.0. To move it to the new default:

   ```sh
   rabbithole store path            # shows the current store and the per-user target
   mkdir -p ~/.local/share/rabbithole && chmod 700 ~/.local/share/rabbithole
   mv ./rabbithole/data/rabbithole.db ~/.local/share/rabbithole/
   ```

   Move the `-wal` and `-shm` files beside it too, if they exist, with `serve` stopped. On macOS
   the target is `~/Library/Application Support/rabbithole/`.
4. **Makefile users.** The `make` targets pin `--db ./data/rabbithole.db`, so your data stays in
   `./data`. Run `make serve DB=...` to use another store.

The `store.url` password reference (`$RABBITHOLE_DB_PASSWORD`) in an old config still works as a
store locator. A Postgres user should move to `RABBITHOLE_DB` and `RABBITHOLE_DB_PASSWORD` in `.env`.

## Known limitations

- **A CLI key rotation does not reach a running `serve`.** The server's environment is fixed when it
  starts. After you change a key in `.env` or your shell, restart `serve`. The next CLI command
  sees the new key at once.
- **`.env` writers are not coordinated across processes.** rabbithole replaces the file atomically,
  but two commands that write a key at the same moment can lose one write. Run imports one at a
  time.
- **An explicit `think: true` differs from leaving `think` out.** Both run with thinking on, but
  the stored form is hashed differently, so a file that spells out the default counts as a new
  version the first time you import it over one that omits it.
- **One `--config` per shared store.** With Postgres shared by several instances, pass `--config`
  on one of them, see [The --config file](#the---config-file).

