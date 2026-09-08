#!/usr/bin/env node
// Allegro login helper — run this ONCE on your own computer to capture your
// logged-in allegro.pl **session cookie**, which you then paste into the
// receipts server (receipts_login, provider "allegro").
//
// Why a local browser: Allegro's buyer order list ("Moje zakupy") is served by
// an internal endpoint (api.allegro.pl/myorder-api) that authenticates with the
// website session cookie, not an OAuth token — the public OAuth API is
// seller-only. The login is guarded by a captcha (allegrocaptcha.com) and
// DataDome bot protection (the Allegro app even bundles the DataDome SDK), so
// the session can only be established in a real browser by a human. This helper
// opens one, waits for you to sign in, then reads the cookie for you.
//
// It talks to **only** allegro.pl. The cookie is printed to your terminal and
// sent nowhere else.
//
// Usage:
//   cd tools/allegro-login
//   npm install
//   npx playwright install chromium
//   node allegro-login.mjs                 # opens a browser; log in; done
//   node allegro-login.mjs --headless      # not recommended (captcha)
//
// A session cookie lasts days, not forever — re-run when receipts start
// reporting the cookie is no longer valid.

import { parseArgs } from "node:util";
import { chromium } from "playwright";

const LOGIN_URL = "https://allegro.pl/logowanie";
const ORDERS_URL = "https://allegro.pl/moje-allegro/zakupy/kupione";
// The buyer order endpoint the receipts server calls — used here only to verify
// the captured cookie actually works before you paste it.
const VERIFY_URL = "https://api.allegro.pl/myorder-api/myorders?limit=1&offset=0";
const VERIFY_ACCEPT = "application/vnd.allegro.public.v3+json";

const { values } = parseArgs({ options: { headless: { type: "boolean" } } });
const headless = values.headless ?? process.env.ALLEGRO_HEADLESS === "1";

function log(...a) {
  console.error("[allegro-login]", ...a);
}

// cookieHeader builds a "name=value; …" header from the cookies the browser
// would send to Allegro's API host.
async function cookieHeader(context) {
  const cookies = await context.cookies(["https://api.allegro.pl/", "https://allegro.pl/"]);
  const seen = new Map();
  for (const c of cookies) seen.set(c.name, c.value); // later (more specific) wins
  return [...seen.entries()].map(([k, v]) => `${k}=${v}`).join("; ");
}

// waitForLogin polls until the authenticated cookies appear. Allegro sets
// `wdctx` (the "who am I" context) once you are actually logged in, so that is
// the signal — QXLSESSID alone is set for anonymous sessions too.
async function waitForLogin(context, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const cookies = await context.cookies(["https://allegro.pl/"]);
    const names = new Set(cookies.map((c) => c.name));
    if (names.has("wdctx") && names.has("QXLSESSID")) return true;
    await new Promise((r) => setTimeout(r, 2000));
  }
  return false;
}

async function verify(cookie) {
  try {
    const resp = await fetch(VERIFY_URL, {
      headers: { Cookie: cookie, Accept: VERIFY_ACCEPT, Referer: "https://allegro.pl/" },
    });
    return { status: resp.status, ok: resp.ok };
  } catch (e) {
    return { status: 0, ok: false, error: String(e).slice(0, 120) };
  }
}

async function main() {
  if (headless) log("headless mode — you won't be able to solve the captcha; use a window if login fails");
  const browser = await chromium.launch({ headless });
  const context = await browser.newContext({ locale: "pl-PL" });
  const page = await context.newPage();

  log("opening allegro.pl login — sign in (solve the captcha if shown)…");
  await page.goto(LOGIN_URL, { waitUntil: "domcontentloaded" }).catch(() => {});

  const ok = await waitForLogin(context, 5 * 60 * 1000);
  if (!ok) {
    await browser.close();
    throw new Error("timed out waiting for login (no wdctx cookie); re-run and finish signing in");
  }
  log("logged in — capturing the session cookie…");

  // Land on the orders page once so any lazy order-scoped cookies are set too.
  await page.goto(ORDERS_URL, { waitUntil: "domcontentloaded" }).catch(() => {});

  const cookie = await cookieHeader(context);
  const check = await verify(cookie);
  await browser.close();

  console.log("\n=== Allegro session cookie ===\n");
  console.log(cookie);
  if (check.ok) {
    log(`verified: the order endpoint accepted the cookie (HTTP ${check.status}).`);
  } else {
    log(`note: the verify call returned HTTP ${check.status}${check.error ? " (" + check.error + ")" : ""}. The cookie is still worth pasting — the server will tell you if it is not accepted.`);
  }
  console.log("\n(paste this as the cookie field of receipts_login, provider \"allegro\".");
  console.log(" It lasts days; re-run this helper when receipts say the cookie expired.)\n");
}

main().catch((e) => {
  log("FAILED:", e.message);
  process.exit(1);
});
