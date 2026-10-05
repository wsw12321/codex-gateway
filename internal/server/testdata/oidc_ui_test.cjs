"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const callback = fs.readFileSync(path.join(__dirname, "../assets/oidc-callback.js"), "utf8");
const source = fs.readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));
const deferred = () => {let resolve; const promise = new Promise((value) => {resolve = value;}); return {promise, resolve};};
const settle = () => new Promise((resolve) => setImmediate(resolve));
class Element {
  constructor() {
    this.listeners = {}; this.hidden = true; this.disabled = false; this.value = ""; this.elements = {}; this.open = false;
    this.classList = {add: () => {this.hidden = true;}, remove: () => {this.hidden = false;}, toggle: (_, value) => {this.hidden = value;}};
  }
  addEventListener(name, handler) {this.listeners[name] = handler;}
  reset() {}
  showModal() {this.open = true;}
  close() {this.open = false;}
  closest() {return null;}
  querySelector() {return new Element();}
  querySelectorAll() {return this.options || [];}
  focus() {}
  reportValidity() {return Boolean(this.elements.username?.value && this.elements.display_name?.value);}
}
function setup(script, handler, query = "?state=original-state&code=secret-code") {
  const nodes = new Map(), calls = [], navigation = [], storage = new Map(), listeners = {};
  const node = (id) => {if (!nodes.has(id)) nodes.set(id, new Element()); return nodes.get(id);};
  node("oidc-register-form").elements = {username: new Element(), display_name: new Element()};
  const context = vm.createContext({URL, URLSearchParams, Object, setTimeout, clearTimeout,
    document: {getElementById: node, querySelectorAll: () => []},
    history: {replaceState() {}}, location: {href: "https://gateway.test/", search: query, hash: "#billing", replace: (url) => navigation.push(url), assign: (url) => navigation.push(url)},
    sessionStorage: {getItem: (key) => storage.get(key), setItem: (key, value) => storage.set(key, value), removeItem: (key) => storage.delete(key)},
    window: {addEventListener: (name, handler) => {listeners[name] = handler;}}, navigator: {},
    fetch: async (pathname, options) => {
      calls.push({pathname, body: options.body ? JSON.parse(options.body) : undefined, keepalive: options.keepalive});
      const value = await handler(pathname);
      return {ok: !value.httpStatus || value.httpStatus < 400, status: value.httpStatus || 200, json: async () => value};
    },
  });
  const run = (code) => vm.runInContext(code, context);
  run(script);
  return {node, run, context, calls, navigation, storage, listeners};
}
const registration = {result: "registration_required", flow_id: "registration-flow", masked_email: "s***@example.test", expires_at: "2099-01-01T00:00:00Z"};
function fillRegistration(ui, username = "member", displayName = "中文用户") {
  ui.run("oidcOpenRegistration()");
  ui.node("oidc-register-form").elements.username.value = username;
  ui.node("oidc-register-form").elements.display_name.value = displayName;
}

test("first login waits for an explicit choice and creates only once with its flow ID", async () => {
  const pending = deferred();
  const ui = setup(callback, (pathname) => pathname === "/auth/oidc/complete" ? registration : pending.promise);
  await settle();
  assert.equal(ui.node("oidc-registration").hidden, false);
  assert.equal(ui.calls.length, 1);
  ui.run("oidcOpenRegistration()");
  await ui.run('oidcRegister("create")');
  assert.equal(ui.calls.length, 1, "empty profile must not create an account");
  fillRegistration(ui);
  const first = ui.run('oidcRegister("create")');
  await ui.run('oidcRegister("create")');
  assert.deepEqual(ui.calls.at(-1).body, {flow_id: "registration-flow", username: "member", display_name: "中文用户"});
  assert.equal(ui.calls.length, 2);
  pending.resolve({result: "login"}); await first;
  assert.deepEqual(ui.navigation, ["/#overview"]);
});

test("binding choice cancels opening and continues through the fixed local login marker", async () => {
  const ui = setup(callback, (pathname) => pathname === "/auth/oidc/complete" ? registration : {ok: true});
  await settle(); await ui.run('oidcRegister("link")');
  assert.equal(ui.calls.at(-1).pathname, "/auth/oidc/cancel");
  assert.deepEqual(ui.calls.at(-1).body, {flow_id: "original-state"});
  assert.deepEqual(ui.navigation, ["/?link=water5"]);
  assert.equal(ui.storage.size, 0);
});

test("explicit cancellation leaves no account and never retries a consumed choice", async () => {
  const ui = setup(callback, (pathname) => pathname === "/auth/oidc/complete" ? registration : {ok: true});
  await settle(); await ui.run('oidcRegister("cancel")'); await ui.run('oidcRegister("create")');
  assert.equal(ui.calls.length, 2);
  assert.match(ui.node("oidc-message").textContent, /未创建本站账号/);
  assert.equal(ui.navigation.length, 0);
});

