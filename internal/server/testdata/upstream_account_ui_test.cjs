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
  const manual = status === "unavailable" ? "manual_disabled" : "enabled";
  return {id, status, can_manage, cliproxy_status: "active", gateway_manual_status: manual, gateway_quota_status: "available",
    email_masked: `${id}@example.test`, plan: "Plus", allocation_weight: 1,
    rolling_cost_usd: "1.00", rolling_cost_share: "0.2", target_share: "0.2"};
}

function statusResponse(id, status) {
  return {...account(id, status), id, status};
}

function dashboard(accounts = [account()], extra = {}) {
  const root = new TestElement();
  const nodes = new Map();
  for (const id of ["upstream-account-list", "upstream-account-filter", "upstream-account-period",
    "upstream-allocation-period", "upstream-account-loading", "upstream-account-action-message", "upstream-account-refresh-message", "operation-status"]) {
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
    window: {clearTimeout, setTimeout}, location: {hash: "#upstream-accounts"}, AbortController, URLSearchParams, DOMException, accounts, extra,
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (source) => vm.runInContext(source, context);
  run('state = {user: {id: "owner-1", role: "owner"}, recently_verified: true}; renderUpstreamAccounts({accounts, ...extra}, new URLSearchParams("all=true"));');
  return {context, run, node: (id) => nodes.get(id), cards: () => root.querySelectorAll(".upstream-account-card[data-account-id]"),
    button: (index = 0) => root.querySelectorAll(".upstream-account-status-button")[index],
    badge: (index = 0) => root.querySelectorAll(".upstream-account-status")[index],
    weightInput: (index = 0) => root.querySelectorAll(".upstream-allocation-input")[index],
    weightButton: (index = 0) => root.querySelectorAll(".upstream-allocation-save")[index],
    allocationState: (index = 0) => root.querySelectorAll(".upstream-allocation-state")[index],
    weightForm: (index = 0) => root.querySelectorAll(".upstream-allocation-form")[index],
    api: (handler) => { context.handler = handler; run("api = handler;"); },
    change: (index = 0) => run(`changeUpstreamAccountStatus(upstreamAccounts[${index}])`),
    saveWeight: (index = 0) => run(`saveUpstreamAllocationWeight(upstreamAccounts[${index}], all(".upstream-allocation-form")[${index}])`),
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

test("split source statuses render independently and final routing is fail-closed", () => {
  const ui = dashboard([
    {...account("sidecar-down"), status: "unavailable", cliproxy_status: "unavailable", gateway_manual_status: "enabled", gateway_quota_status: "available"},
    {...account("manual-off", "unavailable"), cliproxy_status: "active", gateway_manual_status: "manual_disabled"},
    {...account("quota-locked"), status: "unavailable", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "quota_exhausted"},
    {id: "old-sidecar", status: "available", can_manage: true},
  ]);
  assert.match(ui.cards()[0].querySelector(".upstream-account-cliproxy-status").textContent, /暂不可用/);
  assert.match(ui.cards()[0].querySelector(".upstream-account-manual-status").textContent, /已启用/);
  assert.equal(ui.badge(0).dataset.status, "unavailable");
  assert.equal(ui.button(0).textContent, "禁用");
  assert.equal(ui.button(0).disabled, false);
  assert.equal(ui.button(1).textContent, "重新启用");
  assert.equal(ui.button(2).textContent, "重新启用");
  assert.equal(ui.badge(3).dataset.status, "unknown");
  assert.equal(ui.button(3).disabled, true);
});

test("disable and re-enable use confirmed responses and never query quota", async () => {
  const ui = dashboard();
  const calls = [];
  let status = "available";
  ui.api(async (url, options) => {
    calls.push({url, options});
    if (options?.method === "PUT") {
      status = JSON.parse(options.body).enabled ? "available" : "unavailable";
      return statusResponse("account-1", status);
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
  status.resolve(statusResponse("account-1", "unavailable"));
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
    if (options) return statusResponse("account-1", "unavailable");
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
  ui.api(async (_, options) => options ? statusResponse("account-1", "unavailable") :
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
  ui.api(async (_, options) => { calls++; return options ? statusResponse("account-1", "unavailable") : {accounts: [account("account-1", "unavailable")]}; });
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
  status.resolve(statusResponse("account-1", "unavailable"));
  await pending;
  assert.equal(ui.badge().dataset.status, "unavailable");
});

test("a status result from an invalidated session cannot change the dashboard", async () => {
  const ui = dashboard();
  const status = deferred();
  let calls = 0;
  ui.api(async () => { calls++; return status.promise; });
  const pending = ui.change();
  ui.run("upstreamAccountOperation = null; state = null;");
  status.resolve(statusResponse("account-1", "unavailable"));
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

test("allocation costs and shares use their independent rolling window", () => {
  const ui = dashboard([{...account(), allocation_weight: 5, rolling_cost_usd: "0.123456789012345678",
    rolling_cost_share: "0.125", target_share: "0.2"}, {...account("account-2"), allocation_weight: 0}], {
    allocation_from: "2026-09-19T09:00:00Z", allocation_until: "2026-09-20T09:00:00Z",
  });
  const values = ui.cards()[0].querySelector(".upstream-allocation-stats").querySelectorAll("strong").map((node) => node.textContent);
  assert.deepEqual(values, ["US$0.123456789012345678", "12.5%", "20.0%"]);
  assert.equal(ui.weightInput().value, "5");
  assert.match(ui.allocationState(1).textContent, /停止接收新对话.*已有有效绑定继续使用/);
  assert.match(ui.node("upstream-account-period").textContent, /全部历史/);
  assert.match(ui.node("upstream-allocation-period").textContent, /近 24 小时.*独立于历史统计筛选/);
  assert.match(ui.node("upstream-allocation-period").textContent, /所有已归因账号费用为分母/);
});

test("allocation inputs reject invalid integers locally without issuing requests", async () => {
  const ui = dashboard();
  let calls = 0;
  ui.api(async () => { calls++; throw new Error("must not request"); });
  for (const invalid of ["", "-1", "+1", "1.5", "1e2", "0x10", "abc", "2147483648", "9007199254740993"]) {
    ui.weightInput().value = invalid;
    await ui.saveWeight();
    assert.equal(ui.weightInput().attributes["aria-invalid"], "true", invalid);
    assert.match(ui.weightForm().querySelector(".form-message").textContent, /0 至 2147483647 的整数/);
    assert.equal(ui.weightButton().disabled, false);
  }
  assert.equal(calls, 0);
});

test("allocation save accepts zero and PostgreSQL integer maximum and confirms exact values", async () => {
  for (const weight of [0, 20, 2147483647]) {
    const ui = dashboard();
    const writes = [];
    ui.api(async (url, options) => {
      if (options) {
        writes.push({url, method: options.method, body: JSON.parse(options.body)});
        return {id: "account-1", allocation_weight: weight};
      }
      return {accounts: [{...account(), allocation_weight: weight, target_share: weight ? "1" : "0"}]};
    });
    ui.weightInput().value = String(weight);
    await ui.saveWeight();
    assert.deepEqual(writes, [{url: "/admin/upstream-accounts/account-1/allocation-weight", method: "PUT", body: {weight}}]);
    assert.equal(ui.weightInput().value, String(weight));
    assert.equal(ui.weightButton().disabled, false);
    assert.equal(ui.node("upstream-account-action-message").dataset.kind, "ok");
    assert.equal(ui.allocationState().dataset.draining, String(weight === 0));
  }
});

test("allocation permission and synchronization failures block edits", async () => {
  const scenarios = [dashboard([account("historical", "unavailable", false)]),
    dashboard([account()], {sync_warning: "unavailable"}), dashboard()];
  scenarios[2].run('state.user.role = "member"; syncUpstreamAccountControls();');
  for (const ui of scenarios) {
    ui.api(async () => { throw new Error("must not request"); });
    assert.equal(ui.weightInput().disabled, true);
    assert.equal(ui.weightButton().disabled, true);
    await ui.saveWeight();
  }
});

test("allocation operation blocks other writes and refreshes until confirmation", async () => {
  const ui = dashboard([account(), account("account-2")]);
  const mutation = deferred();
  let calls = 0;
  ui.api(async (_, options) => {
    calls++;
    return options ? mutation.promise : {accounts: [{...account(), allocation_weight: 0}, account("account-2")]};
  });
  ui.weightInput().value = "0";
  const pending = ui.saveWeight();
  assert.equal(ui.weightButton().textContent, "保存中…");
  assert.equal(ui.button().textContent, "禁用");
  assert.equal(ui.weightButton(1).disabled, true);
  assert.equal(ui.button(1).disabled, true);
  assert.equal(ui.allocationState().dataset.draining, "false");
  await ui.saveWeight();
  await ui.saveWeight(1);
  await ui.change();
  await ui.load();
  assert.equal(calls, 1);
  mutation.resolve({id: "account-1", allocation_weight: 0});
  await pending;
  assert.equal(calls, 2);
  assert.equal(ui.allocationState().dataset.draining, "true");
});

test("allocation refresh failure retains the confirmed weight and drain state", async () => {
  const ui = dashboard();
  ui.api(async (_, options) => {
    if (options) return {id: "account-1", allocation_weight: 0};
    throw new Error("统计暂不可用");
  });
  ui.weightInput().value = "0";
  await ui.saveWeight();
  assert.equal(ui.run("upstreamAccounts[0].allocation_weight"), 0);
  assert.match(ui.allocationState().textContent, /停止接收新对话/);
  assert.equal(ui.node("upstream-account-action-message").dataset.kind, "ok");
  assert.match(ui.node("upstream-account-refresh-message").textContent, /操作已成功，但列表与统计刷新失败/);
  assert.equal(ui.weightButton().disabled, true);
});

test("allocation failures and mismatched confirmations never report a saved weight", async () => {
  for (const response of [{id: "other", allocation_weight: 0}, {id: "account-1", allocation_weight: 1},
    {id: "account-1", allocation_weight: "0"}, new Error("账号不存在")]) {
    const ui = dashboard();
    ui.api(async () => { if (response instanceof Error) throw response; return response; });
    ui.weightInput().value = "0";
    await ui.saveWeight();
    assert.equal(ui.run("upstreamAccounts[0].allocation_weight"), 1);
    assert.equal(ui.allocationState().dataset.draining, "false");
    assert.equal(ui.node("upstream-account-action-message").dataset.kind, "error");
    assert.match(ui.node("upstream-account-action-message").textContent, /保存未确认/);
    assert.equal(ui.weightButton().disabled, true);
  }
});

test("allocation verification precedes writes and cancellation issues no write", async () => {
  const ui = dashboard();
  const verification = deferred();
  ui.context.verification = verification.promise;
  ui.run("state.recently_verified = false; reauthenticate = () => verification;");
  let calls = 0;
  ui.api(async (_, options) => { calls++; return options ? {id: "account-1", allocation_weight: 20} :
    {accounts: [{...account(), allocation_weight: 20}]}; });
  ui.weightInput().value = "20";
  const pending = ui.saveWeight();
  assert.equal(calls, 0);
  verification.resolve();
  await pending;
  assert.equal(calls, 2);

  const cancelled = dashboard();
  cancelled.run('state.recently_verified = false; reauthenticate = async () => { throw new DOMException("取消验证", "AbortError"); };');
  cancelled.api(async () => { throw new Error("must not send a request"); });
  await cancelled.saveWeight();
  assert.match(cancelled.node("upstream-account-action-message").textContent, /已取消/);
});

test("an invalidated allocation operation cannot alter the dashboard", async () => {
  const ui = dashboard();
  const response = deferred();
  ui.api(async () => response.promise);
  ui.weightInput().value = "0";
  const pending = ui.saveWeight();
  ui.run("upstreamAccountOperation = null; state = null;");
  response.resolve({id: "account-1", allocation_weight: 0});
  await pending;
  assert.equal(ui.allocationState().dataset.draining, "false");
  assert.equal(ui.node("upstream-account-action-message").textContent, "");
});

test("re-enabling an account with zero weight preserves the draining explanation", async () => {
  const ui = dashboard([{...account("account-1", "unavailable"), allocation_weight: 0}]);
  ui.api(async (_, options) => options ? statusResponse("account-1", "available") :
    {accounts: [{...account(), allocation_weight: 0}]});
  await ui.change();
  assert.match(ui.node("upstream-account-action-message").textContent, /系数仍为 0，停止接收新对话/);
  assert.match(ui.allocationState().textContent, /停止接收新对话/);
});

test("concurrency snapshots distinguish explicit zero, missing, malformed, and expired data without replacing edits", () => {
  const ui = dashboard([account(), account("account-2")]);
  const input = ui.weightInput();
  input.value = "37";
  ui.run('upstreamConcurrencySnapshot = {sampled_at: new Date().toISOString(), accounts: [{id: "account-1", active_requests: 0}]}; renderUpstreamConcurrency();');
  assert.equal(ui.cards()[0].querySelector(".upstream-concurrency-count").textContent, "0");
  assert.equal(ui.cards()[1].querySelector(".upstream-concurrency-count").textContent, "暂不可用");
  assert.equal(ui.weightInput(), input);
  assert.equal(input.value, "37");
  for (const value of [-1, 1.5, "2", null]) {
    ui.context.value = value;
    ui.run('upstreamConcurrencySnapshot.accounts[0].active_requests = value; renderUpstreamConcurrency();');
    assert.equal(ui.cards()[0].querySelector(".upstream-concurrency-count").textContent, "暂不可用");
  }
  ui.run('upstreamConcurrencySnapshot = {sampled_at: new Date(Date.now() - 16000).toISOString(), accounts: [{id:"account-1",active_requests:2}]}; renderUpstreamConcurrency();');
  assert.equal(ui.cards()[0].querySelector(".upstream-concurrency-count").textContent, "暂不可用");
});

test("concurrency polling samples immediately and stops on hidden page, route change, logout, and role change", async () => {
  for (const condition of ['document.visibilityState = "hidden"', 'location.hash = "#usage"', 'loggingOut = true', 'state.user.role = "member"']) {
    const ui = dashboard();
    const timers = new Map();
    let id = 0, calls = 0;
    ui.context.window.setTimeout = (fn, delay) => { timers.set(++id, {fn, delay}); return id; };
    ui.context.window.clearTimeout = (id) => timers.delete(id);
    ui.api(async () => { calls++; return {sampled_at: new Date().toISOString(), accounts: [{id: "account-1", active_requests: 3}]}; });
    ui.run('startUpstreamConcurrency(); startUpstreamConcurrency();');
    await new Promise(setImmediate);
    assert.equal(calls, 1);
    assert.equal(ui.cards()[0].querySelector(".upstream-concurrency-count").textContent, "3");
    assert.equal([...timers.values()].some((timer) => timer.delay === 5000), true);
    ui.run(`${condition}; syncVisiblePolling();`);
    assert.equal(timers.size, 0);
    assert.equal(ui.run("upstreamConcurrencyPolling"), false);
    assert.equal(ui.cards()[0].querySelector(".upstream-concurrency-count").textContent, "暂不可用");
  }
});

test("failed concurrency responses clear counts and an aborted request cannot restore them", async () => {
  const ui = dashboard();
  const response = deferred();
  let signal;
  ui.api((_, options) => { signal = options.signal; return response.promise; });
  ui.run("startUpstreamConcurrency(); stopUpstreamConcurrency();");
  assert.equal(signal.aborted, true);
  response.resolve({sampled_at: new Date().toISOString(), accounts: [{id: "account-1", active_requests: 8}]});
  await new Promise(setImmediate);
  assert.equal(ui.cards()[0].querySelector(".upstream-concurrency-count").textContent, "暂不可用");

  ui.api(async () => { throw new Error("unsupported old sidecar"); });
  ui.run("startUpstreamConcurrency();");
  await new Promise(setImmediate);
  assert.equal(ui.cards()[0].querySelector(".upstream-concurrency-count").textContent, "暂不可用");
  ui.run("stopUpstreamConcurrency();");
});
