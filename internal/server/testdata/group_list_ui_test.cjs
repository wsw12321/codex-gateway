"use strict";

// Group list filtering (archived groups hidden by default, name / pinyin
// search) and renaming through the shared group editor, in a stub DOM.
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
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  append(...children) { this.children.push(...children); }
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

const summary = (id, name, extra = {}) => ({id, name, period: "month", custom_days: 0, limit_usd: "100", used_usd: "0", remaining_usd: "100",
  member_limit_usd: null, member_count: 0, period_count: 1, current_period_number: 1, period_starts_at: "2026-10-01T00:00:00Z",
  period_ends_at: "2026-11-01T00:00:00Z", expires_at: "2099-11-01T00:00:00Z", model_ids: [], ...extra});
const groups = () => [
  summary("design", "产品设计团队", {pinyin_full: "chanpinshejituandui", pinyin_initials: "cpsjtd", search_index: "产品设计团队 chanpinshejituandui cpsjtd"}),
  summary("history", "研发历史团队", {archived_at: "2026-09-21T08:00:00Z", pinyin_full: "yanfalishituandui", pinyin_initials: "yflstd"}),
  summary("ops", "Ops Team"),
];

function dashboard(list = groups()) {
  const nodes = new Map();
  const node = (id) => { if (!nodes.has(id)) { const element = new Element(); element.id = id; nodes.set(id, element); } return nodes.get(id); };
  const editor = node("group-form");
  editor.elements = Object.fromEntries(["group_id", "name", "limit_usd", "member_limit_usd", "period", "period_count", "custom_days", "starts_at", "reason"].map((name) => [name, new Element("input")]));
  editor.reset = () => Object.values(editor.elements).forEach((input) => { input.value = ""; });
  const dialogs = {opened: [], closed: []};
  node("group-dialog").close = () => { dialogs.closed.push("group-dialog"); };
  for (const id of ["group-members-form", "upstream-access-form"]) node(id).reset = () => {};
  // bindGroupUI also wires the unrelated group forms; give them inert fields.
  node("upstream-access-form").elements = {mode: new Element("select")};
  node("group-account-usage-form").elements = {enabled: new Element("input")};
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), addEventListener() {}, querySelectorAll: () => []},
    window: {setTimeout: () => 0, clearTimeout() {}, confirm: () => true}, Intl, URLSearchParams, crypto: {randomUUID},
    FormData: class { constructor(form) { this.form = form; } get(name) { return this.form.elements[name]?.value; } },
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "owner", role: "owner"}, recently_verified: true};');
  context.openDialog = (id) => { dialogs.opened.push(id); };
  // The model picker is not under test; leaving it unloaded keeps model_ids omitted.
  context.loadFormModelPicker = async () => {};
  context.sensitiveAction = async (operation) => operation();
  const state = {groups: list, writes: [], reads: []};
  context.api = async (url, options) => {
    if (options) {
      const body = JSON.parse(options.body);
      state.writes.push({url, method: options.method, body});
      const id = decodeURIComponent(url.split("/")[3]);
      const group = state.groups.find((item) => item.id === id);
      const {operation_id: _operation, reason: _reason, ...fields} = body;
      Object.assign(group, fields);
      // The server derives the pinyin index from the current name on every list read.
      if ("name" in fields) for (const key of ["pinyin_full", "pinyin_initials", "search_index"]) delete group[key];
      return {...group, members: []};
    }
    state.reads.push(url);
    if (url === "/admin/groups") return {groups: state.groups.map((group) => ({...group}))};
    if (url === "/admin/billing/users") return {users: []};
    const group = state.groups.find((item) => item.id === decodeURIComponent(url.split("/")[3]));
    return {...group, members: []};
  };
  run("bindGroupUI()");
  const cards = () => node("group-list").querySelectorAll(".group-select");
  const names = () => cards().map((card) => card.children[0].textContent);
  const search = (value) => { node("group-search").value = value; node("group-search").dispatch("input"); };
  const showArchived = async (checked) => { node("group-show-archived").checked = checked; await node("group-show-archived").dispatch("change"); };
  return {node, run, context, state, dialogs, editor, cards, names, search, showArchived, selected: () => run("managedGroup?.id ?? null")};
}

test("archived groups are hidden by default and revealed by the checkbox", async () => {
  const ui = dashboard();
  await ui.run("loadGroups()");
  assert.deepEqual(ui.names(), ["产品设计团队", "Ops Team"]);
  assert.equal(ui.selected(), "design");
  assert.equal(ui.node("group-list-status").textContent, "显示 2 / 3 个群组 · 已隐藏 1 个已归档");
  await ui.showArchived(true);
  assert.deepEqual(ui.names(), ["产品设计团队", "研发历史团队", "Ops Team"]);
  assert.equal(ui.cards()[1].dataset.archived, "true");
  assert.equal(ui.cards()[0].dataset.archived, "false");
  assert.equal(ui.node("group-list-status").textContent, "共 3 个群组 · 含 1 个已归档");
  await ui.showArchived(false);
  assert.deepEqual(ui.names(), ["产品设计团队", "Ops Team"]);
});

