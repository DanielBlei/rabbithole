# Quickstart

From a clean checkout to a ranked feed. Everything runs locally.

## 1. Set up the config

```sh
git clone https://github.com/DanielBlei/rabbithole.git
cd rabbithole
make setup
```

`make setup` creates three files, skipping any that already exist, so it is safe to re-run:

| File | What it does | What to change |
|---|---|---|
| `configs/config.yaml` | How to run: model, scoring, storage, paths | The model, if you don't want the default `qwen3.5:4b` |
| `configs/feeds.yaml` | The feeds to pull from | Add your own. A name and a URL is the minimum |
| `configs/golden.yaml` | Optional hand-scored evaluation set | Edit only when benchmarking models |

They start as working examples, so you can run first and edit later. A fresh database uses
the immutable built-in **Default** interest profile. In the web UI, open
**Settings → Profiles**, duplicate Default or create a profile, and select it. Profile
changes are persisted in SQLite and affect newly scored items without a server restart.
Existing scores stay unchanged unless you explicitly confirm **Rescore recent items**, which
recomputes already-stored items from the last seven days.

Every field is documented in [configuration.md](configuration.md), including the model-free
`heuristic` scorer and OpenAI-compatible endpoints if you would rather not run Ollama.

State lands in a SQLite file under `data/`, and there is nothing else to set up. To run one
store that more than one machine can reach, set `store.url` to a Postgres connection in place of
`store.db_path`; see [configuration.md](configuration.md#store).

## 2. Pull the model

```sh
ollama pull qwen3.5:4b
```

## 3. Start the server

```sh
CONFIG=./configs/config.yaml make serve
```

`CONFIG` matters for the model, database and feed seed path. Every target defaults to
`configs/config.example.yaml`. Set `CONFIG` at the top of the `Makefile` to stop passing it
each time.

## 4. Claim it

Open <http://localhost:8080>. The first visit asks who can open this instance: create an
account with a username and password, or leave it open with no login at all. The choice is
kept across restarts. Sessions are not, so after restarting the server
you log in again, unless you turn on **Stay signed in** under Settings → Account for that
browser.

Forgot the password? On the machine running it, `rabbithole auth reset` sets a new one; see
[auth.md](auth.md) for that and for reaching the app from another device.

## 5. Run an ingest

> Already ingested on the example config? Run `make clean-feeds` first to remove them.

Hit ingest in the web UI. Nothing is fetched until you do. This is the step
that pulls your feeds and scores them.

The first run has to score every item and takes a while on a local model. After that the
**Feed** page fills in, best first, each item with a line on why it is there. Later runs only
score what is new, because the store remembers what it has seen. The **Maze** page is there
for tasks, todos and ideas.

Configuration-file changes are read at startup, so restart the server after editing
`config.yaml`. Profile selections and edits are different: they live in SQLite and apply to
the next ingest immediately. An ingest already running keeps the snapshot it started with.
Switching alone does not rewrite the existing feed. Use **Settings → Profiles → Rescore
recent items** when you intentionally want the last seven days recomputed; it uses stored
titles/summaries, preserves your ratings and may incur model runtime or provider cost.

## Next

- [configuration.md](configuration.md): every config field, other providers, model tuning
- [cli.md](cli.md): running ingest from the terminal, the `items` command, and `auth` for the login
- [api.md](api.md): the JSON API
- [auth.md](auth.md): the first run, sessions, and HTTPS
- [SECURITY.md](../SECURITY.md): read this before exposing `serve` to anything but loopback

`make help` lists every target.
