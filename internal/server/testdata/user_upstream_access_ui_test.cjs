"use strict";

const assert = require("node:assert/strict");
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
const user = (id = "member") => ({id, username: id, display_name: "张三", pinyin_full: "zhangsan", pinyin_initials: "zs", status: "active", role: "member"});
const snapshot = (id = "member") => ({user_id: id, providers: [
  {provider: "codex", mode: "all", account_ids: [], accounts: [
    {id: "c1", display_name: "主账号", email_masked: "al***@example.test", status: "available"},
    {id: "c2", display_name: "备用账号", email_masked: "be***@example.test", status: "unavailable"},
  ]},
  {provider: "antigravity", mode: "selected", account_ids: [], sync_warning: "upstream_account_sync_unavailable",
    accounts: [{id: "a1", display_name: "Gemini 账号", email_masked: "ge***@example.test", status: "unknown"}]},
  {provider: "anthropic", mode: "all", account_ids: [], accounts: [{id: "h1", display_name: "Claude 账号", email_masked: "cl***@example.test", status: "available"}]},
]});

function dashboard() {
  const ids = ["user-upstream-user-search", "user-upstream-user-search-results", "user-upstream-user-search-status", "user-upstream-refresh", "user-upstream-cards", "user-upstream-target", "user-upstream-message"];
  const nodes = new Map(ids.map((id) => [id, new Element()]));
  const context = vm.createContext({
    document: {getElementById: (id) => nodes.get(id), createElement: (tag) => new Element(tag), addEventListener() {},
      querySelectorAll: (selector) => [...nodes.values()].flatMap((node) => node.querySelectorAll(selector))},
    window: {setTimeout: () => 0, clearTimeout() {}}, Intl, URLSearchParams,
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "owner", role: "owner"}, recently_verified: true}; bindUserUpstreamAccess();');
  const nativeAPI = context.api;
  const calls = [];
  context.api = async (url, options) => {
    calls.push({url, options});
    if (url === "/admin/billing/users") return {users: [user(), user("second"), {...user("pending"), status: "pending"}]};
    if (!options) return snapshot(decodeURIComponent(url.split("/")[3]));
    const body = JSON.parse(options.body);
    return {user_id: decodeURIComponent(url.split("/")[3]), provider: url.split("/")[5], mode: body.mode, account_ids: body.account_ids};
  };
  const card = (provider = "codex") => nodes.get("user-upstream-cards").children.find((card) => card.dataset?.provider === provider);
  const form = (provider = "codex") => card(provider).querySelector("form");
  const edit = (field, value, provider = "codex") => {
    const node = field === "mode" ? form(provider).querySelector("select") : field === "search" ? form(provider).querySelector(".user-upstream-account-picker").querySelector("input") : form(provider).querySelector(`input[name="${field}"]`);
    node.value = value; node.dispatch(field === "mode" ? "change" : "input");
  };
  const check = (id, selected, provider = "codex") => {
    const node = form(provider).querySelectorAll(".user-upstream-account-choice").find((row) => row.dataset.accountId === id).querySelector("input");
    node.checked = selected; node.dispatch("change");
  };
  return {context, run, nodes, calls, nativeAPI, card, form, edit, check,
    load: async () => { await run("loadUserUpstreamUsers()"); await run('selectUserUpstreamUser(userUpstreamSearch.users[0])'); },
    submit: (provider = "codex") => form(provider).dispatch("submit"), writes: () => calls.filter((call) => call.options?.method === "PUT")};
}

test("owner page reuses pinyin search, excludes pending users, and shows isolated provider rules", async () => {
  const ui = dashboard(); await ui.load();
  assert.equal(ui.run('ownerOnlySections.has("user-upstream-access")'), true);
  assert.equal(ui.run('userUpstreamSearch.users.length'), 2);
  for (const query of ["member", "张三", "zhangsan", "zs"]) assert.ok(ui.run(`matchingUsers(${JSON.stringify(query)}, userUpstreamSearch.users).some(user => user.id === "member")`));
  assert.equal(ui.form().querySelector("select").value, "all");
  assert.match(ui.form().textContent, /自动涵盖今后新增/);
  assert.match(ui.form("antigravity").textContent, /该类型全部禁用/);
  assert.match(ui.card("antigravity").textContent, /同步失败.*本地快照/);
  assert.match(ui.form().textContent, /al\*\*\*@example.test.*可用/);
  assert.match(ui.form().textContent, /be\*\*\*@example.test.*不可用/);
  assert.match(ui.form().textContent, /账号专属授权、模型权限、账号状态、额度及并发限制/);
});

