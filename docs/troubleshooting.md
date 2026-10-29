# Troubleshooting

Symptoms first, then why they happen and what to do. Run with `DEBUG=1` (for example
`DEBUG=1 make ingest`) to see the log lines quoted here.

## Scores of 0 with reasons that make no sense

**What you see.** Warnings like `dropping unusable verdict, re-asking` with a `rule=` field, a
closing `dropped unusable verdicts during scoring` line, or items that end up `failed` after
retries. Before the guard existed, the same responses were stored as a score of 0 with a reason
such as `:`, `:0}]}]}]...` or `: Skipped; see corrected entry below.`

**Why.** The server enforces the verdict's JSON shape while the model writes it (structured
output). Some thinking models, behind some vLLM-based endpoints, lose their place once the
reasoning ends and the shape takes over: the reply is valid JSON, but the reason is nonsense.
The reasoning itself is usually fine. It is random, so the same request can come back clean on
the next try.

**What rabbithole does.** A verdict with no usable score, a reason without words, a reason
starting with `:`, or a reason containing pieces of the JSON is dropped and the article is asked
again, up to three times. An article that never gets a real verdict counts as failed in the run
history rather than being stored with a made-up score.

**What to do**, roughly in order:

1. **Try another model on the same endpoint**, preferably one that does not reason by default.
   This is usually the cheapest fix.
2. **Turn structured output off**, for that endpoint only. It is on by default and should stay
   on everywhere this problem does not show up:

   ```yaml
   inference:
     structured_output: false
   ```

   Change it in Settings, or save it with `rabbithole config export -o config.yaml`, edit, then `rabbithole config import config.yaml`.

   The model then writes the JSON from the system prompt's instructions, and the checks above
   still apply. Keep a system prompt that spells out the shape: the built-in one does, and so
   does `docs/examples/prompts/system.md`. `reason_max_chars` becomes a request rather than a
   hard limit.
3. **Upgrade vLLM**, if you run it yourself. Several fixes to structured output after reasoning
   are recent.

Lowering the temperature does not help: in testing it made these replies more frequent.

## `think: false` does not reduce thinking

**What you see.** `completion_tokens` in the `scoring response received` debug lines stays in
the hundreds or thousands with `think: false`, and scoring is as slow as with thinking on.

**Why.** The switches are requests the server is free to ignore. `vllm` sends
`chat_template_kwargs: {"enable_thinking": false}`, which only works when the model's chat
template has that switch and the endpoint passes it through. `openai` sends the lowest
`reasoning_effort` the model accepts. A gateway can accept either field and still let the model
think, so startup cannot detect it.

**What to do.** Use a model without a reasoning mode, or keep `think: true` and budget for it
with `model_tuning.tokens_thinking`.

## `model not listed by /v1/models`

**What you see.** A startup warning `model not in /v1/models catalog; accepted via chat probe`,
or a startup error `model "..." not found on openai (not listed by /v1/models at ...); chat
probe: HTTP 4xx: ...`.

**Why.** Gateways often serve models their catalog does not list, so a missing entry is checked
with a small chat request. The warning means that request succeeded and scoring goes ahead.

**What to do.** For the error, read the probe's status: `401` or `403` is the API key, `404` is
usually the model name. Check `inference.model` against the provider's own naming.

## `model rejects reasoning_effort`

**What you see.** A startup warning `openai: model rejects reasoning_effort; sending none`.

**Why.** With `provider: openai`, the configured `inference.effort` is tried once at startup.
The model refused it, either because it has no reasoning levels or because it does not offer
that one (not every model has `xhigh`). The field is left out and scoring uses the model's
default.

**What to do.** Nothing, if the default is fine. Otherwise set an `effort` the model supports.

## `$NAME is not set or empty`

**What you see.** An ingest run fails to start with, for example,
`inference.api_key: $RABBITHOLE_INFERENCE_API_KEY is not set or empty`. After `config import` or
`config rollback`, the same case is a warning: `the config uses $RABBITHOLE_INFERENCE_API_KEY,
which is not set`.

**Why.** The stored config holds a reference, and the variable is missing in the process that
started rabbithole.

**What to do.** Export it in your shell profile, or put it in `./.env`. The binary reads `.env`
itself (`RABBITHOLE_*` keys only, and a variable already in the environment wins). A running
`serve` keeps the environment it started with, so restart it after changing a key.

## `an api_key must be a reference to a RABBITHOLE_*_API_KEY environment variable`

**What you see.** `config import` (or `serve --config`) refuses a file and saves nothing.

