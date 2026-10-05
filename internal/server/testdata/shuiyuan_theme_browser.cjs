"use strict";
// Real browser, full dashboard startup, synthetic identities and intercepted
// same-origin requests. No external service, account or credential is used.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots/shuiyuan");
const origin = "http://127.0.0.1:8765";
const csp = "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'";
const member = {id: "member", username: "lin", display_name: "林同学", role: "member", status: "active"};
const state = {user: member, recently_verified: true, login_methods: {password: true}, passkeys: [], projects: [],
  devices: [{id: "device", name: "开发工作站", status: "active"}],
  api_keys: [{id: "key", device_id: "device", name: "工作凭证", key_prefix: "gw_synthetic", status: "active"}]};
const billing = {user: member, cash_balance_usd: "12.345678", source_disabled: {day: false, week: false, month: false, cash: false},
  subscriptions: {month: {enabled: true, quota_usd: "200", remaining_usd: "165.25", period_count: 0, current_period_number: 2,
    period_started_at: "2099-10-01T00:00:00Z", period_ends_at: "2099-11-01T00:00:00Z", expires_at: null}},
  ledger_entries: [], pagination: {offset: 0, limit: 50, has_more: false}};
const usage = {summary: {requests: 128, tokens: 987650, error_rate: 0.0156, charged_usd: "4.00235000", cache_rate: 0.62},
  requests: [{requested_at: "2026-10-05T08:15:00Z", completed_at: "2026-10-05T08:16:00Z", request_id: "request-1", user_id: "member",
    model: "gpt-6-astra", state: "completed", http_status: 200, device_id: "device", api_key_id: "key", input_tokens: 4200, output_tokens: 760}]};

async function fixture(browser, options = {}) {
  const context = await browser.newContext({viewport: {width: 1440, height: 1080}, locale: "zh-CN", colorScheme: "light", ...options});
  const page = await context.newPage();
  const errors = [], writes = [];
  let signedIn = false;
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (event) => {
    if (event.type() === "error" && !event.text().includes("401 (Unauthorized)")) errors.push(event.text());
  });
  await context.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url());
    assert.equal(url.origin, origin, "all assets and requests remain same-origin");
    const send = (value, status = 200, contentType = "application/json") => route.fulfill({status, contentType,
      headers: {"Content-Security-Policy": csp}, body: typeof value === "string" ? value : JSON.stringify(value)});
    if (["/", "/recover"].includes(url.pathname)) return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
    const files = {"/static/app.js": ["app.js", "application/javascript"], "/static/theme.js": ["theme.js", "application/javascript"],
      "/static/style.css": ["style.css", "text/css"], "/static/favicon.svg": ["favicon.svg", "image/svg+xml"]};
    if (files[url.pathname]) {
      const [name, type] = files[url.pathname];
      return send(fs.readFileSync(path.join(assets, name), "utf8"), 200, type);
    }
    if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
    if (url.pathname === "/auth/oidc/config") return send({enabled: true});
    if (url.pathname === "/admin/state") return signedIn ? send(state) : send({error: {message: "请先登录"}}, 401);
    if (url.pathname === "/auth/password/login") { writes.push(request.postDataJSON()); signedIn = true; return send({ok: true}); }
    if (url.pathname === "/admin/billing/me") return send(billing);
    if (url.pathname === "/admin/billing/plans") return send({plans: []});
    if (url.pathname === "/admin/usage") return send(usage);
    throw new Error(`Unexpected fixture request: ${request.method()} ${url.pathname}`);
  });
  return {context, page, errors, writes};
}

