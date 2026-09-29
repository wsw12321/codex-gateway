"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));

class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {};
    this.className = ""; this.listeners = {}; this.value = ""; this.disabled = false; this.checked = false;
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      toggle: (name, on) => {
        const names = new Set(this.className.split(/\s+/).filter(Boolean));
        if (on === undefined) on = !names.has(name);
        if (on) names.add(name); else names.delete(name);
        this.className = [...names].join(" ");
      },
      add: (name) => this.classList.toggle(name, true), remove: (name) => this.classList.toggle(name, false),
    };
  }
  set textContent(value) { this.text = String(value); this.children = []; }
  get textContent() { return (this.text || "") + this.children.map((child) => child.textContent).join(" "); }
  append(...children) { for (const child of children) { child.parentElement = this; this.children.push(child); } }
  replaceChildren(...children) { this.text = ""; this.children = []; this.append(...children); }
  setAttribute(name, value) { this.attributes[name] = String(value); if (name === "name") this.name = value; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  showModal() { this.open = true; }
  close() { this.open = false; this.listeners.close?.(); }
  reset() { for (const control of Object.values(this.elements || {})) { control.value = ""; control.checked = false; } }
  matches(selector) {
    if (selector.includes(",")) return selector.split(",").some((item) => this.matches(item.trim()));
    if (selector.startsWith(".")) return this.classList.contains(selector.slice(1));
    if (selector.startsWith("#")) return this.id === selector.slice(1);
    const data = selector.match(/^\[data-([a-z-]+)\]$/);
    if (data) return data[1].replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()) in this.dataset;
    if (selector === "button[type=submit]") return this.tagName === "button" && this.type === "submit";
    return this.tagName === selector;
  }
  querySelectorAll(selector) { return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) { return this.matches(selector) ? this : this.parentElement?.closest(selector) || null; }
}

const plan = {id: "plan-one", name: "日常套餐", tier: "day", price_usd: "0.100000000001", allowance_usd: "20",
  min_period_count: 2, active: true, version: 4};
const subscription = {id: "sub-one", enabled: true, quota_usd: "20", remaining_usd: "7.25", period_count: 99,
  current_period_number: 4, config_version: 8, can_renew: true, plan,
  period_started_at: "2099-01-04T00:00:00Z", period_ends_at: "2099-01-05T00:00:00Z", expires_at: "2099-04-10T00:00:00Z"};

