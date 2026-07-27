# Security

## Where things stand today

Right now The Rabbit Hole runs on your own machine, for you alone, and everything below
follows from that. It is the current state rather than a permanent position: if reaching it
from another device becomes a goal, authentication and CSRF protection come with it. Until
then it is better to say plainly what is missing than to imply hardening that is not there.

- **There is no authentication and no CSRF protection.** Anyone who can reach the port can
  read every item, trigger an ingest run, and delete todos and ideas.
- **`serve` binds to `127.0.0.1:8080` by default.** Loopback is the only setup that is
  currently tested or supported.
- **The config file may hold an API key** (`inference.api_key`) in plain text. It is read
  from disk with no special handling, so its permissions are yours to set.

Passing `--addr` something routable is a deliberate act and does not become safe just because
the flag allows it. If you need to reach this from another machine, put it behind a reverse
proxy that terminates TLS and does the authentication, or reach it over a VPN or an SSH
tunnel. Do not put it on the open internet.

## Handling untrusted content

Feed content and model output are both untrusted input, and both end up on a page:

- Rendered Markdown is sanitised with [bluemonday](https://github.com/microcosm-cc/bluemonday)
  before it reaches a template (`internal/web/markdown.go`). Everything else goes through
  `html/template`, which escapes by default.
- Feed URLs are fetched as configured. The tool will request whatever you put in
  `feeds.yaml`, including addresses on your local network, so treat that file as something
  only you write.

## Reporting a vulnerability

Please report privately rather than opening a public issue: use GitHub's
**Security → Report a vulnerability** on this repository, which opens a private advisory.

Include what you did, what happened, and what you expected. A proof of concept helps. There
is currently only one maintainer, so expect a first reply in days rather than hours.

In scope:

- Anything that lets a page in your browser, a malicious feed, or model output read or change
  data it should not, given a loopback-only deployment.
- Injection of any kind, path traversal, or a dependency vulnerability that is actually
  reachable from this code.

Not worth a report, because they are the known state described above rather than something
anyone has missed:

- No authentication on a loopback binding.
- Missing CSRF tokens on the web UI's forms.
- Anything that requires an attacker to already have a shell on the machine.

The first two are gaps waiting on a decision about remote access, not positions being
defended. If you have a view on how they should work, an issue is the right place for it.

## Supported versions

There are no releases yet. Fixes land on `main`, which is the only version that receives
them.