**Why.** The store holds only references to variables named `RABBITHOLE_*_API_KEY`. A reference
to any other variable, such as `$OPENAI_API_KEY`, is refused so a config cannot read arbitrary
environment variables.

**What to do.** Export the key under the allowed name and refer to that:
`export RABBITHOLE_INFERENCE_API_KEY=$OPENAI_API_KEY`, then `api_key: $RABBITHOLE_INFERENCE_API_KEY`.
Or write the literal key in the file once: it is moved to `./.env` and only the reference is kept.

## `ignored for provider ...`

**What you see.** A warning on import, such as
`ignored for provider ollama: inference.effort`.

**Why.** The field is not read by the provider you chose, so it is dropped rather than stored.
[Fields by provider](configuration.md#fields-by-provider) lists which provider reads which field.

**What to do.** Nothing, or remove the line, or change the provider.

## `config export` prints `{}`

**What you see.** `rabbithole config export` prints `{}`, or only a comment.

**Why.** Everything is at its default. Export lists only the settings you changed. Look, fonts and
widgets are kept in the browser, not in the saved settings.

**What to do.** Run `rabbithole config export --defaults` to see every setting with its value.

## The store is not where I expect

**What you see.** An empty feed after an upgrade, or `Create a new SQLite store` on a machine that
already has data.

**Why.** The default store moved to your per-user data folder. Older releases kept it in
`./data` or `./rabbithole/data`, which are still used when no per-user store exists, with a
deprecation warning.

**What to do.** Run `rabbithole store path`: it prints the store in use and why. Point at an
existing file with `--db` or `RABBITHOLE_DB`, or move it; see
[Upgrading from v0.2.x](configuration.md#upgrading-from-v02x).

## A 400 that mentions `max_tokens` or `max_completion_tokens`

**What you see.** Startup or scoring fails with a 400 whose message names one of the two
fields.

**Why.** The two providers name the output cap differently. `openai` sends
`max_completion_tokens`, which OpenAI's reasoning models require. `vllm` sends `max_tokens`,
which older OpenAI-compatible servers expect.

**What to do.** Switch `inference.provider` to the other one: `openai` for OpenAI and gateways
in front of it, `vllm` for a vLLM server you run.

## `Host not allowed` (403) behind a reverse proxy

**What you see.** The page or API answers `403 Host not allowed. Use localhost or 127.0.0.1.`
when reached through a proxy or a domain name, though `serve` runs fine on `localhost`.

**Why.** A server listening on a loopback address (the default `127.0.0.1:8080`) answers only
requests whose `Host` header is `localhost` or a loopback IP address. This stops a web page on
another domain from rebinding its DNS to your machine and reading your data. A proxy that
forwards the public name (`Host: rabbit.example.com`) is refused for the same reason.

**What to do.** Either make the proxy send `Host: localhost` to the upstream (nginx:
`proxy_set_header Host localhost;`), or listen on a non-loopback address with `--addr`, which
needs `--tls-cert` and `--tls-key`, or `--insecure-http` when a TLS proxy sits in front (see
[auth.md](auth.md)). Cross-origin POSTs are refused as well, so the page and its
requests must use the same public name.

## A feed on my network or in Docker stopped fetching

**What you see.** A feed on your own network fails on the Sources page with "This address is on a
private network. Set RABBITHOLE_ALLOW_PRIVATE_FETCH=1 to allow private addresses."

**Why.** Feeds on private addresses are refused by default. That covers
`localhost` and `127.0.0.1`, LAN ranges such as `192.168.x.x` and `10.x.x.x`, Docker networks such
as `172.16.x.x` to `172.31.x.x`, and Tailscale addresses (`100.64.x.x`). Typical cases are a
self-hosted blog on your network, or RSS-Bridge, RSSHub or FreshRSS running in Docker next to
Rabbit Hole.

**What to do.**

1. Allow private addresses. Add this line to `./.env`:

   ```
   RABBITHOLE_ALLOW_PRIVATE_FETCH=1
   ```

   In a container, add it to the `environment:` list instead:

   ```yaml
   environment:
     RABBITHOLE_ALLOW_PRIVATE_FETCH: "1"
   ```

2. Restart `rabbithole serve`. A running server keeps the environment it started with.
3. The next run fetches those feeds.

Link-local addresses, including cloud metadata (`169.254.x.x`), stay blocked, and so does a public
feed that redirects into your network. See [Fetching feeds](configuration.md#fetching-feeds).
