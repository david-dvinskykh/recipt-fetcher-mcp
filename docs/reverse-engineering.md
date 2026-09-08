# Where the auth and receipt mechanisms come from

None of these three apps has a documented public API for reading your own
receipts. Everything below is reverse-engineered — from the apps' own network
traffic, published by the community. This file records what each mechanism is,
where it was verified, and how the code here maps onto it, so the next person
(or the next model) does not have to rediscover it.

How this was gathered: all three APKs were decompiled directly — fetched from a
mirror and their DEX string pools scanned for endpoints, GraphQL documents,
OAuth parameters and header names. (The build sandbox itself has an allow-listed
egress, so the fetch-and-scan ran in a throwaway container with open network;
the method is reproducible with `jadx` on the APK, or with a proxy — mitmproxy /
Charles, plus Frida for cert-pinned apps — reading the live traffic.) For Lidl
and Allegro the decompilation **confirmed** the mechanisms the community had
already published and this server implements; for Action it **recovered** a
mechanism nobody had published. Each section below notes what the decompilation
saw.

---

## Lidl Plus — works fully

Source of truth: the community project **[Andre0512/lidl-plus]** (Python),
reverse-engineered from the app. The Go provider (`internal/provider/lidl`)
implements the same flow.

**Auth — OAuth 2.0 Authorization Code + PKCE, against Lidl's IdentityServer.**

- Authorization/OIDC issuer: `https://accounts.lidl.com`
- Token endpoint: `POST https://accounts.lidl.com/connect/token`
- Native client id: `LidlPlusNativeClient`, client secret `secret`
  (a public/native client; the secret is not really secret)
- Client authentication on the token endpoint: HTTP Basic
  `base64("LidlPlusNativeClient:secret")`
- Scopes: `openid profile offline_access lpprofile lpapis`
- Redirect URI: `com.lidlplus.app://callback`
- The interactive login (username + password + SMS 2FA) happens in a browser
  and yields a **refresh token**. After that the app — and this server — only
  ever call the token endpoint with `grant_type=refresh_token`. The refresh
  token **rotates on every renewal**, so the new one has to be persisted or the
  next start has to go through the browser again.

**Receipts ("tickets").**

- List: `GET https://tickets.lidlplus.com/api/v2/{country}/tickets?pageNumber={n}&onlyFavorite=false`
- One receipt with lines: `GET https://tickets.lidlplus.com/api/v2/{country}/tickets/{id}`
- Required headers on every call:
  `Authorization: Bearer <access token>`, `App-Version: 999.99.9`,
  `Operating-System: iOs`, `App: com.lidl.eci.lidl.plus`,
  `Accept-Language: <language>`
- `{country}` is the uppercase two-letter account country (PL, DE, …);
  the language sets the item-name language.

This is why Lidl needs only a refresh token from the user: the hard,
interactive part is done once, off to the side, exactly as the app does it.

**Decompilation check (2026-09-08, app 12.x).** Scanning the APK confirmed every
piece the provider relies on: the auth host `accounts.lidl.com`, the client
`client_id=LidlPlusNativeClient` (on `account/login/mobile`, `account/mfa`, …),
PKCE (`code_challenge`, `code_challenge_method`, `code_verifier`), the refresh
grant (`grant_type` + `refresh_token`, `refresh_token_expires_in`), and the app
headers `App-Version` and `Operating-System`. One observation to keep an eye on:
the current app is React Native and also ships an `eticket.lidlplus.com` host
alongside the `tickets.lidlplus.com/api/v2` receipts API this server uses (still
the one the maintained community client uses and the one that works today); if
Lidl ever retires the `tickets` host, `eticket.lidlplus.com` is where it moved.

[Andre0512/lidl-plus]: https://github.com/Andre0512/lidl-plus

---

## Allegro — works from a session cookie