test("search matches names, pinyin and initials across every term", async () => {
  const ui = dashboard();
  await ui.run("loadGroups()");
  for (const [query, expected] of [["设计", ["产品设计团队"]], ["CPSJ", ["产品设计团队"]], ["chanpin", ["产品设计团队"]],
    ["ops team", ["Ops Team"]], ["  OPS  ", ["Ops Team"]], ["团队", ["产品设计团队"]], ["", ["产品设计团队", "Ops Team"]]]) {
    ui.search(query);
    assert.deepEqual(ui.names(), expected, query);
  }
  ui.search("yanfa");
  assert.deepEqual(ui.names(), []);
  assert.match(ui.node("group-list").textContent, /没有名称匹配“yanfa”的未归档群组/);
  await ui.showArchived(true);
  assert.deepEqual(ui.names(), ["研发历史团队"], "archived groups are searchable once shown");
  assert.equal(ui.node("group-list-status").textContent, "显示 1 / 3 个群组 · 含 1 个已归档");
  ui.search("不存在");
  assert.match(ui.node("group-list").textContent, /没有名称匹配“不存在”的群组/);
  assert.equal(ui.selected(), "design", "filtering the list keeps the current selection");
});

test("hiding archived groups moves the selection to a visible group", async () => {
  const ui = dashboard();
  await ui.run("loadGroups()");
  await ui.showArchived(true);
  await ui.run('loadGroupDetail("history")');
  assert.equal(ui.selected(), "history");
  assert.equal(ui.node("group-edit").disabled, true, "archived groups cannot be edited or renamed");
  ui.search("ops");
  await ui.showArchived(false);
  assert.equal(ui.selected(), "ops", "the first group still matching the search is chosen");
  assert.equal(ui.node("group-edit").disabled, false);
});

test("a preferred archived group is not reselected while archived groups are hidden", async () => {
  const ui = dashboard();
  await ui.run('loadGroups("history")');
  assert.equal(ui.selected(), "design");
  await ui.showArchived(true);
  await ui.run('loadGroups("history")');
  assert.equal(ui.selected(), "history");
});

test("only archived groups leave an explanatory empty list and no detail", async () => {
  const ui = dashboard([summary("history", "研发历史团队", {archived_at: "2026-09-21T08:00:00Z"})]);
  await ui.run("loadGroups()");
  assert.deepEqual(ui.names(), []);
  assert.equal(ui.selected(), null);
  assert.equal(ui.node("group-detail").classList.contains("hidden"), true);
  assert.match(ui.node("group-list").textContent, /没有未归档的群组。勾选“显示已归档群组”可查看 1 个已归档群组/);
  const empty = dashboard([]);
  await empty.run("loadGroups()");
  assert.match(empty.node("group-list").textContent, /还没有群组/);
  assert.equal(empty.node("group-list-status").textContent, "");
});

test("renaming reuses the group editor and keeps the current settings", async () => {
  const ui = dashboard();
  await ui.run("loadGroups()");
  ui.node("group-edit").dispatch("click");
  assert.deepEqual(ui.dialogs.opened, ["group-dialog"]);
  assert.equal(ui.node("group-dialog-title").textContent, "编辑群组");
  assert.equal(ui.editor.elements.name.value, "产品设计团队");
  ui.editor.elements.name.value = "  设计与体验团队  ";
  ui.editor.elements.reason.value = "部门调整";
  await ui.editor.dispatch("submit");
  assert.equal(ui.state.writes.length, 1);
  const write = ui.state.writes[0];
  assert.equal(write.url, "/admin/groups/design");
  assert.equal(write.method, "PUT");
  assert.equal(write.body.name, "设计与体验团队");
  // Only the name changed: the editor resends the loaded values, never a new start.
  assert.equal(write.body.limit_usd, "100");
  assert.equal(write.body.member_limit_usd, null);
  assert.equal(write.body.period, "month");
  assert.equal(write.body.custom_days, 0);
  assert.equal(write.body.period_count, 1);
  assert.equal("starts_at" in write.body, false);
  assert.equal("model_ids" in write.body, false, "unloaded model picker leaves the model range unchanged");
  assert.deepEqual(ui.dialogs.closed, ["group-dialog"]);
  assert.equal(ui.node("groups-message").textContent, "群组已保存。");
  assert.deepEqual(ui.names(), ["设计与体验团队", "Ops Team"]);
  assert.equal(ui.selected(), "design");
  ui.search("sheji");
  assert.deepEqual(ui.names(), [], "the stale pinyin index of the old name is replaced on reload");
});

test("archived groups never open the editor and identity reset clears filters", async () => {
  const ui = dashboard();
  await ui.run("loadGroups()");
  await ui.showArchived(true);
  await ui.run('loadGroupDetail("history")');
  assert.equal(ui.node("group-edit").disabled, true);
  ui.search("ops");
  ui.run("resetGroupManagement()");
  assert.equal(ui.run("groupListQuery"), "");
  assert.equal(ui.run("groupShowArchived"), false);
  assert.equal(ui.node("group-search").value, "");
  assert.equal(ui.node("group-show-archived").checked, false);
  assert.equal(ui.node("group-list-status").textContent, "");
});

test("search ignores group IDs", async () => {
  const ui = dashboard([summary("6f2a9d5e-0c39-4a5c-9c8e-1f2d3c4b5a69", "研发效能组", {pinyin_full: "yanfaxiaonengzu", pinyin_initials: "yfxnz"})]);
  await ui.run("loadGroups()");
  ui.search("6f2a");
  assert.deepEqual(ui.names(), []);
  ui.search("yfx");
  assert.deepEqual(ui.names(), ["研发效能组"]);
});
