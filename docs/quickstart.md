# Quickstart

From nothing to a ranked feed. Everything runs locally, and you need no config file.

## 1. Install

With a release binary (Linux and macOS):

```sh
curl -fsSL https://raw.githubusercontent.com/DanielBlei/rabbithole/main/scripts/install.sh | sh
```

It verifies the download and installs only the binary, to `~/.local/bin`. See the
[README](../README.md#release-binary) for the options.

From source instead:

```sh
git clone https://github.com/DanielBlei/rabbithole.git
cd rabbithole
make build
```

## 2. Pull the model

The defaults score with a local [Ollama](https://ollama.com) and the `qwen3.5:4b` model:

```sh
ollama pull qwen3.5:4b
```

No Ollama? Other backends (vLLM, OpenAI-compatible endpoints) and the model-free `heuristic`
scorer are covered in [configuration.md](configuration.md#settings).

## 3. Start the server

```sh
rabbithole serve
```

(From a source checkout, `make serve` does the same and keeps its data in `./data`.)

For a container or headless install, import settings explicitly before starting. The import is
idempotent: an unchanged file reports `config unchanged` and saves nothing.

```sh
rabbithole config import config.yaml
rabbithole serve
```

If the file has a `store:` block, set `RABBITHOLE_DB` (and `RABBITHOLE_DB_PASSWORD` for Postgres),
or pass the same `--db` to both commands. The block locates the store for the import only and is
never saved.

The first start finds no store and asks:

```
? Create a new SQLite store at ~/.local/share/rabbithole/rabbithole.db? [Y/n]:
```

Answer `Y`. Your data lives in that per-user folder, and `rabbithole store path` prints it any
time. To use Postgres, answer `n` and set `RABBITHOLE_DB` and `RABBITHOLE_DB_PASSWORD` first; see
[configuration.md](configuration.md#using-postgres).

## 4. Claim it

Open <http://localhost:8080>. The first visit asks who can open this instance: create an
account with a username and password, or leave it open with no login at all. The choice is
kept across restarts. Sessions are not, so after restarting the server
you log in again, unless you turn on **Stay signed in** under Settings → Account for that
browser.

Forgot the password? On the machine running it, `rabbithole auth reset` sets a new one; see
[auth.md](auth.md) for that and for reaching the app from another device.

## 5. Add sources and run an ingest

Add sources from the **Sources** popup: an RSS or Atom URL, or pick **academic** for a paper
search. To start from a list, download the example feeds file and import it, or drop the file on
the Sources page:

```sh
curl -fsSLO https://raw.githubusercontent.com/DanielBlei/rabbithole/main/docs/examples/feeds.yaml
rabbithole sources import feeds.yaml
```

(In a source checkout, `make sources` does this with the file already in the repo.)

Then hit ingest in the web UI. Nothing is fetched until you do. This step pulls your sources and
scores them. Sources apply on the next ingest.

The first run has to score every item and takes a while on a local model. After that the
**Feed** page fills in, best first, each item with a line on why it is there. Later runs only
score what is new, because the store remembers what it has seen. The **Maze** page is there
for tasks, todos and ideas.

A fresh store uses the immutable built-in **Default** interest profile. Open
**Settings → Profiles**, duplicate Default or create a profile, and select it. Profile
changes are stored and apply to the next ingest without a restart. Existing scores stay
unchanged unless you confirm **Settings → Profiles → Rescore recent items**, which recomputes
the last seven days from stored titles and summaries and may incur model runtime or provider
cost.

## 6. Change settings

Click the gear in the top bar to open Settings: model, scoring, profiles, appearance and more.
Changes are staged, and Review and save writes them as one new version. From the command line:

```sh
rabbithole config export -o my.yaml   # the saved settings (add --defaults to see every setting)
$EDITOR my.yaml                       # keep only the lines you want to change
rabbithole config import my.yaml      # saved as a new version
```

A running server uses the new version on its next ingest. `rabbithole config history` lists the
versions and `rabbithole config rollback <version>` goes back (Settings → Advanced does the same).
Every setting and its default is in
[`docs/examples/config.yaml`](https://github.com/DanielBlei/rabbithole/blob/main/docs/examples/config.yaml),
to copy lines from.

An API key never goes in the config. Put `RABBITHOLE_INFERENCE_API_KEY=...` in your shell profile
or in `./.env`, and set `api_key: $RABBITHOLE_INFERENCE_API_KEY`; see
[configuration.md](configuration.md#secrets).

## Next

- [configuration.md](configuration.md): every setting, other providers, model tuning, the store
- [cli.md](cli.md): the commands, including `config`, `sources` and `store`
- [api.md](api.md): the JSON API
- [auth.md](auth.md): the first run, sessions, and HTTPS
- [SECURITY.md](../SECURITY.md): read this before exposing `serve` to anything but loopback

From a checkout, `make help` lists every target.
