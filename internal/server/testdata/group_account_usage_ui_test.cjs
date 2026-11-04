"use strict";

const assert = require("node:assert/strict");
const {randomUUID} = require("node:crypto");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {};
    this.className = ""; this.value = ""; this.disabled = false; this.checked = false; this._text = "";
    this.listeners = new Map();
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => this.classList.toggle(name, true), remove: (name) => this.classList.toggle(name, false),
      toggle: (name, enabled) => {
        const names = new Set(this.className.split(/\s+/).filter(Boolean));
        if (enabled === undefined) enabled = !names.has(name);
        if (enabled) names.add(name); else names.delete(name);
        this.className = [...names].join(" ");
      },
    };
  }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map((child) => typeof child === "string" ? child : child.textContent).join(""); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  removeAttribute(name) { delete this.attributes[name]; }
  // Like the DOM, a null child becomes the text "null"; element() filters it first.
  append(...children) { this.children.push(...children.map((child) => child == null ? String(child) : child)); }
  replaceChildren(...children) { this._text = ""; this.children = []; this.append(...children); }
  querySelectorAll(selector) {
    return this.children.filter((child) => typeof child !== "string").flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  matches(selector) { return selector === this.tagName || selector.startsWith(".") && this.classList.contains(selector.slice(1)); }
  closest() { return null; }
  focus() {}
  addEventListener(name, handler) { this.listeners.set(name, handler); }
  dispatch(name) { return this.listeners.get(name)?.({currentTarget: this, preventDefault() {}}); }
}

const settle = () => new Promise((resolve) => setImmediate(resolve));
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return {promise, resolve, reject}; };

const pools = (claude = ["h1", "h2"]) => [
  {provider: "codex", mode: "all", account_ids: [], accounts: []},
  {provider: "antigravity", mode: "selected", account_ids: ["a1"], accounts: []},
  {provider: "anthropic", mode: claude ? "selected" : "all", account_ids: claude || [], accounts: []},
];

function dashboard({role = "owner", fetch = true} = {}) {
  const nodes = new Map();
  const node = (id) => { if (!nodes.has(id)) { const element = new Element(); element.id = id; nodes.set(id, element); } return nodes.get(id); };
  const usageForm = node("group-account-usage-form");
  usageForm.elements = {enabled: new Element("input"), reason: new Element("input")};
  usageForm.reset = () => { usageForm.elements.enabled.checked = false; usageForm.elements.reason.value = ""; };
  for (const id of ["group-form", "group-members-form", "upstream-access-form"]) node(id).reset = () => {};
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), addEventListener() {}, querySelectorAll: () => []},
    window: {setTimeout: () => 0, clearTimeout() {}}, Intl, URLSearchParams, crypto: {randomUUID},
    ...(fetch ? {fetch: () => { throw new Error("use the api stub"); }} : {}),
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  context.refreshed = [];
  run(`state = {user: {id: "self", username: "self", role: ${JSON.stringify(role)}}, recently_verified: true};
    refreshGroupsAfterWrite = async (id, message) => { refreshed.push({id, message}); };`);
  const calls = [];
  context.groupMutation = async (host, url, method, payload) => { calls.push({url, method, payload}); return context.mutationResult(payload); };
  context.mutationResult = (payload) => ({id: "g1", account_usage_visible: payload.enabled});
  context.api = async (url, options) => { calls.push({url, options}); return context.respond(url, options); };
  context.respond = async () => ({groups: []});
  return {context, run, node, calls, usageForm, effect: () => node("group-account-usage-effect").textContent, refreshed: context.refreshed};
}

function selectGroup(ui, group, rows = pools()) {
  ui.run(`managedGroup = ${JSON.stringify(group)}; groupUpstreamGroupID = ${JSON.stringify(group.id)}; groupUpstreamRows = ${JSON.stringify(rows)}; renderGroupAccountUsage();`);
}

