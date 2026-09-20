"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return {promise, resolve, reject};
}

// Enough DOM behavior to execute the production rendering and control gating.
// Events are explicit, so these tests require no browser or third-party modules.
class Node {
  constructor(tag = "div") {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.listeners = {};
    this.className = "";
    this.disabled = false;
    this.checked = false;
    this.indeterminate = false;
    this.value = "";
    this.parentNode = null;
    this._text = "";
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (...names) => { this.className = [...new Set([...this.className.split(/\s+/).filter(Boolean), ...names])].join(" "); },
      remove: (...names) => { this.className = this.className.split(/\s+/).filter((name) => !names.includes(name)).join(" "); },
      toggle: (name, force) => {
        const add = force === undefined ? !this.classList.contains(name) : force;
        this.classList[add ? "add" : "remove"](name);
        return add;
      },
    };
  }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map((child) => child.textContent ?? String(child)).join(""); }
  get childElementCount() { return this.children.filter((child) => child instanceof Node).length; }
  append(...children) {
    for (const child of children) {
      if (child instanceof Node) child.parentNode = this;
      this.children.push(child);
    }
  }
  replaceChildren(...children) { this.children = []; this._text = ""; this.append(...children); }
  setAttribute(name, value) {
    this.attributes[name] = String(value);
    if (name === "class") this.className = String(value);
    else if (name.startsWith("data-")) this.dataset[name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())] = String(value);
    else if (["id", "name", "type", "value"].includes(name)) this[name] = String(value);
  }
  getAttribute(name) { return this.attributes[name] ?? this[name] ?? null; }
  removeAttribute(name) { delete this.attributes[name]; }
  matches(selector) {
    return selector.split(",").some((part) => {
      const token = part.trim();
      if (!token) return false;
      const tag = token.match(/^[a-z]+/i)?.[0];
      if (tag && this.tagName !== tag.toUpperCase()) return false;
      const id = token.match(/#([\w-]+)/)?.[1];
      if (id && this.id !== id) return false;
      for (const [, name] of token.matchAll(/\.([\w-]+)/g)) if (!this.classList.contains(name)) return false;
      for (const [, name, value] of token.matchAll(/\[([\w-]+)(?:=["']?([^\]"']+)["']?)?\]/g)) {
        const actual = this.getAttribute(name);
        if (actual == null || (value !== undefined && String(actual) !== value)) return false;
      }
      if (token.includes(":checked") && !this.checked) return false;
      return true;
    });
  }
  closest(selector) { return this.matches(selector) ? this : this.parentNode?.closest(selector) ?? null; }
  querySelectorAll(selector) {
    const descendants = this.children.filter((child) => child instanceof Node);
    return descendants.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] ?? null; }
  addEventListener(name, callback) { (this.listeners[name] ??= []).push(callback); }
  dispatch(name, values = {}) {
    const event = {target: this, currentTarget: this, preventDefault() {}, ...values};
    return Promise.all((this.listeners[name] ?? []).map((listener) => listener(event)));
  }
  focus() {}
  showModal() { this.open = true; this.modalOpenings = (this.modalOpenings || 0) + 1; }
  close() { this.open = false; }
  reset() { for (const input of Object.values(this.elements ?? {})) input.value = ""; }
}

const users = [
  {id: "alpha", username: "alpha", display_name: "爱丽丝", role: "member", status: "active"},
  {id: "beta", username: "beta", display_name: "张三", role: "member", status: "active"},
  {id: "gamma", username: "gamma", display_name: "张四", role: "member", status: "disabled"},
];

