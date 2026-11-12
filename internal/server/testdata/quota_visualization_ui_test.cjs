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
    // Named form controls are created on first use, like a parsed form.
    node(id).elements = new Proxy({}, {get: (fields, name) => typeof name === "string" ? (fields[name] ??= new Element("input")) : undefined});
  }
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), addEventListener() {}, querySelectorAll: (selector) => root.querySelectorAll(selector), querySelector: (selector) => root.querySelector(selector)},
    window: {setTimeout: () => 0, clearTimeout() {}}, Intl, URLSearchParams, URL, crypto: {randomUUID}, console,
    // queryFromForm reads the fake form's named elements.
    FormData: class { constructor(form) { this.values = Object.entries(form.elements || {}).map(([name, field]) => [name, field.value]); } [Symbol.iterator]() { return this.values[Symbol.iterator](); } },
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

const usageModels = (extra = {}) => ({
  from: "2026-11-05T06:00:00Z", until: "2026-11-06T06:00:01Z", generated_at: "2026-11-06T06:00:01Z", truncated: false,
  total: {requests: 9, tokens: 4200, charged_usd: "3.750000000001"},
  models: [
    {model: "gpt-5.6", requests: 6, errors: 1, in_progress: 0, tokens: 3000, charged_usd: "3.000000000001"},
    {model: "claude-sonnet-5", requests: 2, errors: 0, in_progress: 1, tokens: 1200, charged_usd: "0.75"},
    {model: "gpt-5.6-mini", requests: 1, errors: 0, in_progress: 0, tokens: 0, charged_usd: "0"},
  ], ...extra,
});

function usageForm(ui) {
  const form = ui.node("usage-filter");
  form.elements = {user_id: new Element("input"), from: new Element("input"), until: new Element("input"), device_id: new Element("select"),
    api_key_id: new Element("select"), project_id: new Element("select"), model: new Element("input"), state: new Element("select"), status: new Element("input")};
  return form;
}

test("decimal sums stay exact past float precision", () => {
  const ui = dashboard();
  assert.equal(ui.run(`sumDecimals(["0.100000000001", "0.2", "3"])`), "3.300000000001");
  assert.equal(ui.run(`sumDecimals(["12.5", "-0.000000000001"])`), "12.499999999999");
  assert.equal(ui.run(`sumDecimals(["1", "abc"])`), null);
});

test("share bars size segments by value, keep a legend with amounts and show details on hover or click", () => {
  const ui = dashboard();
  ui.context.items = [
    {key: "bob", label: "Bob", value: 30, amount: "US$30.00"},
    {key: "alice", label: "Alice", value: 10, amount: "US$10.00"},
  ];
  const bar = ui.run(`shareBar({label: "群组额度", items, scale: 100, remainder: {label: "剩余", amount: "US$60.00"}})`);
  const segments = bar.querySelectorAll(".share-bar-segment");
  assert.deepEqual(segments.map((node) => node.dataset.width), ["30.000", "10.000"]);
  assert.deepEqual(segments.map((node) => node.style.width), ["30%", "10%"]);
  // Slots follow the sorted keys, not the order or size of the segments.
  assert.deepEqual(segments.map((node) => node.dataset.tone), ["2", "1"]);
  assert.equal(segments[0].attributes.title, "Bob：US$30.00（30.0%）");
  const legend = bar.querySelector(".share-bar-legend").textContent;
  assert.equal(legend, "BobUS$30.0030.0%AliceUS$10.0010.0%剩余US$60.0060.0%");
  const detail = bar.querySelector(".share-bar-detail");
  assert.equal(detail.textContent, "悬停或点击色块查看具体额度");
  segments[1].dispatch("mouseenter");
  assert.equal(detail.textContent, "Alice：US$10.00，占 10.0%");
  segments[1].dispatch("mouseleave");
  assert.equal(detail.textContent, "悬停或点击色块查看具体额度");
  segments[0].dispatch("click");
  assert.equal(segments[0].attributes["aria-pressed"], "true");
  assert.equal(bar.querySelectorAll(".share-bar-key")[0].attributes["aria-pressed"], "true");
  segments[1].dispatch("mouseleave");
  assert.equal(detail.textContent, "Bob：US$30.00，占 30.0%", "a selection outlives hovering elsewhere");
  bar.querySelectorAll(".share-bar-key")[0].dispatch("click");
  assert.equal(detail.textContent, "悬停或点击色块查看具体额度", "clicking the selected entry clears it");

  const empty = ui.run(`shareBar({label: "空", items: [], emptyText: "没有数据"})`);
  assert.equal(empty.querySelector(".share-bar-detail").textContent, "没有数据");
  assert.equal(empty.querySelectorAll(".share-bar-segment").length, 0);
});

