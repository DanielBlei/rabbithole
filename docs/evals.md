# Evaluation

## Why

`profile.md` decides what scores well, and you write it blind. Change a line and your feed
changes, but nothing tells you whether it got better or worse. `rabbithole eval benchmark`
closes that loop: you mark a handful of articles yourself, and it shows you where the model
disagreed with you.

## How it works

```mermaid
flowchart LR
    G["golden.yaml<br/>articles you marked"] --> S
    P["profile.md<br/>what you like"] --> S["score them now,<br/>with your model"]
    S --> R["report:<br/>where it disagreed<br/>with you"]
    R -. "edit, run again" .-> P
```

It scores the articles **now**, rather than reading marks recorded earlier. That is the whole
point: two runs either side of an edit are directly comparable, so you can tell whether the
edit did anything. Nothing is written to the store and your profile is never rewritten.

## The set of articles

Copy `configs/golden.example.yaml` to `configs/golden.yaml` (`make setup` does this for you)
and replace the examples with articles from your own feeds. One looks like this:

```yaml
- id: rb001
  source: Source One
  title: "Running Qwen3-30B locally with llama.cpp on a single 3090"
  summary: |
    Walks through the quantisation choices, measures tokens/sec at q4_K_M
    against q8_0, and lists the llama.cpp flags that actually mattered.
  expected_llm_score: 10
  tags: [on-target]
  note: Profile "Running open-source language models" and "Practical write-ups".
```

`expected_llm_score` is the mark **you** would give, 0 to 10. Close is fine: if you said 9
and the model said 8, nothing is wrong. Two things make the set worth having:

- **You should be able to point at the reason.** `profile.md` says what you like, and the
  scoring guide in the system prompt turns that into a number. If neither explains your
  mark, add the missing line instead of changing the mark.
- **Include the awkward ones.** Anything can mark the obvious articles. What teaches you
  something is the sales pitch stuffed with your favourite words, and the genuinely good
  article that happens to use none of them.

`tags` are your own labels for grouping results, and have nothing to do with feed tags on
real items. Give each at least three articles, or its score is really just one article.

**What the model sees.** Only three fields reach it, exactly what a live article would give
it. Your mark never does, because that is the answer being tested.

```mermaid
flowchart LR
    A["source<br/>title<br/>summary"] --> M["model<br/>+ your profile"]
    M --> SC["its mark"]
    SC --> CMP{"compare"}
    B["your mark<br/>+ note"] --> CMP
    CMP --> RPT["report"]
```

A misspelled key, duplicate id, mark outside 0-10 or undeclared tag stops the run rather
than being quietly skipped.

## Running it

```
rabbithole eval benchmark [file] [--model M] [--provider P] [--host URL] [--no-think]
                          [--repeats N] [--limit N] [--show-why]
                          [--format text|markdown|json] [--output-path PATH]
```

| Flag | What it does |
|---|---|
| `--model`, `--provider`, `--host` | Use a different backend than your config, e.g. to try a bigger model than you ingest with |
| `--no-think` | Turn off model reasoning for this run |
| `--repeats` | Score everything N times and report how much the answers wobbled |
| `--limit` | Score only the first N articles, in file order, for a quick pass |
| `--show-why` | Print the model's reasoning and your note beside each article |
| `--format` | `text` (default), `markdown` or `json` |
| `--output-path` | Write to a file instead of the screen |

`text` is the default because reading a run is the common case: it is aligned columns and no
markup, 120 columns wide, and the same bytes whether it lands on a terminal or in a file.
`markdown` is the same report as a document, for pasting into an issue. `--limit` keeps file
order rather than drawing a sample, so a narrowed run stays comparable to the last one.

```sh
# your configured backend, against configs/golden.yaml
rabbithole eval benchmark

# a bigger model than you ingest with
rabbithole eval benchmark --model qwen3.6:35b

# no model at all: a keyword matcher, useful to check the plumbing works
rabbithole eval benchmark --provider heuristic

# save it, to compare against a later run
rabbithole eval benchmark --format json --output-path before.json

# the first ten only, with its reasoning beside your notes
rabbithole eval benchmark --limit 10 --show-why
```

Batch size and parallelism come from your config, not flags: a batched model marks each
article relative to the others beside it, so batching differently from `ingest` would measure
something your feed never does. Progress goes to the error stream and the report to the
normal one, so `--format json > before.json` stays clean.

## Reading the report

**The report explains each number as it prints it**, so none of this needs memorising. Every
table carries a `MEASURES` column saying what the metric is, and a `RANGE` saying which way
is better, so the sections below are background rather than a decoder ring.

**Warnings come first.** When a run is not worth reading, the report says so at the top
rather than leaving you to infer it: a model answering with almost one value, no agreement
beyond chance, or articles it returned nothing for.

**Check the spread of marks next.** Everything else depends on it.

```
SCORE SPREAD  6 distinct values used, golden uses 11
  model   0×7 1×10 2×6 3×4 4×6 5×2
  golden  0×1 1×4 2×3 3×4 4×6 5×3 6×2 7×2 8×3 9×5 10×2
```