test("the owner toggle defaults off and explains which pools members would see", () => {
  const ui = dashboard();
  selectGroup(ui, {id: "g1", name: "研究组", members: []});
  assert.equal(ui.usageForm.elements.enabled.checked, false);
  assert.equal(ui.effect(), "未开启。开启后成员可查看：Claude（指定 2 个账号）；不展示：Codex（全部账号）、Antigravity（不支持）。");
  assert.equal(ui.node("group-account-usage-effect").dataset.active, "false");

  selectGroup(ui, {id: "g1", name: "研究组", members: [], account_usage_visible: true});
  assert.equal(ui.usageForm.elements.enabled.checked, true);
  assert.match(ui.effect(), /^已开启，成员可查看：Claude（指定 2 个账号）；/);
  assert.equal(ui.node("group-account-usage-effect").dataset.active, "true");

  selectGroup(ui, {id: "g1", name: "研究组", members: [], account_usage_visible: true}, pools(null));
  assert.equal(ui.effect(), "已开启，但当前没有选择“指定账号”的 Codex 或 Claude 账号池，成员看不到任何账号；不展示：Codex（全部账号）、Antigravity（不支持）、Claude（全部账号）。");
  assert.equal(ui.node("group-account-usage-effect").dataset.active, "false");

  selectGroup(ui, {id: "g1", members: []}, pools([]));
  assert.match(ui.effect(), /Claude（未选择账号）/);
  ui.run('groupUpstreamGroupID = "other"; renderGroupAccountUsageEffect();');
  assert.equal(ui.effect(), "正在读取账号池…");
});

test("saving sends the checkbox value with a reason and refreshes the group", async () => {
  const ui = dashboard();
  selectGroup(ui, {id: "g1", members: []});
  ui.usageForm.elements.enabled.checked = true;
  ui.usageForm.dataset.dirty = "true";
  await assert.rejects(ui.run('saveGroupAccountUsage({currentTarget: byId("group-account-usage-form")})'), /必须填写操作原因/);
  assert.equal(ui.calls.length, 0);
  ui.usageForm.elements.reason.value = "  让成员看 Claude 用量  ";
  await ui.run('saveGroupAccountUsage({currentTarget: byId("group-account-usage-form")})');
  assert.deepEqual(JSON.parse(JSON.stringify(ui.calls)), [{url: "/admin/groups/g1/account-usage-visibility", method: "PUT", payload: {enabled: true, reason: "让成员看 Claude 用量"}}]);
  assert.equal(ui.usageForm.elements.reason.value, "");
  assert.equal(ui.usageForm.dataset.dirty, undefined);
  assert.equal(ui.run("managedGroup.account_usage_visible"), true);
  assert.match(ui.effect(), /^已开启/);
  assert.deepEqual(ui.refreshed.map((item) => item.id), ["g1"]);
  assert.match(ui.refreshed[0].message, /已允许成员查看用量/);

  ui.context.mutationResult = () => ({id: "g1", account_usage_visible: true});
  ui.usageForm.elements.enabled.checked = false;
  ui.usageForm.elements.reason.value = "关闭";
  await assert.rejects(ui.run('saveGroupAccountUsage({currentTarget: byId("group-account-usage-form")})'), /保存结果与提交内容不一致/);
  assert.equal(ui.usageForm.elements.reason.value, "关闭");
});

test("an unsaved change survives a refresh of the same group but not a group switch", () => {
  const ui = dashboard();
  selectGroup(ui, {id: "g1", members: []});
  ui.usageForm.elements.enabled.checked = true;
  ui.usageForm.elements.enabled.dispatch("change");
  ui.usageForm.dataset.dirty = "true";
  ui.usageForm.elements.reason.value = "草稿";
  selectGroup(ui, {id: "g1", members: []});
  assert.equal(ui.usageForm.elements.enabled.checked, true);
  assert.equal(ui.usageForm.elements.reason.value, "草稿");
  selectGroup(ui, {id: "g2", members: [], account_usage_visible: false});
  assert.equal(ui.usageForm.elements.enabled.checked, false);
  assert.equal(ui.usageForm.elements.reason.value, "");
  assert.equal(ui.usageForm.dataset.groupId, "g2");
});

