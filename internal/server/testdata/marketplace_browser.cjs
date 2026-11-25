"use strict";

// Synthetic fixtures only: no real users, OAuth tokens, keys or sales.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8768";
const output = process.env.SCREENSHOT_DIR || "/tmp/gateway-marketplace-screenshots";
const marketplaceFor = (user) => ({enabled: true, member_suppliers: false, max_accounts_per_user: 5, max_egress_servers_per_user: 2,
  max_plans_per_user: 5, ...(user.role === "owner" ? {pending_reviews: 1} : {})});
const platformPlan = {id: "p1", name: "Codex 月度", platform: true, seller_name: "", description: "", price_usd: "20.000000000000", tier: "month",
  period_days: 31, allowance_usd: "60.000000000000", min_period_count: 1, active: true, version: 2, model_ids: ["family:gpt"],
  providers: [{provider: "codex", official: 4, third_party: 0}], third_party_api: false, own: false};
const sellerPlans = [
  {id: "s1", name: "Alice 的 Claude 周卡", platform: false, seller_name: "Alice", description: "官方 Claude Max 账号，工作日响应快。",
    price_usd: "6.000000000000", tier: "week", period_days: 7, allowance_usd: "15.000000000000", min_period_count: 1, active: true,
    version: 3, model_ids: ["family:claude"], providers: [{provider: "anthropic", official: 2, third_party: 0}], third_party_api: false, own: false},
  {id: "s2", name: "Bob 的 GPT 日卡", platform: false, seller_name: "Bob", description: "含一个 API Key 供应商作为补充。",
    price_usd: "1.500000000000", tier: "day", period_days: 1, allowance_usd: "4.000000000000", min_period_count: 2, active: true,
    version: 1, model_ids: ["family:gpt"], providers: [{provider: "codex", official: 1, third_party: 1}], third_party_api: true, own: false},
];
const billingMe = (user) => ({user: {id: user.id, username: user.username, display_name: user.display_name}, cash_balance_usd: "25.000000000000",
  source_disabled: {day: false, week: false, month: false, cash: false}, cash_preferences: {priority: 100, charge_enabled: true},
  group: null, groups: [], subscriptions: [], ledger_entries: [], pagination: {limit: 50, offset: 0, total: 0, has_more: false, next_offset: 0}});
const myPlans = {
  plans: [
    {...sellerPlans[0], seller_user_id: "alice-id", review_status: "approved", review_note: "", on_sale: true, admin_suspended: false, subscribers: 3},
    {...sellerPlans[0], id: "s3", name: "Alice 的 Codex 月卡", description: "两个 Pro 账号轮换。", price_usd: "18.000000000000", tier: "month",
      period_days: 31, model_ids: ["family:gpt"], seller_user_id: "alice-id", review_status: "rejected", review_note: "请写明每期额度对应的大致请求量。",
      on_sale: false, active: true, admin_suspended: false, subscribers: 0},
    {...sellerPlans[0], id: "s4", name: "Alice 的试用日卡", description: "", price_usd: "0.500000000000", tier: "day", period_days: 1,
      allowance_usd: "1.000000000000", seller_user_id: "alice-id", review_status: "draft", review_note: "", on_sale: false, active: true, admin_suspended: false, subscribers: 0},
  ],
  scopes: {s1: {plan_id: "s1", mode: "selected", revision: 2, updated_at: "2026-11-24T08:00:00Z",
    accounts: [{id: "a000000000000001", provider: "anthropic", label: "Claude Max 主号", usable: true}]}},
  candidates: [{id: "a000000000000001", provider: "anthropic", label: "Claude Max 主号", usable: true},
    {id: "a000000000000002", provider: "anthropic", label: "Claude Max 备用", usable: true}],
};
const sales = (owner) => ({total: 2, summary: {count: 2, buyers: 2, gross_usd: "12.000000000000", commission_usd: "1.200000000000", net_usd: "10.800000000000"},
  sales: [
    {id: 2, kind: "renewal", plan_id: "s1", plan_name: "Alice 的 Claude 周卡", plan_version: 3, buyer_number: 2, period_count: 1, gross_usd: "6.000000000000",
      commission_rate: "0.1", commission_usd: "0.600000000000", net_usd: "5.400000000000", created_at: "2026-11-24T10:00:00Z",
      ...(owner ? {seller_name: "Alice", buyer_name: "Carol"} : {})},
    {id: 1, kind: "purchase", plan_id: "s1", plan_name: "Alice 的 Claude 周卡", plan_version: 3, buyer_number: 1, period_count: 1, gross_usd: "6.000000000000",
      commission_rate: "0.1", commission_usd: "0.600000000000", net_usd: "5.400000000000", created_at: "2026-11-23T10:00:00Z",
      ...(owner ? {seller_name: "Alice", buyer_name: "Dave"} : {})},
  ]});