test("more entities than slots fold into one exact 其他 segment", () => {
  const ui = dashboard();
  ui.context.items = Array.from({length: 9}, (_, index) => ({key: `m${index}`, label: `m${index}`, value: 9 - index, exact: `${9 - index}.000000000001`, amount: `US$${9 - index}`}));
  const folded = ui.run(`foldShareItems(items, {otherLabel: "其他模型"})`);
  assert.equal(folded.length, 7);
  assert.deepEqual(JSON.parse(JSON.stringify(folded.slice(0, 6).map((item) => item.key))), ["m0", "m1", "m2", "m3", "m4", "m5"]);
  const other = folded[6];
  assert.equal(other.label, "其他模型（3 项）");
  assert.equal(other.tone, "other");
  assert.equal(other.amount, "US$6.000000000003");
  assert.match(other.detail, /^其他模型：m6 US\$3、m7 US\$2、m8 US\$1$/);
});

test("the usage page opens on 24-hour per-model charges and keeps the request list collapsed", async () => {
  const ui = dashboard({role: "member"});
  usageForm(ui);
  ui.context.respond = async (url) => url.startsWith("/admin/usage/models") ? usageModels() : {summary: {requests: 1}, requests: []};
  await ui.run(`loadUsageModels()`);
  assert.deepEqual(ui.calls.map(({url}) => url), ["/admin/usage/models"]);
  assert.equal(ui.node("usage-models-charged").textContent, "US$3.750000000001");
  assert.equal(ui.node("usage-models-requests").textContent, "9");
  assert.equal(ui.node("usage-models-count").textContent, "3 个模型");
  assert.match(ui.node("usage-models-time").textContent, /^数据时间：\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}$/);
  assert.match(ui.node("usage-models-period").textContent, /^统计区间：2026\/11\/0\d \d{2}:00:00 — 2026\/11\/0\d \d{2}:00:01$/);
  const segments = ui.node("usage-models-share").querySelectorAll(".share-bar-segment");
  assert.deepEqual(segments.map((node) => node.attributes.title), ["gpt-5.6：US$3.000000000001（80.0%）", "claude-sonnet-5：US$0.75（20.0%）"]);
  const rows = ui.node("usage-model-rows").children;
  assert.equal(rows.length, 3);
  assert.equal(rows[0].textContent, "gpt-5.6US$3.00000000000180.0%61 个失败3,000");
  assert.equal(rows[1].textContent, "claude-sonnet-5US$0.7520.0%21 个进行中1,200");
  assert.equal(rows[2].textContent, "gpt-5.6-miniUS$0.000.0%10");
  assert.equal(rows[2].querySelector(".usage-model-swatch").dataset.tone, "other", "models without charges have no slot");

  assert.equal(ui.run("usageDetailsOpen"), false);
  await ui.run(`toggleUsageDetails()`);
  assert.equal(ui.node("usage-details").classList.contains("hidden"), false);
  assert.equal(ui.node("usage-details-toggle").textContent, "收起请求明细");
  assert.equal(ui.node("usage-details-toggle").attributes["aria-expanded"], "true");
  const listReads = () => ui.calls.filter(({url}) => url === "/admin/usage" || url.startsWith("/admin/usage?")).length;
  assert.equal(listReads(), 1);
  await ui.run(`toggleUsageDetails()`);
  assert.equal(ui.node("usage-details").classList.contains("hidden"), true);
  assert.equal(ui.node("usage-details-toggle").textContent, "展开请求明细");
  await ui.run(`toggleUsageDetails()`);
  assert.equal(listReads(), 1, "reopening reuses the loaded list");
});