function app(respond = () => ({}), readRespond = null) {
  const nodes = new Map();
  const getNode = (id) => {
    if (!nodes.has(id)) { const node = new Node(); node.id = id; nodes.set(id, node); }
    return nodes.get(id);
  };
  const makeForm = (id, values) => {
    const form = getNode(id);
    form.tagName = "FORM";
    form.className = "billing-batch-form";
    form.elements = {};
    for (const [name, value] of Object.entries(values)) {
      const input = new Node(name === "tier" ? "select" : "input");
      input.name = name;
      input.value = value;
      form.elements[name] = input;
      form.append(input);
    }
    const submit = new Node("button");
    submit.type = "submit";
    form.append(submit);
    return form;
  };
  const rechargeForm = makeForm("billing-batch-recharge-form", {cny_amount: " 1000000000.123456 ", reason: " 团队充值 "});
  const subscriptionForm = makeForm("billing-batch-subscription-form", {tier: "day", quota_usd: " 25.123456 ", period_count: "1", reason: " 团队订阅 "});
  const searchInput = getNode("billing-batch-search");
  searchInput.tagName = "INPUT";
  const selectAll = getNode("billing-batch-select-all");
  selectAll.tagName = "INPUT";
  selectAll.type = "checkbox";
  getNode("billing-batch-panel").append(rechargeForm, subscriptionForm, searchInput, selectAll, getNode("billing-batch-user-rows"));
  const search = {setUsers() {}, unavailable() {}, reset() {}};
  const requests = [];
  const notices = [];
  let nextID = 0;
  const context = vm.createContext({
    document: {
      getElementById: getNode,
      createElement: (tag) => new Node(tag),
      querySelectorAll: (selector) => [...new Set([...nodes.values()].flatMap((node) => [node, ...node.querySelectorAll("*")]))].filter((node) => node.matches(selector)),
      addEventListener() {},
    },
    window: {clearTimeout() {}, setTimeout() { return 0; }, confirm: () => true},
    URLSearchParams, crypto: {randomUUID: () => `00000000-0000-4000-8000-${String(++nextID).padStart(12, "0")}`},
    FormData: class {
      constructor(form) { this.values = Object.fromEntries(Object.entries(form.elements).map(([name, input]) => [name, input.value])); }
      get(name) { return this.values[name] ?? null; }
    },
    rechargeForm, subscriptionForm, search,
    request: async (pathname, options = {}) => {
      requests.push({pathname, options});
      if (!options.method) {
        if (readRespond) return readRespond(pathname, options);
        if (pathname === "/admin/billing/users") return {users};
        const userID = pathname.split("?")[0].split("/").pop();
        return {user: {id: userID === "me" ? "owner" : userID}, subscriptions: {}, ledger: []};
      }
      return respond(pathname, options);
    },
    captureNotice: (...values) => notices.push(values),
  });
  vm.runInContext(application, context, {filename: "assets/app.js"});
  const run = (code) => vm.runInContext(code, context);
  run(`
    state = {user: {id: "owner", username: "owner", role: "owner"}, recently_verified: true};
    billingUsers = ${JSON.stringify(users)};
    billingUserSearch = search;
    globalUserSearch = search;
    billingBatchUsersReady = true;
    api = request;
    notice = captureNotice;
    renderBillingDetail = (detail) => { billingDetail = detail; };
  `);
  return {
    run, getNode, requests, notices, rechargeForm, subscriptionForm,
    set: (name, value) => { context[name] = value; },
    snapshot: (code) => JSON.parse(run(`JSON.stringify(${code})`)),
    select: (...ids) => run(`billingBatchSelectedIDs = new Set(${JSON.stringify(ids)})`),
    prepare: (kind = "recharge") => run(`prepareBillingBatch({currentTarget: ${kind === "recharge" ? "rechargeForm" : "subscriptionForm"}, preventDefault() {}}, ${JSON.stringify(kind)})`),
    writes: () => requests.filter(({options}) => options.method),
  };
}

test("batch writes require an explicit selection and a loaded owner user list", async (t) => {
  for (const setup of [
    "", "billingBatchUsersReady = false; billingBatchSelectedIDs.add('alpha')",
    "state.user.role = 'member'; billingBatchSelectedIDs.add('alpha')",
    "state = null; billingBatchSelectedIDs.add('alpha')",
    "loggingOut = true; billingBatchSelectedIDs.add('alpha')",
    "billingBatchSelectedIDs.add('missing-user')",
  ]) {
    await t.test(setup || "empty selection never falls back to the owner", () => {
      const harness = app();
      harness.run(setup);
      assert.throws(() => harness.prepare());
      assert.equal(harness.run("billingBatch"), null);
      assert.equal(harness.writes().length, 0);
    });
  }
});

test("batch amounts use exact decimals, rejecting unsupported and zero amounts", () => {
  const harness = app();
  assert.equal(harness.run('billingBatchAmount(" 999999999999999999.999999 ")'), "999999999999999999.999999");
  assert.equal(harness.run('billingBatchTotal("999999999999999999.999999", 3)'), "2999999999999999999.999997");
  assert.equal(harness.run('billingBatchTotal("0.000001", 3)'), "0.000003");
  assert.equal(harness.run('billingBatchTotal("0.1", 3)'), "0.3");
  for (const invalid of ["", "0", "0.000000", "-1", "+1", "1e3", ".1", "1.", "01", "1,000", "NaN", "1.0000001", "1000000000000000000"]) {
    assert.throws(() => harness.run(`billingBatchAmount(${JSON.stringify(invalid)})`), invalid);
  }
});

