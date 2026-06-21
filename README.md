# ai-searcher

A personal RSS reading assistant. It fetches your RSS/Atom feeds (Medium, arXiv, blogs, …),
scores each new item against an editable interest profile using an LLM, and writes a dated
markdown digest of **what to read today** — best first, each with a one-line reason. The
same data is also available as a small JSON API via `ai-searcher serve`.

Pluggable model backend: **Ollama** by default, any **OpenAI-compatible** endpoint (vLLM) as
a drop-in, or a model-free **heuristic** for offline use.

## Requirements

- Go 1.26+
- One of:
  - [Ollama](https://ollama.com) running locally (default provider), or
  - an OpenAI-compatible chat endpoint (e.g. vLLM), or
  - nothing — the `heuristic` provider needs no model, useful for a first run or offline testing

State is a local SQLite file via a pure-Go driver — no database server or cgo toolchain
required.

## Getting started

```bash
cp configs/config.example.yaml configs/config.yaml
cp configs/profile.example.md  configs/profile.md
$EDITOR configs/profile.md      # describe what you care about — this drives ranking

ollama serve
ollama pull qwen3:4b
go run . run
```

This writes a digest to `data/digests/YYYY-MM-DD.md`. For everything else — full config
reference, the `run`/`items`/`serve` commands, the HTTP API, and how the pieces fit
together — see [docs/](docs):

- [docs/configuration.md](docs/configuration.md)
- [docs/cli.md](docs/cli.md)
- [docs/api.md](docs/api.md)
- [docs/architecture.md](docs/architecture.md)

## Contributing

Issues and PRs are welcome. Before submitting a change:

```bash
make check   # gofmt + go vet + go test ./...
```

Keep changes focused and add tests for new behavior — see
[docs/architecture.md](docs/architecture.md) for the package layout.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).