test("pagehide cancels the exact in-flight exchange and rejects its late registration response", async () => {
  const pending = deferred();
  const ui = setup(callback, (pathname) => pathname === "/auth/oidc/complete" ? pending.promise : {ok: true});
  ui.listeners.pagehide();
  assert.equal(ui.calls.at(-1).pathname, "/auth/oidc/cancel");
  assert.equal(ui.calls.at(-1).keepalive, true);
  assert.deepEqual(ui.calls.at(-1).body, {flow_id: "original-state"});
  pending.resolve(registration); await settle();
  assert.equal(ui.node("oidc-registration").hidden, true);
  assert.equal(ui.navigation.length, 0);
});

test("leaving an unconsumed registration page cancels the original authorization state", async () => {
  const ui = setup(callback, (pathname) => pathname === "/auth/oidc/complete" ? registration : {ok: true});
  await settle(); ui.listeners.pagehide();
  assert.deepEqual(ui.calls.at(-1).body, {flow_id: "original-state"});
});

test("safe profile failures rotate tokens while preserving input and the original deadline", async () => {
  for (const [httpStatus, code] of [[400, "invalid_profile"], [409, "username_taken"]]) {
    let attempts = 0;
    const ui = setup(callback, (pathname) => pathname === "/auth/oidc/complete" ? registration : ++attempts === 1
      ? {httpStatus, error: {code, message: "请修改名称"}, flow_id: "retry-flow", expires_at: registration.expires_at} : {result: "login"});
    await settle(); fillRegistration(ui, "taken", "保留名称");
    await ui.run('oidcRegister("create")');
    assert.equal(ui.node("oidc-registration").hidden, false);
    assert.equal(ui.node("oidc-register-form").elements.display_name.value, "保留名称");
    assert.equal(ui.node("oidc-register-message").hidden, false);
    ui.node("oidc-register-form").elements.username.value = "available";
    await ui.run('oidcRegister("create")');
    assert.deepEqual(ui.calls.at(-1).body, {flow_id: "retry-flow", username: "available", display_name: "保留名称"});
    assert.deepEqual(ui.navigation, ["/#overview"]);
  }
});

test("cancellation after token rotation uses the original state and discards a late retry", async () => {
  const pending = deferred(); let attempts = 0;
  const ui = setup(callback, (pathname) => pathname === "/auth/oidc/complete" ? registration : pathname === "/auth/oidc/cancel" ? {ok: true} : ++attempts === 1
    ? {httpStatus: 409, error: {code: "username_taken"}, flow_id: "retry-flow", expires_at: registration.expires_at} : pending.promise);
  await settle(); fillRegistration(ui);
  await ui.run('oidcRegister("create")');
  const retry = ui.run('oidcRegister("create")');
  ui.listeners.pagehide();
  assert.deepEqual(ui.calls.at(-1).body, {flow_id: "original-state"});
  pending.resolve({result: "login"}); await retry;
  assert.equal(ui.navigation.length, 0);
  assert.equal(ui.node("oidc-registration").hidden, true);
});

test("network, binding, expiry and malformed retry responses terminate registration", async () => {
  for (const result of [new Error("network"), {httpStatus: 409, error: {code: "identity_already_linked"}, flow_id: "retry-flow", expires_at: registration.expires_at},
    {httpStatus: 400, error: {code: "invalid_profile"}, flow_id: "retry-flow", expires_at: "2000-01-01T00:00:00Z"},
    {httpStatus: 409, error: {code: "username_taken"}},
    {httpStatus: 409, error: {code: "username_taken"}, flow_id: "registration-flow", expires_at: registration.expires_at}]) {
    const ui = setup(callback, (pathname) => {if (pathname === "/auth/oidc/complete") return registration; if (result instanceof Error) throw result; return result;});
    await settle(); fillRegistration(ui);
    await ui.run('oidcRegister("create")'); await ui.run('oidcRegister("create")');
    assert.equal(ui.calls.length, 2);
    assert.equal(ui.node("oidc-registration").hidden, true);
    assert.equal(ui.navigation.length, 0);
  }
});

test("reauthentication consumes navigation metadata and returns to the original feature without a mutation", async () => {
  const pending = deferred();
  const ui = setup(callback, () => pending.promise);
  ui.storage.set("cg_oidc_reauth_return", JSON.stringify({section: "billing"}));
  pending.resolve({result: "reauthenticated"}); await settle();
  assert.deepEqual(ui.navigation, ["/?verified=water5#billing"]);
  assert.equal(ui.storage.size, 0);
  ui.listeners.pagehide();
  assert.equal(ui.calls.length, 1, "successful return must not cancel its verification");
});

test("untrusted return metadata cannot redirect away from the gateway", async () => {
  const pending = deferred(); const ui = setup(callback, () => pending.promise);
  ui.storage.set("cg_oidc_reauth_return", JSON.stringify({section: "//evil.test/path"}));
  pending.resolve({result: "reauthenticated"}); await settle();
  assert.deepEqual(ui.navigation, ["/?verified=water5#security"]);
});

function setupReauth(handler) {
  const ui = setup(application, handler);
  const form = ui.node("reauth-form");
  form.elements.method = new Element(); form.elements.password = new Element();
  form.elements.method.options = ["passkey", "password", "oidc"].map((value) => Object.assign(new Element(), {value}));
  ui.run(`state = {user: {id: "member"}, recently_verified: false, login_methods: {oidc: true}};
    notice = setLocalMessage = setBusy = () => {};`);
  return ui;
}

