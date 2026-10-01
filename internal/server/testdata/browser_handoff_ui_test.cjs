"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));
class Element {
  constructor(id = "") {
    this.id = id; this.children = []; this.dataset = {}; this.attributes = {}; this.value = ""; this.checked = false; this.open = false; this.listeners = {};
    this.classList = {toggle() {}, add() {}, remove() {}};
  }
  replaceChildren(...children) { this.children = children; }
  append(...children) { this.children.push(...children); }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  showModal() { this.open = true; }
  close() { this.open = false; this.listeners.close?.(); }
  reset() {}
}
const user = {id: "user", role: "member", status: "active"};
const device = {id: "device", name: "设备", status: "active", created_at: "2026-01-01"};
const key = {id: "key", name: "工作密钥", device_id: "device", status: "active", secret_available: true, key_prefix: "gw_public", expires_at: "2099-01-01"};
const initial = (overrides = {}) => ({user, browser_client_enabled: true, recently_verified: true, devices: [device], api_keys: [key], projects: [], passkeys: [], ...overrides});
function deferred() { let resolve, reject; const promise = new Promise((a, b) => {resolve = a; reject = b;}); return {promise, resolve, reject}; }
function setup(value = initial(), handler = () => ({})) {
  const nodes = new Map(), calls = [], navigation = [], messages = [];
  const node = (id) => { if (!nodes.has(id)) nodes.set(id, new Element(id)); return nodes.get(id); };
  node("browser-handoff-name").value = "网页工作台"; node("browser-handoff-days").value = "90";
  const context = vm.createContext({URLSearchParams, initial: value, document: {getElementById: node, querySelectorAll: () => [], createElement: () => new Element()},
    location: {assign: (url) => navigation.push(url)}, localMessage: (_, message) => messages.push(message),
    request: async (pathname, options = {}, current) => { calls.push({pathname, options}); if (current && !current()) throw Object.assign(new Error("stale"), {code: "stale_request"}); return pathname === "/admin/state" ? value : handler(pathname, options); },
  });
  vm.runInContext(application, context);
  const run = (code) => vm.runInContext(code, context);
  run(`state = initial; api = request; setLocalMessage = localMessage; renderState = (value) => {state = value; renderBrowserHandoff();};
    renderDevices = renderAPIKeys = renderSelects = renderResourceSummary = renderOnboarding = () => {};
    bindBrowserHandoff();`);
  return {run, context, node, calls, navigation, messages, value, submit: () => run("submitBrowserHandoff({preventDefault() {}})")};
}

test("only current, retrievable keys with active devices are selectable; devices sort newest and unique names include disabled entries", () => {
  const ui = setup(initial({devices: [device, {...device, id: "new", created_at: "2026-09-01"}, {...device, id: "off", name: "网页工作台设备", status: "disabled"}, {...device, id: "off2", name: "网页工作台设备 2", status: "disabled"}],
    api_keys: [key, {...key, id: "expired", expires_at: "2020-01-01"}, {...key, id: "invalid-time", expires_at: "bad"}, {...key, id: "disabled", status: "disabled"}, {...key, id: "old", secret_available: false}, {...key, id: "orphan", device_id: "gone"}, {...key, id: "off-device", device_id: "off"}]}));
  assert.equal(ui.run("browserHandoffKeys().map(k => k.id).join(',')"), "key");
  assert.equal(ui.run("browserHandoffDevices()[0].id"), "new");
  assert.equal(ui.run("browserHandoffDeviceName()"), "网页工作台设备 3");
});

test("unconfigured entry cannot open; both HTML entries share one modal", async () => {
  const ui = setup(initial({browser_client_enabled: false})); await ui.run("openBrowserHandoff()");
  assert.equal(ui.calls.length, 0); assert.equal(ui.node("browser-handoff-dialog").open, false);
  const html = fs.readFileSync(path.join(__dirname, "../assets/index.html"), "utf8");
  assert.equal((html.match(/data-browser-handoff-open disabled/g) || []).length, 2);
  assert.equal((html.match(/id="browser-handoff-dialog"/g) || []).length, 1);
});

test("existing key launches same tab, remembers only explicit choice, and double submit signs once", async () => {
  const pending = deferred(); const ui = setup(initial(), () => pending.promise);
  await ui.run("openBrowserHandoff()"); assert.equal(ui.run("browserHandoffDraft.remember"), false);
  assert.match(ui.node("browser-handoff-key-detail").textContent, /工作密钥.*gw_public.*2099/);
  ui.run("browserHandoffDraft.remember = true");
  const first = ui.submit(); await ui.submit();
  assert.equal(ui.calls.filter(c => c.pathname === "/admin/browser-handoffs").length, 1);
  assert.deepEqual(JSON.parse(ui.calls.at(-1).options.body), {api_key_id: "key", remember_key: true});
  pending.resolve({launch_url: "https://ai.water555.com/#handoff_version=1&code=synthetic"}); await first;
  assert.deepEqual(ui.navigation, ["https://ai.water555.com/#handoff_version=1&code=synthetic"]);
});

