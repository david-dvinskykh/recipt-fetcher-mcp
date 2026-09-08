# Lidl Plus login helper

Turns your Lidl Plus **e-mail + password** into a **refresh token** you paste
into the receipts server once. After that the server fetches your receipts on
its own and renews the token automatically — you never run this again unless
the token is revoked (e.g. you change your password).

## Why this is a local tool, not a server button

Unlike Action, Lidl's login page (`accounts.lidl.com`) is guarded by
**reCAPTCHA Enterprise** and **Akamai Bot Manager**, and finishes with an
**SMS/e-mail 2FA code**. Those defeat any server-side, headless login — they
need a real desktop browser on a normal home connection, and they need *you*
to read the SMS. (The community reference client,
[Andre0512/lidl-plus](https://github.com/Andre0512/lidl-plus), drives a full
desktop Chrome for the same reason.) So this helper runs on **your** machine:
it opens a visible Chromium, pre-fills what it can, and hands you the captcha
and the code. It then captures the OAuth code from the app callback and
exchanges it for tokens for you.

It talks to **only** `accounts.lidl.com`. Your password is used to fill the
login form and is never stored or sent anywhere else.

## Run it

Needs Node 18+.

```bash
cd tools/lidl-login
npm install
npx playwright install chromium        # one-time: fetch the browser

# simplest — type e-mail/password into the browser yourself:
node lidl-login.mjs

# or pre-fill them:
node lidl-login.mjs --email you@example.com --password 'your-password'

# other countries (default is PL / pl):
LIDL_COUNTRY=DE LIDL_LANGUAGE=de node lidl-login.mjs
```

A browser window opens on the Lidl login page. Sign in (solve the captcha if it
appears, enter the SMS/e-mail code when asked). When login completes, the
window closes and the refresh token is printed to the terminal:

```
=== Lidl Plus refresh token ===

xxxxxxxxxxxxxxxxxxxxxxxx
```

## Use it

Call `receipts_login` for provider **lidl** with that value as `refresh_token`
(and `country` / `language` if not PL/pl). Done — receipts now work headlessly.

## Options

| flag | env | default | meaning |
|------|-----|---------|---------|
| `--email` | `LIDL_EMAIL` | — | e-mail or phone to pre-fill |
| `--password` | `LIDL_PASSWORD` | — | password to pre-fill |
| `--country` | `LIDL_COUNTRY` | `PL` | two-letter account country |
| `--language` | `LIDL_LANGUAGE` | `pl` | item-name language |
| `--headless` | `LIDL_HEADLESS=1` | off | run without a window (not recommended — you can't solve the captcha) |

## If it doesn't finish

- **The form looks different / auto-fill stopped:** just type into the visible
  browser yourself. The helper only automates the parts it recognises; the
  capture + token exchange still work however you sign in.
- **Timeout after 5 minutes:** the login didn't reach the app callback. Re-run
  and complete the steps in the window; make sure you finish the 2FA prompt.
- **`no refresh_token in the response`:** the account may need to accept updated
  legal terms — do that in the browser once (in the app or on lidlplus.com),
  then re-run.
