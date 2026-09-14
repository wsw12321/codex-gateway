"use strict";

// Exercise the shipped dashboard code without adding runtime dependencies.
const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class TestElement {
  constructor(tag = "div") {
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.className = "";
    this.textContent = "";
    this.disabled = false;
    this.isConnected = true;
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => this.classList.toggle(name, true),
      remove: (name) => this.classList.toggle(name, false),
      toggle: (name, enabled) => {
        const values = new Set(this.className.split(/\s+/).filter(Boolean));
        if (enabled === undefined) enabled = !values.has(name);
        if (enabled) values.add(name); else values.delete(name);
        this.className = [...values].join(" ");
      },
    };
  }
  append(...children) {
    for (const child of children) { child.parent = this; this.children.push(child); }
  }
  replaceChildren(...children) { this.children = []; this.append(...children); }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener() {}
  matches(selector) {
    if (selector === ".upstream-account-card[data-account-id]") {
      return this.classList.contains("upstream-account-card") && "accountId" in this.dataset;
    }
    if (selector === "button[type=submit]") return this.tagName === "button" && this.type === "submit";
    if (selector.startsWith(".")) return this.classList.contains(selector.slice(1));
    return this.tagName === selector;
  }
  querySelectorAll(selector) {
    return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return {promise, resolve, reject};
}

function account(id = "account-1", status = "available", can_manage = true) {
  return {id, status, can_manage, email_masked: `${id}@example.test`, plan: "Plus"};
}

