"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

// Run the production functions without bootstrapping the dashboard or a browser.
const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return {promise, resolve, reject};
}

function node() {
  return {
    value: "", dataset: {}, disabled: false, textContent: "",
    classList: {add() {}, remove() {}, toggle() {}},
    setAttribute() {}, append() {}, replaceChildren() {},
    querySelector() { return null; }, querySelectorAll() { return []; },
    matches() { return false; }, closest() { return this; },
  };
}

function picker() {
  return {
    users: [], disabled: true,
    setUsers(users) { this.users = users; this.disabled = false; },
    unavailable() { this.users = []; this.disabled = true; },
    reset() { this.unavailable(); },
  };
}

function reply(pathname, options) {
  if (options.method === "POST") return {};
  if (pathname === "/admin/billing/users") return {users: [{id: "target-user", username: "target"}]};
  const userID = decodeURIComponent(pathname.split("?")[0].split("/").pop());
  return {user: {id: userID, username: userID}, subscriptions: {}, ledger: []};
}

function app(respond = reply) {
  const nodes = new Map();
  const getNode = (id) => {
    if (!nodes.has(id)) nodes.set(id, node());
    return nodes.get(id);
  };
  const requests = [];
  const search = picker();
  const globalSearch = picker();
  const form = {
    values: {cny_amount: " 1000000000.123456 ", reason: " 指定用户充值 "},
    resets: 0,
    reset() { this.resets++; this.values = {}; },
  };
  const context = vm.createContext({
    document: {getElementById: getNode, querySelectorAll: () => [], createElement: node},
    URLSearchParams, crypto: {randomUUID: () => "9fb20881-d775-4f61-b9b3-bd7fdc0361f8"},
    FormData: class {
      constructor(value) { this.values = {...value.values}; }
      get(name) { return this.values[name] ?? null; }
    },
    form, search, globalSearch,
    request: async (pathname, options = {}) => {
      requests.push({pathname, options});
      return respond(pathname, options);
    },
  });
  vm.runInContext(application, context, {filename: "assets/app.js"});
  const run = (code) => vm.runInContext(code, context);
  run(`
    state = {user: {id: "owner", username: "owner", role: "owner"}};
    billingUserID = "target-user";
    billingUserSearch = search;
    globalUserSearch = globalSearch;
    api = request;
    sensitiveAction = (operation) => operation();
    notice = () => {};
    renderBillingDetail = (detail) => { billingDetail = detail; };
  `);
  return {run, getNode, requests, search, globalSearch, form};
}

test("recharge requires successfully loaded details for the selected user", async (t) => {
  for (const state of ["unloaded", "loading", "failed", "mismatched"]) {
    await t.test(state, async () => {
      const response = deferred();
      const harness = app(() => response.promise);
      let loading;
      if (state !== "unloaded") loading = harness.run("loadBillingDetail()");
      if (state === "failed") {
        response.reject(new Error("billing unavailable"));
        await assert.rejects(loading, /billing unavailable/);
      } else if (state === "mismatched") {
        response.resolve({user: {id: "another-user"}});
        await assert.rejects(loading, /与所选用户不一致/);
      }
      await assert.rejects(harness.run("rechargeBillingUser({currentTarget: form})"), /加载成功后再操作/);
      assert.equal(harness.requests.filter(({options}) => options.method === "POST").length, 0);
      if (state === "loading") {
        response.resolve({user: {id: "target-user"}});
        await loading;
      }
    });
  }
});

test("search text cannot change the loaded recharge target or exact amount", async () => {
  const harness = app();
  await harness.run("loadBillingDetail()");
  harness.getNode("billing-user-search").value = "another-user";
  assert.equal(harness.run("selectedBillingUserID()"), "target-user");
  harness.getNode("billing-user-search").value = "";
  await harness.run("rechargeBillingUser({currentTarget: form})");
  const writes = harness.requests.filter(({options}) => options.method === "POST");
  assert.equal(writes.length, 1);
  assert.equal(writes[0].pathname, "/admin/billing/users/target-user/recharges");
  assert.deepEqual(JSON.parse(writes[0].options.body), {
    cny_amount: "1000000000.123456", reason: "指定用户充值",
    operation_id: "9fb20881-d775-4f61-b9b3-bd7fdc0361f8",
  });
  assert.equal(harness.form.resets, 1);
});

test("an old recharge completion preserves the newly selected user's details and form", async () => {
  const write = deferred();
  const harness = app((pathname, options) => options.method === "POST" ? write.promise : reply(pathname, options));
  await harness.run("loadBillingDetail()");
  const recharge = harness.run("rechargeBillingUser({currentTarget: form})");
  harness.run('billingUserID = "new-user"');
  await harness.run("loadBillingDetail()");
  const beforeCompletion = harness.requests.length;
  write.resolve({});
  await recharge;
  assert.deepEqual(harness.requests.slice(beforeCompletion).map(({pathname}) => pathname), ["/admin/billing/users"]);
  assert.equal(harness.run("billingDetail.user.id"), "new-user");
  assert.equal(harness.form.resets, 0);
});

test("late billing and global lists cannot refill search after session invalidation", async () => {
  const response = deferred();
  const harness = app(() => response.promise);
  harness.search.setUsers([{id: "old-user"}]);
  harness.globalSearch.setUsers([{id: "old-user"}]);
  const billing = harness.run("loadBillingUsers()");
  const global = harness.run("loadGlobalUsage(new URLSearchParams())");
  harness.run("handleUnauthorized()");
  response.resolve({users: [{id: "late-user", username: "late"}]});
  await Promise.all([billing, global]);
  for (const search of [harness.search, harness.globalSearch]) {
    assert.deepEqual(search.users, []);
    assert.equal(search.disabled, true);
  }
});

test("recharge completion during logout cannot start new reads or refill search", async () => {
  const write = deferred();
  const harness = app((pathname, options) => options.method === "POST" ? write.promise : reply(pathname, options));
  await harness.run("loadBillingDetail()");
  const recharge = harness.run("rechargeBillingUser({currentTarget: form})");
  harness.run("loggingOut = true; resetBillingUserSearch(); globalUserSearch.reset()");
  const beforeCompletion = harness.requests.length;
  write.resolve({});
  await recharge;
  await harness.run("loadBillingUsers()");
  await harness.run('loadBillingDetail("owner")');
  assert.equal(harness.requests.length, beforeCompletion);
  assert.deepEqual(harness.search.users, []);
  assert.equal(harness.search.disabled, true);
});
