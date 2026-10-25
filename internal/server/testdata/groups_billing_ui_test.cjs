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
  const node = (id) => { if (!nodes.has(id)) { const element = new Element(); element.id = id; nodes.set(id, element); } return nodes.get(id); };
  const form = node("group-form");
  form.elements = Object.fromEntries(["group_id", "name", "limit_usd", "member_limit_usd", "period", "period_count", "custom_days", "starts_at", "reason"].map((name) => [name, new Element("input")]));
  form.reset = () => Object.values(form.elements).forEach((input) => { input.value = ""; });
  node("group-dialog").close = () => {};
  const context = vm.createContext({document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: () => []},
    window: {confirm: () => true}, Intl, URLSearchParams, FormData: class { constructor(form) { this.form = form; } get(name) { return this.form.elements[name]?.value; } }});
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  const originals = run("({groupMutation, syncGroupControls, refreshGroupsAfterWrite})");
  run('state = {user: {id: "owner", role: "owner"}}; openDialog = () => {}; syncGroupControls = () => {}; refreshGroupsAfterWrite = async () => {};');
  const calls = [];
  context.groupMutation = async (_host, url, method, payload) => { calls.push({url, method, payload}); return {id: "group"}; };
  const group = {id: "group", name: "预算组", period: "week", custom_days: 0, limit_usd: "1000", used_usd: "400", remaining_usd: "600", member_limit_usd: "100",
    period_count: 3, current_period_number: 2, expires_at: "2026-10-15T00:00:00Z",
    period_starts_at: "2026-10-01T00:00:00Z", period_ends_at: "2026-10-08T00:00:00Z", model_ids: ["gpt-5.6", "claude-sonnet"]};
  const edit = (cap = "100") => {
    run(`openGroupEditor(${JSON.stringify({...group, member_limit_usd: cap})})`);
    form.elements.reason.value = "调整成员上限";
  };
  const save = () => run('saveGroup({currentTarget: byId("group-form")})');
  return {node, form, context, group, calls, run, edit, save, originals};
}