test("search matches usernames and display names while select-all affects only visible users", () => {
  const harness = app();
  harness.run('setBillingBatchUserSelected("alpha", true)');
  harness.getNode("billing-batch-search").value = " 张 ";
  harness.run("renderBillingBatchUsers()");
  assert.deepEqual(harness.snapshot("billingBatchMatches().map(user => user.id)"), ["beta", "gamma"]);
  assert.equal(harness.run('billingBatchSelectedIDs.has("alpha")'), true);
  harness.run("selectBillingBatchMatches(true)");
  assert.deepEqual(harness.snapshot("Array.from(billingBatchSelectedIDs).sort()"), ["alpha", "beta", "gamma"]);
  harness.run("selectBillingBatchMatches(false)");
  assert.deepEqual(harness.snapshot("Array.from(billingBatchSelectedIDs)"), ["alpha"]);
  harness.getNode("billing-batch-search").value = " ALPHA ";
  harness.run("renderBillingBatchUsers()");
  assert.deepEqual(harness.snapshot("billingBatchMatches().map(user => user.id)"), ["alpha"]);
  assert.equal(harness.getNode("billing-batch-select-all").checked, true);
  harness.getNode("billing-batch-search").value = "no matches";
  harness.run("renderBillingBatchUsers(); selectBillingBatchMatches(true)");
  assert.equal(harness.run("billingBatchSelectedIDs.size"), 1);
  assert.equal(harness.getNode("billing-batch-select-all").disabled, true);
  harness.run("clearBillingBatchSelection()");
  assert.equal(harness.run("billingBatchSelectedIDs.size"), 0);
});

test("selection renders accessible checkboxes and locks during a prepared batch", () => {
  const harness = app();
  harness.run("renderBillingBatchUsers()");
  const list = harness.getNode("billing-batch-user-rows");
  const checkboxes = list.querySelectorAll('input[type="checkbox"]');
  assert.equal(checkboxes.length, 3);
  assert.ok(checkboxes.every((box) => box.getAttribute("aria-label") || box.closest("label")));
  harness.select("alpha");
  harness.run("syncBillingBatchControls()");
  assert.equal(harness.rechargeForm.querySelector("button").disabled, false);
  harness.prepare();
  assert.ok(Object.values(harness.rechargeForm.elements).every((input) => input.disabled));
  assert.ok(Object.values(harness.subscriptionForm.elements).every((input) => input.disabled));
  assert.equal(harness.getNode("billing-batch-search").disabled, true);
  assert.equal(harness.getNode("billing-batch-start").disabled, false);
  harness.run('setBillingBatchUserSelected("beta", true); selectBillingBatchMatches(true); clearBillingBatchSelection()');
  assert.deepEqual(harness.snapshot("Array.from(billingBatchSelectedIDs)"), ["alpha"]);
  assert.throws(() => harness.prepare());
});

test("list reload failures disable new previews and late results cannot repopulate logged-out selections", async () => {
  const response = deferred();
  const harness = app(() => ({}), () => response.promise);
  harness.select("alpha");
  const loading = harness.run("loadBillingUsers()");
  assert.equal(harness.run("billingBatchUsersReady"), false);
  assert.throws(() => harness.prepare());
  harness.run("handleUnauthorized()");
  response.resolve({users});
  await loading;
  assert.equal(harness.run("billingBatchUsersReady"), false);
  assert.equal(harness.run("billingBatchSelectedIDs.size"), 0);
  assert.deepEqual(harness.snapshot("billingUsers"), []);

  const failed = app(() => ({}), () => { throw new Error("user list unavailable"); });
  failed.select("alpha");
  await assert.rejects(failed.run("loadBillingUsers()"), /user list unavailable/);
  assert.equal(failed.run("billingBatchUsersReady"), false);
  assert.throws(() => failed.prepare());
});

