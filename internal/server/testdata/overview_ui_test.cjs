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
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.className = "";
    this.listeners = {};
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => this.classList.toggle(name, true),
      remove: (name) => this.classList.toggle(name, false),
      toggle: (name, enabled) => {
        const names = new Set(this.className.split(/\s+/).filter(Boolean));
        if (enabled === undefined) enabled = !names.has(name);
        if (enabled) names.add(name); else names.delete(name);
        this.className = [...names].join(" ");
      },
    };
  }
  set textContent(value) { this.text = String(value); this.children = []; }
  get textContent() { return this.text || this.children.map((child) => typeof child === "string" ? child : child.textContent).join(" "); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.text = ""; this.children = children; }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  closest() { return this; }
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return {promise, resolve, reject};
}

const billing = (id = "owner") => ({user: {id}, cash_balance_usd: "123.000000000123", subscriptions: {
  day: {enabled: true, remaining_usd: "9.000000000001", expires_at: "2099-10-01T12:00:00Z"},
  week: {enabled: false, remaining_usd: "87", expires_at: "2099-09-30T12:00:00Z"},
  month: {enabled: true, remaining_usd: "201", expires_at: null, period_count: 0},
}});
const available = {status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available"};

function fixture(pathname) {
  const url = new URL(pathname, "http://dashboard.test");
  if (url.pathname === "/admin/billing/me") return billing();
  if (url.pathname === "/admin/usage") return {summary: {requests: 7, tokens: 1700, error_rate: 1 / 7}, requests: Array.from({length: 7}, (_, index) => ({Model: `model-${index}`, State: "completed", HTTPStatus: 200, RequestedAt: "2099-09-29T12:00:00Z"}))};
  if (url.pathname === "/admin/usage/global") return {summary: {usage: {requests: 107, tokens: 17000, actual_cost_usd: "10.123456789123", unpriced_tokens: 0}, active_users: 2, total_users: 4, pricing_coverage: "1"}};
  if (url.pathname === "/admin/upstream-accounts") return {accounts: [available]};
  if (url.pathname === "/admin/antigravity-accounts") return {accounts: [{...available, status: "unavailable", gateway_manual_status: "manual_disabled"}]};
  if (url.pathname === "/admin/anthropic-accounts") return {accounts: [available]};
  if (url.pathname === "/admin/alerts") return {alerts: [{Severity: "critical"}]};
  throw new Error(`Unexpected API ${pathname}`);
}

function dashboard(role = "owner", handler = fixture) {
  const nodes = new Map();
  const node = (id) => {
    if (!nodes.has(id)) nodes.set(id, new Element());
    return nodes.get(id);
  };
  const calls = [];
  const context = vm.createContext({URLSearchParams, role, document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: () => []},
    request: async (pathname, options, current) => {
      calls.push({pathname, options, current});
      return handler(pathname, options, current);
    },
  });
  vm.runInContext(application, context, {filename: "assets/app.js"});
  const run = (code) => vm.runInContext(code, context);
  run(`state = {user: {id: "owner", role}, devices: [{id: "device"}], projects: [], api_keys: [{id: "key", last_used_at: "2099-01-01"}]}; api = request;`);
  return {run, node, calls, context};
}

test("overview uses independent self and monthly snapshots despite other-user filters and provider selection", async () => {
  const ui = dashboard();
  ui.run(`billingUserID = "another-user"; billingDetail = {user: {id: "another-user"}, cash_balance_usd: "999999"}; upstreamAccountProvider = "antigravity"; upstreamAccounts = [{id: "editing-account"}];`);
  await ui.run("loadOverview()");
  assert.equal(ui.node("overview-cash").textContent, "US$123.000000000123");
  assert.match(ui.node("overview-subscriptions").textContent, /US\$9\.000000000001/);
  assert.match(ui.node("overview-subscriptions").textContent, /周订阅 未启用/);
  assert.match(ui.node("overview-next-expiry").textContent, /2099\/10\/01/);
  assert.equal(ui.node("metric-requests").textContent, "7");
  assert.equal(ui.node("metric-global-tokens").textContent, "17,000");
  assert.equal(ui.node("metric-global-cost").textContent, "US$10.123456789123");
  assert.equal(ui.node("overview-recent-requests").children.length, 5);
  assert.match(ui.node("overview-codex-accounts").textContent, /1 可用 \/ 1 个账号/);
  assert.match(ui.node("overview-antigravity-accounts").textContent, /0 可用 \/ 1 个账号 · 1 不可用/);
  assert.equal(ui.run("billingUserID"), "another-user");
  assert.equal(ui.run("billingDetail.cash_balance_usd"), "999999");
  assert.equal(ui.run("upstreamAccounts[0].id"), "editing-account");
  assert.equal(ui.calls.length, 7);
  assert.match(ui.node("overview-anthropic-accounts").textContent, /1 可用 \/ 1 个账号/);
  for (const {pathname, current} of ui.calls) {
    assert.equal(typeof current, "function");
    assert.equal(current(), true);
    const url = new URL(pathname, "http://dashboard.test");
    assert.equal(url.searchParams.has("user_id"), false);
    assert.equal(url.searchParams.has("model"), false);
    if (url.pathname === "/admin/usage") {
      const days = (new Date(url.searchParams.get("until")) - new Date(url.searchParams.get("from"))) / 86400000;
      assert.ok(days >= 6.95 && days <= 7.05);
    }
    if (url.pathname === "/admin/usage/global") assert.equal(new Date(url.searchParams.get("from")).getDate(), 1);
  }
  assert.equal(ui.node("onboarding-complete").classList.contains("hidden"), false);
  assert.equal(ui.node("onboarding").classList.contains("hidden"), true);
});