A model answering with one or two values out of eleven is not marking articles, it is
emitting a constant, and every other number is then measuring where your marks happen to
coincide with it. The report says so outright when it happens.

**The four headline numbers**, printed under `SCORING`. In plain words:

| Printed as | |
|---|---|
| `QWK` | Whether its marks relate to yours at all, once you discount lucky guesses. 1.0 is perfect, **0.0 means no relationship was found**, below 0 means it disagrees systematically. |
| `MAE` | How far off it is, in points. |
| `RMSE` | Always at least the average. The further above, the more a few bad articles account for the damage. |
| `SIGNED MEAN` | Which way it leans. A big number here means it is consistently too generous or too harsh, which is a different problem from being scattered. |

**Is the high-signal tier trustworthy?** Articles marked 7 or above get the high-signal
badge on the feed page and count toward the tile at the top. Nothing is hidden below it,
so this is about whether the badge is telling the truth.

| | Meaning |
|---|---|
| hit | It badged it, and you agree |
| noise | It badged it, and you don't |
| missed | You wanted it badged, and it didn't |

Precision is how much of what it badges deserves it. Recall is how much of what deserves it
gets badged.

**The sweep** shows the same thing at other marks, which is what turns a bad result into a
diagnosis:

```
SWEEP  the same measure at other marks
  MARK  BADGED  WANTED  PRECISION  CHANCE  RECALL
  3+    12      27      0.75       0.77    0.33
  4+    7       23      0.86       0.66    0.26
  5+    3       17      0.67       0.49    0.12
  6+    2       14      0.50       0.40    0.07
  7+    0       12      —          0.34    0.00
  8+    0       10      —          0.29    0.00
  9+    0       7       —          0.20    0.00
```

Read precision against `CHANCE`, never alone: `CHANCE` is what picking at random would score,
and nearly everything clears a low mark. Pulling ahead of it as the mark rises means the model
orders articles well and only the 7 is misplaced, which is a setting rather than a model
problem. Tracking it the whole way means the ordering carries no signal, and moving the mark
will not help. Above, the keyword matcher sits *below* chance at the lowest mark, pulls a
little ahead in the middle, and badges nothing at all from 7 up: some ordering, nowhere near
enough for the badge to fire. That is the right verdict for a keyword matcher tested on a set
built to catch one.

**By tag** is where a bad number becomes actionable. A poor average says only that marking
is off; one tag standing out says which kind of article it is off about.

```
BY TAG  worst first
  TAG                          N    MAE  SIGNED  DESCRIPTION
  on-target                    7   7.14   -7.14  what you most want to read
  clickbait-framing            3   6.33   -6.33  clickbait headline over a real article
  deep-but-off-topic           3   2.00   -2.00  excellent, wrong domain
```

Groups with fewer than three articles sort to the bottom and are marked with `*`, because
their number is one article rather than a pattern.

**Samples** lists every article, worst first, so the rows worth acting on read before the
ones that already agree. Unanswered articles have no distance to sort by and go last.

**`--show-why`** adds a `WHY` section: the model's reasoning beside your note, in the same
order. That pairing is what makes a failure fixable, because you can usually see which line
of your profile it followed. Read it alongside the spread above, though: a long agreement
built from one repeated mark is a warning, not a result.

**Unanswered articles are not zeros.** If the model returns nothing for one it is counted
separately and left out, since marking it 0 would quietly credit it for agreeing with every
low mark of yours.

**Noise floor** appears with `--repeats`. Nothing sets a temperature or a seed, so two
identical runs will not agree exactly. Measure that wobble before trusting a small win: a
change smaller than it is not evidence of anything.

## When it looks bad

| What you see | What it means | What to do |
|---|---|---|
| Only two or three different marks used | It isn't telling articles apart | Try a bigger model |
| `QWK` near 0.0 | Its marks bear no relation to yours | Bigger model, before touching your profile |
| Large `SIGNED MEAN`, misses all one way | Right order, wrong level | Reword your tiers |
| One tag much worse than the rest | It's wrong about that kind of article | Read that tag's articles with `--show-why` |
| Precision tracks `CHANCE` at every mark | The ordering carries no signal | Moving the 7 will not help |

## Comparing two runs

The header carries a fingerprint of your profile, the system prompt and the article set:

```
INPUTS     profile 07377fd54469 · prompt 351a9edc8435 · benchmark 070506edf6e0
```

Two reports only mean something side by side when two of the three match. Otherwise a
number that moved proves nothing about the change you think you made.

## Later: `eval audit`

`benchmark` asks *did my edit help*, on articles you curated. `audit` will ask *what
actually happened*, reading the marks already recorded for your real feed: which sources
earn their slot, where the model disagreed with your thumbs, how marks are spread.

It is not built yet and is hidden from `--help`. Two things it will have to get right:

- **A source's average mark is the wrong measure.** A busy feed averaging 3 with five
  articles at 9 is worth keeping, and the average says drop it.
- **Only the model name is recorded** against an item, not the provider or the settings. An
  audit spanning rows marked under different configurations can blame the profile for a
  configuration change.