test("SSO verification rejects the waiting sensitive operation before navigating", async () => {
  const ui = setupReauth(() => ({authorization_url: "https://accounts.test/authorize?state=reauth-flow"}));
  ui.run('globalThis.mutations = 0; globalThis.failure = ""; sensitiveAction(async () => {mutations++;}).catch((error) => {failure = error.code;});');
  assert.equal(ui.node("reauth-form").elements.method.value, "oidc");
  await ui.run('submitReauthentication({currentTarget: document.getElementById("reauth-form")})'); await settle();
  assert.equal(ui.run("mutations"), 0);
  assert.equal(ui.run("failure"), "reauth_cancelled");
  assert.deepEqual(ui.navigation, ["https://accounts.test/authorize?state=reauth-flow"]);
  assert.deepEqual(JSON.parse(ui.storage.get("cg_oidc_reauth_return")), {section: "billing"});
});

test("cancelled begin response only cancels its exact flow and cannot redirect or verify", async () => {
  const pending = deferred();
  const ui = setupReauth((pathname) => pathname === "/auth/oidc/reauth/begin" ? pending.promise : {ok: true});
  ui.run('reauthenticate().catch(() => {})');
  const request = ui.run('submitReauthentication({currentTarget: document.getElementById("reauth-form")})');
  ui.run("cancelReauthentication()");
  pending.resolve({authorization_url: "https://accounts.test/authorize?state=old-flow"});
  await assert.rejects(request, {code: "stale_request"});
  assert.deepEqual(ui.calls.at(-1).body, {flow_id: "old-flow"});
  assert.equal(ui.calls.at(-1).pathname, "/auth/oidc/cancel");
  assert.equal(ui.navigation.length, 0);
  assert.equal(ui.run("state.recently_verified"), false);
});

test("only allowlisted navigation survives leaving the app; credentials and mutation inputs do not", async () => {
  const ui = setupReauth(() => ({authorization_url: "https://accounts.test/authorize?state=private-flow"}));
  ui.run('location.hash = "#billing?password=private&operation=buy"; reauthenticate().catch(() => {});');
  ui.node("reauth-form").elements.password.value = "private-password";
  await ui.run('submitReauthentication({currentTarget: document.getElementById("reauth-form")})');
  assert.deepEqual([...ui.storage], [["cg_oidc_reauth_return", '{"section":"security"}']]);
  assert.equal(callback.includes("sessionStorage.setItem"), false, "authorization callback must never persist credentials");
});

test("restoring the original page cancels its exact pending reauthentication without replay", async () => {
  const ui = setupReauth(() => ({authorization_url: "https://accounts.test/authorize?state=abandoned-flow"}));
  ui.run('reauthenticate().catch(() => {});');
  await ui.run('submitReauthentication({currentTarget: document.getElementById("reauth-form")})');
  ui.run("cancelOIDCReauthenticationRedirect()");
  assert.equal(ui.calls.at(-1).pathname, "/auth/oidc/cancel");
  assert.deepEqual(ui.calls.at(-1).body, {flow_id: "abandoned-flow"});
  assert.equal(ui.run("state.recently_verified"), false);
  ui.run("cancelOIDCReauthenticationRedirect()");
  assert.equal(ui.calls.filter((item) => item.pathname === "/auth/oidc/cancel").length, 1);
});

test("manual unlink after an SSO return uses the existing verification window", async () => {
  const ui = setupReauth(() => ({logged_out: true}));
  ui.run(`state.login_methods.password = true; state.recently_verified = true;
    state.recent_verification_expires_at = new Date(Date.now() + 60000).toISOString();
    window.confirm = () => true;
    reauthenticate = async () => {throw new Error("must reuse completed SSO verification");};`);
  await ui.run("unlinkIdentity()");
  assert.deepEqual(ui.calls.map((value) => value.pathname), ["/admin/identity-link"]);
  assert.deepEqual(ui.navigation, ["/"]);
});

test("unlink still needs confirmation and an expired window cannot send the deletion before SSO verification", async () => {
  const ui = setupReauth(() => ({authorization_url: "https://accounts.test/authorize?state=unlink-flow"}));
  ui.run("state.login_methods.password = true; window.confirm = () => false;");
  await ui.run("unlinkIdentity()");
  assert.equal(ui.calls.length, 0);
  ui.run("window.confirm = () => true;");
  const unlink = ui.run("unlinkIdentity()");
  const cancelled = assert.rejects(unlink, {code: "reauth_cancelled"});
  ui.node("reauth-form").elements.method.value = "oidc";
  await ui.run('submitReauthentication({currentTarget: document.getElementById("reauth-form")})');
  await cancelled;
  assert.deepEqual(ui.calls.map((value) => value.pathname), ["/auth/oidc/reauth/begin"]);
  assert.deepEqual(ui.navigation, ["https://accounts.test/authorize?state=unlink-flow"]);
});
