# Login

`rabbithole serve` puts the web UI and the JSON API behind one login. There is one user and
no accounts. The login exists so that reaching the port is not the same as reaching your
notes, ideas and feeds.

## First run

A fresh database starts with the default login, `admin` / `admin`, and the login card says so
in a **First run** note above the fields; the note goes away once a password is set. Logging
in with it lands on a setup page that asks for one of two things:

- **Set a password.** Pick a username and a password of at least 8 characters. From then on
  the login asks for those.
- **Continue without a password.** The login switches off and the app is open to anyone who
  can reach it. Settings → Account offers **set a password** to lock it again later.

The choice is stored in the database, so it survives restarts and config edits.

The same setup card also picks the look: **Default** (terminal chrome, dense feed) or
**Minimal** (no shell, hairline chrome), with an example feed drawn in each. Picking one
restyles the page on the spot. It is saved in the browser, like every other display setting,
and Settings → Theme changes it later. Later logins show the plain login card in that look.

## Sessions

Logging in gives the browser a session cookie. Sessions live in the server's memory only, so:

- restarting `serve` logs every browser out, unless that browser chose to stay signed in (below);
- a session ends after 7 days without a request, or on **log out** in Settings → Account;
- a session ends 30 days after its login however much it is used;
- setting a password, running `auth reset` or `auth disable`, or **log out everywhere** ends
  every session at once, even while the server is running.

The cookie is `HttpOnly` and `SameSite=Lax`, and `Secure` whenever the browser reached the
server over HTTPS. Requests another site's page tries to make on your behalf are refused.

## Settings → Account

Who is signed in, and what this browser's login does:

- **Stay signed in** keeps *this browser* signed in through server restarts, for up to 30 days
  from its login. It is off by default, so any other machine still has to log in. Turning it
  on gives the browser a second cookie, signed by the server with a key kept in the database;
  after a restart the server checks the signature and picks the session back up, with no list
  of browsers stored anywhere.
- **Log out everywhere** ends every other browser's session and every stay-signed-in cookie,
  keeping this one. Use it after logging in on a machine that is not yours.
- **Forgot password?** points at `rabbithole auth reset`; changing a password is CLI only.
- **Log out** ends this browser's session and forgets its stay-signed-in cookie. On an open
  instance the same button reads **set a password** instead.

Because the stay-signed-in cookie is checked by its signature rather than looked up, logging
out removes it from your browser but cannot recall a copy taken from it: such a copy works
until it expires, or until **log out everywhere** or a new password voids every one at once.

Five wrong passwords from one address lock it out for 30 seconds, doubling with each further
miss up to 15 minutes; an IPv6 address counts as its whole /64. Each address gets one attempt
at a time, and the server checks at most four passwords at once, answering `503` with
`Retry-After` beyond that, so a flood of guesses can neither outrun the count nor exhaust memory.

## Forgotten password, and switching it off

Only from the machine the database lives on:

```
rabbithole auth status    # is the login on, and for which user
rabbithole auth reset     # set a new password (asked for twice, not echoed)
rabbithole auth disable   # switch the login off
```

Pass the same `--config` you run `serve` with. `reset` keeps the username unless you give
`--username`, and for scripts reads the password from stdin with `--password-stdin`:

```
printf '%s\n' "$NEW_PASSWORD" | rabbithole auth reset --password-stdin
```

A reset goes straight to the new password rather than back to `admin` / `admin`, so there is
never a moment when whoever reaches the port first could claim the instance.

Once a password is set, nothing in the web UI or the config file can switch the login off:
that takes a shell on the machine, the same access it would take to edit the database by
hand. The login page's **forgot password?** says so.

## Reaching it from another machine

Over plain HTTP the password and the session cookie cross the network readable, so by default
`serve` refuses plain HTTP anywhere but loopback. For another machine, serve HTTPS, or put a
TLS-terminating proxy you trust in front and pass `--insecure-http` (see below). Serving HTTPS
itself:

```
rabbithole serve --addr :8443 --tls-cert cert.pem --tls-key key.pem
```

`serve` accepts TLS 1.2 and up, and looks at the two files every 30 seconds: a renewed
certificate is picked up without a restart (which would log everyone out), and one that fails
to load leaves the previous one serving. When a request comes over HTTPS under a real host
name, served here or through a trusted proxy (below), the response also carries
`Strict-Transport-Security`; it does not for `localhost` or an IP address, so a test
certificate there cannot lock plain HTTP out of the same name.

A reverse proxy that handles HTTPS works too. When it runs on the same machine, leave `serve`
on `127.0.0.1` and nothing else is needed. With [Caddy](https://caddyserver.com):

```
rabbithole.example.lan {
    reverse_proxy 127.0.0.1:8080
}
```

or with [Tailscale](https://tailscale.com/kb/1312/serve), `tailscale serve --bg 8080`.

The proxy's `X-Forwarded-For` and `X-Forwarded-Proto` headers are what let the login tell
clients apart for the lockout, mark the cookie `Secure` and send `Strict-Transport-Security`. They are believed only from
`--trusted-proxies`, loopback by default, and ignored from anyone else, since any client can
send them.

When the proxy runs on another host, `serve` has to listen on an address that host can reach,
`--insecure-http` says the plain-HTTP listener is intentional, and the proxy's address goes in
`--trusted-proxies`:

```
rabbithole serve --addr 10.0.0.5:8080 --insecure-http --trusted-proxies 10.0.0.2
```

## What it is not

The login guards the web UI and the API. It does not encrypt the database: anyone who can
read the SQLite file can read everything in it, apart from the password, which is stored as
an argon2id hash. The file's permissions are yours to set.
