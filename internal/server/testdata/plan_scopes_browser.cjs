"use strict";

// Synthetic fixtures only: no real OAuth tokens, keys or egress credentials.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8767";
const output = process.env.SCREENSHOT_DIR || "/tmp/gateway-plan-scope-screenshots";
const marketplace = {enabled: true, member_suppliers: false, max_accounts_per_user: 5, max_egress_servers_per_user: 2};
const plans = [
  {id: "p1", name: "Codex 月度", price_usd: "20.000000000000", tier: "month", period_days: 31, allowance_usd: "60.000000000000",
    min_period_count: 1, active: true, version: 2, model_ids: ["family:gpt"]},
  {id: "p2", name: "Claude 周卡", price_usd: "8.000000000000", tier: "week", period_days: 7, allowance_usd: "20.000000000000",
    min_period_count: 1, active: true, version: 1, model_ids: ["family:claude"]},
];
const scopes = {
  scopes: {
    p1: {plan_id: "p1", mode: "selected", revision: 3, updated_at: "2026-11-24T08:00:00Z", accounts: [
      {id: "c000000000000001", provider: "codex", label: "主力 Pro", usable: true},
      {id: "c000000000000002", provider: "codex", label: "备用 Plus", usable: true},
      {id: "c000000000000009", provider: "codex", label: "已转出账号", usable: false}]},
    p2: {plan_id: "p2", mode: "all", revision: 0, updated_at: "2026-11-24T08:00:00Z", accounts: []},
  },
  candidates: [
    {id: "c000000000000001", provider: "codex", label: "主力 Pro", usable: true},
    {id: "c000000000000002", provider: "codex", label: "备用 Plus", usable: true},
    {id: "c000000000000003", provider: "codex", label: "o***@example.test", usable: false},
    {id: "d000000000000001", provider: "anthropic", label: "Claude Max", usable: true},
  ],
};
const memberState = {
  user: {id: "alice-id", username: "alice", display_name: "Alice"}, cash_balance_usd: "12.500000000000",
  source_disabled: {day: false, week: false, month: false, cash: false}, cash_preferences: {priority: 100, charge_enabled: true},
  group: null, groups: [], self_use_enabled: true,
  subscriptions: [{id: "s1", tier: "month", enabled: true, name: "Codex 月度", period_days: 31, model_ids: ["family:gpt"], priority: 1, charge_enabled: true,
    entitlements: [], quota_usd: "60.000000000000", remaining_usd: "41.200000000000", period_count: 1, current_period_number: 1,
    expires_at: "2026-12-25T08:00:00Z", period_id: "pp1", period_started_at: "2026-11-24T08:00:00Z", period_ends_at: "2026-12-25T08:00:00Z",
    updated_at: "2026-11-24T08:00:00Z", plan: plans[0], can_renew: true, config_version: 4,
    provider_scope: {plan_id: "p1", mode: "selected", seller: false, restricted: true, account_counts: {codex: 2}}},
  // A seller plan whose seller added an API key supplier after the purchase.
  {id: "s2", tier: "day", enabled: true, name: "卖家 Claude 日卡", period_days: 1, model_ids: ["family:claude"], priority: 2, charge_enabled: true,
    entitlements: [], quota_usd: "5.000000000000", remaining_usd: "4.100000000000", period_count: 7, current_period_number: 2,
    expires_at: "2026-12-01T08:00:00Z", period_id: "pp2", period_started_at: "2026-11-25T08:00:00Z", period_ends_at: "2026-11-26T08:00:00Z",
    updated_at: "2026-11-24T08:00:00Z", plan: null, can_renew: false, config_version: 2,
    provider_scope: {plan_id: "p9", mode: "all", seller: true, restricted: true, account_counts: {anthropic: 1},
      third_party_api: true, third_party_acknowledged: false}}],
  ledger_entries: [
    {id: 3, entry_type: "usage_charge", amount_usd: "0.420000000000", actual_cost_usd: "0.420000000000", charged_usd: "0.000000000000",
      uncovered_usd: "0.000000000000", self_use: true, model: "gpt-5.4", reason: "request usage charge", created_at: "2026-11-24T09:30:00Z"},
    {id: 2, entry_type: "usage_charge", amount_usd: "1.100000000000", actual_cost_usd: "1.100000000000", charged_usd: "1.100000000000",
      uncovered_usd: "0.000000000000", model: "gpt-5.4", reason: "request usage charge", created_at: "2026-11-24T09:00:00Z"},
  ],
  pagination: {limit: 50, offset: 0, total: 2, has_more: false, next_offset: 2},
};

async function dashboard(browser, viewport, user, hash, handler) {
  const page = await browser.newPage({viewport, locale: "zh-CN"});
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.route("**/*", async route => {
    const request = route.request(), url = new URL(request.url());
    assert.equal(url.origin, origin);
    const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
    if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
    if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
    for (const [name, contentType] of [["style.css", "text/css"], ["theme.js", "application/javascript"], ["favicon.svg", "image/svg+xml"]]) {
      if (url.pathname === `/static/${name}`) return send(fs.readFileSync(path.join(assets, name), "utf8"), 200, contentType);
    }
    if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
    const answer = handler(request, url);
    if (answer !== undefined) return send(answer);
    // Other dashboard panels are outside this fixture; the provider routes are not.
    if (url.pathname.startsWith("/admin/billing/plan") || url.pathname === "/admin/billing/me/self-use") {
      throw new Error(`Unexpected fixture request ${request.method()} ${url.pathname}`);
    }
    return send({error: {message: "不在本测试夹具中", code: "fixture_missing"}}, 404);
  });
  await page.goto(`${origin}/#${hash}`);
  await page.evaluate(value => {window.open = () => null; bindUI(); initializeDateFilters(); renderState(value);},
    {user, recently_verified: true, marketplace, devices: [], projects: [], api_keys: [], passkeys: []});
  return {page, errors};
}

