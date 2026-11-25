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
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    location: {hash: "#billing"}, URLSearchParams, URL, TextEncoder, DOMException, AbortController,
    window: {setTimeout: () => 0, clearTimeout: () => {}, open: (...args) => opened.push(args)},
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


const scopes = {
  scopes: {
    p1: {plan_id: "p1", mode: "selected", revision: 3, accounts: [
      {id: "c000000000000003", provider: "codex", label: "主力", usable: true},
      {id: "e000000000000005", provider: "codex", label: "旧号", usable: false}]},
    p2: {plan_id: "p2", mode: "all", revision: 0, accounts: []},
  },
  candidates: [
    {id: "c000000000000003", provider: "codex", label: "主力", usable: true},
    {id: "d000000000000004", provider: "anthropic", label: "Claude 号", usable: false},
  ],
};

test("scope texts describe plans and subscriptions", () => {
  const ui = dashboard("owner");
  assert.equal(ui.run(`billingPlanScopeText(${JSON.stringify(scopes.scopes.p1)})`), "供应商：指定 1 个账号，1 个已失效");
  assert.equal(ui.run(`billingPlanScopeText(${JSON.stringify(scopes.scopes.p2)})`), "供应商：不限制（全部平台账号）");
  assert.equal(ui.run("billingPlanScopeText(undefined)"), "供应商：不限制（全部平台账号）");
  assert.equal(ui.run('billingSubscriptionScopeText({restricted: false, mode: "all"})'), "供应商范围：不限制");
  assert.equal(ui.run('billingSubscriptionScopeText({restricted: true, mode: "selected", account_counts: {codex: 2, anthropic: 1}})'),
    "供应商范围：指定账号（当前可用 Codex 2 个、Claude 1 个）");
  assert.equal(ui.run('billingSubscriptionScopeText({restricted: true, seller: true, account_counts: {}})'), "供应商范围：卖家的账号（当前没有可用账号）");
});

test("the Owner loads scopes only while the marketplace is enabled", async () => {
  const owner = dashboard("owner");
  owner.responses.push(scopes);
  await owner.run("loadBillingPlanScopes(() => true)");
  assert.equal(owner.calls[0].path, "/admin/billing/plan-provider-scopes");
  assert.equal(owner.run("billingPlanScopes.p1.revision"), 3);
  const closed = dashboard("owner", {enabled: false});
  await closed.run("loadBillingPlanScopes(() => true)");
  assert.equal(closed.calls.length, 0);
  assert.equal(closed.run("billingPlanScopes"), null);
  const member = dashboard("member");
  await member.run("loadBillingPlanScopes(() => true)");
  assert.equal(member.calls.length, 0);
});

test("the scope editor lists platform accounts and saves the checked ones", async () => {
  const ui = dashboard("owner");
  ui.responses.push(scopes);
  await ui.run("loadBillingPlanScopes(() => true)");
  ui.run("renderBillingPlans = () => {};");
  const form = ui.node("billing-plan-scope-form");
  form.elements = {mode: {value: "all", addEventListener() {}}};
  ui.run('openBillingPlanScope({id: "p1", name: "月度", version: 1})');
  assert.equal(form.elements.mode.value, "selected");
  assert.equal(ui.node("billing-plan-scope-dialog").open, true);
  const boxes = ui.node("billing-plan-scope-accounts").querySelectorAll("input");
  assert.equal(boxes.length, 2);
  assert.deepEqual(boxes.map((box) => [box.value, box.checked, box.disabled]),
    [["c000000000000003", true, false], ["d000000000000004", false, false]]);
  assert.match(ui.node("billing-plan-scope-accounts").textContent, /Claude 号（已停用）/);
  assert.match(ui.node("billing-plan-scope-stale").textContent, /旧号/);
  // Checking the suspended Claude account and saving sends both, with the
  // revision the editor opened.
  boxes[1].checked = true;
  ui.responses.push({plan_id: "p1", mode: "selected", revision: 4, accounts: []});
  await ui.run("submitBillingPlanScope()");
  assert.deepEqual(ui.calls[1], {path: "/admin/billing/plans/p1/provider-scope", method: "PUT",
    body: {mode: "selected", account_ids: ["c000000000000003", "d000000000000004"], revision: 3}});
  assert.equal(ui.run("billingPlanScopes.p1.revision"), 4);
  assert.equal(ui.node("billing-plan-scope-dialog").open, false);
});