test("model usage follows the drilled-down user and reports failures beside the data time", async () => {
  const ui = dashboard();
  const form = usageForm(ui);
  form.elements.user_id.value = "member-2";
  ui.context.respond = async () => { throw new Error("服务暂不可用"); };
  await assert.rejects(ui.run(`loadUsageModels()`), /服务暂不可用/);
  assert.equal(ui.calls[0].url, "/admin/usage/models?user_id=member-2");
  assert.equal(ui.node("usage-models-time").textContent, "数据时间：加载失败");
  assert.equal(ui.node("usage-models-charged").textContent, "—");
  assert.match(ui.node("usage-model-rows").textContent, /模型用量加载失败：服务暂不可用/);

  ui.context.respond = async () => usageModels({models: [], total: {requests: 0, tokens: 0, charged_usd: "0.000000000000"}});
  await ui.run(`loadUsageModels()`);
  assert.equal(ui.node("usage-model-rows").textContent, "最近 24 小时没有请求记录。");
  assert.equal(ui.node("usage-models-share").querySelector(".share-bar-detail").textContent, "最近 24 小时没有请求。");
});

const breakdown = (extra = {}) => ({
  group_id: "g1", current_period: true, data_complete: true,
  period: {id: "p2", starts_at: "2026-11-01T00:00:00Z", ends_at: "2026-12-02T00:00:00Z"},
  periods: [{id: "p2", starts_at: "2026-11-01T00:00:00Z", ends_at: "2026-12-02T00:00:00Z"}, {id: "p1", starts_at: "2026-10-01T00:00:00Z", ends_at: "2026-11-01T00:00:00Z"}],
  limit_usd: "100.000000000000", member_limit_usd: "40.000000000000", used_usd: "50.000000000000", remaining_usd: "50.000000000000",
  members: [
    {user_id: "alice", username: "alice", display_name: "Alice", current_member: true, group_used_usd: "30.000000000000"},
    {user_id: "bob", username: "bob", display_name: "", current_member: true, group_used_usd: "12.000000000000"},
    {user_id: "carol", username: "carol", display_name: "陈卡罗", current_member: false, group_used_usd: "5.000000000000"},
    {user_id: "dave", username: "dave", display_name: "Dave", current_member: true, group_used_usd: "0.000000000000"},
  ],
  accounts: [
    {account_id: "codex-a", provider: "codex", alias: "Codex 主力", in_pool: true, total_cost_usd: "50.000000000000", member_cost_usd: "42.000000000000",
      non_member_cost_usd: "8.000000000000", group_charged_usd: "37.000000000000", members: [{user_id: "alice", cost_usd: "30.000000000000"}, {user_id: "bob", cost_usd: "12.000000000000"}]},
    {account_id: "claude-a", provider: "anthropic", email_masked: "c***@example.test", in_pool: false, total_cost_usd: "0.000000000000", member_cost_usd: "0.000000000000",
      non_member_cost_usd: "0.000000000000", group_charged_usd: "0.000000000000", members: []},
  ], ...extra,
});

