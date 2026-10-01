"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {};
    this.className = ""; this.value = ""; this.disabled = false; this._text = "";
    this.listeners = new Map();
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
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map((child) => typeof child === "string" ? child : child.textContent).join(""); }
  get childNodes() {
    return [...(this._text ? [this._text] : []), ...this.children].map((child) => {
      if (typeof child !== "string") return child;
      const text = new Element("#text"); text._text = child; return text;
    });
  }
  cloneNode(deep = false) {
    const clone = new Element(this.tagName);
    clone.className = this.className; clone._text = this._text; clone.value = this.value; clone.disabled = this.disabled;
    clone.dataset = {...this.dataset}; clone.attributes = {...this.attributes};
    if (deep) clone.append(...this.children.map((child) => typeof child === "string" ? child : child.cloneNode(true)));
    return clone;
  }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this._text = ""; this.children = []; this.append(...children); }
  querySelectorAll(selector) {
    return this.children.filter((child) => typeof child !== "string").flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  matches(selector) { return selector === this.tagName || selector.startsWith(".") && this.classList.contains(selector.slice(1)); }
  closest() { return null; }
  addEventListener(name, handler) { this.listeners.set(name, handler); }
  dispatch(name) { return this.listeners.get(name)?.({currentTarget: this, preventDefault() {}}); }
}

function dashboard() {
  const nodes = new Map();
  const node = (id) => { if (!nodes.has(id)) nodes.set(id, new Element()); return nodes.get(id); };
  const form = node("group-form");
  form.elements = Object.fromEntries(["group_id", "name", "limit_usd", "member_limit_usd", "period", "custom_days", "starts_at", "reason"].map((name) => [name, new Element("input")]));
  form.reset = () => Object.values(form.elements).forEach((input) => { input.value = ""; });
  node("group-dialog").close = () => {};
  const context = vm.createContext({document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: () => []},
    window: {confirm: () => true}, Intl, URLSearchParams, FormData: class { constructor(form) { this.form = form; } get(name) { return this.form.elements[name]?.value; } }});
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "owner", role: "owner"}}; openDialog = () => {}; syncGroupControls = () => {}; refreshGroupsAfterWrite = async () => {};');
  const calls = [];
  context.groupMutation = async (_host, url, method, payload) => { calls.push({url, method, payload}); return {id: "group"}; };
  const group = {id: "group", name: "预算组", period: "week", custom_days: 0, limit_usd: "1000", used_usd: "400", remaining_usd: "600", member_limit_usd: "100",
    period_starts_at: "2026-10-01T00:00:00Z", period_ends_at: "2026-10-08T00:00:00Z"};
  const edit = (cap = "100") => {
    run(`openGroupEditor(${JSON.stringify({...group, member_limit_usd: cap})})`);
    form.elements.reason.value = "调整成员上限";
  };
  const save = () => run('saveGroup({currentTarget: byId("group-form")})');
  return {node, form, context, group, calls, run, edit, save};
}

test("group editor preserves nullable, zero and exact decimal member caps", () => {
  const ui = dashboard();
  for (const [cap, shown] of [[null, ""], ["0.000000", "0"], ["12.340000", "12.34"]]) {
    ui.edit(cap);
    assert.equal(ui.form.elements.member_limit_usd.value, shown);
  }
});

test("blank clears a member cap and zero explicitly disables group funding", async () => {
  const ui = dashboard(); ui.edit();
  ui.form.elements.member_limit_usd.value = "  "; await ui.save();
  assert.equal(ui.calls.at(-1).method, "PUT");
  assert.equal(ui.calls.at(-1).payload.member_limit_usd, null);
  ui.form.elements.member_limit_usd.value = "0"; await ui.save();
  assert.equal(ui.calls.at(-1).payload.member_limit_usd, "0");
  ui.form.elements.member_limit_usd.value = "999999999999999999.123456"; await ui.save();
  assert.equal(ui.calls.at(-1).payload.member_limit_usd, "999999999999999999.123456");
  ui.form.elements.group_id.value = "";
  ui.form.elements.member_limit_usd.value = ""; await ui.save();
  assert.equal(ui.calls.at(-1).method, "POST");
  assert.equal(ui.calls.at(-1).payload.member_limit_usd, null);
});

