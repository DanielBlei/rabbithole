# Fonts

Self-hosted so a page load contacts nothing but this server, and so the UI keeps its
typography offline. These files are embedded in the binary by `//go:embed static` in
`internal/web/embed.go`, and the `@font-face` blocks that point at them live at the top of
`../style.css`.

## What is here

| Family | Weights | Used for |
|---|---|---|
| Archivo | 600, 700 | The wordmark |
| IBM Plex Mono | 400, 500, 600 | `--font-mono`, the default face everywhere |
| IBM Plex Sans, Inter, Manrope, Space Grotesk | 400, 500, 600 | Selectable title/text faces |
| Source Serif 4 | 500, 600 | Selectable title/text face |

The selectable faces are the `FACES` list in `../js/fonts.js`.

## Provenance

Downloaded from [google-webfonts-helper](https://gwfh.mranftl.com) on 2026-07-30, `latin` and
`latin-ext` subsets, `woff2` only. Each filename carries the family, the upstream Google Fonts
version and the subsets, so refreshing means re-requesting the same variants at a newer
version and updating the `src` paths.

Coverage is deliberate: `latin` and `latin-ext` cover Western and Central European titles.
A Cyrillic, Greek or Vietnamese feed title falls back to a system font rather than pulling a
further subset over the network. CJK falls back regardless, as none of these faces support it.

## Licensing

All seven are under the SIL Open Font License 1.1, which permits bundling and embedding them
in software. Each family's licence, carrying its own copyright holder, is in `LICENSES/`. They
are separate from the project's Apache-2.0 licence, which covers the code.

- Archivo, Omnibus-Type
- IBM Plex Mono and IBM Plex Sans, IBM Corp
- Inter, Rasmus Andersson
- Manrope, Mikhail Sharanda
- Space Grotesk, Florian Karsten
- Source Serif 4, Adobe

The OFL treats subsetting as a modification, and reserved font names may be kept only where
functional equivalence holds, which a latin subset does not strictly meet. Recording the
source and version above is the proportionate response: Google Fonts' own delivery is
subset-based, and the clause targets confusingly different designs circulating under a known
name rather than webfont delivery.