test("the group usage card splits the quota by member and each account by members versus outsiders", () => {
  const ui = dashboard();
  ui.context.result = breakdown();
  ui.run(`renderGroupUsageBreakdown(result, new Date(2026, 10, 6, 12, 0, 0))`);
  assert.equal(ui.node("group-usage-time").textContent, "数据时间：2026/11/06 12:00:00");
  const body = ui.node("group-usage-body");
  const [quota, accounts] = body.querySelectorAll(".share-bar");
  const quotaSegments = quota.querySelectorAll(".share-bar-segment");
  assert.deepEqual(quotaSegments.map((node) => node.attributes.title), [
    "Alice：US$30.00（30.0%）", "bob：US$12.00（12.0%）", "陈卡罗（已移出）：US$5.00（5.0%）", "未归属成员：US$3.00（3.0%）",
  ]);
  assert.deepEqual(quotaSegments.map((node) => node.dataset.width), ["30.000", "12.000", "5.000", "3.000"]);
  assert.match(quota.querySelector(".share-bar-legend").textContent, /剩余额度US\$50\.0050\.0%$/);
  quotaSegments[0].dispatch("click");
  assert.equal(quota.querySelector(".share-bar-detail").textContent, "Alice：本期群组支付 US$30.00，占群组额度 30.0%，占已用 60.0%");
  assert.match(body.textContent, /已用 US\$50\.00 \/ 额度 US\$100\.00（50\.0%） · 剩余 US\$50\.00/);

  const accountSegments = accounts.querySelectorAll(".share-bar-segment");
  assert.deepEqual(accountSegments.map((node) => node.attributes.title), ["Alice：US$30.00（60.0%）", "bob：US$12.00（24.0%）", "非群组用户：US$8.00（16.0%）"]);
  assert.equal(accountSegments[2].dataset.tone, "outside");
  // One user keeps one color in every bar of the card.
  assert.equal(accountSegments[0].dataset.tone, quotaSegments[0].dataset.tone);
  assert.equal(accountSegments[1].dataset.tone, quotaSegments[1].dataset.tone);
  assert.notEqual(quotaSegments[0].dataset.tone, quotaSegments[1].dataset.tone);
  const cards = body.querySelectorAll(".group-usage-account");
  assert.match(cards[0].textContent, /^Codex 主力Codex账号池总成本 US\$50\.00 · 群组成员 84\.0% · 非群组用户 16\.0% · 群组支付 US\$37\.00/);
  assert.match(cards[1].textContent, /^c\*\*\*@example\.testClaude不在账号池 · 本周期群组曾支付本周期暂无用量/);

  const limits = body.querySelectorAll(".group-usage-member-limit");
  assert.deepEqual(limits.map((row) => row.textContent), [
    "AliceUS$30.00 / US$40.00", "bobUS$12.00 / US$40.00", "陈卡罗（已移出）US$5.00 / US$40.00", "DaveUS$0.00 / US$40.00",
  ]);
  assert.equal(limits[0].querySelector("progress").attributes.value, "75.00");

  const select = ui.node("group-usage-period");
  const options = select.children.map((option) => option.textContent);
  assert.equal(options.length, 2);
  assert.match(options[0], /^2026\/1[01]\/\d{2} \d{2}:\d{2} — 2026\/1[12]\/\d{2} \d{2}:\d{2}（当前）$/);
  assert.match(options[1], /^2026\/(09|10)\/\d{2} \d{2}:\d{2} — 2026\/1[01]\/\d{2} \d{2}:\d{2}$/);
  assert.equal(select.value, "p2");
  assert.equal(select.disabled, false);
});

test("the group usage card hides member caps without a limit and explains an empty account pool", () => {
  const ui = dashboard();
  ui.context.result = breakdown({member_limit_usd: null, accounts: [], data_complete: false, used_usd: "47.000000000000"});
  ui.run(`renderGroupUsageBreakdown(result)`);
  const body = ui.node("group-usage-body");
  assert.equal(body.querySelectorAll(".group-usage-member-limit").length, 0);
  assert.match(body.textContent, /群组未指定账号池，且本周期没有由群组额度支付的账号请求。/);
  assert.match(body.textContent, /部分账务历史已清理/);
  assert.equal(body.querySelectorAll(".share-bar")[0].querySelectorAll(".share-bar-segment").length, 3, "no unattributed part when members add up");
});

test("users beyond the color slots share one exact 其他成员 segment", () => {
  const ui = dashboard();
  const members = Array.from({length: 9}, (_, index) => ({user_id: `u${index}`, username: `user${index}`, current_member: true, group_used_usd: `${9 - index}.000000000001`}));
  ui.context.result = breakdown({members, used_usd: "45.000000000009", accounts: []});
  ui.run(`renderGroupUsageBreakdown(result)`);
  const segments = ui.node("group-usage-body").querySelectorAll(".share-bar")[0].querySelectorAll(".share-bar-segment");
  assert.equal(segments.length, 7);
  assert.equal(segments[6].attributes.title, "其他成员（3 人）：US$6.000000000003（6.0%）");
  assert.equal(segments[6].dataset.tone, "other");
  assert.equal(new Set(segments.slice(0, 6).map((node) => node.dataset.tone)).size, 6, "six distinct slots");
});