test("invalid member caps never submit a group operation", async () => {
  const ui = dashboard(); ui.edit();
  for (const value of ["-1", "1e2", "Infinity", ".5", "1.", "01", "1000000000000000000", "0.0000001"]) {
    ui.form.elements.member_limit_usd.value = value;
    await assert.rejects(ui.save(), /成员上限必须/, value);
  }
  assert.equal(ui.calls.length, 0);
});

test("personal group summary uses the selected user's independent counters", () => {
  const ui = dashboard();
  ui.run(`renderBillingGroup(${JSON.stringify({...ui.group, members: [{display_name: "其他成员秘密", used_usd: "77"}]})}, {group_member_used_usd: "95", group_member_remaining_usd: "5"})`);
  const text = ui.node("billing-group").textContent;
  assert.match(text, /群组已用(?:US)?\$400/);
  assert.match(text, /本用户本期群组已用(?:US)?\$95/);
  assert.match(text, /本用户本期上限剩余(?:US)?\$5/);
  assert.doesNotMatch(text, /其他成员秘密/);
  assert.match(text, /群组优先支付/);
  ui.run(`renderBillingGroup(${JSON.stringify({...ui.group, member_limit_usd: null})}, {group_member_used_usd: "95", group_member_remaining_usd: null})`);
  assert.match(ui.node("billing-group").textContent, /单个成员周期上限不限/);
  assert.match(ui.node("billing-group").textContent, /本用户本期上限剩余不限/);
  ui.run("renderBillingGroup(null)");
  assert.equal(ui.node("billing-group").textContent, "");
  assert.equal(ui.node("billing-group").classList.contains("hidden"), true);
});

test("group members show their own group payment and cap remaining", () => {
  const ui = dashboard();
  ui.run(`managedGroup = ${JSON.stringify({...ui.group, members: [{user_id: "alice", used_usd: "95", remaining_usd: "5"}, {user_id: "bob", used_usd: "2", remaining_usd: "98"}]})};
    groupUsers = [{id: "alice", username: "alice", group_id: "group"}, {id: "bob", username: "bob", group_id: "group"}]; renderGroupMembers();`);
  const rows = ui.node("group-member-list").children;
  assert.match(rows[0].textContent, /本期群组已用 (?:US)?\$95.*上限剩余 (?:US)?\$5/);
  assert.match(rows[1].textContent, /本期群组已用 (?:US)?\$2.*上限剩余 (?:US)?\$98/);
});

test("usage ledger displays group and personal splits including zero funding", () => {
  const ui = dashboard();
  ui.run(`renderBillingLedger({ledger_entries: [
    {entry_type: "usage_charge", actual_cost_usd: "20", charged_usd: "18", group_charged_usd: "5", personal_charged_usd: "13", uncovered_usd: "2"},
    {entry_type: "usage_charge", actual_cost_usd: "20", charged_usd: "20", group_charged_usd: "20", personal_charged_usd: "0", uncovered_usd: "0"},
    {entry_type: "cash_adjustment", amount_usd: "10", group_charged_usd: null, personal_charged_usd: null}
  ]})`);
  const rows = ui.node("billing-ledger-rows").children;
  assert.match(rows[0].textContent, /实际成本：(?:US)?\$20.*群组支付：(?:US)?\$5.*个人支付：(?:US)?\$13.*未覆盖：(?:US)?\$2/);
  assert.match(rows[1].textContent, /群组支付：(?:US)?\$20.*个人支付：(?:US)?\$0/);
  assert.doesNotMatch(rows[2].textContent, /群组支付|个人支付/);
});
