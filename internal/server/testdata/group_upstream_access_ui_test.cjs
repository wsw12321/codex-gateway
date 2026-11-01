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
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this._text = ""; this.children = []; this.append(...children); }
  querySelectorAll(selector) {
    return this.children.filter((child) => typeof child !== "string").flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  matches(selector) {
    if (selector.includes(",")) return selector.split(",").some((part) => this.matches(part.trim()));
    const field = selector.match(/^input\[name="(.*)"\]$/);
    if (field) return this.tagName === "input" && this.attributes.name === field[1];
    return selector === this.tagName || selector.startsWith(".") && this.classList.contains(selector.slice(1));
  }
  closest() { return null; }
  focus() {}
  addEventListener(name, handler) { this.listeners.set(name, handler); }
  dispatch(name) { return this.listeners.get(name)?.({currentTarget: this, preventDefault() {}}); }
}

const deferred = () => { let resolve; const promise = new Promise((done) => { resolve = done; }); return {promise, resolve}; };
const group = (id = "g1", extra = {}) => ({id, name: `预算组 ${id}`, members: [], ...extra});
const snapshot = (groupID = "g1") => ({group_id: groupID, providers: [
  {group_id: groupID, provider: "codex", strategy: "all", mode: "all", account_ids: [], accounts: [
    {id: "c1", display_name: "主账号", email_masked: "al***@example.test", status: "available"},
    {id: "c2", display_name: "备用账号", email_masked: "be***@example.test", status: "unavailable"},
  ]},
  {group_id: groupID, provider: "antigravity", strategy: "selected", mode: "selected", account_ids: ["a1"], sync_warning: "upstream_account_sync_unavailable", accounts: [
    {id: "a1", display_name: "Gemini 账号", email_masked: "ge***@example.test", status: "unknown"},
    {id: "a2", display_name: "Gemini 备用", email_masked: "gb***@example.test", status: "available"},
  ]},
  {group_id: groupID, provider: "anthropic", strategy: "selected", mode: "selected", account_ids: [], accounts: [
    {id: "h1", display_name: "Claude 账号", email_masked: "cl***@example.test", status: "available"},
  ]},
]});

function dashboard() {
  const nodes = new Map();
  const node = (id) => { if (!nodes.has(id)) { const element = new Element(); element.id = id; nodes.set(id, element); } return nodes.get(id); };
  for (const id of ["group-form", "group-members-form", "upstream-access-form"]) node(id).reset = () => {};
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), addEventListener() {}, querySelectorAll: () => []},
    window: {setTimeout: () => 0, clearTimeout() {}}, Intl, URLSearchParams, crypto: {randomUUID},
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "owner", role: "owner"}, recently_verified: true};');
  const verifications = [];
  // Mirrors the real helper: the identity is rechecked after verification.
  context.sensitiveAction = async (operation, current) => { verifications.push(current); context.requireCurrentRequest(current); return operation(); };
  const calls = [];
  const respond = async (url, options) => {
    if (!options) return snapshot(decodeURIComponent(url.split("/")[3]));
    const body = JSON.parse(options.body);
    return {group_id: decodeURIComponent(url.split("/")[3]), provider: url.split("/")[5], strategy: body.mode, mode: body.mode, account_ids: body.account_ids};
  };
  context.api = async (url, options) => { calls.push({url, options}); return respond(url, options); };
  const host = () => node("group-upstream-providers");
  const card = (provider = "codex") => host().children.find((child) => child.dataset?.provider === provider);
  const form = (provider = "codex") => card(provider).querySelector("form");
  const edit = (field, value, provider = "codex") => {
    const target = field === "mode" ? form(provider).querySelector("select") :
      field === "search" ? form(provider).querySelector(".upstream-scope-picker").querySelector("input") : form(provider).querySelector(`input[name="${field}"]`);
    target.value = value; target.dispatch(field === "mode" ? "change" : "input");
  };
  const check = (id, selected, provider = "codex") => {
    const input = form(provider).querySelectorAll(".upstream-scope-choice").find((row) => row.dataset.accountId === id).querySelector("input");
    input.checked = selected; input.dispatch("change");
  };
  const select = (value = group()) => { run(`managedGroup = ${JSON.stringify(value)};`); return run("loadGroupUpstreamAccess(managedGroup)"); };
  return {context, run, node, host, card, form, edit, check, select, calls, respond, verifications,
    submit: (provider = "codex") => form(provider).dispatch("submit"),
    writes: () => calls.filter((call) => call.options?.method === "PUT")};
}