function dashboard(accounts = [account()], extra = {}) {
  const root = new TestElement();
  const nodes = new Map();
  for (const id of ["upstream-account-list", "upstream-account-filter", "upstream-account-period",
    "upstream-account-loading", "upstream-account-action-message", "upstream-account-refresh-message", "operation-status"]) {
    const node = new TestElement();
    nodes.set(id, node);
    root.append(node);
  }
  const filter = nodes.get("upstream-account-filter");
  filter.elements = {range: {value: "all"}};
  const submit = new TestElement("button");
  submit.type = "submit";
  filter.append(submit);
  const context = vm.createContext({
    document: {getElementById: (id) => nodes.get(id), createElement: (tag) => new TestElement(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    window: {clearTimeout, setTimeout}, URLSearchParams, DOMException, accounts, extra,
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (source) => vm.runInContext(source, context);
  run('state = {user: {id: "owner-1", role: "owner"}, recently_verified: true}; renderUpstreamAccounts({accounts, ...extra}, new URLSearchParams("all=true"));');
  return {context, run, node: (id) => nodes.get(id), cards: () => root.querySelectorAll(".upstream-account-card[data-account-id]"),
    button: (index = 0) => root.querySelectorAll(".upstream-account-status-button")[index],
    badge: (index = 0) => root.querySelectorAll(".upstream-account-status")[index],
    api: (handler) => { context.handler = handler; run("api = handler;"); },
    change: (index = 0) => run(`changeUpstreamAccountStatus(upstreamAccounts[${index}])`),
    load: () => run('loadUpstreamAccounts(new URLSearchParams("all=true"))'),
  };
}

test("available, unavailable, historical, and failed-sync accounts have safe controls", () => {
  const ui = dashboard([account(), account("account-2", "unavailable"), account("historical", "unavailable", false)]);
  assert.equal(ui.button().textContent, "禁用");
  assert.equal(ui.button(1).textContent, "重新启用");
  assert.equal(ui.button().disabled, false);
  assert.equal(ui.button(2).disabled, true);
  assert.equal(dashboard([account()], {sync_warning: "sync_failed"}).button().disabled, true);
  assert.equal(dashboard([{id: "missing-manage-flag", status: "available"}]).button().disabled, true);
  ui.run('state.user.role = "member"; syncUpstreamAccountControls();');
  assert.equal(ui.button().disabled, true);
});

test("disable and re-enable use confirmed responses and never query quota", async () => {
  const ui = dashboard();
  const calls = [];
  let status = "available";
  ui.api(async (url, options) => {
    calls.push({url, options});
    if (options?.method === "PUT") {
      status = JSON.parse(options.body).enabled ? "available" : "unavailable";
      return {id: "account-1", status};
    }
    return {accounts: [account("account-1", status)]};
  });
  await ui.change();
  assert.equal(ui.badge().dataset.status, "unavailable");
  assert.equal(ui.button().textContent, "重新启用");
  assert.equal(ui.button().disabled, false);
  await ui.change();
  assert.equal(ui.badge().dataset.status, "available");
  assert.deepEqual(calls.filter((call) => call.options).map((call) => JSON.parse(call.options.body)), [{enabled: false}, {enabled: true}]);
  assert.equal(calls.every((call) => !call.url.endsWith("/quota")), true);
});

test("pending operations block duplicate submissions and list refreshes", async () => {
  const ui = dashboard([account(), account("account-2")]);
  const status = deferred(), refresh = deferred();
  let calls = 0;
  ui.api(async (_, options) => { calls++; return options ? status.promise : refresh.promise; });
  const pending = ui.change();
  assert.equal(ui.badge().dataset.status, "available");
  assert.equal(ui.button().textContent, "禁用中…");
  assert.equal(ui.button(1).disabled, true);
  await ui.change();
  await ui.change(1);
  await ui.load();
  assert.equal(calls, 1);
  status.resolve({id: "account-1", status: "unavailable"});
  await new Promise(setImmediate);
  assert.equal(ui.badge().dataset.status, "unavailable");
  assert.equal(ui.node("upstream-account-action-message").dataset.kind, "ok");
  assert.equal(ui.button().disabled, true);
  refresh.resolve({accounts: [account("account-1", "unavailable"), account("account-2")]});
  await pending;
  assert.equal(ui.button().disabled, false);
  assert.equal(calls, 2);
});

test("refresh failure preserves confirmed status and reports it separately", async () => {
  const ui = dashboard();
  ui.api(async (_, options) => {
    if (options) return {id: "account-1", status: "unavailable"};
    throw new Error("统计服务暂不可用");
  });
  await ui.change();
  assert.equal(ui.badge().dataset.status, "unavailable");
  assert.equal(ui.button().textContent, "重新启用");
  assert.equal(ui.node("upstream-account-action-message").dataset.kind, "ok");
  assert.match(ui.node("upstream-account-refresh-message").textContent, /操作已成功，但列表与统计刷新失败/);
  assert.equal(ui.button().disabled, true);
  ui.api(async () => ({accounts: [account("account-1", "unavailable")]}));
  await ui.load();
  assert.equal(ui.button().disabled, false);
  assert.equal(ui.node("upstream-account-action-message").dataset.kind, "ok");
});

test("mismatched confirmation and vanished account errors fail closed", async () => {
  for (const response of [{id: "other", status: "unavailable"}, {id: "account-1", status: "available"}, new Error("上游账号不存在")]) {
    const ui = dashboard();
    let calls = 0;
    ui.api(async () => { calls++; if (response instanceof Error) throw response; return response; });
    await ui.change();
    assert.equal(ui.badge().dataset.status, "available");
    assert.equal(ui.button().disabled, true);
    assert.equal(ui.node("upstream-account-action-message").dataset.kind, "error");
    await ui.change();
    assert.equal(calls, 1);
  }
});

test("a failed sidecar synchronization disables controls while retaining action success", async () => {
  const ui = dashboard();
  ui.api(async (_, options) => options ? {id: "account-1", status: "unavailable"} :
    {accounts: [account("account-1", "unavailable")], sync_warning: "upstream_account_sync_unavailable"});
  await ui.change();
  assert.equal(ui.badge().dataset.status, "unavailable");
  assert.equal(ui.button().disabled, true);
  assert.equal(ui.node("upstream-account-action-message").dataset.kind, "ok");
  assert.match(ui.node("upstream-account-refresh-message").textContent, /上游状态同步失败/);
});

test("recent verification completes before the PUT and cancellation sends no PUT", async () => {
  const ui = dashboard();
  const verification = deferred();
  ui.context.verification = verification.promise;
  ui.run("state.recently_verified = false; reauthenticate = () => verification;");
  let calls = 0;
  ui.api(async (_, options) => { calls++; return options ? {id: "account-1", status: "unavailable"} : {accounts: [account("account-1", "unavailable")]}; });
  const pending = ui.change();
  assert.equal(calls, 0);
  verification.resolve();
  await pending;
  assert.equal(calls, 2);

  const cancelled = dashboard();
  cancelled.run('state.recently_verified = false; reauthenticate = async () => { throw new DOMException("取消验证", "AbortError"); };');
  cancelled.api(async () => { throw new Error("must not send a request"); });
  await cancelled.change();
  assert.match(cancelled.node("upstream-account-action-message").textContent, /已取消/);
});

test("an older list response cannot overwrite an in-flight status operation", async () => {
  const ui = dashboard();
  const oldList = deferred(), status = deferred();
  let reads = 0;
  ui.api(async (_, options) => {
    if (options) return status.promise;
    reads++;
    if (reads === 1) return oldList.promise;
    return {accounts: [account("account-1", reads > 2 ? "unavailable" : "available")]};
  });
  const oldRequest = ui.load();
  await ui.load();
  const pending = ui.change();
  oldList.resolve({accounts: [account("obsolete-account")]});
  await oldRequest;
  assert.equal(ui.cards()[0].dataset.accountId, "account-1");
  assert.equal(ui.button().textContent, "禁用中…");
  status.resolve({id: "account-1", status: "unavailable"});
  await pending;
  assert.equal(ui.badge().dataset.status, "unavailable");
});

test("a status result from an invalidated session cannot change the dashboard", async () => {
  const ui = dashboard();
  const status = deferred();
  let calls = 0;
  ui.api(async () => { calls++; return status.promise; });
  const pending = ui.change();
  ui.run("upstreamAccountStatusOperation = null; state = null;");
  status.resolve({id: "account-1", status: "unavailable"});
  await pending;
  assert.equal(calls, 1);
  assert.equal(ui.badge().dataset.status, "available");
  assert.equal(ui.node("upstream-account-action-message").textContent, "");
});

test("an obsolete list failure does not disable or report failure over a newer list", async () => {
  const ui = dashboard();
  const older = deferred();
  let calls = 0;
  ui.api(async () => ++calls === 1 ? older.promise : {accounts: [account("current-account")]});
  const pending = ui.load();
  await ui.load();
  older.reject(new Error("obsolete failure"));
  await pending;
  assert.equal(ui.cards()[0].dataset.accountId, "current-account");
  assert.equal(ui.button().disabled, false);
  assert.equal(ui.node("upstream-account-refresh-message").textContent, "");
});