function dashboard(role = "member", sub = subscription) {
  const root = new Element(), nodes = new Map(), writes = [], reads = [], confirmations = [];
  const node = (id, tag = "div") => {
    if (!nodes.has(id)) { const value = new Element(tag); value.id = id; nodes.set(id, value); root.append(value); }
    return nodes.get(id);
  };
  const createForm = (id, names, dialogID) => {
    const form = node(id, "form"); form.elements = {};
    for (const name of names) {
      const input = new Element(["active", "tier"].includes(name) ? "select" : "input");
      input.name = name; form.elements[name] = input; form.append(input);
    }
    const message = new Element("p"); message.className = "form-message"; form.append(message);
    if (dialogID) node(dialogID, "dialog").append(form);
    return form;
  };
  const purchase = createForm("billing-purchase-form", ["period_count", "confirmed"], "billing-purchase-dialog");
  const editor = createForm("billing-plan-form", ["name", "price_usd", "tier", "allowance_usd", "min_period_count", "active", "reason"], "billing-plan-dialog");
  for (const tier of ["day", "week", "month"]) {
    const form = createForm(`billing-subscription-${tier}`, ["quota_usd", "period_count", "reason"]);
    form.className = "billing-subscription-form";
    const disable = new Element("button"); disable.dataset.disableSubscription = tier; form.append(disable);
  }
  const context = vm.createContext({
    initialPlan: structuredClone(plan), initialSub: sub ? structuredClone(sub) : null, role,
    document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    crypto: {randomUUID: (() => { let counter = 0; return () => `operation-${++counter}`; })()}, URLSearchParams,
    window: {confirm: (message) => { confirmations.push(message); return true; }},
    FormData: class { constructor(form) { this.form = form; } get(name) { return this.form.elements[name]?.value ?? null; } },
    request: async (url, options) => {
      if (!options?.method) { reads.push(url); return {plans: [structuredClone(plan)]}; }
      writes.push({url, method: options.method, body: options.body});
      if (context.response) return context.response(url, options);
      return url.includes("/plans") ? structuredClone(plan) : {plan: structuredClone(plan), subscription: structuredClone(subscription), total_usd: "0.200000000002", balance_usd: "12.5", ledger: {}};
    },
  });
  vm.runInContext(application, context, {filename: "assets/app.js"});
  const run = (code) => vm.runInContext(code, context);
  run(`state = {user: {id: "self", role}, recently_verified: true}; billingUserID = "self";
    billingDetail = {user: state.user, cash_balance_usd: "100", subscriptions: initialSub ? {day: initialSub} : {}};
    billingPlans = [initialPlan]; const productionAPI = api; api = request; let refreshes = 0;
    refreshBillingPlanData = async () => { refreshes++; return true; }; notice = () => {};`);
  return {run, context, writes, reads, confirmations, node, purchase, editor,
    controls: (selector) => root.querySelectorAll(selector),
    open: (kind = "purchase") => run(`openBillingPurchase(initialPlan, "${kind}")`),
    confirm: () => { purchase.elements.confirmed.checked = true; },
    message: (form = purchase) => form.querySelector(".form-message").textContent,
    snapshot: (expression) => JSON.parse(run(`JSON.stringify(${expression})`)),
  };
}

test("decimal totals and single transaction counts are exact", () => {
  const page = dashboard();
  assert.equal(page.run('billingPlanTotal("0.100000000001", 99)'), "9.900000000099");
  assert.equal(page.run('billingPlanTotal("999999999999999999.000001", 99)'), "98999999999999999901.000099");
  for (const raw of ["0", "100", "2.5", "1e1", "-1", "01"]) assert.throws(() => page.run(`billingPlanPeriodCount(${JSON.stringify(raw)}, 2)`));
  assert.throws(() => page.run('billingPlanPeriodCount("1", 2)'));
  assert.equal(page.run('billingPlanPeriodCount("99", 2)'), 99);
});

test("cumulative subscriptions render over 99 without truncating the admin reopen input", () => {
  const page = dashboard("owner", {...subscription, period_count: 198});
  assert.equal(page.run('billingPeriodProgress(initialSub)'), "当前第 4/198 个周期");
  page.run("renderBillingAdminValues(billingDetail)");
  const input = page.node("billing-subscription-day").elements.period_count;
  assert.equal(input.value, "");
  assert.match(input.placeholder, /198.*0–99/);
});

test("manual, same-plan and other-plan subscriptions all require overwrite confirmation", async () => {
  for (const existing of [null, plan, {...plan, id: "old-plan", name: "旧套餐"}]) {
    const page = dashboard("member", {...subscription, plan: existing, can_renew: Boolean(existing)});
    page.open();
    assert.match(page.node("billing-purchase-warning").textContent, /覆盖.*剩余额度.*不结转、不自动退款/);
    assert.match(page.node("billing-purchase-summary").textContent, /第 1 期.*补满 US\$20\.00/);
    await assert.rejects(page.run("submitBillingPurchase()"), /先核对/);
    assert.equal(page.writes.length, 0);
    page.confirm(); await page.run("submitBillingPurchase()");
    const body = JSON.parse(page.writes[0].body);
    assert.deepEqual(body, {plan_id: plan.id, plan_version: 4, subscription_config_version: 8, period_count: 2, operation_id: "operation-1"});
    assert.equal(page.run("refreshes"), 1);
  }
});

