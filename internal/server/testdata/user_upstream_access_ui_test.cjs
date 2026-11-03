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
    return selector === this.tagName || selector.startsWith(".") && this.classList.contains(selector.slice(1));
  }
  closest() { return null; }
  focus() {}
  addEventListener(name, handler) { this.listeners.set(name, handler); }
  dispatch(name) { return this.listeners.get(name)?.({currentTarget: this, preventDefault() {}}); }
}

const deferred = () => { let resolve; const promise = new Promise((done) => { resolve = done; }); return {promise, resolve}; };
const user = (id, display, pinyin, initials) => ({id, username: id, display_name: display, pinyin_full: pinyin, pinyin_initials: initials, status: "active", role: "member"});
const users = () => [user("member", "张三", "zhangsan", "zs"), user("second", "李四", "lisi", "ls"), {...user("pending", "王五", "wangwu", "ww"), status: "pending"}];
const snapshot = () => ({
  providers: [
    {provider: "codex", accounts: [
      {id: "c1", display_name: "主账号", email_masked: "al***@example.test", status: "available"},
      {id: "c2", display_name: "备用账号", alias: "冷备", email_masked: "be***@example.test", status: "unavailable"},
    ]},
    {provider: "antigravity", sync_warning: "upstream_account_sync_unavailable",
      accounts: [{id: "a1", display_name: "Gemini 账号", email_masked: "ge***@example.test", status: "unknown"}]},
    {provider: "anthropic", accounts: [{id: "h1", display_name: "Claude 账号", email_masked: "cl***@example.test", status: "available"}]},
  ],
  rules: [
    {user_id: "second", provider: "codex", mode: "selected", account_ids: ["c2"]},
    {user_id: "second", provider: "anthropic", mode: "selected", account_ids: []},
  ],
});

// An independent model of the server batch semantics used by the PUT stub.
function serverApply(data, body) {
  const owners = new Map(data.providers.flatMap((row) => row.accounts.map((account) => [account.id, row.provider])));
  const providers = body.action === "all" ? body.providers : [...new Set(body.account_ids.map((id) => owners.get(id)))].sort();
  const access = [];
  let changed = 0;
  for (const userID of body.user_ids) {
    for (const provider of providers) {
      const index = data.rules.findIndex((rule) => rule.user_id === userID && rule.provider === provider);
      const previous = index >= 0 ? data.rules[index] : {user_id: userID, provider, mode: "all", account_ids: []};
      const chosen = body.account_ids.filter((id) => owners.get(id) === provider);
      const registered = data.providers.find((row) => row.provider === provider).accounts.map((account) => account.id);
      let next = {user_id: userID, provider, mode: "selected", account_ids: []};
      if (body.action === "all" || body.action === "enable" && previous.mode === "all") next.mode = "all";
      else if (body.action === "enable") next.account_ids = [...new Set([...previous.account_ids, ...chosen])].sort();
      else next.account_ids = (previous.mode === "all" ? registered : previous.account_ids).filter((id) => !chosen.includes(id)).sort();
      if (next.mode !== previous.mode || next.account_ids.join() !== previous.account_ids.join()) {
        changed++;
        if (index >= 0) data.rules[index] = next; else data.rules.push(next);
      } else next = previous;
      access.push(next);
    }
  }
  return {access, target_count: access.length, changed_count: changed, converted_count: 0};
}

