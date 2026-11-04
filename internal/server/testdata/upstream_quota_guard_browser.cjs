"use strict";

// Optional browser validation of the quota guard: install Playwright outside
// the application, then set PLAYWRIGHT_MODULE to its package path if it is not
// on Node's module path. Every request is mocked; no upstream is contacted.
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
const hour = 60 * 60 * 1000;
const at = (offset) => new Date(Date.now() + offset).toISOString();
const base = {plan: "Plus", status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available",
  can_manage: true, allocation_weight: 1, concurrent_limit: 2, rolling_cost_usd: "8.20", rolling_cost_share: "0.3", target_share: "0.5",
  request_count: 96, input_tokens: 210000, output_tokens: 12000, error_count: 0, equivalent_cost_usd: "40.1", last_synced_at: at(-hour)};
const accounts = [
  {...base, id: "account-alpha", alias: "主力 Plus", email_masked: "al***@example.test", target_share: "0",
    quota_guard: {status: "limited", source: "auto", limited_window: "five_hour", reset_at: at(2 * hour), restore_at: at(2 * hour + 5 * 60 * 1000),
      observed_at: at(-5 * 60 * 1000), five_hour: {used_percent: 96, reset_at: at(2 * hour)}, seven_day: {used_percent: 41, reset_at: at(80 * hour)}}},
  {...base, id: "account-beta", email_masked: "be***@example.test", plan: "Pro",
    quota_guard: {status: "available", source: "auto", observed_at: at(-12 * 60 * 1000), next_check_at: at(48 * 60 * 1000),
      five_hour: {used_percent: 38, reset_at: at(3 * hour)}, seven_day: {used_percent: 22, reset_at: at(100 * hour)}}},
  {...base, id: "account-gamma", email_masked: "ga***@example.test",
    quota_guard: {status: "available", source: "auto"}},
];

async function main() {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1000}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [];
    let percent = 5;
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765", "all traffic must stay on the mocked origin");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/admin/egress-servers") return send({servers: [], version: 1, runtime: {ready: true, applied_version: 1}});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/theme.js") return send(fs.readFileSync(path.join(assets, "theme.js"), "utf8"), 200, "application/javascript");
      if (url.pathname === "/static/favicon.svg") return send(fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), 200, "image/svg+xml");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/upstream-accounts/concurrency") return send({sampled_at: new Date().toISOString(), accounts: accounts.map((account) => ({id: account.id, active_requests: 0}))});
      if (url.pathname === "/admin/upstream-accounts") {
        return send({all: true, until: at(0), allocation_from: at(-24 * hour), allocation_until: at(0), quota_guard_safety_percent: percent, accounts});
      }
      if (url.pathname === "/admin/upstream-quota-guard" && request.method() === "PUT") {
        const body = request.postDataJSON();
        writes.push({path: url.pathname, body});
        percent = body.safety_percent;
        return send({safety_percent: percent, revision: 2, updated_at: at(0)});
      }
      const match = url.pathname.match(/^\/admin\/upstream-accounts\/([^/]+)\/quota-guard$/);
      if (match && request.method() === "PUT") {
        const body = request.postDataJSON(), account = accounts.find((item) => item.id === match[1]);
        assert.ok(account);
        writes.push({path: url.pathname, body});
        account.quota_guard = body.limited ?
          {...account.quota_guard, status: "limited", source: "manual", limited_window: "five_hour", reset_at: account.quota_guard.five_hour?.reset_at || null,
            restore_at: account.quota_guard.five_hour ? new Date(Date.parse(account.quota_guard.five_hour.reset_at) + 5 * 60 * 1000).toISOString() : null} :
          {...account.quota_guard, status: "available", source: "manual", limited_window: undefined, reset_at: null, restore_at: null};
        return send({id: account.id, quota_guard: account.quota_guard});
      }
      throw new Error(`Unexpected mocked request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#upstream-accounts");
    await page.evaluate(async (value) => {
      bindUI(); initializeDateFilters(); renderState(value);
      byId("upstream-account-filter").elements.range.value = "all"; syncUpstreamAccountRange();
      await loadUpstreamAccounts(upstreamAccountQueryFromForm()); setConnection("已连接", "ok");
    }, initialState);
    const card = (id) => page.locator(`[data-account-id="${id}"]`);
    const settled = () => page.waitForFunction(() => !upstreamAccountOperation && !upstreamAccountListLoading);

    assert.equal(await page.locator("#upstream-quota-guard-form input[name=safety_percent]").inputValue(), "5");
    const firstBadge = card("account-alpha").locator(".upstream-summary-status > *").first();
    assert.equal(await firstBadge.textContent(), "达到限额");
    assert.match(await card("account-alpha").locator(".upstream-quota-guard-detail").textContent(), /5 小时额度 .* 刷新 · .* 自动恢复/);
    assert.match(await card("account-beta").locator(".upstream-quota-guard-detail").textContent(), /5 小时额度剩余 62% · 周额度剩余 78%/);
    fs.mkdirSync(screenshots, {recursive: true});
    await page.evaluate(() => { hide("notice"); window.scrollTo(0, 0); });
    await page.screenshot({path: path.join(screenshots, "upstream-quota-guard-desktop.png"), fullPage: true});

    await card("account-beta").locator(".upstream-quota-guard-button").click(); await settled();
    assert.equal(await card("account-beta").locator(".upstream-summary-status > *").first().textContent(), "达到限额");
    assert.match(await page.locator("#upstream-account-action-message").textContent(), /已设为达到限额/);
    await card("account-beta").locator(".upstream-quota-guard-button").click(); await settled();
    assert.equal(await card("account-beta").locator(".upstream-summary-status > *").first().textContent(), "有剩余额度");

    await page.locator("#upstream-quota-guard-form input[name=safety_percent]").fill("60");
    await page.locator("#upstream-quota-guard-form button[type=submit]").click();
    assert.match(await page.locator("#upstream-quota-guard-form .form-message").textContent(), /0 至 50/);
    await page.locator("#upstream-quota-guard-form input[name=safety_percent]").fill("10");
    await page.locator("#upstream-quota-guard-form button[type=submit]").click();
    await page.locator("#upstream-quota-guard-form .form-message").filter({hasText: "已保存为 10%"}).waitFor();
    await settled();
    assert.deepEqual(writes.map((write) => [write.path, write.body]), [
      ["/admin/upstream-accounts/account-beta/quota-guard", {limited: true}],
      ["/admin/upstream-accounts/account-beta/quota-guard", {limited: false}],
      ["/admin/upstream-quota-guard", {safety_percent: 10}],
    ]);

    await page.locator("#upstream-account-status-filter").selectOption("quota_limited");
    assert.equal(await card("account-alpha").isVisible(), true);
    assert.equal(await card("account-gamma").isVisible(), false);
    await page.locator("#upstream-account-status-filter").selectOption("all");

    await page.setViewportSize({width: 390, height: 844});
    await page.evaluate(() => { hide("notice"); window.scrollTo(0, 0); });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile layout must not overflow horizontally");
    await page.screenshot({path: path.join(screenshots, "upstream-quota-guard-mobile.png"), fullPage: true});
    for (const width of [320, 850, 1100]) {
      await page.setViewportSize({width, height: 844});
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `${width}px layout must not overflow`);
    }
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: await browser.version(), writes: writes.length, errors, desktop: "1440x1000", mobile: "390x844"}));
  } finally { await browser.close(); }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
