"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {}; this.className = "";
    this.value = ""; this.disabled = false; this.listeners = {}; this.text = ""; this.title = ""; this.href = "";
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => this.classList.toggle(name, true), remove: (name) => this.classList.toggle(name, false),
      toggle: (name, active) => { const set = new Set(this.className.split(/\s+/).filter(Boolean)); if (active) set.add(name); else set.delete(name); this.className = [...set].join(" "); },
    };
  }
  get textContent() { return this.text + this.children.map((child) => child.textContent).join(""); }
  set textContent(value) { this.text = value; this.children = []; }
  append(...children) { children.filter(Boolean).forEach((child) => { if (typeof child === "string") { const text = new Element("#text"); text.text = child; child = text; } child.parent = this; this.children.push(child); }); }
  replaceChildren(...children) { this.text = ""; this.children = []; this.append(...children); }
  setAttribute(key, value) { this.attributes[key] = value; }
  getAttribute(key) { return this.attributes[key]; }
  removeAttribute(key) { delete this.attributes[key]; }
  addEventListener(name, listener) { this.listeners[name] = listener; }
  matches(selector) { return selector.startsWith(".") ? this.classList.contains(selector.slice(1)) : this.tagName === selector; }
  querySelectorAll(selector) { return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest() { return null; }
  reset() {}
  showModal() { this.open = true; }
  close() { this.open = false; }
}

function dashboard(role = "member", marketplace = {enabled: true, max_accounts_per_user: 2, max_egress_servers_per_user: 1}) {
  const nodes = new Map(), root = new Element();
  const node = (id) => { if (!nodes.has(id)) { const value = new Element(); nodes.set(id, value); root.append(value); } return nodes.get(id); };
  const opened = [];
  let operationCount = 0;
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    location: {hash: "#billing"}, URLSearchParams, URL, TextEncoder, DOMException, AbortController,
    window: {setTimeout: () => 0, clearTimeout: () => {}, open: (...args) => opened.push(args), confirm: () => true},
    crypto: {randomUUID: () => `op-${++operationCount}`},
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  context.initialState = {user: {id: role === "owner" ? "owner-id" : "member-id", role}, recently_verified: true, marketplace};
  run("state = initialState; notice = () => {};");
  const calls = [];
  const responses = [];
  context.respond = async (requestPath, options = {}) => {
    calls.push({path: requestPath, method: options.method || "GET", body: options.body ? JSON.parse(options.body) : null});
    const next = responses.shift();
    if (!next) throw new Error(`unexpected ${requestPath}`);
    if (next instanceof Error) throw next;
    return typeof next === "function" ? next(requestPath, options) : next;
  };
  run("api = respond;");
  const form = (id, values) => {
    const host = node(id);
    host.elements = Object.fromEntries(Object.entries(values).map(([key, value]) => [key, {value}]));
    return host;
  };
  return {run, node, calls, responses, opened, form, context};
}



const sellerPlan = {id: "s1", name: "Alice Pro", platform: false, seller_name: "Alice", description: "官方账号", price_usd: "2", tier: "day",
  period_days: 1, allowance_usd: "10", min_period_count: 1, active: true, version: 3, model_ids: ["gpt-test"],
  providers: [{provider: "codex", official: 2, third_party: 1}], third_party_api: true, own: false};
const platformPlan = {id: "p1", name: "平台月卡", platform: true, price_usd: "5", tier: "month", period_days: 31, allowance_usd: "50",
  min_period_count: 1, active: true, version: 1, model_ids: ["gpt-test"], providers: [], third_party_api: false, own: false};

test("the catalog merges marketplace plans by role", () => {
  const ui = dashboard("member");
  const merged = (platform, market, role) => JSON.parse(ui.run(`JSON.stringify(mergeMarketPlans(${JSON.stringify(platform)}, ${JSON.stringify(market)}, "${role}"))`));
  assert.deepEqual(merged([platformPlan], null, "member").map((plan) => plan.id), ["p1"]);
  assert.deepEqual(merged([platformPlan], [platformPlan, sellerPlan], "member").map((plan) => plan.id), ["p1", "s1"]);
  assert.deepEqual(merged([{...platformPlan, id: "p0", active: false}], [platformPlan, sellerPlan], "owner").map((plan) => plan.id), ["p0", "s1"]);
});