function dashboard() {
  const ids = ["user-upstream-refresh", "user-upstream-message", "user-upstream-user-search", "user-upstream-user-select-all",
    "user-upstream-user-clear", "user-upstream-user-count", "user-upstream-user-list", "user-upstream-account-search",
    "user-upstream-account-select-all", "user-upstream-account-clear", "user-upstream-account-count", "user-upstream-account-list",
    "user-upstream-matrix", "user-upstream-action", "user-upstream-providers", "user-upstream-provider-codex",
    "user-upstream-provider-antigravity", "user-upstream-provider-anthropic", "user-upstream-preview", "user-upstream-reason", "user-upstream-submit"];
  const nodes = new Map(ids.map((id) => [id, new Element(id.includes("search") || id.includes("select-all") || id.includes("provider-") || id.endsWith("reason") ? "input" : "div")]));
  const form = new Element("form");
  const formMessage = new Element("p"); formMessage.className = "form-message hidden";
  form.append(formMessage);
  nodes.set("user-upstream-batch-form", form);
  nodes.get("user-upstream-action").value = "enable";
  const confirms = [];
  const context = vm.createContext({
    document: {getElementById: (id) => nodes.get(id), createElement: (tag) => new Element(tag), addEventListener() {},
      querySelectorAll: (selector) => [...nodes.values()].flatMap((node) => node.querySelectorAll(selector))},
    window: {setTimeout: () => 0, clearTimeout() {}, confirm: (message) => { confirms.push(message); return true; }}, Intl, URLSearchParams,
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  // Values from the VM realm have foreign prototypes; compare them as JSON.
  const json = (code) => JSON.parse(run(`JSON.stringify(${code})`));
  run('state = {user: {id: "owner", role: "owner"}, recently_verified: true}; bindUserUpstreamAccess();');
  const nativeAPI = context.api;
  const calls = [];
  const data = snapshot();
  context.api = async (url, options) => {
    calls.push({url, options});
    if (url === "/admin/billing/users") return {users: users()};
    if (url === "/admin/upstream-access/users" && !options) return JSON.parse(JSON.stringify(data));
    if (url === "/admin/upstream-access/users" && options.method === "PUT") return serverApply(data, JSON.parse(options.body));
    throw new Error(`unexpected request ${url}`);
  };
  const node = (id) => nodes.get(id);
  const userRow = (id) => node("user-upstream-user-list").querySelectorAll(".user-upstream-user").find((row) => row.dataset.userId === id);
  const accountRow = (id) => node("user-upstream-account-list").querySelectorAll(".user-upstream-account").find((row) => row.dataset.accountId === id);
  const toggle = (row, checked = true) => { const input = row.querySelector("input"); input.checked = checked; input.dispatch("change"); };
  const set = (id, value) => { node(id).value = value; node(id).dispatch(id === "user-upstream-action" ? "change" : "input"); };
  const check = (id, checked = true) => { node(id).checked = checked; node(id).dispatch(id.includes("clear") ? "click" : "change"); };
  return {context, run, json, nodes, node, calls, data, confirms, nativeAPI, form, formMessage, userRow, accountRow, toggle, set, check,
    selectUsers: (...ids) => ids.forEach((id) => toggle(userRow(id))),
    selectAccounts: (...ids) => ids.forEach((id) => toggle(accountRow(id))),
    load: () => run("loadUserUpstreamAccess()"),
    submit: () => form.dispatch("submit"),
    writes: () => calls.filter((call) => call.options?.method === "PUT")};
}

test("owner page loads users and accounts once, excludes pending users, and keeps pinyin search", async () => {
  const ui = dashboard(); await ui.load();
  assert.equal(ui.run('ownerOnlySections.has("user-upstream-access")'), true);
  assert.deepEqual(ui.calls.map((call) => call.url), ["/admin/billing/users", "/admin/upstream-access/users"]);
  assert.equal(ui.run("userUpstreamUsers.length"), 2);
  assert.equal(ui.userRow("pending"), undefined);
  for (const query of ["member", "张三", "zhangsan", "zs"]) {
    ui.set("user-upstream-user-search", query);
    assert.ok(ui.userRow("member"), query);
    assert.equal(ui.userRow("second"), undefined, query);
  }
  ui.set("user-upstream-user-search", "");
  assert.match(ui.userRow("member").textContent, /Codex 全部 · Antigravity 全部 · Claude 全部/);
  assert.match(ui.userRow("second").textContent, /Codex 1\/2 · Antigravity 全部 · Claude 已禁用/);
  const groups = ui.node("user-upstream-account-list").querySelectorAll(".user-upstream-account-group");
  assert.deepEqual(groups.map((group) => group.dataset.provider), ["codex", "antigravity", "anthropic"]);
  assert.match(groups[1].textContent, /同步失败.*本地快照/);
  assert.match(ui.accountRow("c1").textContent, /al\*\*\*@example.test.*可用/);
  assert.match(ui.accountRow("c2").textContent, /be\*\*\*@example.test.*不可用/);
  assert.equal(ui.node("user-upstream-submit").disabled, true);
  assert.equal(ui.node("user-upstream-preview").textContent, "请先勾选用户。");
});

test("the batch plan mirrors every server rule", async () => {
  const ui = dashboard(); await ui.load();
  const plan = (users, action, accounts, providers = []) => JSON.parse(ui.run(`JSON.stringify(userUpstreamBatchPlan(userUpstreamSnapshot, ${JSON.stringify(users)}, ${JSON.stringify(action)}, ${JSON.stringify(accounts)}, ${JSON.stringify(providers)}))`));
  const next = (result) => Object.fromEntries(result.changes.map((change) => [`${change.user_id}:${change.provider}`, change.next]));
  let result = plan(["member", "second"], "enable", ["c1"]);
  assert.deepEqual([result.changes.length, result.unchanged, result.converted, result.emptied], [1, 1, 0, 0]);
  assert.deepEqual(next(result), {"second:codex": {mode: "selected", account_ids: ["c1", "c2"]}});
  result = plan(["member"], "disable", ["c1"]);
  assert.deepEqual(next(result), {"member:codex": {mode: "selected", account_ids: ["c2"]}});
  assert.equal(result.converted, 1);
  result = plan(["member"], "disable", ["c1", "c2"]);
  assert.deepEqual([result.converted, result.emptied], [1, 1]);
  assert.deepEqual(next(result)["member:codex"], {mode: "selected", account_ids: []});
  result = plan(["second"], "disable", ["c2"]);
  assert.deepEqual([result.converted, result.emptied], [0, 1]);
  result = plan(["second"], "disable", ["a1", "h1"]);
  assert.deepEqual(result.providers, ["antigravity", "anthropic"]);
  assert.deepEqual([result.changes.length, result.unchanged, result.converted, result.emptied], [1, 1, 1, 1]);
  result = plan(["member", "second"], "all", [], ["codex", "anthropic"]);
  assert.deepEqual(Object.keys(next(result)).sort(), ["second:anthropic", "second:codex"]);
  assert.equal(result.unchanged, 2);
});

test("selections survive searches, select-all applies to matches, and the matrix shows current access", async () => {
  const ui = dashboard(); await ui.load();
  ui.set("user-upstream-user-search", "zs");
  ui.check("user-upstream-user-select-all");
  assert.deepEqual(ui.json("[...userUpstreamSelectedUsers]"), ["member"]);
  ui.set("user-upstream-user-search", "");
  assert.equal(ui.node("user-upstream-user-select-all").indeterminate, true);
  ui.selectUsers("second");
  assert.equal(ui.node("user-upstream-user-count").textContent, "已选 2 人");
  ui.set("user-upstream-account-search", "be***");
  assert.equal(ui.accountRow("c1"), undefined);
  ui.check("user-upstream-account-select-all");
  ui.set("user-upstream-account-search", "");
  assert.deepEqual(ui.json("[...userUpstreamSelectedAccounts]"), ["c2"]);
  ui.selectAccounts("h1");
  assert.equal(ui.node("user-upstream-account-count").textContent, "已选 2 个账号");
  assert.match(ui.accountRow("c2").textContent, /已允许 2 \/ 2 人/);
  assert.match(ui.accountRow("h1").textContent, /已允许 1 \/ 2 人/);
  const cells = ui.node("user-upstream-matrix").querySelectorAll(".user-upstream-cell").map((cell) => cell.textContent);
  assert.deepEqual(cells, ["允许（全部）", "允许（全部）", "允许", "禁用"]);
  ui.check("user-upstream-user-clear");
  assert.equal(ui.run("userUpstreamSelectedUsers.size"), 0);
  assert.match(ui.node("user-upstream-matrix").textContent, /勾选用户和账号后显示当前权限/);
});

test("batch disable freezes the payload, confirms the conversion, and applies the server result", async () => {
  const ui = dashboard(); await ui.load();
  ui.selectUsers("second", "member"); ui.selectAccounts("c1", "a1");
  ui.set("user-upstream-action", "disable");
  assert.match(ui.node("user-upstream-preview").textContent, /将修改 3 项设置（2 人）；其中 3 项将从「全部账号」改为「仅指定账号」，今后新增账号不会自动加入；2 项将禁用该类型全部账号；1 项已是目标状态，将跳过。/);
  assert.equal(ui.node("user-upstream-preview").dataset.warning, "true");
  ui.node("user-upstream-reason").value = "  retire primary  ";
  ui.context.window.confirm = (message) => { ui.confirms.push(message); return false; };
  await ui.submit();
  assert.equal(ui.writes().length, 0);
  assert.match(ui.confirms[0], /今后新增账号不会自动加入[\s\S]*确定继续/);
  ui.context.window.confirm = () => true;
  const verification = deferred();
  ui.context.sensitiveAction = async (operation) => { await verification.promise; return operation(); };
  const saving = ui.submit(); await ui.submit();
  assert.equal(ui.node("user-upstream-submit").disabled, true);
  assert.equal(ui.node("user-upstream-submit").textContent, "保存中…");
  assert.equal(ui.node("user-upstream-refresh").disabled, true);
  assert.equal(ui.userRow("member").querySelector("input").disabled, true);
  ui.toggle(ui.accountRow("c2"));
  assert.equal(ui.run("userUpstreamSelectedAccounts.has('c2')"), false);
  verification.resolve(); await saving;
  assert.equal(ui.writes().length, 1);
  assert.deepEqual(JSON.parse(ui.writes()[0].options.body), {user_ids: ["member", "second"], action: "disable", account_ids: ["a1", "c1"], providers: [], reason: "retire primary"});
  assert.deepEqual(ui.json("userUpstreamRule(userUpstreamSnapshot, 'member', 'codex')"), {mode: "selected", account_ids: ["c2"]});
  assert.match(ui.formMessage.textContent, /已更新 3 项设置；已有会话的下一次请求使用新权限/);
  assert.equal(ui.node("user-upstream-reason").value, "");
  assert.match(ui.userRow("member").textContent, /Codex 1\/2 · Antigravity 已禁用 · Claude 全部/);
  const cells = ui.node("user-upstream-matrix").querySelectorAll(".user-upstream-cell").map((cell) => cell.textContent);
  assert.deepEqual(cells, ["禁用", "禁用", "禁用", "禁用"]);
  assert.equal(ui.node("user-upstream-preview").textContent, "所选用户均已是目标状态，无需修改。");
});

test("restore all requires a provider and sends only providers", async () => {
  const ui = dashboard(); await ui.load();
  ui.selectUsers("second"); ui.selectAccounts("c1");
  ui.set("user-upstream-action", "all");
  assert.equal(ui.node("user-upstream-providers").classList.contains("hidden"), false);
  assert.equal(ui.node("user-upstream-preview").textContent, "请选择要恢复的账号类型。");
  assert.equal(ui.node("user-upstream-submit").disabled, true);
  ui.check("user-upstream-provider-anthropic"); ui.check("user-upstream-provider-codex");
  assert.match(ui.node("user-upstream-preview").textContent, /将修改 2 项设置（1 人）。/);
  ui.node("user-upstream-reason").value = "restore default";
  await ui.submit();
  assert.equal(ui.confirms.length, 0);
  assert.deepEqual(JSON.parse(ui.writes()[0].options.body), {user_ids: ["second"], action: "all", account_ids: [], providers: ["anthropic", "codex"], reason: "restore default"});
  assert.match(ui.userRow("second").textContent, /Codex 全部 · Antigravity 全部 · Claude 全部/);
  ui.set("user-upstream-action", "enable");
  assert.equal(ui.node("user-upstream-providers").classList.contains("hidden"), true);
});

test("no-op batches, missing reasons, and non-owners cannot write", async () => {
  const ui = dashboard(); await ui.load();
  ui.selectUsers("member"); ui.selectAccounts("c1");
  assert.equal(ui.node("user-upstream-preview").textContent, "所选用户均已是目标状态，无需修改。");
  assert.equal(ui.node("user-upstream-submit").disabled, true);
  await ui.submit(); assert.equal(ui.writes().length, 0);
  ui.set("user-upstream-action", "disable");
  ui.node("user-upstream-reason").value = "  ";
  await ui.submit();
  assert.match(ui.formMessage.textContent, /必须填写操作原因/);
  assert.equal(ui.writes().length, 0);
  ui.node("user-upstream-action").value = "invalid";
  ui.node("user-upstream-reason").value = "test";
  await ui.submit();
  assert.match(ui.formMessage.textContent, /有效的批量操作/);
  ui.node("user-upstream-action").value = "disable";
  ui.run('state.user.role = "member"; syncUserUpstreamControls();');
  assert.equal(ui.node("user-upstream-submit").disabled, true);
  assert.equal(ui.node("user-upstream-user-search").disabled, true);
  await ui.submit(); assert.equal(ui.writes().length, 0);
});

test("late reads cannot overwrite a newer load, and refresh prunes vanished selections", async () => {
  const ui = dashboard();
  const first = deferred(); let reads = 0;
  const original = ui.context.api;
  ui.context.api = (url, options) => ++reads <= 2 ? first.promise.then(() => original(url, options)) : original(url, options);
  const old = ui.load();
  ui.data.rules = [];
  await ui.load();
  ui.data.rules = [{user_id: "member", provider: "codex", mode: "selected", account_ids: []}];
  first.resolve(); await old;
  assert.match(ui.userRow("second").textContent, /Codex 全部/);
  assert.match(ui.userRow("member").textContent, /Codex 全部/);
  ui.selectUsers("member", "second"); ui.selectAccounts("c1", "a1");
  ui.data.providers[1].accounts = [];
  ui.context.api = async (url, options) => url === "/admin/billing/users" ? {users: [user("member", "张三", "zhangsan", "zs")]} : original(url, options);
  await ui.run("loadUserUpstreamAccess()");
  assert.deepEqual(ui.json("[...userUpstreamSelectedUsers]"), ["member"]);
  assert.deepEqual(ui.json("[...userUpstreamSelectedAccounts]"), ["c1"]);
  assert.match(ui.node("user-upstream-message").textContent, /已从选择中移除/);
  ui.context.api = async () => { throw new Error("database unavailable"); };
  await ui.run("loadUserUpstreamAccess()");
  assert.match(ui.node("user-upstream-message").textContent, /database unavailable；已保留上次加载的数据和选择/);
  assert.deepEqual(ui.json("[...userUpstreamSelectedUsers]"), ["member"]);
});

test("identity reset during verification prevents the write and clears private state", async () => {
  const ui = dashboard(); await ui.load();
  ui.selectUsers("member"); ui.selectAccounts("c1"); ui.set("user-upstream-action", "disable");
  ui.node("user-upstream-reason").value = "private";
  const verification = deferred(); ui.context.sensitiveAction = async (operation) => { await verification.promise; return operation(); };
  const saving = ui.submit();
  ui.run('resetUserUpstreamAccess(); identityGeneration++; state.user.id = "new-owner";');
  verification.resolve(); await saving;
  assert.equal(ui.writes().length, 0);
  assert.equal(ui.run("userUpstreamSelectedUsers.size + userUpstreamSelectedAccounts.size"), 0);
  assert.equal(ui.run("userUpstreamSnapshot"), null);
  assert.equal(ui.node("user-upstream-reason").value, "");
  assert.equal(ui.node("user-upstream-action").value, "enable");
  assert.equal(ui.node("user-upstream-user-list").textContent, "打开页面后加载用户。");
  assert.equal(ui.node("user-upstream-matrix").textContent, "勾选用户和账号后显示当前权限。");
});

test("late write responses and stale unauthorized reads cannot restore UI or log out a new identity", async () => {
  const ui = dashboard(); await ui.load();
  ui.selectUsers("member"); ui.selectAccounts("c1"); ui.set("user-upstream-action", "disable");
  ui.node("user-upstream-reason").value = "save";
  const response = deferred(); ui.context.api = () => response.promise;
  const saving = ui.submit();
  ui.run('resetUserUpstreamAccess(); identityGeneration++; state.user.id = "new-owner";');
  response.resolve({access: [{user_id: "member", provider: "codex", mode: "selected", account_ids: ["c2"]}], changed_count: 1}); await saving;
  assert.equal(ui.run("userUpstreamSnapshot"), null);
  assert.equal(ui.formMessage.textContent, "");
  ui.context.api = ui.nativeAPI;
  const read = deferred(); ui.context.fetch = () => read.promise;
  const pending = ui.load();
  ui.run('resetUserUpstreamAccess(); identityGeneration++; state.user.id = "third-owner";');
  read.resolve({status: 401, ok: false, json: async () => ({error: {code: "invalid_session"}})}); await pending;
  assert.equal(ui.run("state.user.id"), "third-owner");
  assert.equal(ui.node("user-upstream-message").textContent, "");
});

test("save failures keep input, conflicts ask for a refresh, and mismatched results are rejected", async () => {
  const ui = dashboard(); await ui.load();
  ui.selectUsers("member", "second"); ui.selectAccounts("c1"); ui.set("user-upstream-action", "disable");
  ui.node("user-upstream-reason").value = "retain";
  const original = ui.context.api;
  ui.context.api = async () => { throw Object.assign(new Error("resource conflict"), {status: 409}); };
  await ui.submit();
  assert.match(ui.formMessage.textContent, /用户或账号已变化，请刷新后重试；输入已保留/);
  assert.equal(ui.node("user-upstream-reason").value, "retain");
  ui.context.api = async () => ({access: [{user_id: "member", provider: "codex", mode: "selected", account_ids: ["c2"]}], changed_count: 1});
  await ui.submit();
  assert.match(ui.formMessage.textContent, /保存结果与提交内容不一致/);
  assert.equal(ui.run("userUpstreamRule(userUpstreamSnapshot, 'member', 'codex').mode"), "all");
  ui.context.api = async () => { throw new Error("database unavailable"); };
  await ui.submit();
  assert.match(ui.formMessage.textContent, /保存未确认：database unavailable；输入已保留/);
  assert.equal(ui.node("user-upstream-submit").disabled, false);
  ui.context.api = original; await ui.submit();
  assert.equal(ui.writes().length, 1);
});

test("account rows and matrix headers show an alias beside the original name and search it", async () => {
  const ui = dashboard(); await ui.load();
  assert.equal(ui.accountRow("c1").querySelector("strong").textContent, "主账号");
  assert.equal(ui.accountRow("c2").querySelector("strong").textContent, "冷备（备用账号）");
  ui.selectUsers("second");
  ui.selectAccounts("c2");
  assert.match(ui.node("user-upstream-matrix").textContent, /冷备（备用账号）/);
  ui.set("user-upstream-account-search", "冷备");
  assert.deepEqual(ui.json("userUpstreamAccountMatches().map((account) => account.id)"), ["c2"]);
});
