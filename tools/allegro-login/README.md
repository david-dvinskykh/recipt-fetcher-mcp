# Allegro login helper

Captures your logged-in **allegro.pl session cookie** once so the receipts
server can read your purchase history ("Moje zakupy"). You paste the cookie
into the server; when it eventually expires (days), you run this again.

## Why this is a local tool

Allegro's buyer order list is served by an internal endpoint
(`api.allegro.pl/myorder-api`) that authenticates with the **website session
cookie**, not an OAuth token — the public OAuth API only exposes *seller*
orders. Decompiling the Android app (v9.89) confirms it: buyer orders live under
`pl.allegro.android.buyers.myorders` on the mobile gateway `edge.allegro.pl`,
and the app bundles the **DataDome** bot-protection SDK plus an
`allegrocaptcha.com` captcha on login. So a session can only be created by a
human in a real browser — which is what this helper gives you. It talks to
**only** `allegro.pl`, prints the cookie, and sends it nowhere else.

## Run it

Needs Node 18+.

```bash
cd tools/allegro-login
npm install
npx playwright install chromium        # one-time: fetch the browser
node allegro-login.mjs
```

A browser opens on the Allegro login page. Sign in (solve the captcha if it
appears). When you're logged in, the window closes and the cookie prints:

```
=== Allegro session cookie ===

QXLSESSID=…; wdctx=…; datadome=…
```

The helper also does a quick check that the order endpoint accepts the cookie.

## Use it

Call `receipts_login` for provider **allegro** with that value as `cookie`:

```json
{"provider": "allegro", "fields": {"cookie": "QXLSESSID=…; wdctx=…; datadome=…"}}
```

## Notes

- **Timeout after 5 minutes:** login didn't complete (no `wdctx` cookie). Re-run
  and finish signing in within the window.
- **Cookie lifetime:** a few days. When receipts start reporting the cookie is
  no longer valid, run this again for a fresh one.
- `--headless` exists but is not recommended — you can't solve the captcha
  without a visible window.
