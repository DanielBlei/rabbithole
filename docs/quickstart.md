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
| `configs/config.yaml` | How to run: model, scoring, storage, paths | The model, if you don't want the default `qwen3:4b` |
| `configs/feeds.yaml` | The feeds to pull from | Add your own. A name and a URL is the minimum |
| `configs/profile.md` | Your interests, which is what drives the ranking | Write what you care about, in your own words |

They start as working examples, so you can run first and edit later. `profile.md` is the one
that matters most. The model is handed it verbatim on every scoring call, so it decides what
surfaces and what does not.

Every field is documented in [configuration.md](configuration.md), including the model-free
`heuristic` scorer and OpenAI-compatible endpoints if you would rather not run Ollama.

## 2. Pull the model

```sh
ollama pull qwen3:4b
```

## 3. Start the server

```sh
CONFIG=./configs/config.yaml make serve
```

`CONFIG` matters. Every target defaults to `configs/config.example.yaml`, which ranks against
a generic profile rather than yours. Set `CONFIG` at the top of the `Makefile` to stop passing
it each time.

## 4. Run an ingest

Open <http://localhost:8080> and hit ingest. Nothing is fetched until you do. This is the step
that pulls your feeds and scores them.

The first run has to score every item and takes a while on a local model. After that the
**Feed** page fills in, best first, each item with a line on why it is there. Later runs only
score what is new, because the store remembers what it has seen. The **Maze** page is there
for tasks, todos and ideas.

Config changes are read at startup, so restart the server after editing any of the three files.

## Next

- [configuration.md](configuration.md): every config field, other providers, model tuning
- [cli.md](cli.md): running ingest from the terminal, and the `items` command
- [api.md](api.md): the JSON API
- [SECURITY.md](../SECURITY.md): `serve` is loopback-only and unauthenticated, so read this
  before exposing it to anything else

`make help` lists every target.