test("archived groups and running group operations disable the toggle", async () => {
  const ui = dashboard();
  selectGroup(ui, {id: "g1", members: []});
  assert.equal(ui.usageForm.elements.enabled.disabled, false);
  ui.run("groupOperation = true; syncGroupControls();");
  assert.equal(ui.usageForm.elements.enabled.disabled, true);
  ui.run("groupOperation = false; syncGroupControls();");
  assert.equal(ui.usageForm.elements.enabled.disabled, false);
  selectGroup(ui, {id: "g1", members: [], archived_at: "2026-11-01T00:00:00Z"});
  assert.equal(ui.usageForm.elements.enabled.disabled, true);
  assert.equal(ui.usageForm.elements.reason.disabled, true);
  ui.usageForm.elements.reason.value = "试图修改";
  await ui.run('saveGroupAccountUsage({currentTarget: byId("group-account-usage-form")})');
  assert.equal(ui.calls.length, 0);
});

const usage = {groups: [{group_id: "g1", group_name: "研究组", accounts: [
  {provider: "codex", alias: "主力", source: "live", updated_at: "2026-11-04T06:00:00Z", limited: false,
    five_hour: {used_percent: 42, reset_at: "2026-11-04T08:00:00Z"}, seven_day: {used_percent: 18, reset_at: "2026-11-09T00:00:00Z"}},
  {provider: "anthropic", alias: "", source: "observed", updated_at: "2026-11-04T05:30:00Z", limited: true,
    cooldown_until: "2999-01-01T00:00:00Z", five_hour: {used_percent: 100, reset_at: null}, seven_day: null},
  {provider: "anthropic", alias: "", error_code: "usage_rate_limited", limited: false, five_hour: null, seven_day: null},
  {provider: "anthropic", alias: "", source: "observed", limited: true, five_hour: null, seven_day: {used_percent: 100, reset_at: null}},
]}]};

function billingGroup(id, name = id) {
  return {id, name, limit_usd: "10", used_usd: "1", remaining_usd: "9", period: "day", period_count: 0, current_period_number: 1,
    period_starts_at: "2026-11-04T00:00:00Z", period_ends_at: "2026-11-05T00:00:00Z", model_ids: ["gpt-test"], priority: 10};
}

function renderBilling(ui, groups = [billingGroup("g1", "研究组"), billingGroup("g2", "工程组")]) {
  ui.run(`renderBillingGroup(null, ${JSON.stringify({groups})})`);
}

const section = (ui, id) => ui.node("billing-group").querySelectorAll(".billing-account-usage").find((item) => item.dataset.groupId === id);
const cards = (ui, id) => section(ui, id).querySelectorAll(".billing-account-usage-card");

