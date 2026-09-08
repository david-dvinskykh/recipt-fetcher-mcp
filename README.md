# receipts-mcp

An MCP server, written in Go, that logs in to shops and hands your receipts to
an assistant so they can be categorized: **Lidl Plus**, **Action**, **Allegro**.

It speaks MCP over **stdio**, keeps credentials encrypted on disk, and returns
one normalized receipt shape for every shop — store, date, total, items, taxes,
discounts — with exact amounts (both a decimal string and integer minor units).
It does not categorize anything itself; that is the assistant's job.

## What actually works, and what needs setting up

Being blunt about this matters, because two of these three shops publish no API
at all:

| Shop | Primary source | State |
| --- | --- | --- |
| Lidl Plus | the mobile app's own API (`tickets.lidlplus.com`) | works from a refresh token; full item lines, discounts, taxes |
| Allegro | the buyer order endpoint the website calls (`api.allegro.pl/myorder-api/myorders`) | works from a browser session cookie, which expires within days |
| Action | not published — recovered by decompiling the app | GraphQL gateway + Gigya OIDC login; works from a refresh/access token; full item lines |

Where each auth flow and receipt endpoint comes from — and how to capture
Action's yourself — is written up in [docs/reverse-engineering.md](docs/reverse-engineering.md).

Allegro's *public* REST API (`developer.allegro.pl`, OAuth) is a seller API:
`GET /order/checkout-forms` returns the orders placed **with** you, not the ones
you placed. The buyer's "Moje zakupy" list has no OAuth equivalent — see
[allegro/allegro-api discussion #5394](https://github.com/allegro/allegro-api/discussions/5394),
where Allegro's own answer is that the buyer methods were not carried over to
the REST API and that `myorder-api/myorders` only accepts browser cookies.

Action's digital receipts live in the Action app and in the Mijn Action account
([action.com/nl-nl/app](https://www.action.com/nl-nl/app/)); Action has never
described the endpoint the app calls. Rather than ship a guessed URL that would
silently return nothing, the Action provider treats the endpoint as
configuration: give it `api_base` (see below) and it works, leave it out and it
says so and falls back to e-mail.

### The e-mail fallback

Action and Allegro therefore have a second source: their order confirmation
mails, read over IMAP and parsed with rules you can edit. A mail almost always
gives the shop, the date and the amount — enough to categorize spending — but
usually not the item lines. Every receipt says which source it came from
(`"source": "api"` or `"email"`) and whether its lines are complete
(`items_complete`), and mail-derived receipts carry a `notes` entry saying so.

When both sources answer, they are merged: the API receipt wins, and a mail that
describes the same purchase (same day, same total) is dropped.

## Build

Needs Go 1.25 or newer.

```bash
make build          # -> bin/receipts-mcp
make test           # go test ./...
make check          # vet + tests + gofmt check
```

## Connect it to a client

The server talks stdio, so any MCP client starts it as a subprocess.

Claude Code / Claude Desktop:

```json
{
  "mcpServers": {
    "receipts": {
      "command": "/path/to/bin/receipts-mcp",
      "env": {
        "RECEIPTS_STATE_DIR": "/var/lib/receipts-mcp",
        "RECEIPTS_LIDL_COUNTRY": "PL",
        "RECEIPTS_LIDL_LANGUAGE": "pl"
      }
    }
  }
}
```

In MetaMCP, add it as a **STDIO** server with the same command and environment.

Check it without a client at all:

```bash
bin/receipts-mcp -status | jq
```

## Tools

| Tool | What it does |
| --- | --- |
| `receipts_providers` | which shops exist, what each is missing, which credential fields it wants. Start here. |
| `receipts_login` | store credentials for one shop, or for the shared mailbox (`provider: "mail"`). |
| `receipts_logout` | delete one shop's credentials. |
| `receipts_list` | receipt summaries across shops for a date range, newest first. |
| `receipts_get` | one receipt with item lines, taxes, discounts; `include_raw` adds the untouched payload. |
| `receipts_export` | a whole period as `json`, `ndjson` or `csv`; `include_items` fetches every receipt's lines (CSV then has one row per item). Give `path` to write a file instead of returning the data. |
| `receipts_mail_probe` | diagnose the e-mail fallback: how many mails matched the senders, passed the subject filter, and parsed. |

Dates are `YYYY-MM-DD` or RFC3339. A plain `to` date covers the whole day.

Credentials are never returned by any tool: logins report only *which* fields
were stored.

## Logging in

### Lidl Plus

Lidl's login page is guarded by reCAPTCHA Enterprise + Akamai and finishes with
an SMS/e-mail 2FA code, so it needs a real desktop browser and can't be done
server-side. Get a **refresh token** once with the bundled helper — it opens a
browser, you sign in (captcha + code), and it prints the token:

```bash
cd tools/lidl-login
npm install && npx playwright install chromium
node lidl-login.mjs            # add --email / --password to pre-fill; see its README
```

(The community CLI [Andre0512/lidl-plus](https://github.com/Andre0512/lidl-plus)
— the project this provider's endpoints and headers are documented by — is an
alternative: `pip install "lidl-plus[auth]" && lidl-plus auth`.)

Then hand the token over:

```json
{
  "provider": "lidl",
  "fields": {"refresh_token": "2D4F…", "country": "PL", "language": "pl"}
}
```

The token rotates on every renewal and the new one is saved automatically, so
this is a one-time step unless you log out of the app everywhere.

### Allegro

Log in to allegro.pl in a browser, open the developer tools, find any request to
`api.allegro.pl`, and copy its whole `Cookie` header:

```json
{"provider": "allegro", "fields": {"cookie": "QXLSESSID=…; wdctx=…"}}
```

The login is verified immediately and tells you how many orders it can see. The
cookie expires after a few days; when it does, listings say so and come from
e-mail until you paste a fresh one. `endpoint` can override the URL if Allegro
moves it.

### Action

Action logs in with your account's own **e-mail + password** — the server signs
in to `www.action.com` for you and reads receipts from the site's GraphQL API
(see [docs/reverse-engineering.md](docs/reverse-engineering.md)):

```json
{"provider": "action", "fields": {"email": "you@example.com", "password": "…"}}
```

The password is stored encrypted so the session can be renewed on its own. With
it stored, listings and `receipts_get` return the real receipts, item lines
included.

As an alternative, a captured app token still works via the Gigya gateway —
`{"refresh_token": "…"}` (or a ready `token`), with `client_id` / `endpoint`
overridable. Use this for accounts whose Action login is a social provider
(e.g. Google), where there is no website password.

### The mailbox (fallback for Action and Allegro)

```json
{
  "provider": "mail",
  "fields": {
    "host": "imap.gmail.com",
    "port": "993",
    "username": "you@gmail.com",
    "password": "<app password>",
    "mailbox": "INBOX"
  }
}
```

For Gmail this must be an **app password**, not the account password. The
credentials are verified by connecting once.

Then check that the rules find your mail:

```json
{"provider": "allegro", "days": 180}   // receipts_mail_probe
```

The probe tells you where it broke: no mail from those senders, a subject filter
that rejected everything, or subjects that matched but carried no readable
total — each with the rule field to fix.

### Tuning the e-mail rules

The built-in rules are in [`internal/mailbox/rules.json`](internal/mailbox/rules.json).
The sender addresses and phrasings there are a starting point, not gospel: mail
templates differ per country and change over time. Copy that file, edit it, and
point the server at it:

```bash
RECEIPTS_MAIL_RULES=/etc/receipts-mcp/rules.json bin/receipts-mcp
```

Each rule is data — senders, subject filters, and regular expressions for the
total, the order number, the date and (optionally) item lines — so fixing a
shop's mail format is a JSON edit, not a rebuild. Bad patterns are rejected at
startup rather than at the first mail.

The order number matters more than it looks: it becomes the receipt id, which is
what lets a mail-derived receipt deduplicate against the same purchase read from
the shop's API.

## Configuration

| Variable | Meaning |
| --- | --- |
| `RECEIPTS_STATE_DIR` | where the encrypted credentials and the local key live. Default `$XDG_STATE_HOME/receipts-mcp`, else `~/.local/state/receipts-mcp`. |
| `RECEIPTS_SECRET_KEY` | base64 of a 32-byte key for the credential file. Unset: a key file is generated in the state dir with mode 600. |
| `RECEIPTS_LIDL_COUNTRY` | default country for a Lidl login, e.g. `PL`. |
| `RECEIPTS_LIDL_LANGUAGE` | default language for Lidl item names, e.g. `pl`. |
| `RECEIPTS_MAIL_RULES` | path to a rules file that overrides the built-in ones. |
| `RECEIPTS_USER_AGENT` | User-Agent sent with every request. |

Flags: `-state-dir`, `-status`, `-version`.

## Storage and privacy

Credentials are encrypted with AES-256-GCM and written with mode 600, as is the
generated key file. Set `RECEIPTS_SECRET_KEY` if you would rather hold the key
somewhere else (a systemd credential, a secrets manager). Nothing else is
persisted: receipts are fetched on demand and are not cached on disk.

Logs go to stderr — stdout is the MCP transport — and never contain credentials.

## Caveats worth knowing

- The Lidl and Allegro endpoints are not public APIs. They can change without
  notice; when they do, the server says which endpoint moved instead of
  returning empty results.
- Receipt data goes to whatever assistant is connected. That includes shop, time
  and everything you bought.
- Amounts assume two decimal places, which holds for every currency these shops
  bill in (EUR, PLN, CZK, …).
- Reading your own receipts is what these accounts are for; the request rate
  here is a handful of calls per listing, and the server identifies itself
  honestly in its User-Agent rather than pretending to be a browser.

## Layout

```
cmd/receipts-mcp        stdio entry point
internal/mcpserver      MCP tools
internal/provider       Provider interface, registry, API+mail fallback
internal/provider/lidl      Lidl Plus: OAuth refresh, tickets, mapping
internal/provider/allegro   Allegro: cookie session, buyer orders
internal/provider/action    Action: configurable endpoint
internal/mailbox        IMAP search, HTML flattening, rule-based parsing
internal/receipt        normalized model, JSON/NDJSON/CSV export
internal/money          exact amounts in minor units
internal/jsonx          tolerant field probing for undocumented payloads
internal/secret         encrypted credential store
internal/httpx          shared HTTP client with retries
```