test("preview freezes targets, exact parameters and unique per-user operation IDs without writing", async () => {
  const harness = app();
  harness.select("alpha", "beta", "alpha");
  harness.prepare();
  assert.equal(harness.run("billingBatch.phase"), "preview");
  assert.equal(harness.writes().length, 0);
  const original = harness.snapshot("billingBatch.items");
  assert.deepEqual(original.map((item) => item.user.id), ["alpha", "beta"]);
  assert.equal(new Set(original.map((item) => item.operationID)).size, 2);
  assert.deepEqual(harness.snapshot("billingBatch.payload"), {cny_amount: "1000000000.123456", reason: "团队充值"});
  harness.select("gamma");
  harness.rechargeForm.elements.cny_amount.value = "1";
  harness.rechargeForm.elements.reason.value = "changed";
  harness.run('billingUsers[0].username = "renamed"; billingUserID = "gamma"');
  await harness.run("startBillingBatch()");
  assert.deepEqual(harness.writes().map(({pathname}) => pathname), ["/admin/billing/users/alpha/recharges", "/admin/billing/users/beta/recharges"]);
  for (const [index, {options}] of harness.writes().entries()) {
    assert.deepEqual(JSON.parse(options.body), {cny_amount: "1000000000.123456", reason: "团队充值", operation_id: original[index].operationID});
    assert.equal(options.method, "POST");
  }
  assert.equal(harness.run("billingBatch.items[0].user.username"), "alpha");
});

test("each subscription tier forwards a shared quota and finite or unlimited period count", async (t) => {
  for (const tier of ["day", "week", "month"]) {
    for (const count of [0, 1, 99]) {
      await t.test(`${tier}: ${count}`, async () => {
        const harness = app();
        harness.select("alpha", "beta");
        harness.subscriptionForm.elements.tier.value = tier;
        harness.subscriptionForm.elements.period_count.value = String(count);
        harness.prepare("subscription");
        await harness.run("startBillingBatch()");
        assert.equal(harness.writes().length, 2);
        for (const {pathname, options} of harness.writes()) {
          assert.ok(pathname.endsWith(`/subscriptions/${tier}`));
          assert.equal(options.method, "PUT");
          const payload = JSON.parse(options.body);
          assert.equal(payload.quota_usd, "25.123456");
          assert.equal(payload.period_count, count);
          assert.equal(payload.reason, "团队订阅");
        }
      });
    }
  }
});

test("invalid subscription parameters and missing reasons never prepare a batch", () => {
  for (const [name, value] of [["tier", "year"], ["period_count", "100"], ["period_count", "-1"], ["period_count", "1.5"], ["period_count", ""], ["quota_usd", "0"], ["reason", "  "]]) {
    const harness = app();
    harness.select("alpha");
    harness.subscriptionForm.elements[name].value = value;
    assert.throws(() => harness.prepare("subscription"), `${name}=${value}`);
    assert.equal(harness.writes().length, 0);
  }
});

test("execution is sequential and repeated confirmation clicks cannot duplicate writes", async () => {
  const first = deferred();
  const second = deferred();
  const harness = app((pathname) => pathname.includes("/alpha/") ? first.promise : second.promise);
  harness.select("alpha", "beta");
  harness.prepare();
  const executing = harness.run("startBillingBatch()");
  await Promise.resolve();
  assert.equal(harness.writes().length, 1);
  await harness.run("startBillingBatch()");
  assert.equal(harness.writes().length, 1);
  first.resolve({});
  for (let index = 0; index < 10 && harness.writes().length < 2; index++) await Promise.resolve();
  assert.equal(harness.writes().length, 2);
  second.resolve({});
  await executing;
  assert.deepEqual(harness.snapshot("billingBatch.items.map(item => item.status)"), ["success", "success"]);
  assert.equal(harness.run("billingBatch.phase"), "finished");
  await harness.run("startBillingBatch()");
  assert.equal(harness.writes().length, 2);
});

test("partial failures continue and retries retain exact bodies while skipping successful users", async () => {
  let fail = true;
  const harness = app((pathname) => {
    if (pathname.includes("/beta/") && fail) throw Object.assign(new Error("user unavailable"), {status: 422, code: "invalid_user"});
    return {};
  });
  harness.select("alpha", "beta", "gamma");
  harness.prepare();
  await harness.run("startBillingBatch()");
  assert.deepEqual(harness.snapshot("billingBatch.items.map(item => item.status)"), ["success", "failed", "success"]);
  const failed = harness.writes()[1];
  fail = false;
  await harness.run("retryBillingBatch()");
  assert.equal(harness.writes().length, 4);
  assert.deepEqual(harness.writes()[3], failed);
  assert.deepEqual(harness.snapshot("billingBatch.items.map(item => item.status)"), ["success", "success", "success"]);
});

