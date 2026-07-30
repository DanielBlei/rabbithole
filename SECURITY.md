# Security

## Reporting a vulnerability

Report privately rather than in a public issue: GitHub's **Security → Report a vulnerability**
on this repository opens a private advisory. Say what you did, what happened, and what you
expected; a proof of concept helps. One maintainer, so expect days rather than hours.

In scope: anything that lets a page in your browser, a malicious feed, or model output read or
change data it should not; injection of any kind; path traversal; a dependency vulnerability
reachable from this code.

Out of scope, because they are known and deliberate rather than something anyone missed (see
[Where things stand](#where-things-stand)): no authentication on a loopback binding, missing
CSRF tokens, and anything that needs an attacker to already have a shell on the machine.

## Where things stand

There is no login. Anyone who can reach the port gets the whole app: your items, the ingest
runs, the todos and ideas. That works today because `serve` listens on `127.0.0.1:8080`, so
whoever can reach it is already on your machine.

Authentication and CSRF protection are future work, waiting on a question that is still open:
whether this should be reachable from another device at all.

Two more things worth knowing:

- **Loopback is the only setup tested.** To reach it from another machine, put it behind a
  reverse proxy that handles TLS and the login, or use a VPN or an SSH tunnel. Not the open
  internet.
- **`inference.api_key` sits in the config in plain text**, so its file permissions are yours
  to set.

## Untrusted content

Feed content and model output both end up on a page. Rendered Markdown is sanitised with
[bluemonday](https://github.com/microcosm-cc/bluemonday) (`internal/web/markdown.go`);
everything else goes through `html/template`, which escapes by default.

Feeds are fetched as configured, local network addresses included, so treat `feeds.yaml` as
something only you write.

## What leaves your machine

The server talks to your feeds and your inference host, nothing else. Fonts ship inside the
binary, so no page load reaches a CDN.

The exception is the Maze weather widget, on by default. The browser calls Open-Meteo with
your coordinates for the forecast and pollen, and their geocoding endpoint when you search for
a city. Coordinates come from the browser's location prompt or that search, stay in
`localStorage`, and never reach the server. Decline the prompt and nothing is requested; it is
asked once, not on every visit. Switching the widget off in Settings → Weather stops all of
it. See `internal/web/static/js/weather.js`.

## Supported versions

<!-- Update once 0.1.0 is tagged. -->
No releases yet. Fixes land on `main`.
