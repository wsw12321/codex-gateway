"use strict";

// Optional Chromium regression; all HTTP responses and screenshot data are synthetic.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));
const initial = {user: {id: "owner", username: "owner", display_name: "管理员示例", role: "owner", status: "active"},
  recently_verified: true, login_methods: {password: true}, devices: [{id: "device", name: "工作设备", status: "active"}], projects: [],
  api_keys: [{id: "key", name: "开发凭证", key_prefix: "gw_demo", status: "active", last_used_at: "2026-09-29T06:00:00Z"}], passkeys: []};
const ownBilling = (id) => ({user: {id, username: id}, cash_balance_usd: id === "member" ? "48.75" : "123.456789123456", source_disabled: {},
  subscriptions: {
    day: {enabled: true, remaining_usd: "9.21", quota_usd: "20", period_count: 3, current_period_number: 1, expires_at: "2099-10-01T00:00:00Z", period_ends_at: "2099-09-29T00:00:00Z"},
    week: {enabled: false, quota_usd: "100", remaining_usd: "0"},
    month: {enabled: true, remaining_usd: "150.67", quota_usd: "300", period_count: 0, expires_at: null, period_ends_at: "2099-10-15T00:00:00Z"},
  }, ledger_entries: [], pagination: {offset: 0, limit: 50, total: 0}});
const account = {status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available"};
const usage = {summary: {requests: 128, tokens: 180234, error_rate: 2 / 128, charged_usd: "0.000012", cache_rate: .78, cache_write_tokens: 125, p95_ttft_ms: 280, p95_duration_ms: 2840},
  requests: Array.from({length: 7}, (_, index) => ({Model: index === 2 ? "model-with-a-very-long-provider-name-and-version-that-must-wrap-cleanly" : ["gpt-example", "gemini-example"][index % 2],
    RequestedAt: `2026-09-29T06:${String(50 - index).padStart(2, "0")}:00Z`, State: index === 3 ? "failed" : "completed", HTTPStatus: index === 3 ? 503 : 200,
    DeviceID: "device", APIKeyID: "key", KeyPrefix: "gw_demo", InputTokens: 110, OutputTokens: 45}))};

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1080}, locale: "zh-CN"});
    const errors = [], calls = [];
    let actor = "owner", failAGY = false, delayNextBilling = false, releaseBilling;
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const url = new URL(route.request().url());
      assert.equal(url.origin, "http://127.0.0.1:8765");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/app.js") return send(application, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      calls.push(url.pathname);
      if (url.pathname === "/admin/billing/me") {
        if (delayNextBilling) {
          delayNextBilling = false;
          await new Promise((resolve) => { releaseBilling = resolve; });
          return send({error: {code: "invalid_session", message: "旧身份失效"}}, 401);
        }
        return send(ownBilling(actor));
      }
      if (url.pathname === "/admin/usage") return send(usage);
      if (url.pathname === "/admin/usage/global") return send({summary: {usage: {tokens: 9012345, requests: 2010, actual_cost_usd: "128.123456789", unpriced_tokens: 45}, pricing_coverage: "0.99", active_users: 7, total_users: 12}});
      if (url.pathname === "/admin/upstream-accounts") return send({accounts: [account, {...account, status: "unavailable"}]});
      if (url.pathname === "/admin/antigravity-accounts") return failAGY ? send({error: {message: "模拟 Antigravity 加载失败"}}, 503) : send({accounts: [account]});
      if (url.pathname === "/admin/alerts") return send({alerts: [{Severity: "warning"}]});
      throw new Error(`Unexpected request ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#overview");
    await page.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, initial);
    await page.waitForFunction(() => Object.values(overviewSnapshots).filter(Boolean).length === 6);
    assert.equal(await page.locator("#overview-cash").textContent(), "US$123.456789123456");
    assert.equal(await page.locator("#overview-recent-requests .overview-request-row").count(), 5);
    assert.equal(await page.locator("#onboarding-complete").isVisible(), true);
    assert.match(await page.locator("#overview-codex-accounts").textContent(), /1 可用.*1 不可用/);
    assert.match(await page.locator("#overview-antigravity-accounts").textContent(), /1 可用/);
    await page.evaluate(() => { billingUserID = "other"; billingDetail = {user: {id: "other"}, cash_balance_usd: "99999"}; upstreamAccountProvider = "antigravity"; });
    await page.locator("#overview-refresh").click();
    await page.waitForFunction(() => document.getElementById("overview-refresh").disabled === false);
    assert.equal(await page.locator("#overview-cash").textContent(), "US$123.456789123456");
    assert.equal(await page.evaluate(() => billingUserID), "other");
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({width, height: width === 1440 ? 1080 : 844});
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `overview overflow at ${width}`);
      await page.screenshot({path: path.join(screenshots, `overview-owner-${width}.png`), fullPage: true});
    }
    assert.deepEqual(errors, []);
    failAGY = true;
    await page.evaluate(() => loadOverview());
    assert.match(await page.locator("#overview-antigravity-accounts").textContent(), /加载失败/);
    assert.match(await page.locator("#overview-codex-accounts").textContent(), /1 可用/);
    assert.equal(await page.locator("#metric-requests").textContent(), "128");
    failAGY = false;
    delayNextBilling = true;
    await page.evaluate(() => { window.pendingOverview = loadOverview(); });
    await page.waitForFunction(() => document.getElementById("overview-subscriptions").getAttribute("aria-busy") === "true");
    while (!releaseBilling) await new Promise((resolve) => setTimeout(resolve, 10));
    actor = "member";
    const countBeforeMember = calls.length;
    await page.evaluate((value) => { renderState({...value, user: {...value.user, id: "member", username: "member", display_name: "成员示例", role: "member"}}); }, initial);
    await page.waitForFunction(() => overviewSnapshots.billing?.user.id === "member");
    releaseBilling();
    await page.evaluate(() => window.pendingOverview);
    assert.equal(await page.evaluate(() => state.user.id), "member", "late 401 cannot invalidate replacement identity");
    assert.equal(await page.locator("#overview-cash").textContent(), "US$48.75");
    assert.equal(await page.locator(".overview-management").isVisible(), false);
    assert.deepEqual(calls.slice(countBeforeMember).sort(), ["/admin/billing/me", "/admin/usage"]);
    await page.setViewportSize({width: 390, height: 844});
    await page.screenshot({path: path.join(screenshots, "overview-member-390.png"), fullPage: true});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    await page.locator("[data-overview-usage]").first().click();
    await page.waitForURL("**/#usage");
    assert.equal(await page.locator("#usage-filter [name=user_id]").inputValue(), "");
    assert.deepEqual(errors, []);
    console.log("Overview browser regression passed; synthetic screenshots:", screenshots);
  } finally { await browser.close(); }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