test("a new subscription captures config version zero and first period benefits", async () => {
  const page = dashboard("member", null);
  page.open(); page.confirm(); await page.run("submitBillingPurchase()");
  assert.equal(JSON.parse(page.writes[0].body).subscription_config_version, 0);
  assert.match(page.node("billing-purchase-warning").textContent, /立即开始/);
});

test("renewal confirms added periods and extended expiry while preserving current quota", async () => {
  const page = dashboard(); page.open("renewal");
  const summary = page.node("billing-purchase-summary").textContent;
  assert.match(summary, /追加 2 期/); assert.match(summary, /剩余额度 US\$7\.25/);
  assert.match(summary, /保留本周期和剩余额度/);
  assert.equal(page.run('billingRenewalExpiry(initialSub, "day", 2)'), "2099-04-12T00:00:00.000Z");
  assert.equal(page.run('billingRenewalExpiry(initialSub, "month", 2)'), "2099-06-11T00:00:00.000Z");
  page.confirm(); await page.run("submitBillingPurchase()");
  assert.equal(page.writes[0].url, "/admin/billing/me/subscriptions/day/renewals");
  assert.equal(JSON.parse(page.writes[0].body).plan_id, undefined);
  assert.equal(page.snapshot("billingDetail.subscriptions.day").remaining_usd, "7.25");
});

test("unbound or ineligible subscriptions cannot open renewal", () => {
  for (const sub of [{...subscription, plan: null, can_renew: false}, {...subscription, can_renew: false}]) {
    const page = dashboard("member", sub);
    page.open("renewal");
    assert.equal(page.node("billing-purchase-dialog").open, undefined);
    page.run("renderBillingSubscriptions(billingDetail)");
    assert.equal(page.controls("[data-billing-renew]").length, 0);
    if (!sub.plan) assert.match(page.node("billing-subscriptions").textContent, /未绑定套餐，无法续费/);
  }
});

test("unknown response locks the exact original payload until retry succeeds", async () => {
  const page = dashboard(); page.open(); page.confirm();
  page.context.response = async () => { throw Object.assign(new Error("lost response"), {network: true}); };
  await page.run("submitBillingPurchase()");
  const original = page.writes[0].body;
  assert.equal(page.run("billingPlanOperation.uncertain"), true);
  assert.equal(page.purchase.elements.period_count.disabled, true);
  assert.equal(page.node("billing-purchase-submit").textContent, "重试原操作");
  assert.match(page.message(), /原请求和操作 ID 已保留/);
  page.node("billing-purchase-dialog").close();
  page.run("initialPlan.version = 9; initialPlan.price_usd = '999'; openBillingPurchase(initialPlan)");
  assert.equal(page.node("billing-purchase-dialog").open, false);
  page.run("resumeBillingPlanOperation()");
  assert.equal(page.node("billing-purchase-dialog").open, true);
  page.purchase.elements.period_count.value = "99";
  page.context.response = null;
  await page.run("submitBillingPurchase()");
  assert.equal(page.writes[1].body, original);
  assert.equal(page.run("billingPlanOperation"), null);
});

test("unknown retry retains the operation after later permission failures or malformed success", async () => {
  const page = dashboard(); page.open(); page.confirm();
  page.context.response = async () => ({});
  await page.run("submitBillingPurchase()");
  assert.equal(page.run("billingPlanOperation.uncertain"), true);
  page.context.response = async () => { throw Object.assign(new Error("denied"), {status: 403}); };
  await page.run("submitBillingPurchase()");
  assert.equal(page.writes[0].body, page.writes[1].body);
  assert.equal(page.run("billingPlanOperation.uncertain"), true);
});