test("one aggregate read renders collapsible provider rows with the shared picker", async () => {
  const ui = dashboard(); await ui.select();
  assert.deepEqual(ui.calls.map((call) => call.url), ["/admin/groups/g1/upstream-access"]);
  assert.deepEqual(ui.host().children.map((child) => [child.tagName, child.dataset.provider]), [["details", "codex"], ["details", "antigravity"], ["details", "anthropic"]]);
  assert.match(ui.card("codex").querySelector("summary").textContent, /^Codex全部账号（默认）2 个账号$/);
  assert.match(ui.card("antigravity").querySelector("summary").textContent, /已选 1 个账号/);
  const empty = ui.card("anthropic").querySelector(".group-upstream-scope");
  assert.equal(empty.textContent, "未选择账号");
  assert.equal(empty.dataset.empty, "true");
  assert.match(ui.form("anthropic").querySelector(".upstream-scope-selection").textContent, /由本群组额度支付的该类型请求将被拒绝/);
  assert.match(ui.card("antigravity").textContent, /同步失败.*本地快照/);
  assert.match(ui.form().textContent, /al\*\*\*@example.test.*可用/);
  assert.match(ui.form().textContent, /be\*\*\*@example.test.*不可用/);
  assert.match(ui.form().textContent, /仅约束由本群组额度支付的请求.*用户账号权限/);
  assert.equal(ui.form().classList.contains("group-upstream-form"), true);
  assert.equal(ui.form().classList.contains("user-upstream-form"), false);
  assert.equal(ui.form().dataset.groupId, "g1");
  assert.equal(ui.form().querySelector(".upstream-scope-save").textContent, "保存账号池");
  assert.equal(ui.form().querySelector(".upstream-scope-picker").classList.contains("hidden"), true);
  assert.equal(ui.form().querySelector("select").disabled, false);
});

test("collapsed summary follows the draft and open rows survive a refresh", async () => {
  const ui = dashboard(); await ui.select();
  ui.edit("mode", "selected"); ui.check("c2", true); ui.edit("reason", "keep");
  assert.equal(ui.card().querySelector(".group-upstream-scope").textContent, "已选 1 个账号");
  ui.edit("search", "al***");
  const choices = ui.form().querySelectorAll(".upstream-scope-choice");
  assert.deepEqual(choices.map((choice) => choice.classList.contains("hidden")), [false, true]);
  ui.card().open = true; ui.card().dispatch("toggle");
  await ui.run("loadGroupUpstreamAccess(managedGroup)");
  assert.equal(ui.card().attributes.open, "");
  assert.equal(ui.card("antigravity").attributes.open, undefined);
  assert.equal(ui.form().querySelector("select").value, "selected");
  assert.equal(ui.form().querySelector('input[name="reason"]').value, "keep");
  assert.equal(ui.form().querySelectorAll(".upstream-scope-choice")[1].querySelector("input").checked, true);
});

test("save sends an exact payload through verification and reuses the operation id on retry", async () => {
  const ui = dashboard(); await ui.select();
  ui.edit("mode", "selected"); ui.check("c2", true); ui.check("c1", true); ui.edit("reason", "  团队专用  ");
  let failures = 1;
  ui.context.api = async (url, options) => {
    ui.calls.push({url, options});
    if (options && failures-- > 0) throw new Error("网络请求失败");
    return ui.respond(url, options);
  };
  await ui.submit();
  assert.match(ui.form().textContent, /保存未确认：网络请求失败；输入已保留/);
  assert.equal(ui.form().querySelector('input[name="reason"]').value, "  团队专用  ");
  await ui.submit();
  const [first, second] = ui.writes();
  assert.equal(first.url, "/admin/groups/g1/upstream-accounts/codex");
  const body = JSON.parse(second.options.body);
  assert.deepEqual({...body, operation_id: undefined}, {mode: "selected", account_ids: ["c1", "c2"], reason: "团队专用", operation_id: undefined});
  assert.match(body.operation_id, /^[0-9a-f-]{36}$/);
  assert.equal(JSON.parse(first.options.body).operation_id, body.operation_id);
  assert.equal(ui.verifications.length, 2);
  assert.equal(ui.form().querySelector('input[name="reason"]').value, "");
  assert.match(ui.form().textContent, /账号池已保存；仅影响之后受理的请求/);
  assert.equal(ui.card().querySelector(".group-upstream-scope").textContent, "已选 2 个账号");
  assert.equal(ui.run("groupOperation"), false);
  assert.equal(ui.form().querySelector(".upstream-scope-save").textContent, "保存账号池");
});

test("validation blocks invalid writes and one provider save keeps the other drafts", async () => {
  const ui = dashboard(); await ui.select();
  await ui.submit(); assert.match(ui.form().textContent, /必须填写操作原因/);
  ui.edit("mode", "invalid"); ui.edit("reason", "test"); await ui.submit();
  assert.match(ui.form().textContent, /有效的账号范围/);
  assert.equal(ui.writes().length, 0);
  ui.edit("reason", "Gemini 草稿", "antigravity"); ui.check("a2", true, "antigravity");
  ui.edit("mode", "all"); await ui.submit();
  assert.deepEqual(JSON.parse(ui.writes()[0].options.body).account_ids, []);
  assert.equal(ui.form("antigravity").querySelector('input[name="reason"]').value, "Gemini 草稿");
  assert.equal(ui.card("antigravity").querySelector(".group-upstream-scope").textContent, "已选 2 个账号");
  ui.edit("reason", "清空 Claude", "anthropic"); await ui.submit("anthropic");
  assert.deepEqual(JSON.parse(ui.writes()[1].options.body).account_ids, []);
  assert.equal(JSON.parse(ui.writes()[1].options.body).mode, "selected");
});

