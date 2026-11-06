"use strict";

// Quota freshness labels, refresh buttons and the line-bar visualizations on
// the billing, overview, group and upstream account pages.
const assert = require("node:assert/strict");
const {randomUUID} = require("node:crypto");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tagName = String(tag).toUpperCase(); this.children = []; this.dataset = {}; this.attributes = {};
    this.className = ""; this.value = ""; this.disabled = false; this.checked = false; this.open = false; this._text = "";
    this.listeners = new Map(); this.style = {setProperty: (name, value) => { this.style[name] = value; }};
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => this.classList.toggle(name, true), remove: (name) => this.classList.toggle(name, false),
      toggle: (name, enabled) => {
        const names = new Set(this.className.split(/\s+/).filter(Boolean));
        if (enabled === undefined) enabled = !names.has(name);
        if (enabled) names.add(name); else names.delete(name);
        this.className = [...names].join(" ");
        return enabled;
      },
    };
  }
  get childElementCount() { return this.children.filter((child) => typeof child !== "string").length; }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map((child) => typeof child === "string" ? child : child.textContent).join(""); }
  get isConnected() { return true; }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  removeAttribute(name) { delete this.attributes[name]; }
  append(...children) { this.children.push(...children.map((child) => child == null ? String(child) : child)); }
  replaceChildren(...children) { this._text = ""; this.children = []; this.append(...children); }
  matches(selector) {
    return selector.split(",").map((part) => part.trim()).some((part) => {
      if (part.startsWith(".")) return part.slice(1).split(".").every((name) => this.classList.contains(name));
      const data = part.match(/^\[data-([a-z-]+)\]$/);
      if (data) return data[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase()) in this.dataset;
      return part.toUpperCase() === this.tagName;
    });
  }
  querySelectorAll(selector) {
    return this.children.filter((child) => typeof child !== "string").flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest() { return null; }
  focus() {}
  addEventListener(name, handler) { this.listeners.set(name, handler); }
  dispatch(name, event = {}) { return this.listeners.get(name)?.({currentTarget: this, target: this, preventDefault() {}, ...event}); }
}

const settle = () => new Promise((resolve) => setImmediate(resolve));

function dashboard({role = "owner", userID = "self"} = {}) {
  const nodes = new Map();
  const root = new Element("main");
  const node = (id) => {
    if (!nodes.has(id)) { const element = new Element(); element.id = id; nodes.set(id, element); root.append(element); }
    return nodes.get(id);
  };
  for (const id of ["group-form", "group-members-form", "upstream-access-form", "group-account-usage-form"]) {
    node(id).reset = () => {};
    node(id).elements = {enabled: new Element("input"), reason: new Element("input")};
  }
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), addEventListener() {}, querySelectorAll: (selector) => root.querySelectorAll(selector), querySelector: (selector) => root.querySelector(selector)},
    window: {setTimeout: () => 0, clearTimeout() {}}, Intl, URLSearchParams, URL, crypto: {randomUUID}, console,
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run(`state = {user: {id: ${JSON.stringify(userID)}, username: "self", display_name: "自己", role: ${JSON.stringify(role)}}, recently_verified: true};`);
  const calls = [];
  context.respond = async () => ({});
  context.api = async (url, options) => { calls.push({url, options}); return context.respond(url, options); };
  return {context, run, node, calls, root};
}

const billing = (userID = "self", extra = {}) => ({
  user: {id: userID, username: userID}, cash_balance_usd: "12.5",
  subscriptions: [{id: "s1", name: "日卡", enabled: true, quota_usd: "10", remaining_usd: "4", period_days: 1, period_ends_at: "2099-01-01T00:00:00Z", period_count: 0, current_period_number: 1}],
  ledger_entries: [], ...extra,
});

test("data times keep seconds and the refresh button runs the given loader", async () => {
  const ui = dashboard();
  const at = new Date(2026, 10, 6, 9, 5, 7);
  let refreshed = 0;
  ui.context.at = at;
  ui.context.onRefresh = async () => { refreshed++; };
  const row = ui.run(`quotaFreshness({at, onRefresh, subject: "测试额度"})`);
  const time = row.querySelector(".quota-freshness-time");
  assert.equal(time.textContent, "数据时间：2026/11/06 09:05:07");
  assert.equal(time.attributes.datetime, at.toISOString());
  assert.equal(row.dataset.state, "ready");
  const button = row.querySelector(".quota-refresh");
  assert.equal(button.textContent, "刷新");
  assert.equal(button.attributes["aria-label"], "刷新测试额度");
  await button.dispatch("click");
  assert.equal(refreshed, 1);

  const empty = ui.run(`quotaFreshness({})`);
  assert.equal(empty.textContent, "数据时间：尚未加载");
  assert.equal(empty.querySelector(".quota-refresh"), null);
  assert.equal(ui.run(`quotaFreshness({pending: "正在刷新…", onRefresh() {}})`).querySelector(".quota-refresh").disabled, true);

  ui.run(`setQuotaFreshness("group-detail-time", at)`);
  assert.equal(ui.node("group-detail-time").textContent, "数据时间：2026/11/06 09:05:07");
  assert.equal(ui.node("group-detail-time").dataset.state, "ready");
  ui.run(`setQuotaFreshness("group-detail-time", null)`);
  assert.equal(ui.node("group-detail-time").textContent, "数据时间：尚未加载");
  assert.equal(ui.node("group-detail-time").attributes.datetime, undefined);
});

