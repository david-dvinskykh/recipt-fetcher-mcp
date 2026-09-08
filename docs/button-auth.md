# Button auth: logging in from MetaMCP

This document specifies the mechanism and the protocol that let you authenticate
each store with a **button in MetaMCP**, instead of pasting tokens into a tool
call.

## Why it is not plain OAuth

The obvious design — MetaMCP's native "Sign in" button drives an OAuth redirect
straight to the store and back to a callback we host — does not work for these
three stores:

- Their OAuth clients are the **mobile apps**, with redirect URIs bound to the
  app (`com.lidlplus.app://callback`, `pl.allegro.android://…`, a Gigya app
  scheme). A provider will not redirect an authorization code to a web callback
  we control.
- Lidl adds **SMS 2FA**; Allegro's buyer list is a **web session cookie**, not
  an OAuth scope, and the site runs DataDome bot protection.

So the code→our-callback leg is impossible. What *is* possible is to run the
login the way the app does and capture the resulting token — driven from a small
web page the button opens. That is what this mechanism does.

## The mechanism

The receipts server can run in **HTTP mode** (`receipts-mcp --http :8390`). In
that mode one process serves two things on the same origin:

| Path | What it is |
| --- | --- |
| `/mcp` | the MCP server itself, over Streamable HTTP — what MetaMCP connects to |
| `/login` | a login web app — what the button opens |
| `/status` | JSON login state per store (same data as the `receipts_providers` tool) |
| `/healthz` | liveness |

Both halves share the **same encrypted credential store** (the `secret.Store`
under the state dir). A token captured by `/login` is immediately usable by the
MCP tools, because they read the same store in the same process.

The login web app drives the store login in a **server-side headless browser**
(chromedp). The user never sees the store's pages; they fill a short form on our
page (phone, password, then the SMS code when the store asks for it), and the
server types those into the store's login flow and captures the token. This is
the "browser-driven" path. For stores or situations where automated login is
unreliable (Allegro + DataDome), the same page also accepts the token/cookie
directly, with a per-store hint on where to copy it from.

## The protocol between MetaMCP and the server

MetaMCP needs to do exactly one thing: **open the server's `/login` URL in the
user's browser.** Everything else happens between that page and the server. The
contract is therefore small and transport-agnostic:

1. **Discovery.** For a Streamable HTTP server whose MCP URL is
   `https://host[:port]/mcp`, the login URL is the sibling `…/login`. MetaMCP
   derives it from the configured server URL (strip a trailing `/mcp`, append
   `/login`), or reads it from an optional `authUrl` field on the server config.
2. **The button.** MetaMCP shows a "Connect stores" button on the MCP server's
   page that opens that URL in a new tab. No credentials pass through MetaMCP.
3. **Login.** The page and the server complete the login (see the flow below)
   and persist the token in the server's store.
4. **State.** MetaMCP (or the user) can poll `GET /status` for a per-store
   `logged_in` flag, or just call the `receipts_providers` tool, which reflects
   the same state.

Nothing about steps 3–4 is MetaMCP-specific: any client that can open a URL gets
the same button. MetaMCP is only the place the button lives.

### The interactive login flow (`/login`)

The multi-step login (Lidl's phone → password → SMS) is a small state machine
over HTTP, so a page can drive it without holding a socket open:

```
POST /login/{provider}/start      form fields -> { session, step } | { done }
POST /login/{provider}/step       { session, values } -> { session, step } | { done }
POST /login/{provider}/cancel     { session } -> ok
```

- `start` opens a browser-driven login session and returns the **first form to
  show** (`step`: a title, a note, and a list of fields), or `done` immediately
  when the provider needs only one round (a pasted token).
- `step` submits the user's values for the current form and returns the **next
  form**, or `done` with a message when the login succeeded and the token was
  stored.
- A `session` is a live server-side browser context; it expires after a few
  minutes and is dropped on `cancel` or completion. Sessions never leave the
  server, and the captured token is written straight to the encrypted store —
  it is not returned to the page.

A provider's driver decides how many steps there are. Lidl's is
`phone+password` → (`sms code`) → done; a paste-only provider's is one form →
done.

## Security

- Credentials the user types (phone, password, SMS code) are used to drive the
  login and are **not stored** — only the resulting refresh token / cookie is,
  and only in the encrypted store. The server never logs them.
- `/login` and `/status` are meant to sit behind MetaMCP on a trusted network.
  In HTTP mode the server binds where you tell it; put it behind MetaMCP's auth
  or on a private interface, exactly as you would any other admin surface. It is
  not designed to be exposed to the public internet.
- The headless browser is only launched for a provider that supports the
  browser-driven path and only for the duration of a login session.

## Deploying

```bash
receipts-mcp --http :8390 --state-dir /var/lib/receipts-mcp
# MCP:   http://host:8390/mcp     (add this to MetaMCP as a Streamable HTTP server)
# Login: http://host:8390/login   (the button opens this)
```

The browser-driven path needs a Chromium available to the server; point it at
one with `RECEIPTS_CHROMIUM` (defaults to looking up `chromium`/`chrome` on
PATH). Without a Chromium, the login page still works through the paste path.

stdio mode is unchanged: `receipts-mcp` with no `--http` is exactly the previous
stdio server, and `--http` is purely additive.
