# The Rabbit Hole

A personal RSS reading assistant. It fetches your RSS/Atom feeds (Medium, arXiv, blogs, etc),
scores each new item against an interest profile you write yourself, and ranks what
is worth reading today, the best first, each with a one-line reason.

`rabbithole serve` is the main way to use it. Start it, open the web UI, and run an ingest to
fetch and rank your feeds.

## Requirements

- Go 1.26+
- One of:
  - [Ollama](https://ollama.com) running locally (the default provider), or
  - an OpenAI-compatible chat endpoint such as vLLM, or
  - nothing. The `heuristic` provider needs no model and is handy for a first run or for
    working offline.

State lives in a local SQLite file through a pure-Go driver, so there is no database server
to run and no cgo toolchain to install.

## Getting started

```bash
make setup

# then edit:
#   configs/feeds.yaml   the RSS/Atom feeds to pull from
#   configs/profile.md   what you care about, which is what drives the ranking

# pull default configured model
ollama pull qwen3:4b

# targets default to the example config, so point them at yours
CONFIG=./configs/config.yaml make serve
```

Open <http://localhost:8080> and hit ingest. The first run fetches your feeds and scores
them, which takes a while on a local model; after that the page fills in.

`make setup` will not overwrite a file you already have, so it is safe to re-run. Of the
three files it copies, `configs/profile.md` is the one that matters most. The model is handed
it verbatim on every scoring call, so it decides what surfaces and what does not.

Every target accepts `CONFIG=<path>` and defaults to `configs/config.example.yaml`. Set
`CONFIG` at the top of the `Makefile` if you would rather not pass it each time. Targets that
read it will tell you while you are still on the example.

`make serve` binds to loopback only, and there is no authentication: it assumes it is running
on your own machine. Read [SECURITY.md](SECURITY.md) before exposing it to anything else. Run
`make help` for the full target list.

The web UI also keeps the day's notes and ideas alongside the ranked feed. The same data is
available as a [JSON API](docs/api.md), and `rabbithole ingest --markdown` writes a dated
Markdown file if you would rather read it that way.

The model backend is pluggable: Ollama by default, any OpenAI-compatible endpoint such as
vLLM as a drop-in, or a model-free `heuristic` scorer that needs no model at all.

The rest is in [docs/](docs): the config reference, the `ingest`, `items` and `serve`
commands, the HTTP API, and how the pieces fit together.

- [docs/configuration.md](docs/configuration.md)
- [docs/cli.md](docs/cli.md)
- [docs/api.md](docs/api.md)
- [docs/architecture.md](docs/architecture.md)
- [docs/store.md](docs/store.md)

## Contributing

Ideas and patches are welcome. For anything beyond a small fix, please open an issue first,
so you do not end up building against a decision you had no way of seeing. Small fixes can
come straight as a PR. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