test("the group usage card loads the chosen period and reports failures beside its data time", async () => {
  const ui = dashboard();
  ui.run(`bindGroupUI()`);
  ui.context.respond = async () => breakdown();
  ui.run(`managedGroup = {id: "g1", members: []}`);
  await ui.run(`loadGroupUsageBreakdown()`);
  assert.equal(ui.calls.at(-1).url, "/admin/groups/g1/usage-breakdown");
  assert.match(ui.node("group-usage-time").textContent, /^数据时间：/);

  const select = ui.node("group-usage-period");
  select.value = "p1";
  select.dispatch("change");
  await settle();
  assert.equal(ui.calls.at(-1).url, "/admin/groups/g1/usage-breakdown?period_id=p1");
  select.value = "p2";
  select.dispatch("change");
  await settle();
  assert.equal(ui.calls.at(-1).url, "/admin/groups/g1/usage-breakdown", "the newest period follows the group");

  ui.context.respond = async () => breakdown({group_id: "other"});
  await assert.rejects(ui.run(`loadGroupUsageBreakdown()`), /群组用量响应格式无效/);
  assert.equal(ui.node("group-usage-time").textContent, "数据时间：加载失败");
  assert.match(ui.node("group-usage-body").textContent, /用量加载失败/);
  ui.run(`resetGroupManagement()`);
  assert.equal(ui.node("group-usage-time").textContent, "数据时间：尚未加载");
  assert.equal(ui.node("group-usage-body").textContent, "选择群组后加载。");
});

const poolUsage = (extra = {}) => ({group_id: "g1", truncated: false, accounts: [
  {account_id: "codex-a", provider: "codex", alias: "团队主账号", email_masked: "a***@example.test", source: "live", updated_at: "2026-11-06T06:00:05Z", limited: false,
    five_hour: {used_percent: 38, reset_at: "2026-11-06T09:00:00Z"}, seven_day: {used_percent: 64, reset_at: "2026-11-10T00:00:00Z"}},
  {account_id: "codex-b", provider: "codex", email_masked: "b***@example.test", error_code: "usage_rate_limited", limited: false},
  {account_id: "claude-a", provider: "anthropic", display_name: "claude-a", source: "observed", updated_at: "2026-11-06T05:50:00Z", limited: true,
    cooldown_until: "2099-01-01T00:00:00Z", five_hour: {used_percent: 100, reset_at: "2026-11-06T07:00:00Z"}, seven_day: {used_percent: null}},
], ...extra});

test("the owner sees each selected account's 5-hour and weekly bars with when they were read", async () => {
  const ui = dashboard();
  ui.run(`managedGroup = {id: "g1", members: []}`);
  ui.context.respond = async () => poolUsage();
  await ui.run(`loadGroupPoolUsage()`);
  assert.equal(ui.calls.at(-1).url, "/admin/groups/g1/account-usage");
  assert.match(ui.node("group-account-quota-time").textContent, /^数据时间：\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}$/);
  const cards = ui.node("group-account-quota-list").children;
  assert.equal(cards.length, 3);
  assert.match(cards[0].textContent, /^团队主账号正常Codex · a\*\*\*@example\.test5 小时已用 38%重置：.*每周已用 64%重置：.*查询于 \d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:05$/);
  assert.deepEqual(cards[0].querySelectorAll("progress").map((bar) => bar.attributes.value), ["38", "64"]);
  assert.equal(cards[1].textContent, "b***@example.test查询过于频繁，请稍后刷新Codex", "failures show a fixed reason and no bars");
  assert.match(cards[2].textContent, /^claude-a冷却至 .*Claude5 小时已用 100%.*每周未知重置：.*观测于 /);
  assert.equal(cards[2].querySelectorAll("progress").length, 1, "an unknown window has no bar");

  ui.context.respond = async () => poolUsage({accounts: []});
  await ui.run(`loadGroupPoolUsage()`);
  assert.match(ui.node("group-account-quota-list").textContent, /群组没有为 Codex 或 Claude 指定账号/);

  ui.context.respond = async () => { throw new Error("上游超时"); };
  await assert.rejects(ui.run(`loadGroupPoolUsage()`), /上游超时/);
  assert.equal(ui.node("group-account-quota-time").textContent, "数据时间：加载失败");
  assert.match(ui.node("group-account-quota-list").textContent, /账号额度加载失败：上游超时/);
  ui.run(`resetGroupManagement()`);
  assert.equal(ui.node("group-account-quota-time").textContent, "数据时间：尚未加载");
});