test("members see their groups' accounts with aliases, windows and status", async () => {
  const ui = dashboard({role: "member"});
  ui.context.respond = async () => usage;
  renderBilling(ui);
  await settle();
  assert.deepEqual(ui.calls.map((call) => call.url), ["/admin/billing/me/account-usage"]);
  assert.equal(section(ui, "g2").classList.contains("hidden"), true);
  assert.equal(section(ui, "g1").classList.contains("hidden"), false);
  const [codex, cooling, failed, limited] = cards(ui, "g1");
  assert.equal(codex.querySelector("strong").textContent, "主力");
  assert.equal(codex.querySelector(".status-badge").textContent, "正常");
  assert.match(codex.textContent, /5 小时已用 42%/);
  assert.match(codex.textContent, /每周已用 18%/);
  assert.match(codex.textContent, /查询于/);
  assert.equal(codex.querySelector("progress").attributes.value, "42");
  assert.equal(cooling.querySelector("strong").textContent, "Claude 账号 1");
  assert.equal(cooling.querySelector(".status-badge").dataset.status, "stale");
  assert.match(cooling.querySelector(".status-badge").textContent, /^冷却至 /);
  assert.match(cooling.textContent, /每周未知/);
  assert.match(cooling.textContent, /观测于/);
  assert.equal(failed.querySelector("strong").textContent, "Claude 账号 2");
  assert.equal(failed.querySelector(".status-badge").textContent, "查询过于频繁，请稍后刷新");
  assert.equal(failed.querySelector(".status-badge").dataset.status, "failed");
  assert.equal(failed.querySelector("progress"), null);
  assert.equal(limited.querySelector(".status-badge").textContent, "已达限额");
  assert.equal(limited.querySelector("strong").textContent, "Claude 账号 3");
  assert.match(limited.textContent, /观测于 暂无观测/);
  assert.match(section(ui, "g1").textContent, /Claude 为最近一次请求时观测到的快照/);
  assert.doesNotMatch(section(ui, "g1").textContent, /h1|0123456789abcdef|null|undefined/);
});

test("a recent result is reused across re-renders and refresh forces a new read", async () => {
  const ui = dashboard({role: "member"});
  ui.context.respond = async () => usage;
  renderBilling(ui);
  await settle();
  renderBilling(ui);
  await settle();
  assert.equal(ui.calls.length, 1);
  assert.equal(cards(ui, "g1").length, 4);
  await section(ui, "g1").querySelector("button").listeners.get("click")();
  await settle();
  assert.equal(ui.calls.length, 2);
});

test("an owner viewing another user's billing never loads account usage", async () => {
  const ui = dashboard({role: "owner"});
  ui.run('billingUserID = "someone-else";');
  renderBilling(ui);
  await settle();
  assert.equal(ui.calls.length, 0);
  ui.run('billingUserID = "self";');
  renderBilling(ui);
  await settle();
  assert.equal(ui.calls.length, 1);
});

test("failures keep previous data or report once when nothing was shown", async () => {
  const ui = dashboard({role: "member"});
  ui.context.respond = async () => { throw new Error("网络请求失败"); };
  renderBilling(ui);
  await settle();
  const status = ui.node("billing-group").querySelector(".billing-account-usage-status");
  assert.equal(status.classList.contains("hidden"), false);
  assert.match(status.textContent, /账号用量刷新失败：网络请求失败/);
  assert.equal(section(ui, "g1").classList.contains("hidden"), true);

  ui.context.respond = async () => usage;
  await ui.run("loadMemberAccountUsage(true)");
  assert.equal(status.classList.contains("hidden"), true);
  ui.context.respond = async () => { throw new Error("网络请求失败"); };
  await ui.run("loadMemberAccountUsage(true)");
  assert.equal(cards(ui, "g1").length, 4);
  assert.match(section(ui, "g1").querySelector(".form-message").textContent, /账号用量刷新失败/);

  ui.context.respond = async () => ({accounts: []});
  await ui.run("loadMemberAccountUsage(true)");
  assert.match(section(ui, "g1").querySelector(".form-message").textContent, /账号用量响应格式无效/);
});

test("a late response cannot render for a different signed-in user", async () => {
  const ui = dashboard({role: "member"});
  const pending = deferred();
  ui.context.respond = () => pending.promise;
  renderBilling(ui);
  ui.run('identityGeneration++; state = {user: {id: "other", role: "member"}}; resetGroupManagement();');
  pending.resolve(usage);
  await settle();
  assert.equal(ui.run("memberAccountUsage"), null);
  assert.equal(ui.node("billing-group").querySelectorAll(".billing-account-usage-card").length, 0);
});

test("lightweight harnesses without fetch render cards without loading usage", () => {
  const ui = dashboard({role: "member", fetch: false});
  renderBilling(ui);
  assert.equal(ui.calls.length, 0);
  assert.equal(section(ui, "g1").classList.contains("hidden"), true);
});
