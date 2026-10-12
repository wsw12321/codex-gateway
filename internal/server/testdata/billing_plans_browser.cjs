"use strict";
// Synthetic browser regressions; no live account, balance or key is used.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots/multi-subscriptions");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const owner = {id: "synthetic-owner", username: "lin", display_name: "林同学", role: "owner", status: "active"};
const other = {id: "synthetic-other", username: "chen", display_name: "陈同学", role: "member", status: "active"};
const session = {user: owner, recently_verified: true, login_methods: {password: true}, devices: [{id: "device", name: "测试设备", status: "active"}], projects: [], api_keys: [], passkeys: []};
const models = [{id: "gpt-example", group: "GPT"}, {id: "gpt-extra", group: "GPT"}, {id: "gemini-example", group: "Gemini"}, {id: "claude-example", group: "Claude"}];
const plans = [{id: "plan-one", name: "创作月卡", tier: "month", period_days: 31, price_usd: "20.000000000001", allowance_usd: "100", model_ids: models.map((m) => m.id), min_period_count: 1, active: true, version: 4},
  {id: "plan-custom", name: "十日体验", tier: "custom", period_days: 10, price_usd: "8", allowance_usd: "30", model_ids: ["gpt-example"], min_period_count: 1, active: true, version: 1}];
let subscriptions = [{id: "sub-one", name: "已购月卡", tier: "month", period_days: 31, model_ids: ["gpt-example"], priority: 1, charge_enabled: true, enabled: true, quota_usd: "80", remaining_usd: "61.25", period_count: 3, current_period_number: 1, period_started_at: "2099-01-01T00:00:00Z", period_ends_at: "2099-02-01T00:00:00Z", expires_at: "2099-04-04T00:00:00Z", plan: plans[0], can_renew: true, config_version: 8},
  {id: "sub-two", name: "团队月卡", tier: "month", period_days: 31, model_ids: ["gemini-example", "claude-example"], priority: 2, charge_enabled: false, enabled: true, quota_usd: "50", remaining_usd: "49.000000000001", period_count: 0, current_period_number: 1, period_started_at: "2099-01-01T00:00:00Z", period_ends_at: "2099-02-01T00:00:00Z", expires_at: null, plan: null, can_renew: false, config_version: 1},
  {id: "expired", name: "已到期", enabled: true, expires_at: "2000-01-01"}, {id: "disabled", name: "已停用", enabled: false}];
