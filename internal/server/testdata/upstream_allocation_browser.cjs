"use strict";

// Optional browser validation: install Playwright outside the application, then
// set PLAYWRIGHT_MODULE to its package path if it is not on Node's module path.
// All requests are intercepted; this script never contacts an upstream account.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const initialState = {user: {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"},
  recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const accounts = [
  {id: "account-alpha", email_masked: "al***@example.test", plan: "Plus", status: "available", can_manage: true,
    allocation_weight: 5, rolling_cost_usd: "12.345678", rolling_cost_share: "0.25", target_share: "0.2",
    request_count: 128, input_tokens: 384000, cached_input_tokens: 102000, output_tokens: 18200, error_count: 2, equivalent_cost_usd: "145.6728"},
  {id: "account-beta", email_masked: "be***@example.test", plan: "Pro", status: "available", can_manage: true,
    allocation_weight: 20, rolling_cost_usd: "37.037034", rolling_cost_share: "0.75", target_share: "0.8",
    request_count: 410, input_tokens: 892000, cached_input_tokens: 452000, output_tokens: 78300, error_count: 0, equivalent_cost_usd: "481.2941"},
  {id: "account-gamma", email_masked: "ga***@example.test", plan: "Plus", status: "available", can_manage: true,
    allocation_weight: 0, rolling_cost_usd: "0", rolling_cost_share: "0", target_share: "0",
    request_count: 12, input_tokens: 47000, output_tokens: 1300, error_count: 0, equivalent_cost_usd: "3.4629"},
].map((account) => ({last_synced_at: "2026-09-20T08:00:00Z", ...account}));

async function main() {
  const browser = await chromium.launch({headless: true});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [], events = [];
    let failList = false, failSave = false;
    page.on("pageerror", (error) => errors.push(error.message));
    const response = () => {
      const totalWeight = accounts.filter((account) => account.status === "available").reduce((sum, account) => sum + account.allocation_weight, 0);
      return {all: true, until: "2026-09-20T08:00:00Z", allocation_from: "2026-09-19T08:00:00Z", allocation_until: "2026-09-20T08:00:00Z",
        accounts: accounts.map((account) => ({...account, target_share: String(account.status === "available" && totalWeight ? account.allocation_weight / totalWeight : 0)}))};
    };
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765", "all traffic must stay on the mocked origin");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/upstream-accounts/concurrency") return send({sampled_at: new Date().toISOString(), accounts: accounts.map((account) => ({id: account.id, active_requests: 0}))});
      if (url.pathname === "/admin/upstream-accounts") {
        return failList ? send({error: {message: "模拟统计服务暂不可用"}}, 503) : send(response());
      }
      if (url.pathname === "/auth/password/reauth") { events.push("verified"); return send({ok: true}); }
      const match = url.pathname.match(/^\/admin\/upstream-accounts\/([^/]+)\/(allocation-weight|status)$/);
      if (match && request.method() === "PUT") {
        const body = request.postDataJSON(), account = accounts.find((item) => item.id === match[1]);
        assert.ok(account);
        writes.push({path: url.pathname, body});
        events.push("write");
        if (failSave) return send({error: {message: "模拟配置保存失败"}}, 503);
        if (match[2] === "status") { account.status = body.enabled ? "available" : "unavailable"; return send({id: account.id, status: account.status}); }
        account.allocation_weight = body.weight;
        return send({id: account.id, allocation_weight: account.allocation_weight});
      }
      throw new Error(`Unexpected mocked request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#upstream-accounts");
    await page.evaluate(async (value) => {
      bindUI(); initializeDateFilters(); renderState(value);
      byId("upstream-account-filter").elements.range.value = "all"; syncUpstreamAccountRange();
      await loadUpstreamAccounts(upstreamAccountQueryFromForm()); setConnection("已连接", "ok");
    }, initialState);
    const card = (id = "account-alpha") => page.locator(`[data-account-id="${id}"]`);
    const input = () => card().locator(".upstream-allocation-input");
    const save = () => card().locator(".upstream-allocation-save").click();
    const settled = () => page.waitForFunction(() => !upstreamAccountOperation && !upstreamAccountListLoading);
    const refresh = async () => { await page.locator("#upstream-account-filter button[type=submit]").click(); await settled(); };
    assert.equal(await input().inputValue(), "5");
    assert.match(await card("account-gamma").locator(".upstream-allocation-state").textContent(), /停止接收新对话/);
    assert.match(await page.locator("#upstream-allocation-period").textContent(), /独立于历史统计筛选/);

    await input().fill("1.5"); await save();
    assert.equal(writes.length, 0);
    assert.equal(await input().getAttribute("aria-invalid"), "true");
    assert.match(await card().locator(".upstream-allocation-form .form-message").textContent(), /整数/);

    await page.evaluate(() => { state.recently_verified = false; });
    await input().fill("20"); await save();
    await page.locator("#reauth-dialog").waitFor({state: "visible"});
    assert.equal(writes.length, 0);
    await page.locator('#reauth-form input[name="password"]').fill("synthetic-test-password");
    await page.locator('#reauth-form button[type="submit"]').click(); await settled();
    assert.deepEqual(events.slice(0, 2), ["verified", "write"]);
    assert.equal(await input().inputValue(), "20");

    await input().fill("0"); await save(); await settled();
    assert.match(await card().locator(".upstream-allocation-state").textContent(), /停止接收新对话.*已有有效绑定/);
    await input().fill("5"); await save(); await settled();
    await page.locator("#upstream-account-filter select[name=range]").selectOption("month");
    await refresh();
    assert.match(await card().locator(".upstream-allocation-stats").textContent(), /12\.345678/);
    assert.match(await page.locator("#upstream-allocation-period").textContent(), /2026\/09\/19/);

    await page.evaluate(() => { state.user.role = "member"; syncUpstreamAccountControls(); });
    assert.equal(await input().isDisabled(), true);
    await page.evaluate(() => { state.user.role = "owner"; syncUpstreamAccountControls(); });

    failSave = true; await input().fill("10"); await save(); await settled();
    assert.match(await page.locator("#upstream-account-action-message").textContent(), /保存未确认.*模拟配置保存失败/);
    failSave = false; await refresh();
    assert.equal(await input().inputValue(), "5");

    failList = true; await input().fill("0"); await save(); await settled();
    assert.match(await page.locator("#upstream-account-action-message").textContent(), /已保存为 0/);
    assert.match(await page.locator("#upstream-account-refresh-message").textContent(), /操作已成功，但列表与统计刷新失败/);
    assert.equal(await input().isDisabled(), true);
    await page.screenshot({path: path.join(screenshots, "upstream-allocation-refresh-failure.png"), fullPage: true});
    failList = false; await refresh(); await input().fill("5"); await save(); await settled();

    await page.evaluate(() => { window.scrollTo(0, 0); });
    await page.screenshot({path: path.join(screenshots, "upstream-allocation-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile layout must not overflow horizontally");
    await page.screenshot({path: path.join(screenshots, "upstream-allocation-mobile.png"), fullPage: true});
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: await browser.version(), writes: writes.length, errors, desktop: "1440x1080", mobile: "390x844"}));
  } finally { await browser.close(); }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