function memberDashboard() {
  const ui = dashboard();
  Object.assign(ui.context, ui.originals, {crypto: {randomUUID}, sensitiveAction: async (operation) => operation()});
  const form = ui.node("group-members-form");
  form.elements = {reason: new Element("input")};
  form.elements.reason.value = "清空当前群组";
  form.reset = () => { form.elements.reason.value = ""; };
  ui.node("upstream-access-form").reset = () => {};
  const group = {...ui.group, member_count: 2, members: [{user_id: "alice", used_usd: "95", remaining_usd: "5"}, {user_id: "bob", used_usd: "2", remaining_usd: "98"}]};
  const users = [{id: "alice", username: "alice", group_id: "group"}, {id: "bob", username: "bob", group_id: "group"}, {id: "carol", username: "carol", group_id: "other"}];
  ui.run(`managedGroup = ${JSON.stringify(group)}; managedGroups = [managedGroup]; groupUsers = ${JSON.stringify(users)}; renderGroupDetail(managedGroup);`);
  const requests = [], confirmations = [];
  ui.context.window.confirm = (message) => { confirmations.push(message); return true; };
  const emptyGroup = {...group, member_count: 0, members: []};
  ui.context.api = async (url, options) => {
    if (options) { requests.push({url, method: options.method, body: JSON.parse(options.body)}); return emptyGroup; }
    if (url === "/admin/groups") return {groups: [emptyGroup]};
    if (url === "/admin/billing/users") return {users: users.map((user) => user.group_id === "group" ? {...user, group_id: null} : user)};
    return emptyGroup;
  };
  return {...ui, memberForm: form, group, emptyGroup, requests, confirmations, removeAll: () => ui.run("removeAllGroupMembers()")};
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

test("group period count defaults to one and preserves finite or unlimited edits", async () => {
  const ui = dashboard();
  ui.run("openGroupEditor()");
  assert.equal(ui.form.elements.period_count.value, "1");
  ui.form.elements.name.value = "新群组";
  ui.form.elements.limit_usd.value = "100";
  ui.form.elements.reason.value = "新建";
  await ui.save();
  assert.equal(ui.calls.at(-1).payload.period_count, 1);
  ui.edit();
  assert.equal(ui.form.elements.period_count.value, "3");
  await ui.save();
  assert.equal(ui.calls.at(-1).payload.period_count, 3);
  ui.run(`openGroupEditor(${JSON.stringify({...ui.group, period_count: 0, expires_at: null})})`);
  assert.equal(ui.form.elements.period_count.value, "0");
  ui.form.elements.reason.value = "无限期";
  await ui.save();
  assert.equal(ui.calls.at(-1).payload.period_count, 0);
});

test("invalid group period counts and totals below the current period cannot submit", async () => {
  const ui = dashboard(); ui.edit();
  for (const value of ["", "-1", "100", "1.5", "1e1", "01", "Infinity"]) {
    ui.form.elements.period_count.value = value;
    await assert.rejects(ui.save(), /周期数必须/, value);
  }
  ui.run(`managedGroup = ${JSON.stringify(ui.group)}`);
  ui.form.elements.period_count.value = "1";
  await assert.rejects(ui.save(), /不得小于当前第 2 期/);
  assert.equal(ui.calls.length, 0);
  ui.form.elements.period.value = "day";
  await ui.save();
  assert.equal(ui.calls.at(-1).payload.period_count, 1, "an explicit period restart permits a lower total");
});

test("group period display includes final expiry and keeps legacy snapshots finite", () => {
  const ui = dashboard();
  ui.run("Date.now = () => new Date('2026-10-07T00:00:00Z').getTime()");
  ui.run(`renderBillingGroup(${JSON.stringify(ui.group)})`);
  assert.match(ui.node("billing-group").textContent, /生效中.*第 2\/3 期.*最终到期：/);
  ui.run(`managedGroups = [${JSON.stringify({...ui.group, expires_at: "2026-10-07T00:00:00Z"})}]; renderGroupList()`);
  assert.match(ui.node("group-list").textContent, /已到期.*第 2\/3 期.*最终到期：/);
  ui.run(`renderBillingGroup(${JSON.stringify({...ui.group, period_count: 0, expires_at: null})})`);
  assert.match(ui.node("billing-group").textContent, /生效中.*第 2 期 · 无限期.*最终到期：无限期/);
  ui.run(`renderBillingGroup(${JSON.stringify({...ui.group, period_starts_at: "2026-10-08T00:00:00Z"})})`);
  assert.match(ui.node("billing-group").textContent, /待开始/);
  const legacy = {...ui.group, period_ends_at: "2026-10-06T00:00:00Z"};
  delete legacy.period_count; delete legacy.current_period_number; delete legacy.expires_at;
  ui.run(`renderBillingGroup(${JSON.stringify(legacy)})`);
  assert.match(ui.node("billing-group").textContent, /已到期.*第 1\/1 期.*最终到期：/);
  assert.doesNotMatch(ui.node("billing-group").textContent, /无限期/);
});

test("group model ranges render in cards and details and are submitted when selected", async () => {
  const ui = dashboard();
  ui.run(`managedGroups = [${JSON.stringify(ui.group)}]; renderGroupList()`);
  assert.match(ui.node("group-list").textContent, /可用模型：gpt-5\.6、claude-sonnet/);
  ui.run(`managedGroup = ${JSON.stringify(ui.group)}; byId("group-detail-models").textContent = groupModelLabel(managedGroup)`);
  assert.match(ui.node("group-detail-models").textContent, /gpt-5\.6.*claude-sonnet/);
  ui.edit();
  ui.form.dataset.modelIDsConfigured = "true";
  ui.form.dataset.modelIDsLoaded = "true";
  ui.node("group-models").modelPicker = {values: () => ["claude-sonnet"]};
  ui.form.elements.reason.value = "更新模型范围";
  await ui.save();
  assert.deepEqual(ui.calls.at(-1).payload.model_ids, ["claude-sonnet"]);
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

test("remove all members ignores searches and selections and refreshes local membership", async () => {
  const ui = memberDashboard();
  ui.node("group-member-search").value = "alice";
  ui.run('groupSelectedUsers.add("alice"); renderGroupMembers()');
  await ui.removeAll();
  assert.equal(ui.requests.length, 1);
  const request = ui.requests[0];
  assert.equal(request.url, "/admin/groups/group/members");
  assert.equal(request.method, "PUT");
  assert.equal(request.body.action, "remove_all");
  assert.equal(request.body.reason, "清空当前群组");
  assert.equal("user_ids" in request.body, false);
  assert.match(request.body.operation_id, /^[0-9a-f-]{36}$/);
  assert.match(ui.confirmations[0], /预算组.*全部成员.*执行时.*账号和历史用量会保留/);
  assert.equal(ui.run("managedGroup.member_count"), 0);
  assert.equal(ui.run("managedGroups[0].member_count"), 0);
  assert.equal(ui.run("groupSelectedUsers.size"), 0);
  assert.equal(ui.run('groupUsers.find((user) => user.id === "alice").group_id'), null);
  assert.equal(ui.run('groupUsers.find((user) => user.id === "bob").group_id'), null);
  assert.equal(ui.run('groupUsers.find((user) => user.id === "carol").group_id'), "other");
  assert.doesNotMatch(ui.node("group-member-list").textContent, /当前成员/);
  assert.equal(ui.node("group-members-remove-all").disabled, true);
  assert.equal(ui.node("group-archive").disabled, false);
  assert.equal(ui.memberForm.dataset.operationId, undefined);
});

test("remove all requires a reason and confirmation without changing selections on cancellation", async () => {
  const ui = memberDashboard();
  ui.run('groupSelectedUsers.add("alice")');
  ui.memberForm.elements.reason.value = " ";
  await assert.rejects(ui.removeAll(), /必须填写操作原因/);
  assert.equal(ui.confirmations.length, 0);
  ui.memberForm.elements.reason.value = "清空成员";
  ui.context.window.confirm = () => false;
  await ui.removeAll();
  assert.equal(ui.requests.length, 0);
  assert.equal(ui.run("groupSelectedUsers.size"), 1);
  assert.equal(ui.run("managedGroup.member_count"), 2);
});

test("remove all controls and handler reject empty, archived, busy and unavailable groups", async () => {
  for (const change of ["managedGroup.members = []", 'managedGroup.archived_at = "2026-10-01"', "groupOperation = true", 'state.user.role = "member"', "loggingOut = true", "managedGroup = null"]) {
    const ui = memberDashboard();
    assert.equal(ui.node("group-members-remove-all").disabled, false, "no selection is required");
    ui.run(`${change}; syncGroupControls()`);
    assert.equal(ui.node("group-members-remove-all").disabled, true, change);
    await ui.removeAll();
    assert.equal(ui.requests.length, 0, change);
    assert.equal(ui.confirmations.length, 0, change);
  }
  const ui = memberDashboard();
  ui.node("group-members-remove-all").dataset.busy = "true";
  ui.run("syncGroupControls()");
  assert.equal(ui.node("group-members-remove-all").disabled, true, "the button stays disabled while refreshing");
});

test("remove all preserves successful local state when refreshing fails", async () => {
  const ui = memberDashboard();
  const api = ui.context.api;
  ui.context.api = (url, options) => options ? api(url, options) : Promise.reject(new Error("模拟刷新失败"));
  await ui.removeAll();
  assert.equal(ui.requests.length, 1);
  assert.equal(ui.run("managedGroup.members.length"), 0);
  assert.equal(ui.run("managedGroups[0].member_count"), 0);
  assert.equal(ui.run('groupUsers.filter((user) => user.group_id === "group").length'), 0);
  assert.match(ui.node("groups-message").textContent, /已移除.*全部成员.*刷新失败/);
  assert.equal(ui.node("group-members-remove-all").disabled, true);
  assert.equal(ui.node("groups-refresh").disabled, false);
});

test("remove all retries reuse the operation ID and retain members added after the original success", async () => {
  const ui = memberDashboard();
  const requests = [];
  const currentGroup = {...ui.group, member_count: 1, members: [{user_id: "bob", used_usd: "2", remaining_usd: "98"}]};
  ui.context.api = async (url, options) => {
    if (options) {
      requests.push(JSON.parse(options.body));
      if (requests.length === 1) throw new Error("提交成功后响应丢失");
      return ui.emptyGroup;
    }
    if (url === "/admin/groups") return {groups: [currentGroup]};
    if (url === "/admin/billing/users") return {users: [{id: "bob", username: "bob", group_id: "group"}]};
    return currentGroup;
  };
  await assert.rejects(ui.removeAll(), /响应丢失/);
  const operationID = ui.memberForm.dataset.operationId;
  assert.equal(ui.run("groupOperation"), false);
  await ui.removeAll();
  assert.equal(requests.length, 2);
  assert.equal(requests[0].operation_id, operationID);
  assert.equal(requests[1].operation_id, operationID);
  assert.equal(ui.memberForm.dataset.operationId, undefined);
  assert.equal(ui.run("managedGroup.members[0].user_id"), "bob");
  assert.equal(ui.node("group-members-remove-all").disabled, false);
});

test("a late remove-all response cannot restore groups after an identity change", async () => {
  const ui = memberDashboard();
  let resolveWrite;
  ui.context.api = () => new Promise((resolve) => { resolveWrite = resolve; });
  const button = ui.node("group-members-remove-all");
  button.tagName = "button";
  button.textContent = "移除全部成员";
  ui.run('setBusy(byId("group-members-remove-all"), true, "移除中…")');
  const pending = ui.removeAll();
  assert.equal(ui.run("groupOperation"), true);
  await ui.removeAll();
  assert.equal(ui.confirmations.length, 1, "a concurrent duplicate never submits");
  ui.run('identityGeneration++; state = {user: {id: "new-user", role: "member"}}; resetGroupManagement()');
  resolveWrite(ui.emptyGroup);
  await pending;
  assert.equal(ui.run("managedGroup"), null);
  assert.equal(ui.run("managedGroups.length"), 0);
  assert.equal(ui.node("group-member-list").textContent, "");
  assert.equal(ui.node("groups-message").textContent, "");
  assert.equal(button.dataset.busy, "false");
  assert.equal(button.textContent, "移除全部成员");
  assert.equal(ui.memberForm.dataset.operationId, undefined);
});
