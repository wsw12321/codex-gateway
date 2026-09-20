"use strict";

// Optional browser verification. Install Playwright outside the application and
// set PLAYWRIGHT_MODULE if needed. Every request is served by a synthetic fixture.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const initialState = {user: {id: "member", username: "lin", display_name: "林同学", role: "member", status: "active"},
  recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const sources = {day: false, week: false, month: false, cash: false};
const billing = (id = "member") => ({user: id === "member" ? initialState.user : {id, username: "chen", display_name: "陈同学"},
  cash_balance_usd: "123.45", source_disabled: id === "member" ? {...sources} : {day: true, week: false, month: false, cash: true},
  subscriptions: {
    day: {enabled: true, quota_usd: "10", remaining_usd: "8.50", period_count: 3, current_period_number: 1,
      period_started_at: "2026-09-20T00:00:00Z", period_ends_at: "2026-09-21T00:00:00Z", expires_at: "2026-09-23T00:00:00Z"},
    month: {enabled: true, quota_usd: "200", remaining_usd: "165.25", period_count: 0, current_period_number: 2,
      period_started_at: "2026-09-01T00:00:00Z", period_ends_at: "2026-10-02T00:00:00Z"},
  }, ledger_entries: [], pagination: {offset: 0, limit: 50, has_more: false}});

async function main() {
  const browser = await chromium.launch({headless: true});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [];
    let failRead = false, failWrite = false, writeGate = null;
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/auth/password/reauth") return send({ok: true});
      if (url.pathname === "/admin/billing/me" || url.pathname === "/admin/billing/users/other") {
        return failRead ? send({error: {message: "模拟额度加载失败"}}, 503) : send(billing(url.pathname.endsWith("other") ? "other" : "member"));
      }
      const match = url.pathname.match(/^\/admin\/billing\/me\/sources\/(day|week|month|cash)\/status$/);
      if (match && request.method() === "PUT") {
        const body = request.postDataJSON();
        writes.push({source: match[1], body});
        if (writeGate) await writeGate;
        if (failWrite) return send({error: {message: "模拟设置保存失败"}}, 503);
        sources[match[1]] = body.disabled;
        return send({source: match[1], disabled: body.disabled});
      }
      throw new Error(`Unexpected mocked request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#billing");
    await page.evaluate(async (value) => {
      bindUI(); initializeDateFilters(); renderState(value);
      await loadBillingDetail(); setConnection("已连接", "ok");
    }, initialState);
    const control = (source) => page.locator(`[data-billing-source="${source}"]`);
    const status = (source) => page.locator(`[data-billing-source-state="${source}"]`);
    const settled = () => page.waitForFunction(() => !billingSourceOperation && !billingDetailLoading);
    const toggle = async (source) => { await control(source).click(); await settled(); };
    assert.equal(await page.locator("[data-billing-source]:visible").count(), 4);
    assert.equal(await control("week").isEnabled(), true, "unopened subscriptions can be configured");

    await page.evaluate(() => { state.recently_verified = false; });
    await control("day").click();
    await page.locator("#reauth-dialog").waitFor({state: "visible"});
    assert.equal(writes.length, 0);
    await page.locator('#reauth-form input[name="password"]').fill("synthetic-test-password");
    await page.locator('#reauth-form button[type="submit"]').click(); await settled();
    assert.equal(await status("day").textContent(), "已禁用扣费");
    await toggle("day");
    assert.equal(await status("day").textContent(), "允许扣费");

    let release;
    writeGate = new Promise((resolve) => { release = resolve; });
    await control("cash").click();
    await page.waitForFunction(() => Boolean(billingSourceOperation));
    assert.equal(await page.locator("[data-billing-source]:enabled").count(), 0);
    const before = writes.length;
    await page.evaluate(() => Promise.all([changeBillingSource("cash"), changeBillingSource("day")]));
    assert.equal(writes.length, before);
    release(); await settled(); writeGate = null;
    assert.equal(await status("cash").textContent(), "已禁用扣费");
    assert.equal(await page.locator("#billing-cash-balance").textContent(), "US$123.45");
    await toggle("cash");

    failWrite = true; await toggle("week");
    assert.equal(await status("week").textContent(), "允许扣费");
    assert.match(await page.locator("#billing-source-message").textContent(), /保存未确认.*模拟设置保存失败.*已重新加载服务器设置/);
    failWrite = false; failRead = true; await toggle("week");
    assert.equal(await page.locator("[data-billing-source]:enabled").count(), 0);
    assert.match(await page.locator("#billing-source-message").textContent(), /额度刷新失败/);
    failRead = false; await page.evaluate(() => loadBillingDetail());
    assert.equal(await status("week").textContent(), "已禁用扣费");

    await page.evaluate(async (value) => { renderState({...value, user: {...value.user, role: "owner"}}); await loadBillingDetail(); }, initialState);
    writeGate = new Promise((resolve) => { release = resolve; });
    const oldWrite = page.evaluate(() => changeBillingSource("day"));
    await page.waitForFunction(() => Boolean(billingSourceOperation));
    await page.evaluate(() => selectBillingUser({id: "other", username: "chen"}));
    release(); await oldWrite; writeGate = null;
    assert.equal(await page.evaluate(() => billingDetail.user.id), "other");
    assert.equal(await page.locator("[data-billing-source]:visible").count(), 0);
    assert.equal(await status("cash").textContent(), "已禁用扣费");
    assert.match(await page.locator("#billing-source-readonly").textContent(), /仅本人可修改/);
    assert.equal(await page.locator("#billing-source-message").textContent(), "");

    await page.evaluate(async (value) => { renderState(value); await loadBillingDetail(); }, initialState);
    await toggle("cash");
    await page.evaluate(() => { hide("notice"); window.scrollTo(0, 0); });
    fs.mkdirSync(screenshots, {recursive: true});
    await page.screenshot({path: path.join(screenshots, "billing-sources-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile layout must not overflow horizontally");
    await page.screenshot({path: path.join(screenshots, "billing-sources-mobile.png"), fullPage: true});
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: await browser.version(), writes: writes.length, errors, desktop: "1440x1080", mobile: "390x844"}));
  } finally { await browser.close(); }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