test("stale confirmation refreshes, closes the dialog and requires a new confirmation", async () => {
  const page = dashboard(); page.open(); page.confirm();
  // Background refresh must not silently rebase the version already confirmed.
  page.run("billingDetail.subscriptions.day.config_version = 9");
  page.context.response = async () => { throw Object.assign(new Error("changed"), {status: 409}); };
  await page.run("submitBillingPurchase()");
  assert.equal(JSON.parse(page.writes[0].body).subscription_config_version, 8);
  assert.equal(page.run("billingPlanOperation"), null);
  assert.equal(page.node("billing-purchase-dialog").open, false);
  assert.equal(page.run("refreshes"), 1);
  assert.match(page.node("billing-plan-message").textContent, /重新选择并确认/);
  page.context.response = null; page.open();
  assert.equal(page.purchase.elements.confirmed.checked, false);
  page.confirm(); await page.run("submitBillingPurchase()");
  assert.equal(JSON.parse(page.writes[1].body).subscription_config_version, 9);
  assert.notEqual(JSON.parse(page.writes[0].body).operation_id, JSON.parse(page.writes[1].body).operation_id);
});

test("insufficient balance is a definitive failure with no success refresh", async () => {
  const page = dashboard(); page.open(); page.confirm();
  page.context.response = async () => { throw Object.assign(new Error("余额不足"), {status: 402}); };
  await page.run("submitBillingPurchase()");
  assert.equal(page.run("billingPlanOperation"), null);
  assert.equal(page.run("refreshes"), 0);
  assert.match(page.message(), /余额不足/);
});

test("owner viewing another user has no purchase or renewal actions", () => {
  const page = dashboard("owner");
  page.run("billingUserID = 'other'; billingDetail.user = {id: 'other'}; renderBillingPlans(); renderBillingSubscriptions(billingDetail)");
  assert.equal(page.controls("[data-billing-buy]").length, 0);
  assert.equal(page.controls("[data-billing-renew]").length, 0);
  assert.ok(page.controls("[data-billing-plan-edit]").length > 0);
  assert.match(page.node("billing-subscriptions").textContent, /套餐绑定有效，仅本人可续费/);
  assert.doesNotMatch(page.node("billing-subscriptions").textContent, /当前订阅不能续费/);
  page.open(); assert.equal(page.run("billingPurchaseDraft"), null);
});

test("switching account scope during recent identity verification prevents the write", async () => {
  const page = dashboard("owner"); page.open(); page.confirm();
  page.run("sensitiveAction = async (operation) => { billingUserID = 'other'; return operation(); }");
  await page.run("submitBillingPurchase()");
  assert.equal(page.writes.length, 0);
  assert.equal(page.run("billingPlanOperation"), null);
  assert.match(page.message(), /查看的用户已变化/);
});

test("a late unauthorized purchase response cannot log out a newer identity", async () => {
  const page = dashboard(); page.open(); page.confirm();
  let finishResponse, fetches = 0;
  page.context.fetch = async () => {
    fetches++;
    return new Promise((resolve) => { finishResponse = resolve; });
  };
  page.run("api = productionAPI; let unauthorized = 0; handleUnauthorized = () => { unauthorized++; };");
  const pending = page.run("submitBillingPurchase()");
  assert.equal(fetches, 1);
  page.run("identityGeneration++; resetBillingPlans(); state = {user: {id: 'new-user', role: 'member'}, recently_verified: true};");
  finishResponse({status: 401, ok: false, json: async () => ({error: {code: "invalid_session"}})});
  await pending;
  assert.equal(page.run("unauthorized"), 0);
  assert.equal(page.run("state.user.id"), "new-user");
  assert.equal(page.run("state.recently_verified"), true);
  assert.equal(page.run("billingPlanOperation"), null);
  assert.equal(page.run("refreshes"), 0);
});