test("lost responses and server errors remain uncertain and retry the original operation", async (t) => {
  for (const error of [Object.assign(new Error("network lost"), {network: true}), Object.assign(new Error("server error"), {status: 503})]) {
    await t.test(error.message, async () => {
      let attempts = 0;
      const harness = app(() => { if (!attempts++) throw error; return {}; });
      harness.select("alpha", "beta");
      harness.prepare();
      await harness.run("startBillingBatch()");
      assert.deepEqual(harness.snapshot("billingBatch.items.map(item => item.status)"), ["unknown", "success"]);
      await harness.run("retryBillingBatch()");
      assert.equal(harness.writes().length, 3);
      assert.deepEqual(harness.writes()[2], harness.writes()[0]);
      assert.deepEqual(harness.snapshot("billingBatch.items.map(item => item.status)"), ["success", "success"]);
    });
  }
});

test("reauthentication cancellation pauses pending users, and retry resumes the same operation", async () => {
  const harness = app();
  harness.select("alpha", "beta");
  harness.prepare();
  const originalIDs = harness.snapshot("billingBatch.items.map(item => item.operationID)");
  harness.run(`state.recently_verified = false; reauthenticate = async () => {
    const error = new Error("身份验证已取消。"); error.code = "reauth_cancelled"; throw error;
  }`);
  await harness.run("startBillingBatch()");
  assert.equal(harness.writes().length, 0);
  assert.equal(harness.run("billingBatch.phase"), "paused");
  assert.deepEqual(harness.snapshot("billingBatch.items.map(item => item.status)"), ["pending", "pending"]);
  harness.run("state.recently_verified = true");
  await harness.run("retryBillingBatch()");
  assert.deepEqual(harness.writes().map(({options}) => JSON.parse(options.body).operation_id), originalIDs);
});

test("expired verification retries the same per-user operation through sensitiveAction", async () => {
  let writes = 0;
  const harness = app(() => {
    if (!writes++) throw Object.assign(new Error("verify again"), {code: "recent_identity_verification_required", status: 403});
    return {};
  });
  harness.run("reauthenticate = async () => { state.recently_verified = true; }");
  harness.select("alpha");
  harness.prepare();
  await harness.run("startBillingBatch()");
  assert.equal(harness.writes().length, 2);
  assert.deepEqual(harness.writes()[1], harness.writes()[0]);
  assert.equal(harness.run("billingBatch.items[0].status"), "success");
});

test("permission failures pause the remaining queue and preserve earlier uncertainty on retry", async () => {
  let attempt = 0;
  const harness = app((pathname) => {
    if (!pathname.includes("/alpha/")) return {};
    if (!attempt++) throw Object.assign(new Error("lost response"), {network: true});
    throw Object.assign(new Error("forbidden"), {status: 403, code: "forbidden"});
  });
  harness.select("alpha", "beta");
  harness.prepare();
  await harness.run("startBillingBatch()");
  assert.deepEqual(harness.snapshot("billingBatch.items.map(item => item.status)"), ["unknown", "success"]);
  await harness.run("retryBillingBatch()");
  assert.equal(harness.run("billingBatch.phase"), "paused");
  assert.deepEqual(harness.snapshot("billingBatch.items.map(item => item.status)"), ["unknown", "success"]);
  assert.deepEqual(harness.writes()[2], harness.writes()[0]);

  const forbidden = app(() => { throw Object.assign(new Error("forbidden"), {status: 403, code: "forbidden"}); });
  forbidden.select("alpha", "beta");
  forbidden.prepare();
  await forbidden.run("startBillingBatch()");
  assert.equal(forbidden.writes().length, 1);
  assert.equal(forbidden.run("billingBatch.phase"), "paused");
  assert.deepEqual(forbidden.snapshot("billingBatch.items.map(item => item.status)"), ["failed", "pending"]);
});