test("provider saves are independent, selected empty disables, and all submits an empty array", async () => {
  const ui = dashboard(); await ui.load();
  ui.edit("mode", "selected"); ui.check("c1", true); ui.check("c2", true); ui.edit("reason", "codex draft");
  ui.edit("reason", "  disable Gemini  ", "antigravity"); await ui.submit("antigravity");
  assert.deepEqual(JSON.parse(ui.writes()[0].options.body), {mode: "selected", account_ids: [], reason: "disable Gemini"});
  assert.match(ui.writes()[0].url, /\/member\/upstream-access\/antigravity$/);
  assert.equal(ui.form().querySelector('input[name="reason"]').value, "codex draft");
  assert.match(ui.form().querySelector(".user-upstream-selection").textContent, /已选 2 个/);
  await ui.submit();
  assert.deepEqual(JSON.parse(ui.writes()[1].options.body).account_ids, ["c1", "c2"]);
  ui.edit("mode", "all"); ui.edit("reason", "restore default"); await ui.submit();
  assert.deepEqual(JSON.parse(ui.writes()[2].options.body).account_ids, []);
  assert.match(ui.form().textContent, /已有会话的下一次请求使用新权限/);
});

test("account filtering and mode toggles retain checked accounts and drafts", async () => {
  const ui = dashboard(); await ui.load();
  ui.edit("mode", "selected"); ui.check("c1", true); ui.edit("reason", "keep"); ui.edit("search", "be***");
  const choices = ui.form().querySelectorAll(".user-upstream-account-choice");
  assert.equal(choices[0].classList.contains("hidden"), true);
  assert.equal(choices[1].classList.contains("hidden"), false);
  ui.edit("mode", "all"); ui.edit("mode", "selected"); ui.edit("search", "");
  assert.equal(choices[0].querySelector("input").checked, true);
  await ui.run("loadUserUpstreamAccess()");
  assert.match(ui.form().querySelector(".user-upstream-selection").textContent, /已选 1 个/);
  assert.equal(ui.form().querySelector('input[name="reason"]').value, "keep");
});

test("validation and role protection prevent invalid writes", async () => {
  const ui = dashboard(); await ui.load();
  await ui.submit(); assert.match(ui.form().textContent, /必须填写操作原因/);
  ui.edit("mode", "invalid"); ui.edit("reason", "test"); await ui.submit();
  assert.match(ui.form().textContent, /有效的账号范围/); assert.equal(ui.writes().length, 0);
  ui.run('state.user.role = "member"; syncUserUpstreamControls();');
  assert.equal(ui.form().querySelector(".user-upstream-save").disabled, true);
  await ui.submit(); assert.equal(ui.writes().length, 0);
});

test("refresh replaces untouched settings and old detached forms cannot save for a new target", async () => {
  const ui = dashboard(); await ui.load();
  const result = snapshot(); result.providers[0].mode = "selected"; result.providers[0].account_ids = ["c2"];
  const original = ui.context.api;
  ui.context.api = async () => result;
  await ui.run("loadUserUpstreamAccess()");
  assert.equal(ui.form().querySelector("select").value, "selected");
  assert.match(ui.form().querySelector(".user-upstream-selection").textContent, /已选 1 个/);
  const staleForm = ui.form();
  ui.context.api = original;
  await ui.run('selectUserUpstreamUser(userUpstreamSearch.users[1])');
  ui.edit("reason", "new target draft");
  await staleForm.dispatch("submit");
  assert.equal(ui.writes().length, 0);
});

test("save freezes the target and one provider, ignores repeat submit, and preserves the other draft", async () => {
  const ui = dashboard(); await ui.load(); ui.edit("reason", "save");
  const verification = deferred();
  ui.context.sensitiveAction = async (operation) => { await verification.promise; return operation(); };
  const saving = ui.submit(); await ui.submit();
  assert.equal(ui.nodes.get("user-upstream-user-search").disabled, true);
  assert.equal(ui.form().querySelector("select").disabled, true);
  assert.equal(ui.form("antigravity").querySelector("select").disabled, false);
  assert.equal(ui.nodes.get("user-upstream-refresh").disabled, true);
  await ui.run('selectUserUpstreamUser(userUpstreamSearch.users[1])');
  assert.equal(ui.run("userUpstreamUser.id"), "member");
  ui.edit("reason", "other draft", "antigravity");
  verification.resolve(); await saving;
  assert.equal(ui.writes().length, 1);
  assert.equal(ui.form("antigravity").querySelector('input[name="reason"]').value, "other draft");
  assert.equal(ui.nodes.get("user-upstream-user-search").disabled, false);
});

