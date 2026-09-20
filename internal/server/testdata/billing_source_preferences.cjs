"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

// Exercise production rendering and async handlers without a browser dependency.
const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));

class Element {
  constructor(tag = "div") {
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.className = "";
    this.disabled = false;
    this.listeners = {};
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
  set textContent(value) { this.text = value; this.children = []; }
  get textContent() { return this.text || ""; }
  append(...children) { for (const child of children) { child.parent = this; this.children.push(child); } }
  replaceChildren(...children) { this.text = ""; this.children = []; this.append(...children); }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  showModal() { this.open = true; }
  close() { this.open = false; this.listeners.close?.(); }
  matches(selector) {
    if (selector === "button[type=submit]") return this.tagName === "button" && this.type === "submit";
    if (selector === "[data-billing-source]") return "billingSource" in this.dataset;
    if (selector === "[data-billing-source-state]") return "billingSourceState" in this.dataset;
    if (selector.startsWith(".")) return this.classList.contains(selector.slice(1));
    return this.tagName === selector;
  }
  querySelectorAll(selector) {
    return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest() { return this; }
  reset() {}
  focus() {}
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return {promise, resolve, reject};
}

function detail(id = "member", disabled = {}) {
  return {user: {id, username: id}, cash_balance_usd: "123.45",
    source_disabled: {day: false, week: false, month: false, cash: false, ...disabled},
    subscriptions: {day: {enabled: true, remaining_usd: "8.50", quota_usd: "10", period_count: 3,
      current_period_number: 1, period_started_at: "2026-09-20T00:00:00Z", period_ends_at: "2026-09-21T00:00:00Z",
      expires_at: "2026-09-23T00:00:00Z"}}, ledger_entries: []};
}

function dashboard(role = "member", initial = detail()) {
  const root = new Element();
  const nodes = new Map();
  const getNode = (id) => {
    if (!nodes.has(id)) { const node = new Element(); nodes.set(id, node); root.append(node); }
    return nodes.get(id);
  };
  const calls = [];
  let snapshot = initial;
  const context = vm.createContext({
    document: {getElementById: getNode, createElement: (tag) => new Element(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    URLSearchParams, initial, role,
    request: async (pathname, options) => {
      calls.push({pathname, options});
      if (options?.method === "PUT") {
        const source = pathname.split("/").at(-2), {disabled} = JSON.parse(options.body);
        snapshot = {...snapshot, source_disabled: {...snapshot.source_disabled, [source]: disabled}};
        return {source, disabled};
      }
      return snapshot;
    },
  });
  vm.runInContext(application, context, {filename: "assets/app.js"});
  const run = (code) => vm.runInContext(code, context);
  run(`
    state = {user: {id: "member", username: "member", role}, recently_verified: true};
    billingUserID = initial.user.id;
    const productionAPI = api;
    api = request;
    renderBillingLedger = () => {};
    renderBillingAdminValues = syncBillingUserControls;
    notice = () => {};
    renderBillingDetail(initial);
  `);
  const buttons = () => root.querySelectorAll("[data-billing-source]");
  return {run, calls, context, node: getNode, buttons,
    button: (source) => buttons().find((node) => node.dataset.billingSource === source),
    label: (source) => root.querySelectorAll("[data-billing-source-state]").find((node) => node.dataset.billingSourceState === source),
    change: (source) => run(`changeBillingSource(${JSON.stringify(source)})`),
    api: (callback) => { context.handler = async (pathname, options) => { calls.push({pathname, options}); return callback(pathname, options); }; run("api = handler"); },
  };
}

test("members and owners can disable and restore every own source, including unopened subscriptions", async (t) => {
  for (const role of ["member", "owner"]) {
    await t.test(role, async () => {
      const ui = dashboard(role);
      assert.equal(ui.buttons().length, 4);
      for (const source of ["cash", "day", "week", "month"]) {
        assert.equal(ui.button(source).disabled, false);
        await ui.button(source).listeners.click();
        assert.equal(ui.button(source).textContent, "恢复扣费");
        assert.equal(ui.label(source).textContent, "已禁用扣费");
        await ui.button(source).listeners.click();
        assert.equal(ui.button(source).textContent, "禁用扣费");
        assert.equal(ui.label(source).textContent, "允许扣费");
        assert.deepEqual(ui.calls.filter(({options}) => options?.method === "PUT").slice(-2).map(({pathname, options}) => ({pathname, body: JSON.parse(options.body)})), [
          {pathname: `/admin/billing/me/sources/${source}/status`, body: {disabled: true}},
          {pathname: `/admin/billing/me/sources/${source}/status`, body: {disabled: false}},
        ]);
      }
      assert.equal(ui.node("billing-cash-balance").textContent, "US$123.45");
      assert.equal(ui.node("billing-day-remaining").textContent, "US$8.50");
      assert.match(ui.node("billing-day-ends").textContent, /最终失效/);
      assert.equal(ui.node("billing-week-remaining").textContent, "未启用");
      assert.equal(ui.calls.filter(({options}) => !options).every(({pathname}) => pathname.startsWith("/admin/billing/me?")), true);
    });
  }
});

test("an owner viewing someone else can only read the source preferences", async () => {
  const ui = dashboard("owner", detail("other", {day: true, cash: true}));
  assert.equal(ui.label("day").textContent, "已禁用扣费");
  assert.equal(ui.label("cash").textContent, "已禁用扣费");
  assert.equal(ui.buttons().every((button) => button.disabled && button.classList.contains("hidden")), true);
  assert.equal(ui.node("billing-source-readonly").classList.contains("hidden"), false);
  for (const source of ["day", "week", "month", "cash"]) await ui.change(source);
  assert.equal(ui.calls.length, 0);
});

test("loading, failed, missing, or mismatched details cannot modify preferences", async (t) => {
  for (const condition of ["loading", "failed", "missing", "mismatched"]) {
    await t.test(condition, async () => {
      const ui = dashboard();
      const read = deferred();
      ui.api(() => read.promise);
      let pending;
      if (condition === "loading" || condition === "failed") pending = ui.run("loadBillingDetail()");
      if (condition === "failed") { read.reject(new Error("read failed")); await assert.rejects(pending, /read failed/); }
      if (condition === "missing") ui.run("billingDetail = null; syncBillingSourceControls()");
      if (condition === "mismatched") ui.run('billingDetail.user.id = "other"; syncBillingSourceControls()');
      assert.equal(ui.buttons().every((button) => button.disabled), true);
      await ui.change("cash");
      assert.equal(ui.calls.filter(({options}) => options?.method === "PUT").length, 0);
      if (condition === "loading") { read.resolve(detail()); await pending; }
    });
  }
});

test("saving and the following refresh block duplicate clicks across all sources", async () => {
  const ui = dashboard();
  const write = deferred(), read = deferred();
  ui.api((pathname, options) => options ? write.promise : read.promise);
  const saving = ui.change("cash");
  assert.equal(ui.button("cash").textContent, "保存中…");
  assert.equal(ui.buttons().every((button) => button.disabled), true);
  await ui.change("cash"); await ui.change("day");
  assert.equal(ui.calls.length, 1);
  write.resolve({source: "cash", disabled: true});
  await new Promise(setImmediate);
  assert.equal(ui.buttons().every((button) => button.disabled), true);
  await ui.change("cash");
  assert.equal(ui.calls.length, 2);
  read.resolve(detail("member", {cash: true}));
  await saving;
  assert.equal(ui.button("cash").disabled, false);
  assert.equal(ui.label("cash").textContent, "已禁用扣费");
});

test("selecting self again during a save keeps duplicate submissions blocked", async () => {
  const ui = dashboard("owner"), write = deferred();
  ui.api(async (pathname, options) => options ? write.promise : detail());
  const saving = ui.change("cash");
  await ui.run('selectBillingUser({id: "member", username: "member"})');
  await ui.change("cash");
  assert.equal(ui.calls.filter(({options}) => options?.method === "PUT").length, 1);
  assert.equal(ui.buttons().every((button) => button.disabled), true);
  write.resolve({source: "cash", disabled: true}); await saving;
});

test("failed or unconfirmed writes reload the server state without optimistic changes", async (t) => {
  for (const committed of [false, true]) {
    await t.test(`server committed: ${committed}`, async () => {
      const ui = dashboard();
      ui.api(async (pathname, options) => {
        if (options) throw new Error("response lost");
        return detail("member", {cash: committed});
      });
      await ui.change("cash");
      assert.equal(ui.label("cash").textContent, committed ? "已禁用扣费" : "允许扣费");
      assert.equal(ui.button("cash").disabled, false);
      assert.match(ui.node("billing-source-message").textContent, /保存未确认.*response lost.*已重新加载服务器设置/);
      assert.equal(ui.node("billing-source-message").attributes.role, "alert");
    });
  }
});

test("refresh failure after a successful or failed write leaves controls disabled", async (t) => {
  for (const saved of [false, true]) {
    await t.test(`write succeeded: ${saved}`, async () => {
      const ui = dashboard();
      ui.api(async (pathname, options) => {
        if (options && saved) return {source: "cash", disabled: true};
        throw new Error(options ? "write failed" : "read failed");
      });
      await ui.change("cash");
      assert.equal(ui.buttons().every((button) => button.disabled), true);
      assert.equal(ui.run("billingDetail"), null);
      assert.match(ui.node("billing-source-message").textContent, /额度刷新失败.*read failed/);
    });
  }
});

test("changing the viewed user before reauthentication finishes prevents the write", async () => {
  const ui = dashboard("owner");
  const reauth = deferred();
  ui.context.verify = () => reauth.promise;
  ui.run("state.recently_verified = false; reauthenticate = verify");
  ui.api(async () => detail("other"));
  const saving = ui.change("cash");
  await ui.run('selectBillingUser({id: "other", username: "other"})');
  reauth.resolve(); await saving;
  assert.equal(ui.calls.filter(({options}) => options?.method === "PUT").length, 0);
  assert.equal(ui.run("billingDetail.user.id"), "other");
  assert.equal(ui.node("billing-source-message").textContent, "");
});

test("stale write completions after target, logout, or identity changes cannot start reads or show messages", async (t) => {
  for (const change of ["target", "logout", "identity"]) {
    await t.test(change, async () => {
      const ui = dashboard("owner"), write = deferred();
      ui.api(async (pathname, options) => options ? write.promise : detail("other"));
      const saving = ui.change("cash");
      if (change === "target") await ui.run('selectBillingUser({id: "other", username: "other"})');
      if (change === "logout") ui.run("loggingOut = true; resetBillingUserSearch()");
      if (change === "identity") ui.run('state.user = {id: "new-login", role: "member"}; resetBillingUserSearch()');
      const count = ui.calls.length;
      write.resolve({source: "cash", disabled: true}); await saving;
      assert.equal(ui.calls.length, count);
      assert.equal(ui.node("billing-source-message").textContent, "");
      if (change === "target") assert.equal(ui.run("billingDetail.user.id"), "other");
      else assert.equal(ui.run("billingDetail"), null);
    });
  }
});

test("a stale refresh cannot overwrite another user's preferences", async () => {
  const ui = dashboard("owner"), read = deferred();
  ui.api(async (pathname, options) => options ? {source: "cash", disabled: true}
    : pathname.includes("/me?") ? read.promise : detail("other", {week: true}));
  const saving = ui.change("cash");
  await new Promise(setImmediate);
  await ui.run('selectBillingUser({id: "other", username: "other"})');
  read.resolve(detail("member", {cash: true})); await saving;
  assert.equal(ui.run("billingDetail.user.id"), "other");
  assert.equal(ui.label("week").textContent, "已禁用扣费");
  assert.equal(ui.label("cash").textContent, "允许扣费");
  assert.equal(ui.node("billing-source-message").textContent, "");
});

test("an old operation cannot unlock a new operation after leaving and returning to self", async () => {
  const ui = dashboard("owner"), oldWrite = deferred(), newWrite = deferred();
  let writes = 0;
  ui.api(async (pathname, options) => {
    if (options) return ++writes === 1 ? oldWrite.promise : newWrite.promise;
    return detail(pathname.includes("/me?") ? "member" : "other");
  });
  const old = ui.change("cash");
  await ui.run('selectBillingUser({id: "other", username: "other"})');
  await ui.run('selectBillingUser({id: "member", username: "member"})');
  const current = ui.change("day");
  oldWrite.resolve({source: "cash", disabled: true}); await old;
  assert.equal(ui.buttons().every((button) => button.disabled), true);
  assert.equal(ui.run("billingSourceOperation.source"), "day");
  newWrite.resolve({source: "day", disabled: true}); await current;
  assert.equal(ui.run("billingSourceOperation"), null);
});

test("stale authentication errors from source writes cannot invalidate or reauthenticate the current page", async (t) => {
  for (const status of [401, 403]) {
    for (const change of ["target", "logout", "identity"]) {
      await t.test(`${status} after ${change} change`, async () => {
        const ui = dashboard("owner"), response = deferred();
        let invalidations = 0, verifications = 0;
        ui.context.fetch = async (pathname, options) => options.method === "PUT" ? response.promise
          : {ok: true, status: 200, json: async () => detail("other")};
        ui.context.invalidated = () => { invalidations++; };
        ui.context.verify = async () => { verifications++; };
        ui.run("api = productionAPI; handleUnauthorized = invalidated; reauthenticate = verify");
        const saving = ui.change("cash");
        if (change === "target") await ui.run('selectBillingUser({id: "other", username: "other"})');
        if (change === "logout") ui.run("loggingOut = true; resetBillingUserSearch()");
        if (change === "identity") ui.run('state.user = {id: "new-login", role: "member"}; resetBillingUserSearch()');
        response.resolve({ok: false, status, json: async () => ({error: {
          code: status === 401 ? "invalid_session" : "recent_identity_verification_required", message: "stale auth error",
        }})});
        await saving;
        assert.equal(invalidations, 0);
        assert.equal(verifications, 0);
        assert.equal(ui.run("state.recently_verified"), true);
        assert.equal(ui.node("billing-source-message").textContent, "");
      });
    }
  }
});

test("a stale billing read's 401 cannot clear a newer logged-in user's detail", async () => {
  const ui = dashboard(), response = deferred();
  let invalidations = 0;
  ui.context.fetch = async () => response.promise;
  ui.context.invalidated = () => { invalidations++; };
  ui.context.nextDetail = detail("new-login", {cash: true});
  ui.run("api = productionAPI; handleUnauthorized = invalidated");
  const reading = ui.run("loadBillingDetail()");
  ui.run('state.user = {id: "new-login", role: "member"}; resetBillingUserSearch(); renderBillingDetail(nextDetail)');
  response.resolve({ok: false, status: 401, json: async () => ({error: {code: "invalid_session"}})});
  await reading;
  assert.equal(invalidations, 0);
  assert.equal(ui.run("billingDetail.user.id"), "new-login");
  assert.equal(ui.label("cash").textContent, "已禁用扣费");
});

test("sensitive action does not retry an old verification error for a new identity", async () => {
  const ui = dashboard(), write = deferred();
  let verifications = 0;
  ui.api(() => write.promise);
  ui.context.verify = async () => { verifications++; };
  ui.run("reauthenticate = verify");
  const saving = ui.change("cash");
  ui.run('state.user = {id: "new-login", role: "member"}; resetBillingUserSearch()');
  write.reject(Object.assign(new Error("verification needed"), {code: "recent_identity_verification_required"}));
  await saving;
  assert.equal(verifications, 0);
  assert.equal(ui.run("state.recently_verified"), true);
});

test("password and passkey verification responses cannot affect a replacement identity or its pending form", async (t) => {
  for (const method of ["password", "passkey"]) {
    for (const oldStatus of [200, 401]) {
      await t.test(`${method}, old response ${oldStatus}`, async () => {
        const ui = dashboard(), oldResponse = deferred(), newResponse = deferred();
        const dialog = ui.node("reauth-dialog"), form = ui.node("reauth-form");
        const select = new Element("select"), option = new Element("option"), fields = new Element(), submit = new Element("button");
        option.value = method; select.append(option);
        fields.className = "reauth-password"; submit.type = "submit";
        form.append(fields, submit);
        form.elements = {method: select, password: {value: "synthetic-password"}};
        form.closest = () => dialog;
        let authRequests = 0, invalidations = 0, notices = 0, writes = 0;
        ui.context.fetch = async (pathname, options) => {
          if (pathname === "/auth/reauth/begin") return {ok: true, status: 200, json: async () => ({flow_id: "synthetic-flow"})};
          if (pathname === "/auth/password/reauth" || pathname === "/auth/reauth/finish") return ++authRequests === 1 ? oldResponse.promise : newResponse.promise;
          if (options.method === "PUT") { writes++; return {ok: true, status: 200, json: async () => ({source: "cash", disabled: true})}; }
          return {ok: true, status: 200, json: async () => detail("new-login", {cash: true})};
        };
        ui.context.invalidated = () => { invalidations++; };
        ui.context.notified = () => { notices++; };
        ui.context.nextDetail = detail("new-login");
        ui.run(`
          api = productionAPI; handleUnauthorized = invalidated; notice = notified;
          state.recently_verified = false; state.login_methods = {${method}: true};
          webAuthnSupported = () => true; getPasskey = async () => ({});
          bindAsync("reauth-form", "submit", submitReauthentication, "验证中…", () => reauthRequestCurrent);
        `);
        const oldSaving = ui.change("cash");
        const oldSubmitting = form.listeners.submit({currentTarget: form, preventDefault() {}});
        await new Promise(setImmediate);
        assert.equal(authRequests, 1);
        ui.run('state.user = {id: "new-login", role: "member"}; identityGeneration++; resetBillingUserSearch(); renderBillingDetail(nextDetail)');
        await oldSaving;
        const newSaving = ui.change("cash");
        const newSubmitting = form.listeners.submit({currentTarget: form, preventDefault() {}});
        await new Promise(setImmediate);
        assert.equal(authRequests, 2);
        oldResponse.resolve({ok: oldStatus === 200, status: oldStatus, json: async () => oldStatus === 200 ? {} : {error: {code: "invalid_session"}}});
        await oldSubmitting;
        assert.equal(ui.run("state.recently_verified"), false);
        assert.equal(form.dataset.busy, "true", "the old handler must not unlock the new verification form");
        assert.equal(dialog.open, true);
        assert.equal(invalidations, 0);
        assert.equal(notices, 0);
        assert.equal(writes, 0);
        newResponse.resolve({ok: true, status: 200, json: async () => ({ok: true})});
        await newSubmitting; await newSaving;
        assert.equal(ui.run("state.recently_verified"), true);
        assert.equal(writes, 1);
        assert.equal(dialog.open, false);
        assert.equal(form.dataset.busy, "false");
      });
    }
  }
});

test("cancelling and reopening verification for the same identity ignores the old response", async () => {
  const ui = dashboard(), oldResponse = deferred(), newResponse = deferred();
  const dialog = ui.node("reauth-dialog"), form = ui.node("reauth-form");
  const select = new Element("select"), option = new Element("option"), fields = new Element(), submit = new Element("button");
  option.value = "password"; select.append(option);
  fields.className = "reauth-password"; submit.type = "submit";
  form.append(fields, submit);
  form.elements = {method: select, password: {value: "synthetic-password"}};
  form.closest = () => dialog;
  let requests = 0, notices = 0;
  ui.context.fetch = async () => ++requests === 1 ? oldResponse.promise : newResponse.promise;
  ui.context.notified = () => { notices++; };
  ui.run(`
    api = productionAPI; notice = notified;
    state.recently_verified = false; state.login_methods = {password: true};
    bindAsync("reauth-form", "submit", submitReauthentication, "验证中…", () => reauthRequestCurrent);
  `);
  const oldVerification = ui.run("chooseReauthentication()");
  const rejected = assert.rejects(oldVerification, /身份验证已取消/);
  const oldSubmitting = form.listeners.submit({currentTarget: form, preventDefault() {}});
  ui.run("cancelReauthentication()"); await rejected;
  const newVerification = ui.run("chooseReauthentication()");
  const newSubmitting = form.listeners.submit({currentTarget: form, preventDefault() {}});
  oldResponse.resolve({ok: true, status: 200, json: async () => ({ok: true})}); await oldSubmitting;
  assert.equal(ui.run("state.recently_verified"), false);
  assert.equal(form.dataset.busy, "true");
  assert.equal(dialog.open, true);
  assert.equal(notices, 0);
  newResponse.resolve({ok: true, status: 200, json: async () => ({ok: true})});
  await newSubmitting; await newVerification;
  assert.equal(ui.run("state.recently_verified"), true);
  assert.equal(form.dataset.busy, "false");
  assert.equal(dialog.open, false);
});

test("old billing user lists and settings cannot render or invalidate a replacement session", async (t) => {
  for (const loader of ["loadBillingUsers", "loadBillingSettings"]) {
    for (const status of [200, 401]) {
      await t.test(`${loader}, response ${status}`, async () => {
        const ui = dashboard("owner"), response = deferred();
        let invalidations = 0, renders = 0;
        ui.context.fetch = async () => response.promise;
        ui.context.invalidated = () => { invalidations++; };
        ui.context.rendered = () => { renders++; };
        ui.run(`
          api = productionAPI; handleUnauthorized = invalidated;
          renderBillingUsers = renderBillingSettings = rendered;
          billingUserSearch = {unavailable() {}, reset() {}};
        `);
        const loading = ui.run(`${loader}()`);
        ui.run("identityGeneration++; resetBillingUserSearch()");
        response.resolve({ok: status === 200, status, json: async () => status === 200
          ? {users: [{id: "stale-user"}], usd_per_cny: "1.00"} : {error: {code: "invalid_session"}}});
        await loading;
        assert.equal(invalidations, 0);
        assert.equal(renders, 0);
      });
    }
  }
});