test("definitive role revocation or account disablement clears the batch and stops all later users", async (t) => {
  for (const code of ["owner_required", "user_disabled"]) {
    await t.test(code, async () => {
      const harness = app(() => { throw Object.assign(new Error("permission changed"), {status: 403, code}); });
      harness.select("alpha", "beta");
      harness.prepare();
      await harness.run("startBillingBatch()");
      assert.equal(harness.writes().length, 1);
      assert.equal(harness.requests.length, 1);
      assert.equal(harness.run("billingBatch"), null);
      assert.equal(harness.run("billingBatchSelectedIDs.size"), 0);
      assert.equal(harness.run("billingBatchUsersReady"), false);
    });
  }
});

test("cancelling verification during an uncertain retry retains the original uncertainty", async () => {
  const harness = app(() => { throw Object.assign(new Error("lost response"), {network: true}); });
  harness.select("alpha");
  harness.prepare();
  await harness.run("startBillingBatch()");
  harness.run(`state.recently_verified = false; reauthenticate = async () => {
    throw Object.assign(new Error("身份验证已取消。"), {code: "reauth_cancelled"});
  }`);
  await harness.run("retryBillingBatch()");
  assert.equal(harness.run("billingBatch.phase"), "paused");
  assert.equal(harness.run("billingBatch.items[0].status"), "unknown");
  assert.equal(harness.writes().length, 1);
});

test("account changes during reauthentication cannot send a frozen batch under another account", async () => {
  const auth = deferred();
  const harness = app();
  harness.select("alpha", "beta");
  harness.prepare();
  harness.run("state.recently_verified = false");
  harness.set("authPromise", auth.promise);
  harness.run("reauthenticate = () => authPromise");
  const executing = harness.run("startBillingBatch()");
  await Promise.resolve();
  harness.run('state = {user: {id: "another-owner", role: "owner"}, recently_verified: true}');
  auth.resolve();
  await executing;
  assert.equal(harness.writes().length, 0);
  assert.equal(harness.run("billingBatch"), null);
});

test("logout and late mutation replies clear sensitive batch state and stop subsequent work", async (t) => {
  for (const invalidate of ["handleUnauthorized()", "loggingOut = true; resetBillingBatchState()", "state.user.role = 'member'"]) {
    await t.test(invalidate, async () => {
      const response = deferred();
      const harness = app(() => response.promise);
      harness.select("alpha", "beta");
      harness.prepare();
      const executing = harness.run("startBillingBatch()");
      await Promise.resolve();
      assert.equal(harness.writes().length, 1);
      harness.run(invalidate);
      const before = harness.requests.length;
      response.resolve({});
      await executing;
      assert.equal(harness.requests.length, before);
      assert.equal(harness.run("billingBatch"), null);
      assert.equal(harness.run("billingBatchSelectedIDs.size"), 0);
    });
  }
});

test("a late response from an invalidated batch cannot clear or execute a new preview", async () => {
  const response = deferred();
  const harness = app(() => response.promise);
  harness.select("alpha");
  harness.prepare();
  const executing = harness.run("startBillingBatch()");
  await Promise.resolve();
  harness.run("resetBillingBatchState(); billingBatchUsersReady = true");
  harness.rechargeForm.elements.cny_amount.value = "2";
  harness.rechargeForm.elements.reason.value = "new batch";
  harness.select("beta");
  harness.prepare();
  const preview = harness.snapshot("billingBatch");
  response.resolve({});
  await executing;
  assert.deepEqual(harness.snapshot("billingBatch"), preview);
  assert.equal(harness.writes().length, 1);
  assert.equal(harness.requests.length, 1);
});

test("ending a batch with uncertain results requires explicit confirmation before IDs are discarded", async () => {
  const harness = app(() => { throw Object.assign(new Error("lost response"), {network: true}); });
  harness.select("alpha");
  harness.prepare();
  await harness.run("startBillingBatch()");
  harness.run("window.confirm = () => false; finishBillingBatch()");
  assert.equal(harness.run("billingBatch.items[0].status"), "unknown");
  harness.run("window.confirm = () => true; finishBillingBatch()");
  assert.equal(harness.run("billingBatch"), null);
  assert.equal(harness.getNode("billing-batch-result").classList.contains("hidden"), true);
  assert.equal(harness.getNode("billing-batch-result-rows").children.length, 0);
});

test("summary refresh errors cannot turn successful writes into failed items", async () => {
  const harness = app();
  harness.run('loadBillingUsers = async () => { throw new Error("summary unavailable"); }; loadBillingDetail = async () => { throw new Error("detail unavailable"); }');
  harness.select("alpha", "beta");
  harness.prepare();
  await harness.run("startBillingBatch()");
  assert.deepEqual(harness.snapshot("billingBatch.items.map(item => item.status)"), ["success", "success"]);
  assert.equal(harness.run("billingBatch.phase"), "finished");
  await harness.run("retryBillingBatch()");
  assert.equal(harness.writes().length, 2);
});

