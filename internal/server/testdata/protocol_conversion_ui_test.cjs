"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {}; this.className = "";
    this.value = ""; this.disabled = false; this.listeners = {}; this.text = "";
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => this.classList.toggle(name, true), remove: (name) => this.classList.toggle(name, false),
      toggle: (name, active) => { const set = new Set(this.className.split(/\s+/).filter(Boolean)); if (active) set.add(name); else set.delete(name); this.className = [...set].join(" "); },
    };
  }
  get textContent() { return this.text + this.children.map((child) => child.textContent).join(""); }
  set textContent(value) { this.text = value; this.children = []; }
  append(...children) { children.forEach((child) => { child.parent = this; this.children.push(child); }); }
  replaceChildren(...children) { this.text = ""; this.children = []; this.append(...children); }
  setAttribute(key, value) { this.attributes[key] = value; }
  addEventListener(name, listener) { this.listeners[name] = listener; }
  matches(selector) { return selector.startsWith(".") ? this.classList.contains(selector.slice(1)) : this.tagName === selector; }
  querySelectorAll(selector) { return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest() { return this; }
}

function dashboard({confirmResult = true} = {}) {
  const nodes = new Map(), root = new Element(), confirms = [];
  const node = (id) => { if (!nodes.has(id)) { const value = new Element(); nodes.set(id, value); root.append(value); } return nodes.get(id); };
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    location: {hash: "#model-access"}, URLSearchParams, DOMException, AbortController,
    window: {setTimeout: () => 0, clearTimeout: () => {}, confirm: (message) => { confirms.push(message); return confirmResult; }},
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "owner", role: "owner"}, recently_verified: true};');
  const calls = [];
  let settings = {claude_responses_enabled: false, revision: 3, updated_at: "2026-11-09T08:00:00Z"};
  const ui = {run, node, calls, confirms, conflict: false};
  context.respond = async (requestPath, options = {}) => {
    const body = options.body ? JSON.parse(options.body) : null;
    calls.push({path: requestPath, method: options.method || "GET", body});
    if (requestPath !== "/admin/protocol-conversion") throw new Error(`unexpected ${requestPath}`);
    if (options.method === "PUT") {
      if (ui.conflict) {
        ui.conflict = false;
        settings = {...settings, revision: settings.revision + 1};
        throw Object.assign(new Error("协议转换设置已被其他管理员修改，请刷新后重试"), {status: 409, code: "protocol_conversion_revision_changed"});
      }
      assert.equal(body.revision, settings.revision);
      settings = {claude_responses_enabled: body.claude_responses_enabled, revision: settings.revision + 1, updated_at: "2026-11-09T09:00:00Z"};
    }
    return settings;
  };
  run("api = respond;");
  return ui;
}

test("the switch loads disabled by default and enables without a confirmation", async () => {
  const ui = dashboard();
  await ui.run("loadProtocolConversion()");
  assert.equal(ui.node("protocol-conversion-status").textContent, "已禁用");
  assert.equal(ui.node("protocol-conversion-status").dataset.status, "disabled");
  assert.equal(ui.node("protocol-conversion-toggle").textContent, "开启转换");
  assert.equal(ui.node("protocol-conversion-toggle").disabled, false);
  await ui.run("toggleProtocolConversion()");
  assert.deepEqual(ui.calls.at(-1), {path: "/admin/protocol-conversion", method: "PUT", body: {claude_responses_enabled: true, revision: 3}});
  assert.equal(ui.confirms.length, 0);
  assert.equal(ui.node("protocol-conversion-status").textContent, "已开启");
  assert.equal(ui.node("protocol-conversion-toggle").textContent, "禁用转换");
  assert.match(ui.node("notice").textContent, /已开启模型协议转换/);
});

test("disabling asks first and sends nothing when the Owner cancels", async () => {
  for (const confirmResult of [false, true]) {
    const ui = dashboard({confirmResult});
    await ui.run("loadProtocolConversion()");
    await ui.run("toggleProtocolConversion()");
    await ui.run("toggleProtocolConversion()");
    assert.equal(ui.confirms.length, 1);
    assert.match(ui.confirms[0], /立即被拒绝/);
    const puts = ui.calls.filter((call) => call.method === "PUT");
    assert.equal(puts.length, confirmResult ? 2 : 1);
    assert.equal(ui.node("protocol-conversion-status").textContent, confirmResult ? "已禁用" : "已开启");
  }
});

test("a stale revision reloads the current state and reports the conflict", async () => {
  const ui = dashboard();
  await ui.run("loadProtocolConversion()");
  ui.conflict = true;
  await assert.rejects(ui.run("toggleProtocolConversion()"), /其他管理员/);
  assert.equal(ui.calls.at(-1).method, "GET");
  assert.equal(ui.run("protocolConversion.revision"), 4);
});

test("the model route card warns only when conversion is known to be off", () => {
  const ui = dashboard();
  for (const [value, hidden] of [[false, false], [true, true], [null, true]]) {
    ui.run(`applyModelRoutes({routes: [], max_routes: 32, protocol_conversion_enabled: ${value}}, [])`);
    assert.equal(ui.node("model-routes-conversion-note").classList.contains("hidden"), hidden);
  }
});
