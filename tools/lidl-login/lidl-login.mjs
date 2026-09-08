#!/usr/bin/env node
// Lidl Plus login helper — run this ONCE on your own computer to turn your
// e-mail + password into a Lidl Plus **refresh token**, which you then paste
// into the receipts server (receipts_login, provider "lidl").
//
// Why a local browser: Lidl's login page (accounts.lidl.com) is guarded by
// reCAPTCHA Enterprise + Akamai Bot Manager and finishes with an SMS/e-mail
// 2FA code. None of that can be done server-side — it needs a real desktop
// Chromium on a residential connection, plus you to read the SMS. So this
// helper opens a visible browser, pre-fills what it can, and lets YOU clear
// the captcha and type the 2FA code. It then captures the OAuth code from the
// app's callback redirect and exchanges it for tokens automatically.
//
// The refresh token it prints is exactly what the Lidl app itself stores; the
// server renews it on its own from then on (it rotates on every renewal, so
// let the server keep the fresh one). Nothing is sent anywhere except to
// accounts.lidl.com — the helper talks to no other host.
//
// Usage:
//   cd tools/lidl-login
//   npm install            # installs playwright (see package.json)
//   npx playwright install chromium
//   node lidl-login.mjs                 # defaults: country PL, language pl
//   LIDL_COUNTRY=DE LIDL_LANGUAGE=de node lidl-login.mjs
//   node lidl-login.mjs --email you@example.com --password 'secret'
//
// Flags / env (all optional — you can also just type into the browser):
//   --email     / LIDL_EMAIL       e-mail (or phone) to pre-fill
//   --password  / LIDL_PASSWORD    password to pre-fill
//   --country   / LIDL_COUNTRY     two-letter account country (default PL)
//   --language  / LIDL_LANGUAGE    item-name language     (default pl)
//   --headless  / LIDL_HEADLESS=1  run headless (NOT recommended: captcha)

import { createHash, randomBytes } from "node:crypto";
import { parseArgs } from "node:util";
import { chromium } from "playwright";

const AUTH_API = "https://accounts.lidl.com";
const CLIENT_ID = "LidlPlusNativeClient";
const CLIENT_SECRET = "secret"; // public/native client secret, not really secret
const REDIRECT_URI = "com.lidlplus.app://callback";
const SCOPE = "openid profile offline_access lpprofile lpapis";

const { values } = parseArgs({
  options: {
    email: { type: "string" },
    password: { type: "string" },
    country: { type: "string" },
    language: { type: "string" },
    headless: { type: "boolean" },
  },
});

const email = values.email ?? process.env.LIDL_EMAIL ?? "";
const password = values.password ?? process.env.LIDL_PASSWORD ?? "";
const country = (values.country ?? process.env.LIDL_COUNTRY ?? "PL").toUpperCase();
const language = (values.language ?? process.env.LIDL_LANGUAGE ?? "pl").toLowerCase();
const headless = values.headless ?? process.env.LIDL_HEADLESS === "1";

function b64url(buf) {
  return buf.toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
const verifier = b64url(randomBytes(32));
const challenge = b64url(createHash("sha256").update(verifier).digest());

const authorizeURL =
  `${AUTH_API}/connect/authorize?` +
  new URLSearchParams({
    client_id: CLIENT_ID,
    response_type: "code",
    scope: SCOPE,
    redirect_uri: REDIRECT_URI,
    code_challenge: challenge,
    code_challenge_method: "S256",
    Country: country,
    language: `${language}-${country}`,
  }).toString();

function log(...a) {
  console.error("[lidl-login]", ...a);
}

// Best-effort auto-fill of the current (2026) React login form. Every step is
// guarded: if a selector has changed, the helper just leaves that field for
// you to fill in the visible browser. The captcha and 2FA are always yours.
async function tryAutofill(page) {
  if (!email) return;
  try {
    const emailInput = page.locator("#input-email, input[type=email], input[name=EmailOrPhone]");
    await emailInput.first().waitFor({ state: "visible", timeout: 15000 });
    await emailInput.first().fill(email);
    log("filled e-mail");
    await page.locator("button[data-testid=button-primary], #button_btn_submit_email").first().click().catch(() => {});
    if (!password) return;
    const pwInput = page.locator("#Password, #field_Password, input[type=password]");
    await pwInput.first().waitFor({ state: "visible", timeout: 15000 });
    await pwInput.first().fill(password);
    log("filled password — now solve the captcha (if shown) and submit in the browser");
  } catch (e) {
    log("auto-fill stopped early (form differs) — continue manually in the browser:", e.message);
  }
}

// Resolve with the OAuth authorization code as soon as IdentityServer redirects
// to the app's custom-scheme callback. We watch responses for the 302 Location
// (the browser can't follow com.lidlplus.app://) and, as a backup, any request
// to that scheme.
function waitForCode(context, page, timeoutMs) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error(`timed out after ${Math.round(timeoutMs / 1000)}s waiting for login to finish`)),
      timeoutMs,
    );
    const tryURL = (url) => {
      if (!url || !url.startsWith("com.lidlplus.app://")) return false;
      const code = new URL(url).searchParams.get("code");
      if (code) {
        clearTimeout(timer);
        resolve(code);
        return true;
      }
      return false;
    };
    context.on("request", (req) => tryURL(req.url()));
    page.on("response", (resp) => {
      const loc = resp.headers()["location"];
      if (loc) tryURL(loc);
    });
    page.on("framenavigated", (frame) => tryURL(frame.url()));
  });
}

async function exchange(code) {
  const basic = Buffer.from(`${CLIENT_ID}:${CLIENT_SECRET}`).toString("base64");
  const resp = await fetch(`${AUTH_API}/connect/token`, {
    method: "POST",
    headers: {
      Authorization: `Basic ${basic}`,
      "Content-Type": "application/x-www-form-urlencoded",
      Accept: "application/json",
    },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      code,
      redirect_uri: REDIRECT_URI,
      code_verifier: verifier,
    }).toString(),
  });
  const text = await resp.text();
  if (!resp.ok) throw new Error(`token endpoint returned HTTP ${resp.status}: ${text.slice(0, 300)}`);
  return JSON.parse(text);
}

async function main() {
  log(`country=${country} language=${language} headless=${headless}`);
  const browser = await chromium.launch({ headless });
  const context = await browser.newContext({ locale: `${language}-${country}` });
  const page = await context.newPage();

  const codePromise = waitForCode(context, page, 5 * 60 * 1000);

  log("opening the Lidl login page…");
  await page.goto(authorizeURL, { waitUntil: "domcontentloaded" }).catch(() => {});
  await tryAutofill(page);

  log("waiting for you to finish login (captcha + SMS/e-mail code) in the browser window…");
  const code = await codePromise;
  log("got the authorization code, exchanging for tokens…");

  const tokens = await exchange(code);
  await browser.close();

  if (!tokens.refresh_token) throw new Error(`no refresh_token in the response: ${JSON.stringify(tokens)}`);

  console.log("\n=== Lidl Plus refresh token ===\n");
  console.log(tokens.refresh_token);
  console.log("\n(paste this as the refresh_token field of receipts_login, provider \"lidl\";");
  console.log(` country=${country}, language=${language}. The server renews it automatically.)\n`);
}

main().catch((e) => {
  log("FAILED:", e.message);
  process.exit(1);
});