test("upstream account cards draw the latest 5-hour and weekly readings as line bars", async () => {
  const ui = dashboard();
  const hour = 3600 * 1000;
  ui.context.account = {id: "0123456789abcdef", quota_guard: {status: "available", observed_at: "2026-11-06T05:53:07Z",
    five_hour: {used_percent: 97, reset_at: new Date(Date.now() + 2 * hour).toISOString()},
    seven_day: {used_percent: 41, reset_at: new Date(Date.now() - hour).toISOString()}}};
  ui.context.queried = 0;
  const host = ui.run(`(() => { const host = element("div", {className: "upstream-quota-bars"}); host.queryQuota = async () => { queried++; }; renderUpstreamQuotaBars(host, account); return host; })()`);
  const [five, week] = host.querySelectorAll(".upstream-quota-bar");
  assert.equal(five.dataset.level, "high", "near-exhausted windows are flagged");
  assert.match(five.textContent, /^5 小时额度已用 97%剩余 3% · .* 重置$/);
  assert.equal(five.querySelector("progress").attributes.value, "97");
  assert.equal(week.dataset.level, "unknown");
  assert.match(week.textContent, /^周额度已重置.* 已重置，等待下次读取$/, "a passed reset never shows a stale bar");
  assert.equal(week.querySelector("progress"), null);
  assert.match(host.querySelector(".quota-freshness-time").textContent, /^额度读取于：\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:07$/);
  const refresh = host.querySelector(".quota-refresh");
  assert.equal(refresh.textContent, "查询额度");
  await refresh.dispatch("click");
  assert.equal(ui.run("queried"), 1, "the bar refresh runs the live quota query");

  const first = host.children[0];
  ui.run(`renderUpstreamQuotaBars(null, account)`); // a card without bars is a no-op
  ui.context.host = host;
  ui.run(`renderUpstreamQuotaBars(host, account)`);
  assert.equal(host.children[0], first, "unchanged readings keep the existing bars");

  ui.context.account = {id: "1123456789abcdef", quota_guard: {status: "available"}};
  ui.run(`renderUpstreamQuotaBars(host, account)`);
  assert.equal(host.querySelector(".quota-freshness-time").textContent, "尚未读取额度");
  assert.deepEqual(host.querySelectorAll(".upstream-quota-bar").map((bar) => bar.textContent), ["5 小时额度未知尚无读数", "周额度未知尚无读数"]);
});

test("a live quota query refreshes the summary bars from Codex minutes or Claude window names", () => {
  const ui = dashboard();
  const at = new Date("2026-11-06T06:00:00Z");
  ui.context.at = at;
  const codex = ui.run(`upstreamQuotaReadings("codex", {rateLimits: {primary: {usedPercent: 40, windowDurationMins: 300, resetsAt: 1800000000},
    secondary: {usedPercent: 25, windowDurationMins: 10080, resetsAt: 1800600000}}}, at)`);
  assert.deepEqual(JSON.parse(JSON.stringify(codex)), {observed_at: "2026-11-06T06:00:00.000Z",
    five_hour: {used_percent: 40, reset_at: new Date(1800000000 * 1000).toISOString()},
    seven_day: {used_percent: 25, reset_at: new Date(1800600000 * 1000).toISOString()}});
  // In CPA plugin mode the Codex snapshot is an observation with its own time.
  const observedCodex = ui.run(`upstreamQuotaReadings("codex", {observed_at: "2026-11-06T05:30:00Z",
    rateLimits: {primary: {usedPercent: 40, windowDurationMins: 300}}}, at)`);
  assert.deepEqual(JSON.parse(JSON.stringify(observedCodex)), {observed_at: "2026-11-06T05:30:00Z",
    five_hour: {used_percent: 40, reset_at: null}});
  const claude = ui.run(`upstreamQuotaReadings("anthropic", {observed_at: "2026-11-06T05:58:00Z", windows: [
    {window: "five_hour", used_percent: 41.6, reset_at: "2026-11-06T09:00:00Z"}, {window: "seven_day", used_percent: null}]}, at)`);
  assert.deepEqual(JSON.parse(JSON.stringify(claude)), {observed_at: "2026-11-06T05:58:00Z", five_hour: {used_percent: 42, reset_at: "2026-11-06T09:00:00Z"}});

  const card = new Element("article");
  card.className = "upstream-account-card";
  card.dataset.accountId = "0123456789abcdef";
  const bars = new Element("div");
  bars.className = "upstream-quota-bars";
  card.append(bars);
  ui.node("upstream-account-list").append(card);
  ui.context.account = {id: "0123456789abcdef", quota_guard: {status: "limited", source: "manual"}};
  ui.run(`upstreamAccountProvider = "codex"; applyUpstreamQuotaReadings(account, "codex", {rateLimits: {primary: {usedPercent: 55, windowDurationMins: 300}}}, at)`);
  assert.equal(ui.context.account.quota_guard.status, "limited", "the guard state stays server-owned");
  assert.equal(ui.context.account.quota_guard.five_hour.used_percent, 55);
  assert.match(bars.textContent, /^5 小时额度已用 55%剩余 45% · 重置时间未知 重置周额度未知尚无读数额度读取于：/);

  ui.run(`applyUpstreamQuotaReadings(account, "antigravity", {}, at)`);
  assert.equal(ui.context.account.quota_guard.five_hour.used_percent, 55, "Antigravity has no windows to apply");
});