test("in-page refresh recovers the user list without discarding uncertain-operation IDs", async () => {
  let readsFail = true;
  const harness = app(
    () => { throw Object.assign(new Error("lost write response"), {network: true}); },
    (pathname) => {
      if (readsFail) throw new Error("read unavailable");
      if (pathname === "/admin/billing/users") return {users};
      return {user: {id: "owner"}, subscriptions: {}, ledger: []};
    },
  );
  harness.select("alpha");
  harness.prepare();
  await harness.run("startBillingBatch()");
  assert.equal(harness.run("billingBatchUsersReady"), false);
  const before = harness.snapshot("billingBatch.items");
  readsFail = false;
  await harness.run("refreshBillingBatchData()");
  assert.equal(harness.run("billingBatchUsersReady"), true);
  assert.deepEqual(harness.snapshot("billingBatch.items"), before);
  assert.equal(harness.writes().length, 1);
});

test("batch event bindings allow a new owner to act while the old owner's request remains outstanding", async () => {
  const oldResponse = deferred();
  const harness = app((pathname) => pathname.includes("/alpha/") ? oldResponse.promise : {});
  harness.run('bindBillingBatchAction("billing-batch-start", "click", startBillingBatch)');
  harness.select("alpha");
  harness.prepare();
  const start = harness.getNode("billing-batch-start");
  const previousClick = start.dispatch("click");
  await Promise.resolve();
  await start.dispatch("click");
  assert.equal(harness.writes().length, 1);
  harness.run('handleUnauthorized(); state = {user: {id: "new-owner", username: "new-owner", role: "owner"}, recently_verified: true}; billingUsers = ' + JSON.stringify(users) + '; billingBatchUsersReady = true');
  harness.rechargeForm.elements.cny_amount.value = "10";
  harness.rechargeForm.elements.reason.value = "new owner action";
  harness.select("beta");
  harness.prepare();
  await start.dispatch("click");
  assert.equal(harness.writes().length, 2);
  assert.equal(harness.run("billingBatch.actorUserID"), "new-owner");
  assert.equal(harness.run("billingBatch.items[0].status"), "success");
  const before = harness.snapshot("billingBatch");
  const noticesBefore = harness.notices.length;
  oldResponse.reject(new Error("old request failed late"));
  await previousClick;
  assert.deepEqual(harness.snapshot("billingBatch"), before);
  assert.equal(harness.notices.length, noticesBefore);
});

test("concurrent sensitive actions share one reauthentication dialog and all resume after verification", async () => {
  const harness = app();
  const form = harness.getNode("reauth-form");
  const dialog = harness.getNode("reauth-dialog");
  dialog.tagName = "DIALOG";
  dialog.append(form);
  form.elements = {method: new Node("select"), password: new Node("input")};
  const password = new Node("option");
  password.value = "password";
  form.elements.method.append(password);
  const fields = new Node();
  fields.className = "reauth-password";
  form.append(form.elements.method, form.elements.password, fields);
  harness.run('state.recently_verified = false; state.login_methods = {password: true}; globalThis.resumedActions = []');
  const first = harness.run('sensitiveAction(async () => { resumedActions.push("first"); })');
  const second = harness.run('sensitiveAction(async () => { resumedActions.push("second"); })');
  assert.equal(dialog.modalOpenings, 1);
  assert.deepEqual(harness.snapshot("resumedActions"), []);
  await harness.run("submitReauthentication({currentTarget: document.getElementById('reauth-form')})");
  await Promise.all([first, second]);
  assert.deepEqual(harness.snapshot("resumedActions.sort()"), ["first", "second"]);
  harness.run("state.recently_verified = false");
  const third = harness.run('sensitiveAction(async () => { resumedActions.push("third"); })');
  assert.equal(dialog.modalOpenings, 2);
  harness.run('reauthReject(Object.assign(new Error("身份验证已取消。"), {code: "reauth_cancelled"}))');
  await assert.rejects(third, /身份验证已取消/);
  assert.deepEqual(harness.snapshot("resumedActions.sort()"), ["first", "second"]);
});