test("late GET results cannot overwrite a newly selected user", async () => {
  const ui = dashboard(); await ui.run("loadUserUpstreamUsers()");
  const first = deferred(), second = deferred(); let reads = 0;
  ui.context.api = () => ++reads === 1 ? first.promise : second.promise;
  const old = ui.run('selectUserUpstreamUser(userUpstreamSearch.users[0])');
  const fresh = ui.run('selectUserUpstreamUser(userUpstreamSearch.users[1])');
  const result = snapshot("second"); result.providers[0].mode = "selected";
  second.resolve(result); await fresh;
  first.resolve(snapshot("member")); await old;
  assert.equal(ui.run("userUpstreamUser.id"), "second");
  assert.equal(ui.form().querySelector("select").value, "selected");
});

test("identity reset stops pending authentication before a write and clears private fields", async () => {
  const ui = dashboard(); await ui.load(); ui.edit("reason", "private");
  const verification = deferred(); ui.context.sensitiveAction = async (operation) => { await verification.promise; return operation(); };
  const saving = ui.submit();
  ui.run('resetUserUpstreamAccess(); identityGeneration++; state.user.id = "new-owner";');
  verification.resolve(); await saving;
  assert.equal(ui.writes().length, 0);
  assert.equal(ui.run("userUpstreamDrafts.size"), 0);
  assert.equal(ui.run("userUpstreamOperations.size"), 0);
  assert.equal(ui.nodes.get("user-upstream-target").textContent, "尚未选择用户");
  assert.equal(ui.nodes.get("user-upstream-cards").textContent, "选择用户后设置账号权限。");
});

test("late write responses and stale unauthorized reads cannot restore UI or log out a new identity", async () => {
  const ui = dashboard(); await ui.load(); ui.edit("reason", "save");
  const response = deferred(); ui.context.api = () => response.promise;
  const saving = ui.submit();
  ui.run('resetUserUpstreamAccess(); identityGeneration++; state.user.id = "new-owner";');
  response.resolve({user_id: "member", provider: "codex", mode: "all", account_ids: []}); await saving;
  assert.equal(ui.nodes.get("user-upstream-cards").textContent, "选择用户后设置账号权限。");
  ui.context.api = ui.nativeAPI;
  const read = deferred(); ui.context.fetch = () => read.promise;
  const pending = ui.run('selectUserUpstreamUser({id:"old-target"})');
  ui.run('resetUserUpstreamAccess(); identityGeneration++; state.user.id = "third-owner";');
  read.resolve({status: 401, ok: false, json: async () => ({error: {code: "invalid_session"}})}); await pending;
  assert.equal(ui.run("state.user.id"), "third-owner");
  assert.equal(ui.nodes.get("user-upstream-message").textContent, "");
});

test("save failures retain input and a sync warning does not block saving a local snapshot", async () => {
  const ui = dashboard(); await ui.load(); ui.edit("reason", "retain", "antigravity");
  const original = ui.context.api;
  ui.context.api = async () => { throw new Error("database unavailable"); };
  await ui.submit("antigravity");
  assert.match(ui.form("antigravity").textContent, /database unavailable.*输入已保留/);
  assert.equal(ui.form("antigravity").querySelector('input[name="reason"]').value, "retain");
  ui.context.api = original; await ui.submit("antigravity");
  assert.equal(ui.writes().length, 1);
});

test('Claude account scope starts with all and independently persists an empty selected deny list', async () => {
  const ui = dashboard(); await ui.load();
  assert.match(ui.card('anthropic').textContent, /Claude/);
  assert.equal(ui.form('anthropic').querySelector('select').value, 'all');
  ui.edit('mode', 'selected', 'anthropic'); ui.edit('reason', 'restrict Claude', 'anthropic');
  await ui.submit('anthropic');
  assert.match(ui.writes().at(-1).url, /\/upstream-access\/anthropic$/);
  assert.deepEqual(JSON.parse(ui.writes().at(-1).options.body), {mode: 'selected', account_ids: [], reason: 'restrict Claude'});
});
