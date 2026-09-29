"use strict";

// Optional browser regression: all data is synthetic and every request is intercepted.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
const initialState = {user: {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"},
  recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const accounts = [
  {id: "agy-alpha", display_name: "team-alpha", email_masked: "al***@example.test", allocation_weight: 5, concurrent_limit: 3,
    rolling_cost_usd: "12.345678", rolling_cost_share: "0.25", target_share: "0.2", request_count: 128, input_tokens: 384000,
    cached_input_tokens: 102000, output_tokens: 18200, error_count: 2, equivalent_cost_usd: "145.6728", access_mode: "shared", authorized_user_ids: []},
  {id: "agy-beta", display_name: "team-beta", email_masked: "be***@example.test", allocation_weight: 20, concurrent_limit: 2,
    rolling_cost_usd: "37.037034", rolling_cost_share: "0.75", target_share: "0.8", request_count: 410, input_tokens: 892000,
    cached_input_tokens: 452000, output_tokens: 78300, error_count: 0, equivalent_cost_usd: "481.2941", access_mode: "exclusive", authorized_user_ids: ["member-1"]},
].map((value) => ({plan: "Antigravity", status: "available", cliproxy_status: "active", gateway_manual_status: "enabled",
  gateway_quota_status: "available", can_manage: true, last_synced_at: "2026-09-28T08:00:00Z", ...value}));

async function main() {
  const browser = await chromium.launch({headless: true});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [], events = [], reads = [];
    let failList = false, syncWarning = false, holdAGY = false, releaseAGY, heldAGY;
    page.on("pageerror", (error) => errors.push(error.message));
    const response = () => ({all: true, until: "2026-09-28T08:00:00Z", allocation_from: "2026-09-27T08:00:00Z", allocation_until: "2026-09-28T08:00:00Z",
      accounts: accounts.map((account) => ({...account})), ...(syncWarning ? {sync_warning: "antigravity_sync_failed"} : {})});
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, origin);
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/antigravity-accounts/concurrency") return send({sampled_at: new Date().toISOString(), accounts: accounts.map((account, index) => ({id: account.id, active_requests: index + 1}))});
      if (url.pathname === "/admin/upstream-accounts/concurrency") return send({sampled_at: new Date().toISOString(), accounts: [{id: "codex-only", active_requests: 0}]});
      if (url.pathname === "/admin/antigravity-accounts") {
        reads.push(url.search);
        if (holdAGY) { heldAGY(); await new Promise((resolve) => { releaseAGY = resolve; }); }
        return failList ? send({error: {message: "模拟统计服务暂不可用"}}, 503) : send(response());
      }
      if (url.pathname === "/admin/upstream-accounts") return send({accounts: [{...accounts[0], id: "codex-only", display_name: "", plan: "Plus"}]});
      if (url.pathname === "/admin/billing/users") return send({users: [initialState.user, {id: "member-1", username: "lin", display_name: "林同学", role: "member"}]});
      if (url.pathname === "/admin/billing/me") return send({cash_balance_usd: "0", subscriptions: [], ledger: []});
      if (url.pathname === "/admin/usage") return send({summary: {requests: 0, tokens: 0, error_rate: 0}, requests: []});
      if (url.pathname === "/auth/password/reauth") { events.push("verified"); return send({ok: true}); }
      const match = url.pathname.match(/^\/admin\/antigravity-accounts\/([^/]+)\/(allocation-weight|concurrent-limit|status|access)$/);
      if (match && request.method() === "PUT") {
        const body = request.postDataJSON(), account = accounts.find((item) => item.id === match[1]);
        assert.ok(account);
        writes.push({path: url.pathname, body}); events.push("write");
        if (match[2] === "status") {
          account.status = body.enabled ? "available" : "unavailable";
          account.gateway_manual_status = body.enabled ? "enabled" : "manual_disabled";
        }
        if (match[2] === "allocation-weight") account.allocation_weight = body.weight;
        if (match[2] === "concurrent-limit") account.concurrent_limit = body.concurrent_limit;
        if (match[2] === "access") { account.access_mode = body.mode; account.authorized_user_ids = body.user_ids; }
        return send({...account});
      }
      throw new Error(`Unexpected mocked request: ${request.method()} ${url.pathname}`);
    });
    await page.goto(`${origin}/#antigravity-accounts`);
    await page.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, initialState);
    const settled = () => page.waitForFunction(() => !upstreamAccountOperation && !upstreamAccountListLoading);
    await settled();
    const card = () => page.locator('[data-account-id="agy-alpha"]');
    const manager = page.locator('[data-section="antigravity-accounts"]');
    const weight = () => card().locator(".upstream-allocation-input");
    const saveWeight = () => card().locator(".upstream-allocation-save").click();
    const refresh = async () => {
      await page.locator(".upstream-history-filter").evaluate((node) => { node.open = true; });
      await page.locator("#upstream-account-filter button[type=submit]").click(); await settled();
    };
    assert.equal(await manager.isVisible(), true);
    assert.equal(await card().locator(".upstream-account-details").getAttribute("open"), null);
    await card().locator(".upstream-account-details > summary").click();
    assert.equal(await manager.locator(".upstream-quota").count(), 0);
    assert.match(await card().textContent(), /账号名称：team-alpha/);
    assert.match(await card().locator(".upstream-concurrency").textContent(), /活跃对话名额1同一 API Key.*同对话重叠请求共享名额.*未识别的请求独占/);
    assert.match(await page.locator("#upstream-concurrency-limit-help").textContent(), /同一 API Key、同一对话的重叠请求.*共享一个名额.*最后一个请求结束后立即释放/);
    assert.match(await page.locator("#upstream-concurrency-limit-help").textContent(), /标题等无法识别对话的请求各占一个名额.*不同 API Key 分别计数/);
    assert.match(await page.locator("#upstream-quota-warning").textContent(), /不提供精确的剩余额度百分比/);

    await card().locator(".upstream-concurrency-limit-input").fill("0");
    await card().locator(".upstream-concurrency-limit-save").click();
    assert.equal(writes.length, 0);
    assert.equal(await card().locator(".upstream-concurrency-limit-input").getAttribute("aria-invalid"), "true");
    await card().locator(".upstream-concurrency-limit-input").fill("4");
    await card().locator(".upstream-concurrency-limit-save").click(); await settled();
    assert.match(await page.locator("#upstream-account-action-message").textContent(), /并发对话上限已保存为 4/);

    await page.evaluate(() => { state.recently_verified = false; });
    const eventOffset = events.length;
    await weight().fill("0"); await saveWeight();
    await page.locator("#reauth-dialog").waitFor({state: "visible"});
    assert.equal(events.length, eventOffset);
    await page.locator('#reauth-form input[name="password"]').fill("synthetic-test-password");
    await page.locator('#reauth-form button[type="submit"]').click(); await settled();
    assert.deepEqual(events.slice(eventOffset), ["verified", "write"]);
    assert.match(await card().locator(".upstream-allocation-state").textContent(), /停止接收新请求/);
    await weight().fill("5"); await saveWeight(); await settled();

    await card().locator(".upstream-access-button").click();
    await page.locator("#upstream-access-dialog").waitFor({state: "visible"});
    await page.locator('#upstream-access-form select[name="mode"]').selectOption("exclusive");
    await page.getByRole("checkbox", {name: "授权用户 lin", exact: true}).check();
    await page.locator('#upstream-access-form input[name="reason"]').fill("仅团队成员使用");
    await page.locator('#upstream-access-form button[type="submit"]').click(); await settled();
    assert.deepEqual(writes.at(-1).body, {mode: "exclusive", user_ids: ["member-1"], reason: "仅团队成员使用"});
    assert.match(await card().locator(".upstream-access").textContent(), /专属 · 1 人/);
    await card().locator(".upstream-account-status-button").click(); await settled();
    assert.equal(await card().locator(".upstream-account-status-button").textContent(), "重新启用");
    await card().locator(".upstream-account-status-button").click(); await settled();

    await page.locator(".upstream-history-filter").evaluate((node) => { node.open = true; });
    await page.locator('#upstream-account-filter select[name="range"]').selectOption("all"); await refresh();
    assert.ok(reads.at(-1).includes("all=true"));
    assert.match(await page.locator("#upstream-account-period").textContent(), /Antigravity账号.*全部历史/);
    syncWarning = true; await refresh();
    assert.equal(await weight().isDisabled(), true);
    syncWarning = false; await refresh();
    failList = true; await weight().fill("7"); await saveWeight(); await settled();
    assert.match(await page.locator("#upstream-account-action-message").textContent(), /已保存为 7/);
    assert.match(await page.locator("#upstream-account-refresh-message").textContent(), /操作已成功，但列表与统计刷新失败/);
    failList = false; await refresh();

    await weight().fill("23");
    await page.locator("#upstream-account-search").fill("team-alpha");

    // Late Antigravity responses must not replace the Codex provider's cards.
    holdAGY = true;
    const held = new Promise((resolve) => { heldAGY = resolve; });
    await page.locator("#upstream-account-filter button[type=submit]").click(); await held;
    await page.locator('[data-upstream-provider="codex"]').click(); await settled();
    await page.locator('[data-account-id="codex-only"]').waitFor({state: "visible"});
    assert.equal(await page.locator('[data-account-id="codex-only"]').isVisible(), true);
    assert.match(await page.locator("#upstream-concurrency-limit-help").textContent(), /活跃 root 对话计数/);
    assert.equal(await page.locator(".upstream-quota").count(), 1);
    releaseAGY(); holdAGY = false;
    await page.waitForTimeout(50);
    assert.equal(await page.locator('[data-account-id="agy-alpha"]').count(), 0);
    await page.locator('[data-upstream-provider="antigravity"]').click(); await settled();
    await card().waitFor({state: "visible"});
    assert.equal(await card().isVisible(), true);
    assert.equal(await manager.locator(".upstream-quota").count(), 0);
    assert.equal(await weight().inputValue(), "23", "provider switch restores unsaved account input");
    assert.equal(await page.locator("#upstream-account-search").inputValue(), "team-alpha");
    await page.goBack();
    await page.locator('[data-account-id="codex-only"]').waitFor({state: "visible"});
    await page.goForward();
    await card().waitFor({state: "visible"});
    assert.equal(await weight().inputValue(), "23", "browser history retains per-provider drafts");
    await page.locator("#upstream-account-search").fill("");

    // Changing providers during permission reauthentication cancels the pending mutation.
    await card().locator(".upstream-access-button").click();
    await page.locator("#upstream-access-dialog").waitFor({state: "visible"});
    await page.locator('#upstream-access-form input[name="reason"]').fill("取消的权限调整");
    await page.evaluate(() => { state.recently_verified = false; });
    const writesBeforeSwitch = writes.length;
    await page.locator('#upstream-access-form button[type="submit"]').click();
    await page.locator("#reauth-dialog").waitFor({state: "visible"});
    await page.evaluate(() => { location.hash = "#upstream-accounts"; });
    await page.locator('[data-account-id="codex-only"]').waitFor({state: "visible"});
    assert.equal(await page.locator("#reauth-dialog").isVisible(), false);
    assert.equal(writes.length, writesBeforeSwitch);
    await page.locator('[data-upstream-provider="antigravity"]').click();
    await card().waitFor({state: "visible"});

    fs.mkdirSync(screenshots, {recursive: true});
    await page.locator(".antigravity-login-help summary").click();
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.evaluate(() => { hide("notice"); });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "desktop must not overflow horizontally");
    await page.screenshot({path: path.join(screenshots, "antigravity-accounts-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.locator(".upstream-history-filter").evaluate((node) => { node.open = false; });
    await card().locator(".upstream-account-details").evaluate((node) => { node.open = false; });
    await page.locator(".antigravity-login-help").evaluate((node) => { node.open = false; });
    await page.evaluate(() => window.scrollTo(0, 0));
    const summary = await card().locator(".upstream-account-summary").boundingBox();
    assert.ok(summary && summary.y + summary.height <= 844, "first account summary must be visible without scrolling");
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile must not overflow horizontally");
    await page.screenshot({path: path.join(screenshots, "antigravity-accounts-mobile.png"), fullPage: true});
    await page.evaluate(() => { renderState({...state, user: {...state.user, role: "member"}}); });
    assert.equal(new URL(page.url()).hash, "#overview");
    assert.equal(await page.locator('nav [data-view="upstream-accounts"]').isVisible(), false);
    assert.equal(await manager.isVisible(), false);
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: await browser.version(), writes: writes.length, errors, desktop: "1440x1080", mobile: "390x844"}));
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