const currentTheme = (page) => page.locator("html").getAttribute("data-theme");
const choice = (page, value) => page.locator(`[data-theme-choice="${value}"]:visible`).first();
const settled = (page) => page.waitForFunction(() => ["overview-subscriptions", "overview-recent-requests"].every((id) => document.getElementById(id).getAttribute("aria-busy") === "false"));
const noOverflow = async (page, label) => assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, label);
const capture = async (page, name) => {
  await page.evaluate(() => {document.activeElement?.blur(); window.scrollTo(0, 0);});
  await page.screenshot({path: path.join(screenshots, name), fullPage: true, animations: "disabled"});
};
const select = async (page, value, expected = value) => {
  const button = choice(page, value);
  assert.equal(await button.evaluate((node) => node.tagName), "BUTTON", "theme choices are keyboard-operable native buttons");
  await button.click();
  assert.equal(await currentTheme(page), expected);
  assert.equal(await button.getAttribute("aria-pressed"), "true");
  for (const other of ["light", "dark", "system"].filter((name) => name !== value)) {
    assert.equal(await choice(page, other).getAttribute("aria-pressed"), "false");
  }
};

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const {context, page, errors, writes} = await fixture(browser);
    await page.goto(origin);
    await page.locator("#oidc-login").waitFor({state: "visible"});
    assert.match(await page.title(), /水源喵/);
    assert.match(await page.locator("#auth").textContent(), /水源喵/);
    assert.equal(await currentTheme(page), "light");
    assert.equal(await choice(page, "system").getAttribute("aria-pressed"), "true");
    assert.equal(await page.evaluate(() => localStorage.length), 0, "system default does not write storage");

    // Follow system updates until the user explicitly chooses a mode.
    await page.emulateMedia({colorScheme: "dark"});
    await page.waitForFunction(() => document.documentElement.dataset.theme === "dark");
    await page.emulateMedia({colorScheme: "light"});
    await page.waitForFunction(() => document.documentElement.dataset.theme === "light");
    await page.locator('#password-login-form input[name="username"]').fill("lin");
    await page.locator('#password-login-form input[name="password"]').fill("synthetic-password");
    await choice(page, "dark").focus();
    await page.keyboard.press("Enter");
    assert.equal(await currentTheme(page), "dark");
    assert.equal(await choice(page, "dark").getAttribute("aria-pressed"), "true");
    assert.equal(await page.locator('#password-login-form input[name="password"]').inputValue(), "synthetic-password");
    assert.deepEqual(writes, [], "theme changes never submit authentication forms");
    await page.emulateMedia({colorScheme: "light"});
    assert.equal(await currentTheme(page), "dark", "explicit preference overrides system");
    await page.reload();
    await page.locator("#oidc-login").waitFor({state: "visible"});
    assert.equal(await currentTheme(page), "dark", "preference survives reload");
    assert.equal(await page.evaluate(() => localStorage.getItem("shuiyuan-theme")), "dark");
    assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).colorScheme), "dark");
    await noOverflow(page, "desktop dark login fits");
    await capture(page, "login-dark-desktop.png");
    const darkSurface = await page.locator("#login-view").evaluate((node) => getComputedStyle(node).backgroundColor);
    await choice(page, "light").focus();
    await page.keyboard.press("Space");
    assert.equal(await currentTheme(page), "light");
    assert.notEqual(await page.locator("#login-view").evaluate((node) => getComputedStyle(node).backgroundColor), darkSurface, "theme changes visible surfaces");
    await capture(page, "login-light-desktop.png");
    for (const theme of ["light", "dark"]) {
      await select(page, theme);
      for (const width of [320, 390]) {
        await page.setViewportSize({width, height: 844});
        await noOverflow(page, `${width} ${theme} login fits`);
        const control = await choice(page, theme).boundingBox();
        assert.ok(control.width >= 40 && control.height >= 40, "mobile mode controls have usable touch targets");
        if (width === 390) await capture(page, `login-${theme}-mobile.png`);
      }
    }

    // Authenticate through the real form handlers and retain the chosen mode.
    await page.locator('#password-login-form input[name="username"]').fill("lin");
    await page.locator('#password-login-form input[name="password"]').fill("synthetic-password");
    await page.locator('#password-login-form button[type="submit"]').click();
    await page.locator("#dashboard").waitFor({state: "visible"});
    await settled(page);
    assert.equal(writes.length, 1);
    assert.equal(await currentTheme(page), "dark");
    assert.match(await page.locator("#sidebar .brand").textContent(), /水源喵/);
    assert.match(await page.locator("#overview-cash").textContent(), /12\.345678/);
    for (const theme of ["dark", "light"]) {
      await select(page, theme);
      for (const width of [1440, 390, 320]) {
        await page.setViewportSize({width, height: width < 600 ? 844 : 1080});
        await noOverflow(page, `${width} ${theme} overview fits`);
        if (width !== 320) await capture(page, `dashboard-${theme}-${width === 1440 ? "desktop" : "mobile"}.png`);
      }
      for (const section of ["resources", "keys", "guide", "billing", "security", "usage"]) {
        await page.evaluate((value) => {location.hash = value;}, section);
        await page.locator(`[data-section="${section}"]`).waitFor({state: "visible"});
        await noOverflow(page, `320 ${theme} ${section} fits`);
      }
      await page.evaluate(() => {location.hash = "overview";});
      await page.locator('[data-section="overview"]').waitFor({state: "visible"});
      await settled(page);
    }
    await select(page, "system", "light");
    await page.emulateMedia({colorScheme: "dark"});
    await page.waitForFunction(() => document.documentElement.dataset.theme === "dark");
    await page.reload();
    await page.locator("#dashboard").waitFor({state: "visible"});
    assert.equal(await choice(page, "system").getAttribute("aria-pressed"), "true");
    assert.equal(await currentTheme(page), "dark");
    await page.goto(`${origin}/recover`);
    await page.locator("#recover-view").waitFor({state: "visible"});
    assert.equal(await currentTheme(page), "dark");
    await noOverflow(page, "320 recovery fits");
    assert.deepEqual(errors, []);
    await context.close();

    // Storage restrictions and invalid old preferences must not break login.
    for (const blocked of [false, true]) {
      const probe = await fixture(browser, {colorScheme: "dark", viewport: {width: 320, height: 844}});
      await probe.context.addInitScript((deny) => {
        if (deny) Object.defineProperty(window, "localStorage", {get() {throw new DOMException("Storage blocked", "SecurityError");}});
        else localStorage.setItem("shuiyuan-theme", "invalid-old-value");
      }, blocked);
      await probe.page.goto(origin);
      await probe.page.locator("#oidc-login").waitFor({state: "visible"});
      assert.equal(await currentTheme(probe.page), "dark");
      await select(probe.page, "light");
      await noOverflow(probe.page, `320 login with ${blocked ? "blocked" : "invalid"} storage fits`);
      await probe.page.locator('#password-login-form input[name="username"]').fill("lin");
      await probe.page.locator('#password-login-form input[name="password"]').fill("synthetic-password");
      await probe.page.locator('#password-login-form button[type="submit"]').click();
      await probe.page.locator("#dashboard").waitFor({state: "visible"});
      assert.equal(await currentTheme(probe.page), "light");
      assert.deepEqual(probe.errors, []);
      await probe.context.close();
    }
    console.log(JSON.stringify({browser: browser.version(), checks: "branding, full login/startup, same-origin CSP, system changes, explicit preference/reload, Enter/Space, storage failures, 320/390/1440 login/dashboard/recovery and all member views", screenshots}));
  } finally {await browser.close();}
}
main().catch((error) => {console.error(error); process.exitCode = 1;});