(async () => {
  fs.mkdirSync(output, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const owner = {id: "owner-id", role: "owner", username: "owner", display_name: "管理员"};
    for (const [name, viewport] of [["desktop", {width: 1440, height: 1080}], ["mobile", {width: 390, height: 844}]]) {
      const writes = [];
      const {page, errors} = await dashboard(browser, viewport, owner, "plans", (request, url) => {
        if (request.method() !== "GET") writes.push({path: url.pathname, method: request.method(), body: request.postDataJSON()});
        if (url.pathname === "/admin/billing/plans") return {plans};
        if (url.pathname === "/admin/billing/plan-provider-scopes") return scopes;
        if (url.pathname === "/admin/billing/plans/p1/provider-scope") return {...scopes.scopes.p1, revision: 4, accounts: scopes.scopes.p1.accounts.slice(0, 2)};
        if (url.pathname === "/admin/billing/me") return {...memberState, user: owner, self_use_enabled: undefined, subscriptions: [], ledger_entries: []};
        return undefined;
      });
      await page.locator(".billing-plan-card", {hasText: "Codex 月度"}).waitFor();
      await page.locator(".billing-plan-card", {hasText: "供应商：指定 2 个账号，1 个已失效"}).waitFor();
      await page.locator(".billing-plan-card", {hasText: "供应商：不限制"}).waitFor();
      await page.screenshot({path: path.join(output, `plan-scope-owner-${name}.png`), fullPage: true});
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await page.locator(".billing-plan-card", {hasText: "Codex 月度"}).getByRole("button", {name: "供应商范围"}).click();
      await page.locator("#billing-plan-scope-dialog").waitFor({state: "visible"});
      await page.locator("#billing-plan-scope-stale", {hasText: "已转出账号"}).waitFor();
      assert.equal(await page.locator("#billing-plan-scope-accounts input:checked").count(), 2);
      await page.screenshot({path: path.join(output, `plan-scope-editor-${name}.png`)});
      if (name === "desktop") {
        await page.getByRole("button", {name: "保存范围"}).click();
        await page.locator("#billing-plan-scope-dialog").waitFor({state: "hidden"});
        assert.deepEqual(writes[0], {path: "/admin/billing/plans/p1/provider-scope", method: "PUT",
          body: {mode: "selected", account_ids: ["c000000000000001", "c000000000000002"], revision: 3}});
        await page.locator(".billing-plan-card", {hasText: "供应商：指定 2 个账号"}).waitFor();
      }
      assert.deepEqual(errors, []);
      await page.close();
    }

    const member = {id: "alice-id", role: "member", username: "alice", display_name: "Alice"};
    for (const [name, viewport] of [["desktop", {width: 1440, height: 1080}], ["mobile", {width: 390, height: 844}]]) {
      const writes = [];
      const {page, errors} = await dashboard(browser, viewport, member, "billing", (request, url) => {
        if (request.method() !== "GET") writes.push({path: url.pathname, method: request.method(), body: request.postDataJSON()});
        if (url.pathname === "/admin/billing/me") return memberState;
        if (url.pathname === "/admin/billing/plans") return {plans};
        if (url.pathname === "/admin/billing/me/self-use") return {self_use_enabled: false};
        if (url.pathname === "/admin/billing/me/subscriptions/s2/third-party-consent") return {subscription_id: "s2", third_party_acknowledged: true};
        return undefined;
      });
      // The harness renders state without loadDashboard, which loads billing.
      await page.evaluate(() => loadBillingDashboard());
      await page.locator("#billing-self-use").waitFor({state: "visible"});
      await page.locator(".subscription-card", {hasText: "供应商范围：指定账号（当前可用 Codex 2 个）"}).waitFor();
      await page.locator("#billing-ledger-rows", {hasText: "自用：使用自己的供应商，不扣费"}).waitFor();
      assert.equal(await page.locator("#billing-self-use-toggle").isChecked(), true);
      await page.screenshot({path: path.join(output, `plan-scope-member-billing-${name}.png`), fullPage: true});
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      // Consent to the seller's API key suppliers belongs to the subscription.
      const consent = page.locator(".subscription-card[data-subscription-id=s2] .subscription-third-party");
      await consent.waitFor();
      assert.match(await consent.textContent(), /你确认之前，它们不为这个订阅服务/);
      await consent.scrollIntoViewIfNeeded();
      await page.locator(".subscription-card[data-subscription-id=s2]").screenshot({path: path.join(output, `third-party-consent-${name}.png`)});
      await consent.getByRole("button", {name: "允许第三方 API 供应商"}).click();
      await page.waitForFunction(() => document.querySelector("#notice")?.textContent.includes("已允许第三方 API 供应商"));
      assert.ok(writes.some((write) => write.path === "/admin/billing/me/subscriptions/s2/third-party-consent" && write.body.acknowledged === true));
      writes.length = 0;
      if (name === "desktop") {
        await page.locator("#billing-self-use-toggle").uncheck();
        await page.locator("#billing-self-use-message", {hasText: "已关闭自用"}).waitFor();
        assert.deepEqual(writes[0], {path: "/admin/billing/me/self-use", method: "PUT", body: {enabled: false}});
      }
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log("Plan scopes browser: Owner scope lines and editor, save, member self-use card, subscription scope and self-use ledger, desktop/mobile passed");
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
