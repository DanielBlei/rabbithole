<p align="center">
  <img src="docs/img/logo.png" alt="The Rabbit Hole" width="480">
</p>

<p align="center">
  <a href="https://github.com/DanielBlei/rabbithole/actions/workflows/ci.yml"><img src="https://github.com/DanielBlei/rabbithole/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI"></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.26+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License: Apache 2.0"></a>
  <img src="https://img.shields.io/badge/backends-Ollama%20%7C%20vLLM%20%7C%20heuristic-informational" alt="Backends: Ollama, vLLM, heuristic">
</p>

**The Rabbit Hole** is a personal RSS reading assistant. It fetches your RSS/Atom feeds
(Medium, arXiv, blogs, etc) and scores each item against an interest profile you write
yourself. What comes back is a reading list ranked by what you actually care about.

Open it in your browser. The **Feed** page is the day's reading ranked. The **Maze** page is
where you put down tasks or todos, throw ideas at the board before they get away, and keep an
eye on the weather.

It runs on your own machine: one binary with the UI inside it, no dependency hell, and state in a local SQLite
file.

<p align="center">
  <img src="docs/img/feed-page.png" alt="The Feed page: items ranked by score, each with a one-line reason" width="900">
</p>

> **Status:** 0.x. Usable day to day, but the config format and the HTTP API can still change
> between releases. Anything breaking is called out in the release notes.

## Requirements

- Go 1.26+
- [Ollama](https://ollama.com) running locally (the default)
- or any OpenAI-compatible endpoint, such as [vLLM](https://docs.vllm.ai)
- or nothing, with the built-in `heuristic` scorer

## Install

```bash
git clone https://github.com/DanielBlei/rabbithole.git
cd rabbithole
make build
```

Or `go install github.com/DanielBlei/rabbithole@latest` for just the binary. Currently it still
needs the config files: copy them out of `configs/` and work from there.

## Quickstart

```bash
ollama pull qwen3.5:4b   # the default model
make serve               # runs on the shipped example config
```

Open <http://localhost:8080> and hit ingest. The first run fetches the feeds and scores them,
which takes a while on a local model; after that the page fills in.

That ranks against an example profile. For a feed ranked against your own interests, see the
full guide: **[docs/quickstart.md](docs/quickstart.md)**.

`make serve` binds to loopback only and has no authentication: it assumes it is running on
your own machine. Read [SECURITY.md](SECURITY.md) before exposing it to anything else.

## Documentation

- [docs/quickstart.md](docs/quickstart.md): the same steps with the details filled in
- [docs/configuration.md](docs/configuration.md): every field in the three config files
- [docs/cli.md](docs/cli.md): the `ingest`, `items` and `serve` commands
- [docs/api.md](docs/api.md): the HTTP API
- [docs/architecture.md](docs/architecture.md): how the pieces fit together, and what is
  planned next
- [docs/store.md](docs/store.md): the SQLite schema and item lifecycle

## Getting help

Questions, or something not behaving? Open an issue. Security reports go privately through
[SECURITY.md](SECURITY.md) instead.

## Contributing

Ideas and patches are welcome. For anything beyond a small fix, open an issue first, so we
can agree the approach before you spend an evening on it. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Acknowledgements

Weather and pollen data by [Open-Meteo](https://open-meteo.com).

The fonts are open source and come bundled with the app, so it looks the same offline. Their
licences sit next to them in [internal/web/static/fonts](internal/web/static/fonts).

## License

Licensed under the [Apache License, Version 2.0](LICENSE). The bundled fonts keep their own
licences, linked above.