test("a save response that differs from the request is reported and keeps the input", async () => {
  const ui = dashboard(); await ui.select();
  ui.edit("mode", "selected"); ui.check("c1", true); ui.edit("reason", "verify");
  ui.context.api = async (url, options) => { ui.calls.push({url, options}); return {...await ui.respond(url, options), account_ids: ["c2"]}; };
  await ui.submit();
  assert.match(ui.form().textContent, /保存结果与提交内容不一致/);
  assert.equal(ui.form().querySelector('input[name="reason"]').value, "verify");
  assert.equal(ui.run('groupUpstreamDrafts.get("codex").dirty'), true);
});

test("late reads for a previous group cannot overwrite the current group and stale forms cannot save", async () => {
  const ui = dashboard();
  const first = deferred(), second = deferred(); let reads = 0;
  ui.context.api = (url, options) => { ui.calls.push({url, options}); return options ? ui.respond(url, options) : ++reads === 1 ? first.promise : second.promise; };
  const old = ui.select(group("g1"));
  const fresh = ui.select(group("g2"));
  const result = snapshot("g2"); result.providers[0].mode = "selected"; result.providers[0].account_ids = ["c2"];
  second.resolve(result); await fresh;
  first.resolve(snapshot("g1")); await old;
  assert.equal(ui.form().dataset.groupId, "g2");
  assert.equal(ui.form().querySelector("select").value, "selected");
  ui.context.api = async (url, options) => { ui.calls.push({url, options}); return ui.respond(url, options); };
  const stale = ui.form();
  await ui.select(group("g3"));
  ui.edit("reason", "new group draft");
  await stale.dispatch("submit");
  assert.equal(ui.writes().length, 0);
});

test("invalid or failed reads fail closed without editable default forms", async () => {
  const ui = dashboard();
  const partial = snapshot(); partial.providers.pop();
  ui.context.api = async () => partial;
  await ui.select();
  assert.equal(ui.host().querySelectorAll("form").length, 0);
  assert.match(ui.host().textContent, /账号池加载失败：账号池响应格式无效/);
  ui.context.api = async () => ({...snapshot("other")});
  await ui.select(group("g2"));
  assert.equal(ui.host().querySelectorAll("form").length, 0);
  ui.context.api = async () => { throw new Error("database unavailable"); };
  await ui.select(group("g3"));
  assert.match(ui.host().textContent, /账号池加载失败：database unavailable/);
  assert.equal(ui.run("groupUpstreamLoading"), false);
});

test("a failed refresh keeps the loaded forms and dirty drafts", async () => {
  const ui = dashboard(); await ui.select();
  ui.edit("reason", "keep me");
  ui.context.api = async () => { throw new Error("temporarily unavailable"); };
  await ui.run("loadGroupUpstreamAccess(managedGroup)");
  assert.equal(ui.host().querySelectorAll("form").length, 3);
  assert.equal(ui.form().querySelector('input[name="reason"]').value, "keep me");
  assert.match(ui.node("groups-message").textContent, /账号池刷新失败：temporarily unavailable/);
  assert.equal(ui.form().querySelector("select").disabled, false);
});

test("archived groups are read-only", async () => {
  const ui = dashboard(); await ui.select(group("g1", {archived_at: "2026-10-01T00:00:00Z"}));
  assert.match(ui.card().textContent, /群组已归档，账号池只读/);
  for (const control of ui.form().querySelectorAll("input, select, button")) assert.equal(control.disabled, true);
  ui.form().querySelector('input[name="reason"]').value = "x";
  ui.run('groupUpstreamDrafts.get("codex").reason = "x"');
  await ui.submit();
  assert.equal(ui.writes().length, 0);
});

test("save freezes pool controls and an identity reset drops a pending save", async () => {
  const ui = dashboard(); await ui.select(); ui.edit("reason", "private");
  const verification = deferred();
  ui.context.sensitiveAction = async (operation, current) => { await verification.promise; ui.context.requireCurrentRequest(current); return operation(); };
  const saving = ui.submit();
  await ui.submit();
  assert.equal(ui.form().querySelector("select").disabled, true);
  assert.equal(ui.form("antigravity").querySelector("select").disabled, true);
  assert.equal(ui.form().querySelector(".upstream-scope-save").textContent, "保存中…");
  assert.equal(ui.form("antigravity").querySelector(".upstream-scope-save").textContent, "保存账号池");
  ui.run('resetGroupManagement(); identityGeneration++; state.user.id = "new-owner";');
  verification.resolve(); await saving;
  assert.equal(ui.writes().length, 0);
  assert.equal(ui.run("groupUpstreamDrafts.size"), 0);
  assert.equal(ui.run("groupUpstreamSaving"), "");
  assert.equal(ui.host().children.length, 0);
});