test("the billing cards share one data time that tracks loading, success and failure", async () => {
  const ui = dashboard({role: "member"});
  let release;
  ui.context.respond = (url) => {
    if (url.startsWith("/admin/billing/me")) return new Promise((resolve) => { release = () => resolve(billing()); });
    return {};
  };
  const loading = ui.run(`loadBillingDetail("self", 0)`);
  assert.equal(ui.node("billing-cash-time").textContent, "正在刷新额度…");
  assert.equal(ui.node("billing-subscriptions-time").dataset.state, "loading");
  release();
  await loading;
  assert.match(ui.node("billing-cash-time").textContent, /^数据时间：\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}$/);
  assert.equal(ui.node("billing-subscriptions-time").textContent, ui.node("billing-cash-time").textContent);
  assert.equal(ui.node("billing-cash-balance").textContent, "US$12.50");

  ui.context.respond = async () => { throw new Error("网络中断"); };
  await assert.rejects(ui.run(`refreshBillingQuota()`), /网络中断/);
  assert.equal(ui.node("billing-cash-time").textContent, "数据时间：加载失败");
  assert.match(ui.calls.at(-1).url, /^\/admin\/billing\/me\?limit=\d+&offset=0$/);
});

test("group quota cards on the billing page carry their own refresh beside the data time", async () => {
  const ui = dashboard({role: "member"});
  const at = new Date(2026, 10, 6, 10, 0, 1);
  ui.context.at = at;
  ui.context.detail = billing("self", {groups: [{id: "g1", name: "研究组", limit_usd: "100", used_usd: "40", remaining_usd: "60", member_used_usd: "5", period_starts_at: "2026-11-01T00:00:00Z", period_ends_at: "2026-12-01T00:00:00Z", period: "month"}]});
  ui.run(`billingUserID = "self"; renderBillingDetail(detail, at)`);
  const host = ui.node("billing-group");
  const freshness = host.querySelector(".quota-freshness");
  assert.equal(freshness.querySelector(".quota-freshness-time").textContent, "数据时间：2026/11/06 10:00:01");
  ui.context.respond = async () => billing();
  await freshness.querySelector(".quota-refresh").dispatch("click");
  await settle();
  assert.ok(ui.calls.some(({url}) => url.startsWith("/admin/billing/me?")), "group refresh re-reads the billing detail");
});

test("the overview balance cards refresh alone and show when the balance was read", async () => {
  const ui = dashboard({role: "member"});
  ui.context.respond = async (url) => url.startsWith("/admin/billing/me") ? billing() : {};
  await ui.run(`loadOverviewBilling()`);
  assert.deepEqual(ui.calls.map(({url}) => url), ["/admin/billing/me?limit=1&offset=0"]);
  assert.equal(ui.node("overview-cash").textContent, "US$12.50");
  assert.match(ui.node("overview-billing-updated").textContent, /^数据时间：\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}$/);
  assert.equal(ui.node("overview-subscriptions-updated").textContent, ui.node("overview-billing-updated").textContent);

  ui.context.respond = async () => billing("someone-else");
  await ui.run(`loadOverviewBilling()`);
  assert.equal(ui.node("overview-cash").textContent, "加载失败");
  assert.match(ui.node("overview-billing-updated").textContent, /身份/);
  assert.equal(ui.node("overview-subscriptions-updated").textContent, "额度数据不可用，请刷新重试。");
});

test("group details record the time their quota was read", () => {
  const ui = dashboard();
  const at = new Date(2026, 10, 6, 11, 22, 33);
  ui.context.at = at;
  ui.context.group = {id: "g1", name: "研究组", limit_usd: "100", used_usd: "30", remaining_usd: "70", period: "month", members: []};
  ui.run(`renderGroupDetail(group, at)`);
  assert.equal(ui.node("group-detail-time").textContent, "数据时间：2026/11/06 11:22:33");
  assert.equal(ui.node("group-detail-refresh").disabled, false);
  ui.run(`resetGroupManagement()`);
  assert.equal(ui.node("group-detail-time").textContent, "数据时间：尚未加载");
});