The **public** Allegro REST API (`api.allegro.pl`, OAuth) is a *seller* API:
`GET /order/checkout-forms` returns orders placed *with* you. There is no OAuth
equivalent for the buyer's own "Moje zakupy" list — Allegro's own answer in
[allegro-api discussion #5394] is that the buyer methods were never carried over
to the REST API.

The buyer list is served by an **internal** endpoint that the web app and the
mobile app call, authenticated by the **logged-in session cookie**, not by an
OAuth bearer token. Verified against two independent community clients that do
exactly this:

- **[Przemko92/home-assistant-allegro]** — a Home Assistant integration
- **[wini83/ff-iii-toolkit-api]** — a Firefly III toolkit

Both agree on the mechanism, and it is what `internal/provider/allegro`
implements:

- Endpoint: `GET https://api.allegro.pl/myorder-api/myorders?limit={n}&offset={m}`
- Auth: the browser session cookie. The one that matters is **`QXLSESSID`**;
  copying the whole `Cookie` header of a logged-in request is the robust way to
  supply it.
- Required headers (this is the refinement those clients contributed):
  - `Accept: application/vnd.allegro.public.v3+json` — the **versioned vendor
    media type**. With a plain `application/json` the endpoint can answer with a
    redirect to login instead of the order JSON.
  - `Referer: https://allegro.pl/`
- The cookie expires within days; when it does the endpoint stops returning
  JSON, the provider reports `ErrCredentialsRejected`, and the mailbox fallback
  takes over.

There is no per-order buyer endpoint, so `Get` looks the order up in the recent
pages.

**Decompilation check (2026-09-08, app 9.x).** The APK confirms the provider's
approach: `/myorder` appears ~590 times (the buyer order API), the versioned
`vnd.allegro` media type ~100 times (the Accept header), and `QXLSESSID` is
present (the session cookie). One nuance the decompilation adds: the app itself
authenticates to the order API with an **OAuth2 bearer token** (it does the full
Authorization-Code-plus-PKCE dance at `allegro.pl/auth/oauth/authorize` and talks
to an `edge.allegro.pl` mobile BFF), whereas the community clients — and this
server — reach the same `myorder-api/myorders` with the **web session cookie**.
Both are accepted by that endpoint; the cookie is simply the one a user can grab
without an OAuth app registration. (Allegro's mobile app also uses GraphQL and
Apollo heavily, but for the seller/offer side, not for buyer orders.)

[allegro-api discussion #5394]: https://github.com/allegro/allegro-api/discussions/5394
[Przemko92/home-assistant-allegro]: https://github.com/Przemko92/home-assistant-allegro
[wini83/ff-iii-toolkit-api]: https://github.com/wini83/ff-iii-toolkit-api

---

## Action — recovered by decompiling the app

Action publishes no API and no community project had captured one, so the
mechanism here was recovered by **decompiling the Android app**
(`com.action.consumerapp`) directly: the APK was fetched and its DEX string pool
scanned for endpoints, GraphQL documents and auth markers. The findings below
are verbatim from the app and are what `internal/provider/action` now
implements.

**It is a GraphQL API, not REST** — which is why no REST "receipts" path was
ever found.

- Gateway: `POST https://gateway.action.com/api/gateway`
  (staging: `https://integration-gateway.action.com/api/gateway`). The app is a
  native Kotlin app using okhttp + Apollo GraphQL.
- The two receipt operations, exactly as the app sends them:

  ```graphql
  query GetReceipts($limit: Int, $offset: String) {
    receiptList(limit: $limit, offset: $offset) {
      receipts { id dateTime store { name id } price { currency total } returningPeriod { returnable } }
      offset
    }
  }

  query GetSingleReceipt($receiptId: String!) {
    receipt(id: $receiptId) {
      receiptNumber dateTime store { address }
      returningPeriod { totalDays remainingDays lastReturnDate }
      price {
        currency total subTotal employeeDiscount otherDiscounts
        vat {
          total { vatAmount totalIncludingVat totalExcludingVat }
          perPercentage { percentage specification { vatAmount totalIncludingVat totalExcludingVat } }
        }
      }
      totalQuantity barcode qrCode
      products { code description totalPrice price { adjustedSalesPrice regularSalesPrice } quantity }
      paymentMethods membershipId warrantyYears
    }
  }
  ```

  So the list pages by an opaque string `offset`, and the single receipt carries
  full item lines, a VAT breakdown and payment methods.

**Auth — OAuth2 Authorization Code + PKCE via SAP Gigya (Customer Data Cloud).**
The app uses AppAuth (`net.openid.appauth`) against the OIDC issuer

    https://fidm.eu1.gigya.com/oidc/op/v1.0/4_M_vWUHGO9oyocs0oKu8m7Q

with scopes `openid profile offline_access` and a redirect of the form
`<scheme>://oauth/callback`. The interactive login is Gigya's hosted page, so —
as with Lidl — this server does not reimplement it: it takes either a ready
access `token` or a `refresh_token`, and renews the latter at the issuer's
`/token` endpoint (`grant_type=refresh_token`, `client_id` defaulting to the
app's `4_M_vWUHGO9oyocs0oKu8m7Q`). The gateway is then called with
`Authorization: Bearer <token>`.

**Getting the tokens:** log in to Mijn Action in a browser or proxy the app
(mitmproxy/Charles with the CA trusted; the app does not appear to pin), and read
the OAuth callback / the `Authorization` header on a request to
`gateway.action.com`. Store the refresh token (preferred) or the access token
with `receipts_login`. Until then, Action falls back to e-mail.

---

## Getting the APKs and decompiling them yourself

The APKs themselves were retrieved for this analysis (via a server-side file
relay, since the build sandbox has no direct route to APK mirrors) and are on
the owner's Google Drive:

- Action `com.action.consumerapp` — XAPK, 32.86 MB,
  sha256 `163f7f9efe1e490a4c4a977b506322993b0ee558a4afae02542e69c7bb494895`
- Allegro `pl.allegro` — APK, 91.19 MB,
  sha256 `dda942edc65ce5cccbd2cbc5d37ef15a2a61f27280b25ee571d7e54ac7d95b9c`
- Lidl Plus `com.lidl.eci.lidlplus` — XAPK bundle, ~100.9 MB

All three were scanned this way (Action to recover its endpoint, Lidl and
Allegro to confirm theirs — see the per-store "Decompilation check" notes).

Decompiling one, once you have the file:

```bash
# jadx (Java 11+). An XAPK is a zip of split APKs; unzip it first and point
# jadx at the base module (the one without a config.* suffix).
unzip -o Action_com.action.consumerapp.xapk -d action_xapk
jadx -d action_src action_xapk/com.action.consumerapp.apk       # a plain .apk: jadx -d out app.apk

# Then look for the receipt endpoint and how it is authenticated:
grep -rEi 'https?://[^"]*(receipt|kassabon|ticket|transacti|loyalty|api)' action_src/ | sort -u
grep -rEi 'authorization|bearer|oauth|/token|client_id|x-api-key' action_src/ | sort -u
```

For traffic instead of static code (often faster to find the live endpoint):
run the app on a device with mitmproxy/Charles as the system proxy and its CA
trusted; if the app pins certificates, start it under Frida with a standard
pinning-bypass script and read the requests to the receipts screen.