test("create defaults inherit models and newest device; issuance failure retries same key ID only", async () => {
  let issuance = 0; const result = {id: "new-key", prefix: "gw_created", api_key: "secret-must-be-cleared", expires_at: "2099-01-01"};
  const ui = setup(initial({devices: [device, {...device, id: "new-device", created_at: "2026-09-01"}], api_keys: []}), (pathname) => {
    if (pathname === "/admin/api-keys") return result;
    if (++issuance === 1) throw Object.assign(new Error("签发失败"), {status: 503});
    return {launch_url: "https://ai.water555.com/#handoff_version=1&code=retry"};
  });
  await ui.run("openBrowserHandoff()"); await ui.submit();
  assert.equal(result.api_key, ""); assert.equal(ui.run("browserHandoffDraft.keyID"), "new-key"); assert.equal(ui.run("browserHandoffDraft.create"), false);
  const create = ui.calls.find(c => c.pathname === "/admin/api-keys");
  assert.deepEqual(JSON.parse(create.options.body), {name: "网页工作台", device_id: "new-device", expires_days: 90, models: []});
  await ui.submit(); assert.equal(ui.calls.filter(c => c.pathname === "/admin/api-keys").length, 1); assert.equal(ui.navigation.length, 1);
});

test("no active device creates a unique device then key; verification cancellation creates nothing", async () => {
  const ui = setup(initial({devices: [{...device, name: "网页工作台设备", status: "disabled"}], api_keys: []}), (pathname) => pathname === "/admin/devices" ? {...device, id: "created", name: "网页工作台设备 2"} : pathname === "/admin/api-keys" ? {id: "created-key", prefix: "gw_created", expires_at: "2099-01-01"} : {launch_url: "https://ai.water555.com/#code=valid"});
  await ui.run("openBrowserHandoff()"); await ui.submit();
  assert.deepEqual(JSON.parse(ui.calls.find(c => c.pathname === "/admin/devices").options.body), {name: "网页工作台设备 2"});
  assert.equal(JSON.parse(ui.calls.find(c => c.pathname === "/admin/api-keys").options.body).device_id, "created");
  const cancelled = setup(initial({devices: [], api_keys: [], recently_verified: false}));
  cancelled.run(`reauthenticate = async () => {throw Object.assign(new Error("已取消"), {code: "reauth_cancelled"});}`);
  await cancelled.run("openBrowserHandoff()"); await cancelled.submit();
  assert.equal(cancelled.calls.length, 1); assert.equal(cancelled.run("browserHandoffDraft.busy"), false);
});

test("ambiguous creation refreshes resources and never repeats a write on retry", async () => {
  const ui = setup(initial({api_keys: []}), (pathname) => {
    if (pathname === "/admin/api-keys") { ui.value.api_keys.push({...key, id: "eventually-created"}); throw Object.assign(new Error("offline"), {network: true}); }
    return {launch_url: "https://ai.water555.com/#code=valid"};
  });
  await ui.run("openBrowserHandoff()"); await ui.submit();
  assert.equal(ui.calls.filter(c => c.pathname === "/admin/state").length, 2);
  assert.equal(ui.run("browserHandoffUncertain.refreshed"), true); assert.equal(ui.run("browserHandoffDraft.create"), false);
  await ui.submit(); assert.equal(ui.calls.filter(c => c.pathname === "/admin/api-keys").length, 1); assert.equal(ui.navigation.length, 1);
});

test("unknown creation without resources requires explicit create after refresh", async () => {
  const ui = setup(initial({api_keys: []}), () => {throw Object.assign(new Error("timeout"), {status: 504});});
  await ui.run("openBrowserHandoff()"); await ui.submit(); await ui.submit();
  assert.equal(ui.calls.filter(c => c.pathname === "/admin/api-keys").length, 1);
  assert.equal(ui.node("browser-handoff-submit").disabled, true);
  ui.node("browser-handoff-new").listeners.click();
  assert.equal(ui.run("browserHandoffUncertain"), null); assert.equal(ui.run("browserHandoffDraft.create"), true);
});

test("closing and reopening during creation retains completion but does not launch or create twice", async () => {
  const pending = deferred(); const ui = setup(initial({api_keys: []}), () => pending.promise);
  await ui.run("openBrowserHandoff()"); const first = ui.submit(); await new Promise(setImmediate);
  ui.node("browser-handoff-dialog").close(); await ui.run("openBrowserHandoff()"); await ui.submit();
  assert.equal(ui.calls.filter(c => c.pathname === "/admin/api-keys").length, 1);
  pending.resolve({id: "created", prefix: "gw_created", expires_at: "2099-01-01"}); await first;
  assert.equal(ui.run("browserHandoffDraft.create"), false); assert.equal(ui.run("browserHandoffDraft.keyID"), "created"); assert.equal(ui.navigation.length, 0);
});

test("closing during ambiguous write keeps reopened creation blocked until resources are refreshed", async () => {
  const pending = deferred(); const ui = setup(initial({api_keys: []}), () => pending.promise);
  await ui.run("openBrowserHandoff()"); const first = ui.submit(); await new Promise(setImmediate);
  ui.node("browser-handoff-dialog").close(); await ui.run("openBrowserHandoff()");
  pending.reject(Object.assign(new Error("offline"), {network: true})); await first;
  assert.equal(ui.run("browserHandoffUncertain.refreshed"), false); assert.equal(ui.run("browserHandoffDraft.resourcesReady"), false);
  await ui.submit(); assert.equal(ui.calls.filter(c => c.pathname === "/admin/api-keys").length, 1);
});

test("logout or identity replacement while signing prevents late navigation", async () => {
  for (const mutation of ["loggingOut = true; resetBrowserHandoff()", "identityGeneration++; state.user = {id:'other'}; resetBrowserHandoff()", "byId('browser-handoff-dialog').close()"] ) {
    const pending = deferred(), ui = setup(initial(), () => pending.promise);
    await ui.run("openBrowserHandoff()"); const first = ui.submit(); ui.run(mutation);
    pending.resolve({launch_url: "https://ai.water555.com/#code=stale"}); await first; assert.equal(ui.navigation.length, 0);
  }
});
