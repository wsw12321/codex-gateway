"use strict";

// Optional Playwright verification, using synthetic users, money and plans only.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots/subscription-plans");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const member = {id: "synthetic-member", username: "lin", display_name: "林同学", role: "member", status: "active"};
const other = {id: "synthetic-other", username: "chen", display_name: "陈同学", role: "member", status: "active"};
const initialState = {user: member, recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const plans = [
  {id: "synthetic-daily", name: "每日轻量", price_usd: "0.100000000001", allowance_usd: "20", tier: "day", min_period_count: 2, active: true, version: 3},
  {id: "synthetic-weekly", name: "每周探索", price_usd: "25", allowance_usd: "100", tier: "week", min_period_count: 1, active: true, version: 1},
  {id: "synthetic-monthly", name: "月度创作", price_usd: "60", allowance_usd: "300", tier: "month", min_period_count: 3, active: true, version: 2},
];
let activeState = structuredClone(initialState), cash = "123.45", ledger = [];
const subscriptions = {
  day: {id: "synthetic-subscription", enabled: true, quota_usd: "20", remaining_usd: "7.25", period_count: 125, current_period_number: 5,
    period_started_at: "2026-09-29T00:00:00Z", period_ends_at: "2026-09-30T00:00:00Z", expires_at: "2027-01-28T00:00:00Z", plan: plans[0], can_renew: true, config_version: 4},
  week: {id: "synthetic-manual", enabled: true, quota_usd: "80", remaining_usd: "65", period_count: 4, current_period_number: 1,
    period_started_at: "2026-09-26T00:00:00Z", period_ends_at: "2026-10-03T00:00:00Z", expires_at: "2026-10-24T00:00:00Z", plan: null, can_renew: false, config_version: 1},
  month: {enabled: false, quota_usd: "0", remaining_usd: "0", period_count: 0, current_period_number: 0, plan: null, can_renew: false, config_version: 0},
};
const billing = (id = member.id) => ({user: id === member.id ? activeState.user : other, cash_balance_usd: cash,
  source_disabled: {day: false, week: false, month: false, cash: false}, subscriptions, ledger_entries: ledger,
  pagination: {offset: 0, limit: 50, has_more: false}});

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN", timezoneId: "Asia/Shanghai"});
    const page = await context.newPage(), errors = [], writes = [], committed = new Map();
    let loseResponse = false;
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => { if (message.type() === "error" && !/net::ERR_FAILED/.test(message.text())) errors.push(message.text()); });
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765");
      const send = (value, contentType = "application/json") => route.fulfill({status: 200, contentType, body: typeof value === "string" ? value : JSON.stringify(value)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), "text/css");
      if (url.pathname === "/static/theme.js") return send(fs.readFileSync(path.join(assets, "theme.js"), "utf8"), "application/javascript");
      if (url.pathname === "/static/favicon.svg") return send(fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), "image/svg+xml");
      if (url.pathname === "/static/app.js") return send(app, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/billing/plans" && request.method() === "GET") return send({plans});
      if (url.pathname === "/admin/billing/me") return send(billing());
      if (url.pathname === `/admin/billing/users/${other.id}`) return send(billing(other.id));
      if (url.pathname === "/admin/billing/users") return send({users: [activeState.user, other].map((user) => ({...user, cash_balance_usd: cash}))});
      if (url.pathname === "/auth/password/reauth") return send({ok: true});
      if (request.method() === "POST" && (url.pathname === "/admin/billing/me/purchases" || /\/renewals$/.test(url.pathname))) {
        const body = request.postDataJSON(); writes.push({url: url.pathname, body: request.postData()});
        let result = committed.get(body.operation_id);
        if (!result) {
          const plan = body.plan_id ? plans.find((item) => item.id === body.plan_id) : plans[0];
          const sub = subscriptions[plan.tier];
          if (body.plan_id) Object.assign(sub, {enabled: true, quota_usd: plan.allowance_usd, remaining_usd: plan.allowance_usd,
            period_count: body.period_count, current_period_number: 1, config_version: sub.config_version + 1, plan, can_renew: true});
          else { sub.period_count += body.period_count; sub.expires_at = "2027-01-30T00:00:00Z"; sub.config_version++; }
          cash = "123.249999999998";
          const entry = {entry_type: body.plan_id ? "plan_purchase" : "plan_renewal", amount_usd: "-0.200000000002",
            balance_after_usd: cash, occurred_at: "2026-09-29T04:00:00Z", subscription_tier: plan.tier, reason: `套餐：${plan.name}，本次 ${body.period_count} 期`};
          ledger.unshift(entry);
          result = structuredClone({plan, subscription: sub, period_count: body.period_count, total_usd: "0.200000000002", balance_usd: cash, ledger: entry});
          committed.set(body.operation_id, result);
        }
        if (loseResponse) { loseResponse = false; return route.abort("failed"); }
        return send(result);
      }
      throw new Error(`Unexpected mocked request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#billing");
    await page.evaluate(async (value) => { bindUI(); initializeDateFilters(); renderState(value); await Promise.all([loadBillingPlans(), loadBillingDetail()]); setConnection("已连接", "ok"); }, activeState);
    const overflow = async () => assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "page must not overflow horizontally");
    const screenshot = async (name) => { await overflow(); await page.screenshot({path: path.join(screenshots, name + ".png"), fullPage: false}); };
    const settled = () => page.waitForFunction(() => !billingPlanOperation?.busy && !billingDetailLoading && !billingPlansLoading);
    assert.equal(await page.locator("[data-billing-buy]:visible").count(), 3);
    assert.equal(await page.locator("[data-billing-renew]:visible").count(), 1);
    assert.match(await page.locator('[data-subscription-tier="week"]').textContent(), /未绑定套餐，无法续费/);
    await page.locator("#billing-plans-panel").scrollIntoViewIfNeeded(); await screenshot("catalog-desktop");

    await page.locator("[data-billing-renew]").click();
    assert.match(await page.locator("#billing-purchase-summary").textContent(), /剩余额度 US\$7\.25.*续费保留本周期和剩余额度/);
    await screenshot("renewal-desktop");
    await page.locator('#billing-purchase-form input[name="confirmed"]').check();
    await page.locator("#billing-purchase-submit").click(); await settled();
    assert.equal(writes.length, 1);
    assert.equal(writes[0].url, "/admin/billing/me/subscriptions/day/renewals");
    assert.equal(subscriptions.day.period_count, 127);
    assert.equal(subscriptions.day.current_period_number, 5);
    assert.equal(subscriptions.day.remaining_usd, "7.25");
    assert.match(await page.locator('[data-subscription-tier="day"]').textContent(), /第 5\/127 个周期/);
    assert.match(await page.locator('[data-subscription-tier="day"]').textContent(), /US\$7\.25/);

    await page.setViewportSize({width: 390, height: 1280});
    await page.locator("#billing-plans-title").click();
    await page.evaluate(() => {
      window.scrollTo(0, window.scrollY + byId("billing-plans-panel").getBoundingClientRect().top - 80);
    });
    assert.ok(await page.locator("#billing-plans-refresh").evaluate((button) => button.getBoundingClientRect().width > 80));
    await screenshot("catalog-mobile");
    await page.setViewportSize({width: 390, height: 844});
    await page.locator('[data-billing-buy="synthetic-daily"]').click();
    assert.match(await page.locator("#billing-purchase-warning").textContent(), /覆盖已有日订阅.*不结转、不自动退款/);
    assert.match(await page.locator("#billing-purchase-summary").textContent(), /US\$0\.200000000002/);
    await screenshot("purchase-mobile");
    await page.locator('#billing-purchase-form input[name="confirmed"]').check();
    loseResponse = true; await page.locator("#billing-purchase-submit").click(); await settled();
    assert.equal(writes.length, 2);
    assert.match(await page.locator('#billing-purchase-form .form-message').textContent(), /原请求和操作 ID 已保留/);
    assert.equal(await page.locator('#billing-purchase-form input[name="period_count"]').isDisabled(), true);
    await page.locator('#billing-purchase-dialog [data-close]').click();
    assert.equal(await page.locator('[data-billing-buy="synthetic-daily"]').isDisabled(), true);
    await page.locator("#billing-plan-resume").click();
    await page.locator("#billing-purchase-submit").click(); await settled();
    assert.equal(writes.length, 3); assert.equal(writes[1].body, writes[2].body);
    assert.equal(committed.size, 2);
    assert.match(await page.locator("#billing-cash-balance").textContent(), /123\.249999999998/);
    assert.match(await page.locator("#billing-ledger-rows").textContent(), /套餐购买并重开/);

    activeState = {...activeState, user: {...member, role: "owner"}};
    await page.setViewportSize({width: 1440, height: 1080});
    await page.evaluate(async (value) => { renderState(value); await Promise.all([loadBillingPlans(), loadBillingDetail(), loadBillingUsers()]); }, activeState);
    await page.locator('[data-billing-plan-edit="synthetic-daily"]').click();
    assert.match(await page.locator("#billing-plan-edit-warning").textContent(), /解除已有订阅的套餐绑定，已购权益保持有效/);
    await screenshot("owner-editor-desktop");
    await page.locator('#billing-plan-dialog [data-close]').click();
    await page.evaluate(async (user) => { await selectBillingUser(user); }, other);
    assert.equal(await page.locator("[data-billing-buy]:visible").count(), 0);
    assert.equal(await page.locator("[data-billing-renew]:visible").count(), 0);
    assert.equal(await page.locator("#billing-plan-readonly").isVisible(), true);
    await overflow();
    assert.deepEqual(errors, []);
    console.log("Billing plan desktop/mobile, renewal, Owner editor, unknown-result retry and account scope checks passed.");
  } finally { await browser.close(); }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