test("texts describe providers, review states and sales", () => {
  const ui = dashboard("member");
  assert.equal(ui.run(`marketPlanProviderText(${JSON.stringify(sellerPlan)})`), "供应商：Codex：官方账号 2、第三方 API 1");
  assert.equal(ui.run("marketPlanProviderText({providers: []})"), "供应商：当前没有可用账号");
  assert.equal(ui.run('sellerPlanStatusText({review_status: "draft"})'), "草稿");
  assert.equal(ui.run('sellerPlanStatusText({review_status: "pending"})'), "待审核");
  assert.equal(ui.run('sellerPlanStatusText({review_status: "approved", on_sale: true, active: true})'), "销售中");
  assert.equal(ui.run('sellerPlanStatusText({review_status: "approved", on_sale: false, active: false})'), "已通过 · 未上架");
  assert.equal(ui.run('sellerPlanStatusText({review_status: "approved", admin_suspended: true})'), "已被 Owner 下架");
  assert.equal(ui.run('marketSalesSummaryText({count: 0})'), "还没有成交记录。");
  assert.equal(ui.run('marketSalesSummaryText({count: 2, buyers: 1, gross_usd: "4", commission_usd: "0.4", net_usd: "3.6"})'),
    "共 2 笔 · 1 位买家 · 成交 US$4.00 · 抽成 US$0.40 · 到账 US$3.60");
  assert.equal(ui.run('formatCommissionRate("0.125")'), "12.5%");
});

test("members load the marketplace and see seller plans with their warnings", async () => {
  const ui = dashboard("member");
  ui.responses.push({plans: [platformPlan]}, {plans: [platformPlan, sellerPlan, {...sellerPlan, id: "s2", own: true, third_party_api: false}]});
  await ui.run("loadBillingPlans()");
  assert.deepEqual(ui.calls.map((call) => call.path), ["/admin/billing/plans", "/admin/market/plans"]);
  const host = ui.node("billing-plans");
  assert.equal(host.children.length, 3);
  const text = host.textContent;
  assert.match(text, /卖家：Alice/);
  assert.match(text, /含第三方 API 供应商/);
  assert.match(text, /这是你发布的套餐/);
  const buyButtons = host.querySelectorAll("button").filter((button) => button.dataset.billingBuy);
  assert.deepEqual(buyButtons.map((button) => button.dataset.billingBuy), ["p1", "s1"]);
  // With the marketplace off the platform list stays the catalog.
  const off = dashboard("member", {enabled: false});
  off.responses.push({plans: [platformPlan]});
  await off.run("loadBillingPlans()");
  assert.deepEqual(off.calls.map((call) => call.path), ["/admin/billing/plans"]);
});

test("a third-party plan needs the buyer's consent", async () => {
  const ui = dashboard("member");
  const form = ui.node("billing-purchase-form");
  form.elements = {confirmed: {checked: true}, acknowledge_third_party: {checked: false}, period_count: {value: "1"}};
  ui.run(`billingPurchaseDraft = {kind: "purchase", plan: ${JSON.stringify(sellerPlan)}, userID: "member-id", generation: identityGeneration, thirdParty: true};`);
  ui.run('billingDetail = {user: {id: "member-id"}}; billingDetailLoading = false;');
  await assert.rejects(ui.run("submitBillingPurchase()"), /第三方 API 供应商/);
  form.elements.acknowledge_third_party.checked = true;
  ui.run("refreshBillingPlanData = async () => true;");
  ui.responses.push({plan: sellerPlan, subscription: {}, total_usd: "2", balance_usd: "8"});
  await ui.run("submitBillingPurchase()");
  assert.equal(ui.calls[0].path, "/admin/billing/me/purchases");
  assert.equal(ui.calls[0].body.acknowledge_third_party, true);
  assert.equal(ui.calls[0].body.plan_id, "s1");
});

const myPlans = {
  plans: [
    {...sellerPlan, id: "m1", name: "草稿", review_status: "draft", on_sale: false, active: true, admin_suspended: false, subscribers: 0},
    {...sellerPlan, id: "m2", name: "在售", review_status: "approved", on_sale: true, active: true, admin_suspended: false, subscribers: 2},
    {...sellerPlan, id: "m3", name: "被驳回", review_status: "rejected", review_note: "价格不清楚", on_sale: false, active: false, admin_suspended: false, subscribers: 0},
  ],
  scopes: {m2: {mode: "selected", revision: 1, accounts: [{id: "a000000000000001", provider: "codex", label: "主号", usable: true}]}},
  candidates: [{id: "a000000000000001", provider: "codex", label: "主号", usable: true}],
};
const mySalesResponse = {sales: [{id: 1, kind: "purchase", plan_name: "在售", buyer_number: 1, period_count: 1, gross_usd: "2",
  commission_rate: "0.2", commission_usd: "0.4", net_usd: "1.6", created_at: "2026-11-24T10:00:00Z"}],
  summary: {count: 1, buyers: 1, gross_usd: "2", commission_usd: "0.4", net_usd: "1.6"}, total: 1};