test("an unrestricted scope sends no accounts, and an empty selection is refused", () => {
  const ui = dashboard("owner");
  const form = ui.node("billing-plan-scope-form");
  form.elements = {mode: {value: "all"}};
  assert.deepEqual(JSON.parse(ui.run('JSON.stringify(billingPlanScopePayload(byId("billing-plan-scope-form"), {revision: 2}))')),
    {mode: "all", account_ids: [], revision: 2});
  form.elements.mode.value = "selected";
  assert.throws(() => ui.run('billingPlanScopePayload(byId("billing-plan-scope-form"), {revision: 2})'), /至少选择一个账号/);
});

test("members switch self-use on their own billing page", async () => {
  const ui = dashboard("member");
  ui.run('billingDetail = {user: {id: "member-id"}, self_use_enabled: true};');
  ui.run("renderBillingSelfUse(billingDetail)");
  assert.equal(ui.node("billing-self-use").classList.contains("hidden"), false);
  assert.equal(ui.node("billing-self-use-toggle").checked, true);
  ui.responses.push({self_use_enabled: false});
  await ui.run("setBillingSelfUse(false)");
  assert.deepEqual(ui.calls[0], {path: "/admin/billing/me/self-use", method: "PUT", body: {enabled: false}});
  assert.equal(ui.node("billing-self-use-toggle").checked, false);
  assert.match(ui.node("billing-self-use-message").textContent, /已关闭自用/);
  // Without the field (marketplace off) or for the Owner the card stays hidden.
  ui.run('renderBillingSelfUse({user: {id: "member-id"}})');
  assert.equal(ui.node("billing-self-use").classList.contains("hidden"), true);
  // Another user's billing page never shows the switch.
  ui.run('renderBillingSelfUse({user: {id: "someone-else"}, self_use_enabled: true})');
  assert.equal(ui.node("billing-self-use").classList.contains("hidden"), true);
  const owner = dashboard("owner");
  owner.run('renderBillingSelfUse({user: {id: "owner-id"}, self_use_enabled: true})');
  assert.equal(owner.node("billing-self-use").classList.contains("hidden"), true);
});

test("buyers give and withdraw third-party consent on their seller subscriptions", async () => {
  const ui = dashboard("member");
  const subscription = (scope) => ({id: "sub-1", provider_scope: {plan_id: "p9", seller: true, restricted: true, account_counts: {codex: 1}, ...scope}});
  ui.run('billingDetail = {user: {id: "member-id"}};');
  const text = (node) => node.textContent;
  const block = (value) => { ui.context.subject = value; return ui.run("billingThirdPartyConsentBlock(billingDetail, subject)"); };
  // No member API supplier and no consent: nothing to show.
  assert.equal(block(subscription({third_party_api: false, third_party_acknowledged: false})), null);
  // A platform plan never asks.
  assert.equal(block({id: "sub-2", provider_scope: {seller: false, restricted: true, third_party_api: true}}), null);
  const pending = block(subscription({third_party_api: true, third_party_acknowledged: false}));
  assert.equal(pending.dataset.thirdParty, "pending");
  assert.match(text(pending), /你确认之前，它们不为这个订阅服务/);
  const allow = pending.querySelector("button");
  assert.equal(allow.textContent, "允许第三方 API 供应商");
  let reloaded = 0;
  ui.context.loadBillingDashboard = async () => { reloaded++; };
  ui.responses.push({subscription_id: "sub-1", third_party_acknowledged: true});
  await ui.run('setBillingThirdPartyConsent("sub-1", true)');
  assert.deepEqual(ui.calls[0], {path: "/admin/billing/me/subscriptions/sub-1/third-party-consent", method: "PUT", body: {acknowledged: true}});
  assert.equal(reloaded, 1);
  // Consent stays visible, and withdrawable, after the seller removes the supplier.
  const given = block(subscription({third_party_api: false, third_party_acknowledged: true}));
  assert.equal(given.dataset.thirdParty, "acknowledged");
  assert.equal(given.querySelector("button").textContent, "撤回确认");
  ui.responses.push({subscription_id: "sub-1", third_party_acknowledged: false});
  await ui.run('setBillingThirdPartyConsent("sub-1", false)');
  assert.deepEqual(ui.calls[1].body, {acknowledged: false});
  // Someone else's billing page shows the state without a button and sends nothing.
  ui.run('billingDetail = {user: {id: "someone-else"}};');
  const foreign = block(subscription({third_party_api: true}));
  assert.equal(foreign.querySelector("button"), null);
  await ui.run('setBillingThirdPartyConsent("sub-1", true)');
  assert.equal(ui.calls.length, 2);
});
