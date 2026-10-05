"use strict";
// Every request is intercepted; screenshots and identities contain synthetic data only.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots/ui-refresh");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
const owner = {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"};
const member = {id: "member", username: "lin", display_name: "林同学", role: "member", status: "active"};
const initial = (user) => ({user, recently_verified: true, login_methods: {password: true}, passkeys: [], projects: [],
  devices: [{id: "device", name: "开发工作站", status: "active"}],
  api_keys: [{id: "key", device_id: "device", name: "工作凭证", key_prefix: "gw_test", status: "active", last_used_at: "2026-09-29T08:00:00Z"}]});
const request = {requested_at: "2026-09-29T08:15:00Z", completed_at: "2026-09-29T08:16:00Z", request_id: "request-1", user_id: "member", username: "lin", display_name: "林同学",
  model: "gpt-6-astra", state: "completed", http_status: 200, device_id: "device", api_key_id: "key", input_tokens: 4200, output_tokens: 760,
  upstream_account_id: "codex-1", upstream_masked_email: "de***@example.test", conversation_hash: "conv-abcdefghijklmnopqrstuv"};
const requests = Array.from({length: 7}, (_, i) => ({...request, request_id: `request-${i}`, model: i === 1 ? "gemini-3.1-pro-high" : request.model, state: i === 2 ? "failed" : "completed", http_status: i === 2 ? 429 : 200}));
const usage = {summary: {requests: 248, tokens: 987650, error_rate: 0.024, charged_usd: "4.00235000", cache_rate: 0.62}, requests};
const account = (provider, i) => ({id: `${provider}-${i}`, display_name: i === 1 ? "开发团队主账号" : "备用账号", email_masked: `de***${i}@example.test`, plan: provider === "codex" ? "Plus" : "Antigravity",
  status: i === 1 ? "available" : "unavailable", cliproxy_status: "active", gateway_manual_status: i === 1 ? "enabled" : "manual_disabled", gateway_quota_status: "available", can_manage: true,
  allocation_weight: 1, concurrent_limit: 4, rolling_cost_usd: i === 1 ? "12.345678" : "4.00001", rolling_cost_share: "0.75", target_share: "0.5", access_mode: "shared", authorized_user_ids: [], request_count: 248, input_tokens: 88000, output_tokens: 7650});
const billing = (user) => ({user, cash_balance_usd: user.id === "owner" ? "123.456789" : "9.876543", source_disabled: {day: false, week: true, month: false, cash: false},
  subscriptions: {day: {enabled: true, quota_usd: "20.000000", remaining_usd: "12.000001", period_count: 3, current_period_number: 1, period_started_at: "2099-09-29T00:00:00Z", period_ends_at: "2099-09-30T00:00:00Z", expires_at: "2099-10-02T00:00:00Z"},
    month: {enabled: true, quota_usd: "200", remaining_usd: "165.25", period_count: 0, current_period_number: 2, period_started_at: "2099-09-01T00:00:00Z", period_ends_at: "2099-10-02T00:00:00Z", expires_at: null}},
  ledger_entries: [{id: "ledger-1", created_at: "2026-09-29T08:15:00Z", type: "usage_charge", amount_usd: "-0.012345678", reason: "合成请求费用", actor_username: "system"}], pagination: {offset: 0, limit: 50, has_more: false}});

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1080}, locale: "zh-CN", colorScheme: process.env.BROWSER_COLOR_SCHEME || "light"});
    const errors = [], reads = [];
    let identity = owner, failAGY = false;
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => { if (message.type() === "error" && !message.text().includes("503")) errors.push(message.text()); });
    await page.route("**/*", async (route) => {
      const req = route.request(), url = new URL(req.url());
      assert.equal(url.origin, origin);
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/theme.js") return send(fs.readFileSync(path.join(assets, "theme.js"), "utf8"), 200, "application/javascript");
      if (url.pathname === "/static/favicon.svg") return send(fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), 200, "image/svg+xml");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      reads.push(url.pathname);
      if (url.pathname === "/admin/billing/me") return send(billing(identity));
      if (url.pathname === "/admin/billing/users") return send({users: [owner, member]});
      if (url.pathname === "/admin/usage") return send(usage);
      if (url.pathname === "/admin/usage/global") return send({summary: {usage: {tokens: 87654321, requests: 21456, actual_cost_usd: "246.12345", unpriced_tokens: 1200}, pricing_coverage: 0.987, active_users: 12, total_users: 16}});
      if (url.pathname === "/admin/alerts") return send({alerts: [{severity: "warning"}]});
      if (url.pathname === "/admin/monitoring") return send({sampled_at: new Date().toISOString(), recent: requests, failures: [requests[2]]});
      if (/^\/admin\/(upstream|antigravity)-accounts(?:\/concurrency)?$/.test(url.pathname)) {
        const provider = url.pathname.includes("antigravity") ? "antigravity" : "codex";
        if (provider === "antigravity" && failAGY) return send({error: {message: "模拟 Antigravity 暂不可用"}}, 503);
        if (url.pathname.endsWith("concurrency")) return send({sampled_at: new Date().toISOString(), accounts: [1, 2].map((i) => ({id: `${provider}-${i}`, active_requests: i}))});
        return send({accounts: [account(provider, 1), account(provider, 2)], until: new Date().toISOString()});
      }
      throw new Error(`Unexpected fixture request: ${req.method()} ${url.pathname}`);
    });
    const initialize = async () => page.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, initial(identity));
    const overviewSettled = () => page.waitForFunction(() => [...document.querySelectorAll('[data-section="overview"] [aria-busy]')].every((n) => n.getAttribute("aria-busy") !== "true"));
    const accountSettled = () => page.waitForFunction(() => !upstreamAccountListLoading && upstreamAccounts.length > 0);
    const capture = async (options) => { await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); }); await page.screenshot(options); };
    const noOverflow = async (label) => assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, label);
    const navigate = async (hash) => { await page.evaluate((value) => { location.hash = value; }, hash); await page.waitForFunction((value) => document.querySelector(`[data-section="${value}"]`)?.classList.contains("hidden") === false, hash); };
    await page.goto(`${origin}/#overview`); await initialize(); await overviewSettled();
    assert.equal(await page.locator(".overview-request-row").count(), 5);
    assert.match(await page.locator("#overview-cash").textContent(), /123\.456789/);
    assert.match(await page.locator("#overview-codex-accounts").textContent(), /1 可用/);
    assert.equal(await page.locator("#onboarding").isVisible(), false);
    await noOverflow("desktop overview");
    await capture({path: path.join(screenshots, "overview-owner-desktop.png"), fullPage: true});
    await page.evaluate((value) => { billingUserID = value.user.id; renderBillingDetail(value); upstreamAccountProvider = "antigravity"; }, billing(member));
    assert.match(await page.locator("#overview-cash").textContent(), /123\.456789/);
    failAGY = true; await page.locator("#overview-refresh").click(); await overviewSettled();
    assert.match(await page.locator("#overview-antigravity-accounts").textContent(), /加载失败/);
    assert.match(await page.locator("#overview-codex-accounts").textContent(), /1 可用/);
    failAGY = false;
    await page.setViewportSize({width: 390, height: 844});
    await page.locator("#navigation-toggle").click();
    assert.equal(await page.evaluate(() => document.querySelector(".workspace").inert), true);
    assert.equal(await page.evaluate(() => document.activeElement.id), "navigation-close");
    assert.ok((await page.locator("#logout").boundingBox()).height >= 44);
    await page.keyboard.press("Shift+Tab");
    assert.equal(await page.evaluate(() => document.activeElement.id), "logout");
    await page.keyboard.press("Tab");
    assert.equal(await page.evaluate(() => document.activeElement.id), "navigation-close");
    await noOverflow("drawer");
    await capture({path: path.join(screenshots, "navigation-mobile.png")});
    await page.keyboard.press("Escape");
    assert.equal(await page.evaluate(() => document.activeElement.id), "navigation-toggle");
    assert.equal(await page.evaluate(() => document.querySelector(".workspace").inert), false);
    await page.locator("#navigation-toggle").click();
    await page.locator("#navigation-close").click();
    assert.equal(await page.evaluate(() => document.activeElement.id), "navigation-toggle");
    await page.locator("#navigation-toggle").click();
    await page.locator("#navigation-backdrop").click({position: {x: 385, y: 200}});
    assert.equal(await page.evaluate(() => document.activeElement.id), "navigation-toggle");
    await page.locator("#navigation-toggle").click();
    await page.locator('#sidebar [data-view="upstream-accounts"]').click(); await accountSettled();
    assert.equal(await page.evaluate(() => document.activeElement.id), "content");
    assert.equal(await page.evaluate(() => navigationDrawerOpen), false);
    const summary = page.locator('[data-account-id="codex-1"]');
    assert.ok((await summary.boundingBox()).y < 650, "first account summary visible on mobile first screen");
    await noOverflow("mobile account summary");
    await capture({path: path.join(screenshots, "accounts-mobile.png")});
    await page.locator('[data-upstream-provider="antigravity"]').click(); await accountSettled();
    await page.locator('[data-account-id="antigravity-1"]').waitFor();
    await page.goBack(); await page.locator('[data-account-id="codex-1"]').waitFor();
    await page.goForward(); await page.locator('[data-account-id="antigravity-1"]').waitFor();
    await page.reload(); await initialize(); await accountSettled();
    assert.equal(await page.locator('[data-account-id="antigravity-1"]').count(), 1);
    await page.locator('[data-upstream-provider="codex"]').click(); await page.locator('[data-account-id="codex-1"]').waitFor();
    await page.reload(); await initialize(); await accountSettled();
    assert.equal(await page.locator('[data-account-id="codex-1"]').count(), 1);
    await page.setViewportSize({width: 1440, height: 1080});
    await capture({path: path.join(screenshots, "accounts-desktop.png"), fullPage: true});
    await summary.locator(".upstream-account-details > summary").click();
    await capture({path: path.join(screenshots, "account-expanded-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 320, height: 844}); await noOverflow("320 account details");
    await navigate("usage"); await page.evaluate((value) => renderPersonalUsage(value), usage);
    await noOverflow("320 usage");
    await page.locator("#usage-rows .record-details summary").first().click();
    assert.equal(await page.locator("#usage-rows .record-details dd").first().isVisible(), true);
    await noOverflow("320 usage details");
    await navigate("monitoring"); await page.locator("#monitoring-recent-rows .record-details summary").first().click(); await noOverflow("320 monitoring details");
    await navigate("billing"); await page.evaluate(async () => { billingUserID = state.user.id; await loadBillingUsers(); await loadBillingDetail(); });
    await noOverflow("320 billing");
    await page.locator("#billing-ledger-rows .record-details summary").first().click(); await noOverflow("320 ledger details");
    await page.setViewportSize({width: 1440, height: 1080});
    await capture({path: path.join(screenshots, "billing-desktop.png"), fullPage: true});
    for (const width of [850, 851]) {
      await page.setViewportSize({width, height: 1080});
      assert.equal(await page.locator("#navigation-toggle").isVisible(), width === 850);
      await noOverflow(`breakpoint ${width}`);
    }
    await page.setViewportSize({width: 850, height: 1080}); await page.locator("#navigation-toggle").click();
    await page.setViewportSize({width: 851, height: 1080});
    await page.waitForFunction(() => !navigationDrawerOpen);
    assert.equal(await page.evaluate(() => navigationDrawerOpen || document.querySelector(".workspace").inert || document.body.classList.contains("navigation-open")), false);
    identity = member;
    await page.evaluate((value) => { history.replaceState(null, "", "#overview"); renderState(value); }, initial(member)); await overviewSettled();
    assert.match(await page.locator("#overview-cash").textContent(), /9\.876543/);
    assert.equal(await page.locator('[aria-labelledby="nav-management"]').isVisible(), false);
    await page.setViewportSize({width: 390, height: 844});
    await noOverflow("member overview mobile");
    await capture({path: path.join(screenshots, "overview-member-mobile.png"), fullPage: true});
    await navigate("billing"); await page.evaluate(async () => { billingUserID = state.user.id; await loadBillingDetail(); });
    await capture({path: path.join(screenshots, "billing-mobile.png"), fullPage: true});
    await page.locator("#navigation-toggle").click(); await page.evaluate(() => handleUnauthorized());
    assert.equal(await page.evaluate(() => navigationDrawerOpen || document.body.classList.contains("navigation-open")), false);
    assert.equal(await page.locator("#auth").evaluate((node) => node.inert), false);
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: browser.version(), checks: "owner/member snapshots, provider failure, old hashes/reload/back/forward, mobile drawer keyboard/focus/inert/logout/resize, 320/390/850/851/1440 overflow, responsive row details", errors, screenshots}));
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
