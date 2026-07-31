# Contributing

The Rabbit Hole exists to make a daily habit less expensive: less time deciding what is worth
reading, more time reading it. If you have an idea that makes it better at that, for you or
for someone else, it is welcome here.

## Talk first, then build

For anything beyond a small fix, open an issue before you write the code. This is to save
your time, not to gate you: it surfaces the design that is already half-decided, or the
reason something was deliberately left out, before you have spent an evening on a patch.

A typo, a broken link, a failing test or an obvious bug fix can come straight as a PR.

There is currently only one maintainer, so feel free to nudge a stale thread.

## Scope

The test for a change is whether it makes someone's daily reading better. Some things are
deliberately outside that:

- **Multi-user.** One person, one profile. No accounts or sharing.
- **A hosted service.** Self-hosting is the deployment model.

It runs on your own machine today, with no authentication and a loopback binding. That is the
current state rather than a fixed position: whether it should be reachable from another
device is genuinely open, and auth would follow that decision rather than lead it.
[SECURITY.md](SECURITY.md) has the details, and is also where to report a vulnerability
rather than in a public issue.

One thing is genuinely undecided: whether this grows into a full self-hosted reader or stays
a focused triage engine. If your idea leans on that answer, say so in the issue. It is a live
question and a good one to argue about.

## Getting set up

```bash
make setup                            # your own config, feeds and profile
export CONFIG=./configs/config.yaml   # targets default to the example config
make heuristic                        # a full ingest, no model needed
```

`make heuristic` scores with the keyword scorer instead of an LLM, so it confirms a working
checkout without Ollama/vLLM running. `make help` lists every target.

Go 1.26+ is required.

## Before a PR

```bash
make check   # golangci-lint + go test -race + build
```

`make check` runs the same gate CI does, so a green run locally is a green run on the PR. It
needs [golangci-lint](https://golangci-lint.run/welcome/install/) — CI pins v2.12.2, so
matching that locally avoids a surprise. The linter also reports formatting, and
`make lint-fix` autofixes most of what it finds, formatting included.

Add tests for new behaviour, and update the page under `docs/` your change makes wrong.

Fork PRs need a maintainer to approve the workflow run before checks start, so expect the
checks to sit idle for a bit on your first contribution.

[docs/architecture.md](docs/architecture.md) has the package map and data flow;
[docs/store.md](docs/store.md) documents the database.

## Commits

`type(scope): imperative summary`, then a line or two on why the change exists.

Sign off your commits with `git commit -s`. That line certifies you wrote the code, or
otherwise have the right to contribute it under the project's license. It is the
[Developer Certificate of Origin](https://developercertificate.org/).

## License

The Rabbit Hole is [Apache-2.0](LICENSE), and contributions come in under the same
terms: anything you deliberately submit for inclusion is licensed Apache-2.0, with no
extra conditions. That is Apache-2.0 section 5, and signing off is how you say it.

You keep the copyright in what you write. There is no CLA and no assignment.

Every Go file opens with two SPDX lines, so keep them on new ones:

```go
// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0
```

They go at the very top, followed by a blank line, so a package doc comment underneath
still reads as the doc comment. "The Rabbit Hole Authors" is everyone who has
contributed, with the git history as the roster. The year is the project's rather than
the file's, so leave it at 2026 instead of bumping it.