subscriptions[0].entitlements = [
  {starts_at: "2099-01-01T00:00:00Z", ends_at: "2099-03-04T00:00:00Z", period_days: 31, allowance_usd: "80", model_ids: ["gpt-example"]},
  {starts_at: "2099-03-04T00:00:00Z", ends_at: "2099-04-04T00:00:00Z", period_days: 31, allowance_usd: "100", model_ids: ["gpt-example", "claude-example"]},
];
let cashPreferences = {priority: 100, charge_enabled: true};
const billing = (user = owner) => ({user, subscriptions, cash_preferences: cashPreferences, cash_balance_usd: "128.123456789012", ledger_entries: [], pagination: {offset: 0, limit: 50, has_more: false}});

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1080}, locale: "zh-CN", timezoneId: "Asia/Shanghai"});
    const errors = [], writes = [], committed = new Map(); let loseResponse = false;
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("dialog", (dialog) => dialog.accept());
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      const send = (value, type = "application/json") => route.fulfill({status: 200, contentType: type, body: typeof value === "string" ? value : JSON.stringify(value)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), "text/html");
      if (url.pathname.startsWith("/static/")) {
        const name = path.basename(url.pathname);
        return send(name === "app.js" ? app : fs.readFileSync(path.join(assets, name), "utf8"), name.endsWith("css") ? "text/css" : name.endsWith("svg") ? "image/svg+xml" : "application/javascript");
      }
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (request.method() === "GET") {
        if (url.pathname === "/admin/model-catalog") return send({models});
        if (url.pathname === "/admin/model-identifications/options") return send({accounts: [{id: "account", masked_email: "test@example.invalid"}], models: models.map((m) => m.id), model_catalog: models, reference_version: "test"});
        if (url.pathname === "/admin/model-identifications") return send({records: []});
        if (url.pathname === "/admin/state") return send(session);
        if (url.pathname === "/admin/billing/plans") return send({plans});
        if (url.pathname === "/admin/billing/me") return send(billing());
        if (url.pathname === `/admin/billing/users/${other.id}`) return send(billing(other));
        if (url.pathname === "/admin/billing/users") return send({users: [owner, other].map((user) => ({...user, cash_balance_usd: "128"}))});
        if (url.pathname === "/admin/model-access/users") return send({users: []});
        return send({});
      }
      const body = request.postDataJSON(); writes.push({path: url.pathname, method: request.method(), body, raw: request.postData()});
      if (url.pathname.endsWith("/preferences")) {
        if (url.pathname.includes("/cash/")) cashPreferences = {priority: body.priority, charge_enabled: body.charge_enabled};
        else Object.assign(subscriptions.find((sub) => url.pathname.includes(sub.id)), {priority: body.priority, charge_enabled: body.charge_enabled});
        return send({priority: body.priority, charge_enabled: body.charge_enabled});
      }
      if (url.pathname === "/admin/billing/me/purchases" || url.pathname.endsWith("/renewals")) {
        let result = committed.get(body.operation_id);
        if (!result) {
          const plan = plans.find((plan) => plan.id === body.plan_id) || plans[0];
          const subscription = body.plan_id ? {...subscriptions[0], id: `bought-${committed.size}`, name: plan.name, plan} : subscriptions[0];
          if (body.plan_id) subscriptions.push(subscription);
          else {
            const starts_at = subscription.expires_at;
            subscription.expires_at = new Date(new Date(starts_at).getTime() + plan.period_days * body.period_count * 86400000).toISOString();
            subscription.entitlements.push({starts_at, ends_at: subscription.expires_at, period_days: plan.period_days, allowance_usd: plan.allowance_usd, model_ids: plan.model_ids});
          }
          result = {plan, subscription, total_usd: plan.price_usd, balance_usd: "100"}; committed.set(body.operation_id, result);
        }
        if (loseResponse) { loseResponse = false; return route.abort("failed"); }
        return send(result);
      }
      if (url.pathname.startsWith("/admin/billing/plans")) return send({...plans[0], ...body, id: "plan-one"});
      if (url.pathname.includes("/subscriptions")) return send(subscriptions[0]);
      if (url.pathname === "/admin/api-keys") return send({api_key: "synthetic-key", prefix: "test"});
      return send({ok: true});
    });
    await page.goto("http://127.0.0.1:8765/#billing");
    await page.evaluate(async (value) => { bindUI(); initializeDateFilters(); renderState(value); await Promise.all([loadBillingPlans(), loadBillingDetail(), loadBillingUsers()]); setConnection("已连接", "ok"); }, session);
    const settled = () => page.waitForFunction(() => !billingPlanOperation?.busy && !billingDetailLoading && !billingPlansLoading && !billingSourceOperation);
    const snapshot = async (name) => {
      await page.evaluate(() => hide("notice"));
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "viewport overflow");
      await page.screenshot({path: path.join(screenshots, `${name}.png`), fullPage: false});
    };
    assert.equal(await page.locator(".subscription-card").count(), 2);
    assert.equal(await page.locator('[data-subscription-id="sub-two"] [data-billing-source-state]').textContent(), "已禁用扣费");
    assert.equal(await page.locator('#billing-plans-panel').isVisible(), false);
    await snapshot("subscriptions-desktop");
    await page.locator('[data-billing-priority="cash"]').fill("2");
    await page.locator('[data-billing-priority-save="cash"]').click(); await settled();
    assert.equal(writes.at(-1).path, "/admin/billing/me/cash/preferences"); assert.equal(writes.at(-1).body.priority, 2);
    await page.locator('[data-billing-source="sub-two"]').click(); await settled(); assert.equal(subscriptions[1].charge_enabled, true);

    // Renew an existing binding while its plan is off sale; current benefits stay.
    await page.evaluate(() => { billingDetail.subscriptions[0].plan.active = false; renderBillingSubscriptions(billingDetail); syncBillingSourceControls(); });
    await page.locator('[data-billing-renew="sub-one"]').click();
    assert.match(await page.locator("#billing-purchase-summary").textContent(), /追加权益生效.*2099/);
    assert.match(await page.locator("#billing-purchase-summary").textContent(), /每期额度：US\$100.*保留本周期和剩余额度/);
    await snapshot("renewal-entitlements-desktop");
    await page.locator('[name="confirmed"]').check(); await page.locator("#billing-purchase-submit").click(); await settled();
    assert.equal(writes.at(-1).path, "/admin/billing/me/subscriptions/sub-one/renewals"); assert.equal(writes.at(-1).body.subscription_config_version, 8);
    assert.equal(subscriptions[0].remaining_usd, "61.25");
    await page.evaluate(() => { location.hash = "plans"; }); await page.waitForSelector('[data-section="plans"]:visible'); await settled();
    await snapshot("plans-desktop");
    await page.locator('[data-billing-buy="plan-one"]').click();
    assert.match(await page.locator("#billing-purchase-warning").textContent(), /新增独立订阅/);
    await page.locator('[name="confirmed"]').check(); loseResponse = true;
    await page.locator("#billing-purchase-submit").click(); await settled();
    assert.match(await page.locator("#billing-purchase-form .form-message").textContent(), /原请求和操作 ID 已保留/);
    const original = writes.at(-1).raw;
    await page.locator("#billing-purchase-submit").click(); await settled();
    assert.equal(writes.at(-1).raw, original); assert.equal(Object.hasOwn(writes.at(-1).body, "subscription_config_version"), false);
    assert.equal(subscriptions.length, 5);

    // Catalog metadata, partial checkbox state, keyboard, search and exact ranges.
    await page.locator('[data-billing-plan-edit="plan-one"]').first().click();
    await page.waitForSelector("#billing-plan-models .model-picker-choice");
    assert.match(await page.locator("#billing-plan-edit-warning").textContent(), /保留已有绑定/);
    await page.locator('#billing-plan-models button', {hasText: "清空"}).click();
    await page.locator('#billing-plan-models [value="gpt-example"]').check();
    assert.equal(await page.getByLabel("全选 GPT 模型", {exact: true}).evaluate((input) => input.indeterminate), true);
    await page.getByLabel("全选 Gemini 模型", {exact: true}).check();
    await page.locator('#billing-plan-form [name="tier"]').selectOption("custom");
    await page.locator('#billing-plan-form [name="period_days"]').fill("10");
    await page.locator('#billing-plan-form [name="reason"]').fill("调整后续售卖配置");
    await snapshot("plan-models-desktop");
    await page.locator("#billing-plan-submit").click(); await settled();
    assert.deepEqual(writes.at(-1).body.model_ids, ["gpt-example", "gemini-example"]); assert.equal(writes.at(-1).body.period_days, 10);

    await page.evaluate(() => { location.hash = "billing"; }); await page.waitForSelector('[data-section="billing"]:visible');
    await page.locator("#billing-subscription-create").click(); await page.waitForSelector("#billing-subscription-models .model-picker-choice");
    await page.locator('#billing-subscription-form [name="name"]').fill("补充十日额度");
    await page.locator('#billing-subscription-form [name="tier"]').selectOption("custom");
    await page.locator('#billing-subscription-form [name="period_days"]').fill("10");
    await page.locator('#billing-subscription-form [name="quota_usd"]').fill("50");
    await page.locator('#billing-subscription-form [name="reason"]').fill("补充权益");
    await page.locator('#billing-subscription-models [value="claude-example"]').check();
    await page.locator("#billing-subscription-save").click(); await page.waitForSelector("#billing-subscription-dialog", {state: "hidden"}); await settled();
    assert.equal(writes.at(-1).path, `/admin/billing/users/${owner.id}/subscriptions`); assert.equal(writes.at(-1).method, "POST");
    await page.locator('[data-subscription-manage="sub-one"]').click();
    assert.match(await page.locator("#billing-subscription-replace").textContent(), /当前权益.*全部待生效权益.*解除套餐绑定/);
    await page.waitForSelector("#billing-subscription-models .model-picker-choice");
    await snapshot("admin-reopen-entitlements-desktop");
    await page.locator('#billing-subscription-dialog [data-close]').click();

    // Explicit API Key restriction never silently falls back to unrestricted.
    await page.evaluate(() => openDialog("key-dialog"));
    await page.locator('#key-form [name="name"]').fill("限定模型");
    await page.locator("#key-model-scope").selectOption("restricted"); await page.waitForSelector("#key-model-picker .model-picker-choice");
    const before = writes.length;
    await page.locator('#key-form [type="submit"]').click();
    assert.equal(writes.length, before); assert.match(await page.locator("#key-form .form-message").textContent(), /至少选择一个/);
    await page.locator('#key-model-picker [value="gpt-example"]').check(); await page.locator('#key-form [type="submit"]').click();
    await page.waitForSelector("#secret-dialog:visible");
    assert.equal(writes.at(-1).body.model_scope, "restricted"); assert.deepEqual(writes.at(-1).body.models, ["gpt-example"]);
    await page.evaluate(() => byId("secret-dialog").close());

    // Single mode shares the native radio keyboard contract in usage and identification.
    await page.evaluate(() => { location.hash = "usage"; loadUsageModelPickers(); });
    await page.waitForSelector('#usage-model-picker [value="gpt-example"]:visible');
    await page.locator('#usage-model-picker [value="gpt-example"]').check();
    await page.locator('#usage-model-picker [value="gpt-example"]').press("ArrowDown");
    assert.equal(await page.locator('#usage-filter [name="model"]').inputValue(), "gpt-extra");
    assert.equal(await page.locator('#usage-model-picker input[type="radio"]:checked').count(), 1);
    await page.locator('#usage-filter [name="model"]').fill("retired-history-model");
    assert.equal(await page.evaluate(() => queryFromForm(byId("usage-filter")).get("model")), "retired-history-model");
    await page.evaluate(() => { location.hash = "model-identification"; });
    await page.waitForSelector('#model-identification-account:enabled');
    await page.locator('#model-identification-account').selectOption("account");
    await page.waitForSelector('#identification-model-picker [value="gpt-example"]:visible');
    await page.locator('#identification-model-picker [value="gpt-example"]').check();
    await page.locator('#identification-model-picker [value="gpt-example"]').press("ArrowDown");
    assert.equal(await page.locator('#model-identification-model').inputValue(), "gpt-extra");
    await page.evaluate(() => { location.hash = "billing"; }); await page.waitForSelector('[data-section="billing"]:visible');

    await page.setViewportSize({width: 390, height: 844});
    await page.evaluate(() => { window.scrollTo(0, 0); }); await snapshot("subscriptions-mobile");
    await page.locator('[data-subscription-id="sub-one"]').evaluate((card) => window.scrollTo(0, card.getBoundingClientRect().top + window.scrollY - 85)); await snapshot("subscription-cards-mobile");
    await page.evaluate(() => openBillingPlanEditor(billingPlans[0])); await page.waitForSelector("#billing-plan-models .model-picker-choice");
    await page.locator('#billing-plan-models input[type="search"]').fill("gemini");
    assert.equal(await page.locator('#billing-plan-models .model-picker-choice:visible').count(), 1);
    await snapshot("model-picker-mobile"); await page.locator('#billing-plan-dialog [data-close]').click();
    await page.evaluate(async (user) => { await selectBillingUser(user); }, other);
    assert.equal(await page.locator('[data-billing-source]:visible').count(), 0);
    assert.equal(await page.locator('[data-billing-priority]:enabled').count(), 0);
    assert.equal(await page.locator('[data-billing-renew]:visible').count(), 0);
    assert.deepEqual(errors, []);
    console.log("Multiple subscriptions, preferences, ID renewals, exact retry, custom cycles, shared model picker, API Key restriction and desktop/mobile checks passed.");
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