const reviews = {
  pending: [{...sellerPlans[1], seller_user_id: "bob-id", review_status: "pending", review_note: "", submitted_at: "2026-11-24T09:00:00Z",
    on_sale: false, admin_suspended: false, subscribers: 0}],
  plans: [
    {...sellerPlans[0], seller_user_id: "alice-id", review_status: "approved", on_sale: true, admin_suspended: false, subscribers: 3},
    {...sellerPlans[1], seller_user_id: "bob-id", review_status: "pending", on_sale: false, admin_suspended: false, subscribers: 0},
  ],
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
    if (url.pathname.startsWith("/admin/market") || url.pathname.startsWith("/admin/me/plans") || url.pathname === "/admin/me/sales") {
      throw new Error(`Unexpected fixture request ${request.method()} ${url.pathname}`);
    }
    return send({error: {message: "不在本测试夹具中", code: "fixture_missing"}}, 404);
  });
  await page.goto(`${origin}/#${hash}`);
  await page.evaluate(value => {window.open = () => null; bindUI(); initializeDateFilters(); renderState(value);},
    {user, recently_verified: true, marketplace: marketplaceFor(user), devices: [], projects: [], api_keys: [], passkeys: []});
  return {page, errors};
}

(async () => {
  fs.mkdirSync(output, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const carol = {id: "carol-id", role: "member", username: "carol", display_name: "Carol"};
    for (const [name, viewport] of [["desktop", {width: 1440, height: 1080}], ["mobile", {width: 390, height: 844}]]) {
      const writes = [];
      const {page, errors} = await dashboard(browser, viewport, carol, "plans", (request, url) => {
        if (request.method() !== "GET") writes.push({path: url.pathname, method: request.method(), body: request.postDataJSON()});
        if (url.pathname === "/admin/billing/plans") return {plans: [platformPlan]};
        if (url.pathname === "/admin/market/plans") return {plans: [platformPlan, ...sellerPlans]};
        if (url.pathname === "/admin/billing/me") return billingMe(carol);
        if (url.pathname === "/admin/billing/me/purchases") return {plan: sellerPlans[1], period_count: 2, total_usd: "3.000000000000",
          balance_usd: "22.000000000000", subscription: {id: "sub-1"}, ledger: {id: 9}};
        return undefined;
      });
      await page.evaluate(() => loadBillingDashboard());
      await page.locator(".billing-plan-card", {hasText: "卖家：Bob"}).waitFor();
      await page.locator(".billing-plan-card", {hasText: "含第三方 API 供应商"}).waitFor();
      await page.screenshot({path: path.join(output, `marketplace-member-${name}.png`), fullPage: true});
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await page.locator(".billing-plan-card", {hasText: "Bob 的 GPT 日卡"}).getByRole("button", {name: "购买订阅"}).click();
      await page.locator("#billing-purchase-dialog").waitFor({state: "visible"});
      await page.locator("#billing-purchase-third-party").waitFor({state: "visible"});
      await page.waitForTimeout(400); // let the dialog finish fading in
      await page.screenshot({path: path.join(output, `marketplace-purchase-consent-${name}.png`)});
      if (name === "desktop") {
        await page.locator("#billing-purchase-form input[name=confirmed]").check();
        await page.getByRole("button", {name: "确认购买"}).click();
        await page.locator("#billing-purchase-form .form-message", {hasText: "第三方 API"}).waitFor();
        assert.equal(writes.length, 0);
        await page.locator("#billing-purchase-form input[name=acknowledge_third_party]").check();
        await page.getByRole("button", {name: "确认购买"}).click();
        await page.locator("#billing-purchase-dialog").waitFor({state: "hidden"});
        assert.deepEqual({...writes[0], body: {...writes[0].body, operation_id: "x"}}, {path: "/admin/billing/me/purchases", method: "POST",
          body: {plan_version: 1, period_count: 2, plan_id: "s2", acknowledge_third_party: true, operation_id: "x"}});
      }
      assert.deepEqual(errors, []);
      await page.close();
    }

    const alice = {id: "alice-id", role: "member", username: "alice", display_name: "Alice"};
    for (const [name, viewport] of [["desktop", {width: 1440, height: 1080}], ["mobile", {width: 390, height: 844}]]) {
      const {page, errors} = await dashboard(browser, viewport, alice, "my-plans", (request, url) => {
        if (url.pathname === "/admin/me/plans") return myPlans;
        if (url.pathname === "/admin/me/sales") return sales(false);
        if (url.pathname === "/admin/me/plans/s1/usage") return {plan_id: "s1", subscribers: 3, consumed_usd: "9.300000000000",
          accounts: [{account_id: "a000000000000001", label: "Claude Max 主号", provider: "anthropic", requests: 128, input_tokens: 940000, output_tokens: 81000, charged_usd: "9.300000000000"}]};
        return undefined;
      });
      await page.locator(".billing-plan-card", {hasText: "驳回原因"}).waitFor();
      await page.locator("#my-sales-body", {hasText: "买家 #2"}).waitFor();
      await page.locator(".billing-plan-card", {hasText: "Alice 的 Claude 周卡"}).getByRole("button", {name: "查看用量"}).click();
      await page.locator("#my-plan-usage-body", {hasText: "Claude Max 主号"}).waitFor();
      assert.equal(await page.locator("#my-sales-body", {hasText: "Carol"}).count(), 0);
      await page.screenshot({path: path.join(output, `marketplace-my-plans-${name}.png`), fullPage: true});
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      if (name === "desktop") {
        await page.evaluate(() => { loadFormModelPicker = () => {}; });
        await page.getByRole("button", {name: "发布套餐"}).click();
        await page.locator("#seller-plan-dialog").waitFor({state: "visible"});
        await page.waitForTimeout(400);
        await page.screenshot({path: path.join(output, `marketplace-seller-editor-${name}.png`)});
      }
      assert.deepEqual(errors, []);
      await page.close();
    }

    const owner = {id: "owner-id", role: "owner", username: "owner", display_name: "管理员"};
    for (const [name, viewport] of [["desktop", {width: 1440, height: 1080}], ["mobile", {width: 390, height: 844}]]) {
      const writes = [];
      const {page, errors} = await dashboard(browser, viewport, owner, "market", (request, url) => {
        if (request.method() !== "GET") writes.push({path: url.pathname, method: request.method(), body: request.postDataJSON()});
        if (url.pathname === "/admin/market/settings") return {enabled: true, commission_rate: "0.1", max_accounts_per_user: 5, max_plans_per_user: 3,
          allow_member_suppliers: false, max_egress_servers_per_user: 2, revision: 7, updated_at: "2026-11-24T08:00:00Z"};
        if (url.pathname === "/admin/market/reviews") return reviews;
        if (url.pathname === "/admin/market/sales") return sales(true);
        if (url.pathname === "/admin/market/reviews/s2") return {...reviews.pending[0], review_status: "rejected"};
        return undefined;
      });
      await page.locator("#market-reviews-list .billing-plan-card", {hasText: "Bob 的 GPT 日卡"}).waitFor();
      await page.locator("#market-sales-body", {hasText: "Carol"}).waitFor();
      assert.equal(await page.locator("#market-review-badge").textContent(), "1");
      await page.screenshot({path: path.join(output, `marketplace-owner-${name}.png`), fullPage: true});
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      if (name === "desktop") {
        await page.locator(".market-review-note").fill("请说明第三方 API 供应商的来源。");
        await page.locator("#market-reviews-list").getByRole("button", {name: "驳回"}).click();
        await page.locator("#market-message", {hasText: "已驳回"}).waitFor();
        assert.deepEqual(writes[0], {path: "/admin/market/reviews/s2", method: "POST",
          body: {decision: "reject", note: "请说明第三方 API 供应商的来源。", version: 1}});
      }
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log("Marketplace browser: member catalog and third-party consent, seller plans with sales and usage, Owner review and sales, desktop/mobile passed");
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
