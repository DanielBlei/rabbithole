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

   The model then writes the JSON from the system prompt's instructions, and the checks above
   still apply. Keep a system prompt that spells out the shape: the built-in one does, and so
   does `configs/prompts/system.example.md`. `reason_max_chars` becomes a request rather than a
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

**What you see.** The config refuses to load with, for example,
`inference.api_key: $RABBITHOLE_KEY is not set or empty`.

**Why.** The value is an environment reference and the variable is missing in the process that
started rabbithole.

**What to do.** Export it, or put it in `.env`. `.env` is only picked up by the `make` targets;
when running the binary directly, load it first (`set -a; source .env; set +a`) or point a
systemd unit at it with `EnvironmentFile=`.

## A 400 that mentions `max_tokens` or `max_completion_tokens`

**What you see.** Startup or scoring fails with a 400 whose message names one of the two
fields.

**Why.** The two providers name the output cap differently. `openai` sends
`max_completion_tokens`, which OpenAI's reasoning models require. `vllm` sends `max_tokens`,
which older OpenAI-compatible servers expect.

**What to do.** Switch `inference.provider` to the other one: `openai` for OpenAI and gateways
in front of it, `vllm` for a vLLM server you run.