test("a late verification response cannot reauthenticate after leaving the account scope", async () => {
  const page = dashboard("owner"); page.open(); page.confirm();
  let finishResponse;
  page.context.fetch = async () => new Promise((resolve) => { finishResponse = resolve; });
  page.run("api = productionAPI; let reauths = 0; reauthenticate = async () => { reauths++; };");
  const pending = page.run("submitBillingPurchase()");
  page.run("billingUserID = 'other';");
  finishResponse({status: 403, ok: false, json: async () => ({error: {code: "recent_identity_verification_required"}})});
  await pending;
  assert.equal(page.run("reauths"), 0);
  assert.equal(page.run("state.recently_verified"), true);
  assert.equal(page.run("refreshes"), 0);
  assert.equal(page.run("billingPlanOperation.uncertain"), true);
});

test("Owner creates inactive plans with default minimum and edits with a version and warning", async () => {
  const page = dashboard("owner");
  page.run("openBillingPlanEditor()");
  assert.equal(page.editor.elements.min_period_count.value, "1");
  assert.equal(page.editor.elements.active.value, "false");
  Object.assign(page.editor.elements.name, {value: "新套餐"});
  page.editor.elements.price_usd.value = "1.01"; page.editor.elements.allowance_usd.value = "10";
  page.editor.elements.reason.value = "试运行";
  await page.run("submitBillingPlan()");
  assert.equal(JSON.parse(page.writes[0].body).active, false);
  assert.equal(JSON.parse(page.writes[0].body).min_period_count, 1);
  assert.equal(page.writes[0].method, "POST");
  page.run("openBillingPlanEditor(initialPlan)");
  page.editor.elements.active.value = "false"; page.editor.elements.reason.value = "下架";
  await page.run("submitBillingPlan()");
  assert.equal(page.writes[1].method, "PUT");
  assert.equal(JSON.parse(page.writes[1].body).version, 4);
  assert.match(page.confirmations[0], /解除已有订阅的套餐绑定，已购权益保持有效/);
});

test("uncertain Owner edits retry the original frozen fields", async () => {
  const page = dashboard("owner"); page.run("openBillingPlanEditor(initialPlan)");
  page.editor.elements.reason.value = "更新价格";
  page.context.response = async () => { throw Object.assign(new Error("timeout"), {status: 503}); };
  await page.run("submitBillingPlan()");
  assert.equal(page.editor.elements.price_usd.disabled, true);
  page.editor.elements.price_usd.value = "999";
  page.context.response = null; await page.run("submitBillingPlan()");
  assert.equal(page.writes[0].body, page.writes[1].body);
  assert.equal(page.confirmations.length, 1);
});

test("catalog only requests inactive plans for Owner and resets private state on logout", async () => {
  for (const role of ["member", "owner"]) {
    const page = dashboard(role); await page.run("loadBillingPlans()");
    assert.equal(page.reads[0], `/admin/billing/plans${role === "owner" ? "?include_inactive=true" : ""}`);
    page.open(); page.confirm();
    page.context.response = async () => { throw new Error("unknown"); };
    await page.run("submitBillingPurchase()");
    page.run("resetBillingPlans()");
    assert.equal(page.run("billingPlanOperation"), null);
    assert.equal(page.run("billingPurchaseDraft"), null);
    assert.equal(page.node("billing-purchase-dialog").open, false);
  }
});

test("refresh after a completed transaction includes balances, subscriptions, ledger and catalog", async () => {
  const page = dashboard("owner");
  // Restore the production refresher independently of the transaction fixture.
  const start = application.indexOf("async function refreshBillingPlanData()");
  const end = application.indexOf("\nasync function executeBillingPlanOperation", start);
  page.run(application.slice(start, end));
  page.run("let refreshedParts = []; loadBillingPlans = async () => refreshedParts.push('plans'); loadBillingDetail = async (id, offset) => refreshedParts.push(`detail:${id}:${offset}`); loadBillingUsers = async () => refreshedParts.push('users')");
  assert.equal(await page.run("refreshBillingPlanData()"), true);
  assert.deepEqual(page.snapshot("refreshedParts"), ["plans", "detail:self:0", "users"]);
});