test("members manage their plans and read anonymous sales", async () => {
  const ui = dashboard("member", {enabled: true, max_plans_per_user: 3});
  ui.responses.push(myPlans, mySalesResponse);
  await ui.run("loadMySellerPlans()");
  assert.deepEqual(ui.calls.map((call) => call.path), ["/admin/me/plans", "/admin/me/sales?limit=50"]);
  assert.match(ui.node("my-plans-summary").textContent, /已发布 3 \/ 3 个套餐 · 在售 1 个/);
  assert.equal(ui.node("my-plan-create").disabled, true);
  const cards = ui.node("my-plans-list").children;
  const labels = cards.map((card) => card.querySelectorAll("button").map((button) => button.textContent));
  assert.ok(labels[0].includes("提交审核"));
  assert.ok(!labels[1].includes("提交审核") && labels[1].includes("下架"));
  assert.ok(labels[2].includes("提交审核") && labels[2].includes("上架"));
  assert.match(cards[2].textContent, /驳回原因：价格不清楚/);
  assert.match(cards[1].textContent, /供应商：指定 1 个账号/);
  assert.match(ui.node("my-sales-body").textContent, /买家 #1/);
  assert.match(ui.node("my-sales-summary").textContent, /到账 US\$1\.60/);
  // The Owner never loads seller routes.
  const owner = dashboard("owner");
  await owner.run("loadMySellerPlans()");
  assert.equal(owner.calls.length, 0);
});

test("the seller editor validates and keeps an unconfirmed operation for retry", async () => {
  const ui = dashboard("member", {enabled: true, max_plans_per_user: 3});
  ui.responses.push(myPlans, mySalesResponse);
  await ui.run("loadMySellerPlans()");
  const values = {name: "新套餐", description: "说明", price_usd: "3", tier: "day", period_days: "1", allowance_usd: "20", min_period_count: "1", active: "true"};
  ui.form("seller-plan-form", values);
  ui.node("seller-plan-models").modelPicker = {values: () => ["gpt-test"]};
  const payload = JSON.parse(ui.run('JSON.stringify(sellerPlanPayload(byId("seller-plan-form"), ["gpt-test"]))'));
  assert.deepEqual(payload, {name: "新套餐", description: "说明", price_usd: "3", tier: "day", period_days: 1, allowance_usd: "20",
    model_ids: ["gpt-test"], min_period_count: 1, active: true});
  ui.form("seller-plan-form", {...values, name: " "});
  assert.throws(() => ui.run('sellerPlanPayload(byId("seller-plan-form"), ["gpt-test"])'), /套餐名称/);
  ui.form("seller-plan-form", values);
  const lost = new Error("network"); lost.network = true;
  ui.responses.push(lost);
  await assert.rejects(ui.run("submitSellerPlan()"), /结果尚未确认/);
  const first = ui.calls.at(-1);
  assert.equal(first.path, "/admin/me/plans");
  assert.equal(first.method, "POST");
  ui.responses.push({id: "m4"}, myPlans, mySalesResponse);
  await ui.run("submitSellerPlan()");
  const retry = ui.calls.at(-3);
  assert.deepEqual(retry.body, first.body);
  assert.equal(ui.run("sellerPlanOperation"), null);
  assert.match(ui.node("my-plans-message").textContent, /草稿/);
});

test("listing, submission and usage call the seller routes", async () => {
  const ui = dashboard("member", {enabled: true, max_plans_per_user: 3});
  ui.run("loadMySellerPlans = async () => {};");
  ui.responses.push({id: "m1"});
  await ui.run(`submitMySellerPlan(${JSON.stringify(myPlans.plans[0])})`);
  assert.deepEqual(ui.calls[0], {path: "/admin/me/plans/m1/submit", method: "POST", body: {version: 3}});
  ui.responses.push({id: "m2"});
  await ui.run(`setMySellerPlanListing(${JSON.stringify(myPlans.plans[1])}, false)`);
  assert.equal(ui.calls[1].path, "/admin/me/plans/m2/listing");
  assert.equal(ui.calls[1].body.active, false);
  assert.equal(ui.calls[1].body.version, 3);
  ui.responses.push({plan_id: "m2", subscribers: 2, consumed_usd: "5", accounts: [{account_id: "a000000000000001", label: "主号", provider: "codex", requests: 3, input_tokens: 10, output_tokens: 20, charged_usd: "5"}]});
  await ui.run(`showMySellerPlanUsage(${JSON.stringify(myPlans.plans[1])})`);
  assert.equal(ui.node("my-plan-usage").classList.contains("hidden"), false);
  assert.match(ui.node("my-plan-usage-body").textContent, /主号/);
  assert.doesNotMatch(ui.node("my-plan-usage-summary").textContent, /member-id/);
});

const reviews = {pending: [{...sellerPlan, id: "r1", review_status: "pending", submitted_at: "2026-11-24T09:00:00Z", subscribers: 0}],
  plans: [{...sellerPlan, id: "r1", review_status: "pending", subscribers: 0}, {...sellerPlan, id: "r2", review_status: "approved", on_sale: true, admin_suspended: false, subscribers: 4}]};
const allSales = {sales: [{...mySalesResponse.sales[0], seller_name: "Alice", buyer_name: "Bob"}], summary: mySalesResponse.summary, total: 1};

test("the Owner reviews plans, takes them down and reads every sale", async () => {
  const ui = dashboard("owner", {enabled: true, pending_reviews: 1});
  ui.run("renderMarketReviewBadge()");
  assert.equal(ui.node("market-review-badge").textContent, "1");
  ui.form("market-settings-form", {enabled: {}, commission_rate: {}, max_accounts_per_user: {}, max_plans_per_user: {}, max_egress_servers_per_user: {}, allow_member_suppliers: {}});
  ui.node("market-settings-form").elements = {enabled: {checked: false}, commission_rate: {value: ""}, max_accounts_per_user: {value: ""},
    max_plans_per_user: {value: ""}, max_egress_servers_per_user: {value: ""}, allow_member_suppliers: {checked: false}};
  ui.responses.push({enabled: true, commission_rate: "0.2", max_accounts_per_user: 4, max_plans_per_user: 2, max_egress_servers_per_user: 1,
    allow_member_suppliers: false, revision: 7}, reviews, allSales);
  await ui.run("loadMarketAdmin()");
  assert.deepEqual(ui.calls.map((call) => call.path), ["/admin/market/settings", "/admin/market/reviews", "/admin/market/sales?limit=50"]);
  assert.equal(ui.node("market-settings-form").elements.commission_rate.value, "0.2");
  assert.equal(ui.node("market-operations").classList.contains("hidden"), false);
  assert.match(ui.node("market-reviews-summary").textContent, /1 个套餐待审核/);
  assert.match(ui.node("market-sales-body").textContent, /Bob/);
  await assert.rejects(ui.run(`reviewMarketPlan(${JSON.stringify(reviews.pending[0])}, false, " ")`), /驳回时请填写原因/);
  ui.run("loadMarketAdmin = async () => {}; refreshState = async () => {};");
  ui.responses.push({id: "r1"});
  await ui.run(`reviewMarketPlan(${JSON.stringify(reviews.pending[0])}, true, "")`);
  assert.deepEqual(ui.calls.at(-1), {path: "/admin/market/reviews/r1", method: "POST", body: {decision: "approve", note: "", version: 3}});
  await assert.rejects(ui.run(`suspendMarketPlan(${JSON.stringify(reviews.plans[1])}, true, "")`), /请填写原因/);
  ui.responses.push({id: "r2"});
  await ui.run(`suspendMarketPlan(${JSON.stringify(reviews.plans[1])}, true, "描述不符")`);
  assert.deepEqual(ui.calls.at(-1), {path: "/admin/market/plans/r2/suspend", method: "PUT", body: {suspended: true, reason: "描述不符"}});
});

test("market settings are validated before saving", () => {
  const ui = dashboard("owner", {enabled: false});
  const form = ui.node("market-settings-form");
  form.elements = {enabled: {checked: true}, commission_rate: {value: "0.15"}, max_accounts_per_user: {value: "5"},
    max_plans_per_user: {value: "3"}, max_egress_servers_per_user: {value: "2"}, allow_member_suppliers: {checked: true}};
  assert.deepEqual(JSON.parse(ui.run('JSON.stringify(marketSettingsPayload(byId("market-settings-form"), 4))')),
    {enabled: true, commission_rate: "0.15", max_accounts_per_user: 5, max_plans_per_user: 3, max_egress_servers_per_user: 2,
      allow_member_suppliers: true, revision: 4});
  form.elements.commission_rate.value = "1.5";
  assert.throws(() => ui.run('marketSettingsPayload(byId("market-settings-form"), 4)'), /抽成比例/);
  form.elements.commission_rate.value = "0.1";
  form.elements.max_plans_per_user.value = "101";
  assert.throws(() => ui.run('marketSettingsPayload(byId("market-settings-form"), 4)'), /上限/);
  // A member's badge never shows.
  const member = dashboard("member", {enabled: true, pending_reviews: 3});
  member.run("renderMarketReviewBadge()");
  assert.equal(member.node("market-review-badge").classList.contains("hidden"), true);
});
