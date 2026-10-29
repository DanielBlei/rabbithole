# Source library

Curated, verified sources a new install can pick from. The Library page of
Sources fetches these files from `main`, so a merged change reaches every
install within an hour. Changes are additive only, and a file with a newer
`schema_version` than the app understands shows an update notice instead.

## Files

- `rss.yaml`: RSS and Atom feeds.
- `academic.yaml`: saved paper searches (arXiv, Crossref, Semantic Scholar).

The filename is the ingestion type. `backlog.yaml` is reserved for leads with no
ingestor yet, and nothing loads it.

## Format

```yaml
schema_version: 1
type: rss
description: Feeds the RSS ingestor reads, grouped for the welcome checklist.
groups:
  - name: blogs
    description: Project and engineering blogs from the AI and cloud-native stack.
    sources:
      - name: Go blog
        url: https://go.dev/blog/feed.atom
        description: Release notes and design posts from the Go team.
        labels: [Go, Open Source]
```

## Rules

1. The filename is the ingestion type, and `type` must equal it.
2. Top-level key order is `schema_version`, `type`, `description`, `groups`.
3. Groups are an ordered list (file order is display order). `name` is a
   lowercase slug unique across the library, optional `title` is the display
   name on one line (without it the name is shown as words, `llm-systems`
   as `Llm systems`), `description` is one line, `sources` is non-empty.
4. Source keys are `name`, `url`, `description`, `labels`, and optional
   `max_items` (a positive integer) only for high-volume sources. The library
   is its own format, not the feeds file. No `type`, `since`, `enabled` or
   defaults: those are user preferences.
5. The URL is https, ASCII with no whitespace, and it is the identity. Names
   and descriptions are non-empty. Names and URLs are unique across the whole
   library, ignoring case. Moving a source between groups is free.
6. Store the final https URL after redirects. Never edit a published URL: remove
   a dead source, and add a new entry if it moved.
7. Academic URLs use only the shapes in
   [docs/configuration.md](../docs/configuration.md#academic-sources). Crossref
   sources filter by `issn`, not `publisher` (see issue #50). The search is
   always `q`, never the docs aliases (`query`, `search_query`).
8. A source that also appears in `docs/examples/feeds.yaml` keeps the same name
   and URL. `make library-check` checks this.
9. Within a `schema_version`, changes are additive and safe for an older loader
   to ignore. Anything else bumps the version.
10. No em or en dashes anywhere in the library files.

## Labels

Labels are curated, source-level topic hints: they say what a source usually
covers, so sources can be grouped and picked. Importing a source may carry its
labels onto the feed's tags.

Labels are not item tags. Item tags (issue #40 keywords and assumptions) come
from each item's own content and are stored per item. `tags`, `keywords`,
`assumptions` and `aliases` are reserved for them and are not library keys.

Write a label in display form: ASCII words separated by single spaces, starting
with an uppercase letter or a digit (`AI`, `Open Source`). Its slug is the
canonical tag form (`open-source`), and one slug has one spelling across the
library. No repeats within a source. Reuse an existing label before adding one
(`grep -h labels: library/*.yaml`).

## Adding a source

1. Fetch the URL (`curl -sL <url> | head`). It must be a live RSS or Atom feed
   with recent items, or for academic a search that returns recent papers.
2. Pick a group.
3. Copy a block and edit it.
4. Run `make library-check` (offline: format, uniqueness, labels, feeds parity).
5. Run `make library-urls` (network: each feed must answer 200 with no
   redirect (a redirect is reported with the URL to store instead) and parse
   through the app's fetcher, though a feed may be empty on a given day, such
   as arXiv at weekends; each academic search runs through the app's fetcher
   and must return a result).

CI runs both when `library/` changes, and weekly to catch dead links. The
checks are Go tests: `internal/library`, plus a fetch test in `internal/ingest`
behind the `network` build tag.