test("member overview requests only their billing and usage", async () => {
  const ui = dashboard("member");
  await ui.run("loadOverview()");
  assert.equal(ui.calls.length, 2);
  assert.deepEqual(ui.calls.map(({pathname}) => new URL(pathname, "http://dashboard.test").pathname).sort(), ["/admin/billing/me", "/admin/usage"]);
  assert.equal(ui.node("metric-global-tokens").textContent, "");
});

test("a failed provider and billing identity mismatch do not overwrite successful independent snapshots", async () => {
  const ui = dashboard("owner", (pathname) => {
    if (pathname.startsWith("/admin/billing/me")) return billing("someone-else");
    if (pathname === "/admin/antigravity-accounts") throw new Error("AGY unreachable");
    return fixture(pathname);
  });
  await ui.run("loadOverview()");
  assert.equal(ui.node("overview-cash").textContent, "加载失败");
  assert.match(ui.node("overview-billing-updated").textContent, /身份/);
  assert.match(ui.node("overview-codex-accounts").textContent, /1 可用/);
  assert.match(ui.node("overview-antigravity-accounts").textContent, /加载失败/);
  assert.doesNotMatch(ui.node("overview-antigravity-accounts").textContent, /0 可用/);
  assert.equal(ui.node("metric-requests").textContent, "7");
});

test("sync failures expose unknown availability instead of claiming cached accounts available", async () => {
  const ui = dashboard("owner", (pathname) => pathname === "/admin/upstream-accounts" ? {sync_warning: "upstream_account_sync_unavailable", accounts: [available]} : fixture(pathname));
  await ui.run("loadOverview()");
  assert.match(ui.node("overview-codex-accounts").textContent, /同步失败.*未知/);
  assert.doesNotMatch(ui.node("overview-codex-accounts").textContent, /1 可用/);
  assert.match(ui.node("overview-antigravity-accounts").textContent, /1 不可用/);
});

test("identity changes discard every late overview response and invalidate API guards", async () => {
  const pending = deferred();
  const ui = dashboard("owner", async (pathname) => { await pending.promise; return fixture(pathname); });
  const loading = ui.run("loadOverview()");
  ui.run(`identityGeneration++; state = {user: {id: "replacement", role: "member"}}; resetOverview();`);
  for (const call of ui.calls) assert.equal(call.current(), false);
  pending.resolve();
  await loading;
  assert.equal(ui.node("overview-cash").textContent, "—");
  assert.equal(ui.node("metric-requests").textContent, "—");
  assert.equal(ui.node("overview-recent-requests").children.length, 0);
  assert.equal(ui.run("Object.keys(overviewSnapshots).length"), 0);
});

test("a newer refresh wins and late failure cannot replace its successful result", async () => {
  const pending = deferred();
  let reads = 0;
  const ui = dashboard("member", async (pathname) => {
    if (++reads <= 2) return pending.promise;
    return fixture(pathname);
  });
  const old = ui.run("loadOverview()");
  await ui.run("loadOverview()");
  pending.reject(new Error("old request failure"));
  await old;
  assert.equal(ui.node("overview-cash").textContent, "US$123.000000000123");
  assert.equal(ui.node("metric-requests").textContent, "7");
  assert.equal(ui.node("overview-recent-requests").attributes["aria-busy"], "false");
});

test("unlimited subscriptions, missing remaining values and incomplete onboarding stay distinct", () => {
  const ui = dashboard("member");
  ui.context.detail = {subscriptions: {day: {enabled: true, expires_at: null, period_count: 0}}};
  ui.run("renderOverviewBilling(detail)");
  assert.equal(ui.node("overview-cash").textContent, "—");
  assert.match(ui.node("overview-subscriptions").textContent, /日订阅 数据不可用/);
  assert.match(ui.node("overview-next-expiry").textContent, /无限期/);
  ui.run("renderOverviewBilling({subscriptions: {day: {enabled: true, expires_at: null}}})");
  assert.match(ui.node("overview-next-expiry").textContent, /数据不可用/);
  ui.run("renderOverviewBilling({})");
  assert.doesNotMatch(ui.node("overview-subscriptions").textContent, /未启用/);
  ui.run(`state.devices = []; state.api_keys = []; overviewSummary = null; renderOnboarding();`);
  assert.equal(ui.node("onboarding-complete").classList.contains("hidden"), true);
  assert.equal(ui.node("onboarding").children.length, 3);
  assert.equal(ui.node("onboarding").classList.contains("hidden"), false);
});