test("live quota windows in the account details carry the same bars", () => {
  const ui = dashboard();
  const window = ui.run(`upstreamQuotaWindow("额度桶 codex", "主窗口", {usedPercent: 92, windowDurationMins: 300, resetsAt: 1800000000})`);
  assert.equal(window.dataset.level, "high");
  assert.equal(window.querySelector("progress").attributes.value, "92");
  const unknown = ui.run(`upstreamQuotaWindow("额度桶 codex", "次窗口", {})`);
  assert.equal(unknown.querySelector("progress"), null);
  ui.run(`upstreamAccountProvider = "anthropic"`);
  const container = new Element("div");
  ui.context.container = container;
  ui.run(`renderAnthropicQuota(container, {observed_at: "2026-11-06T05:58:00Z", windows: [{window: "five_hour", used_percent: 30, status: "allowed"}]})`);
  assert.deepEqual(container.querySelectorAll("progress").map((bar) => bar.attributes.value), ["30"]);
  assert.match(container.textContent, /观测时间：\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:00/);
});

test("an observed Codex quota shows when CPA saw it and ages from then", () => {
  const ui = dashboard();
  ui.context.account = {id: "0123456789abcdef", plan: "Pro"};
  ui.context.container = new Element("div");
  const received = new Date();
  const observedAt = new Date(received.getTime() - 60 * 1000);
  ui.context.received = received;
  ui.context.result = {observed_at: observedAt.toISOString(),
    rateLimits: {limitId: "codex", primary: {usedPercent: 30, windowDurationMins: 300, resetsAt: null}, secondary: null},
    rateLimitsByLimitId: {codex: {limitId: "codex", primary: {usedPercent: 30, windowDurationMins: 300, resetsAt: null}, secondary: null}}};
  ui.run(`upstreamAccountProvider = "codex"; renderUpstreamQuota(account, container, result, received)`);
  const text = ui.context.container.textContent;
  assert.match(text, /^观测于 .* · Pro最近观测/);
  assert.match(text, /额度取自 CPA 对该账号真实请求响应的记录/);
  assert.equal(ui.context.container.dataset.queriedAt, observedAt.toISOString(), "staleness counts from the observation");

  // A live query (the source-patched image) keeps its wording.
  const live = new Element("div");
  ui.context.live = live;
  ui.context.liveResult = {...ui.context.result};
  delete ui.context.liveResult.observed_at;
  ui.run(`renderUpstreamQuota(account, live, liveResult, received)`);
  assert.match(live.textContent, /^查询于 .* · Pro刚刚查询/);
  assert.doesNotMatch(live.textContent, /CPA 对该账号真实请求响应/);
  ui.run(`clearUpstreamQuotaTimer(account.id)`);
});
