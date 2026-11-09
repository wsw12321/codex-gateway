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

const catalog = [{id: "claude-sonnet-5-5", group: "Claude"}, {id: "gpt-6-sol", group: "GPT"}];

function dashboard() {
  const nodes = new Map(), root = new Element();
  const node = (id) => { if (!nodes.has(id)) { const value = new Element(); nodes.set(id, value); root.append(value); } return nodes.get(id); };
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    location: {hash: "#keys"}, URLSearchParams, DOMException, AbortController,
    window: {setTimeout: () => 0, clearTimeout: () => {}, confirm: () => true},
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "member", role: "member"}, recently_verified: true};');
  const calls = [];
  context.respond = async (path, options = {}) => {
    calls.push({path, method: options.method || "GET", body: options.body ? JSON.parse(options.body) : null});
    if (path.startsWith("/admin/model-catalog")) return {models: catalog};
    if (path === "/admin/model-routes" && options.method === "PUT") {
      const routes = JSON.parse(options.body).routes.map((route) => ({...route, available: true}));
      return {routes, max_routes: 32};
    }
    if (path === "/admin/model-routes") {
      return {routes: [
        {source_model: "gpt-6-sol", target_model: "claude-sonnet-5-5", available: true},
        {source_model: "old-alias", target_model: "claude-retired", available: false},
      ], max_routes: 32};
    }
    throw new Error(`unexpected ${path}`);
  };
  run("api = respond;");
  return {run, node, calls};
}

test("loading renders each rule and flags targets that are no longer available", async () => {
  const ui = dashboard();
  await ui.run("loadModelRoutes()");
  const rows = ui.node("model-routes").querySelectorAll(".model-route-row");
  assert.equal(rows.length, 2);
  assert.equal(rows[0].querySelector(".model-route-source").value, "gpt-6-sol");
  assert.equal(rows[0].querySelector(".model-route-target").value, "claude-sonnet-5-5");
  assert.equal(rows[0].querySelector(".model-route-warning").classList.contains("hidden"), true);
  const retired = rows[1].querySelector(".model-route-target");
  assert.match(retired.textContent, /claude-retired（当前不可用）/);
  assert.equal(rows[1].querySelector(".model-route-warning").classList.contains("hidden"), false);
  assert.equal(ui.node("model-routes-count").textContent, "2 / 32 条");
  assert.equal(ui.node("model-routes-save").disabled, false);
});

test("saving sends the whole trimmed list, skips blank rows and applies the server result", async () => {
  const ui = dashboard();
  await ui.run("loadModelRoutes()");
  ui.run('modelRoutes.draft.splice(1, 1); addModelRoute(); addModelRoute();');
  ui.run('modelRoutes.draft[1].source = "  gpt-mini  "; modelRoutes.draft[1].target = "claude-sonnet-5-5";');
  await ui.run("saveModelRoutes()");
  const put = ui.calls.find((call) => call.method === "PUT");
  assert.deepEqual(put.body, {routes: [
    {source_model: "gpt-6-sol", target_model: "claude-sonnet-5-5"},
    {source_model: "gpt-mini", target_model: "claude-sonnet-5-5"},
  ]});
  assert.equal(ui.run("modelRoutes.draft.length"), 2);
  assert.match(ui.node("notice").textContent, /已保存 2 条模型路由/);
});

test("invalid drafts are rejected before any request", async () => {
  for (const [draft, message] of [
    ['[{source: "gpt-6-sol", target: ""}]', /填写原模型并选择目标模型/],
    ['[{source: "bad model", target: "claude-sonnet-5-5"}]', /格式无效/],
    ['[{source: "claude-sonnet-5-5", target: "claude-sonnet-5-5"}]', /不能相同/],
    ['[{source: "a", target: "claude-sonnet-5-5"}, {source: "a", target: "gpt-6-sol"}]', /重复/],
    ['[{source: "a", target: "b"}, {source: "b", target: "claude-sonnet-5-5"}]', /不能串联/],
  ]) {
    const ui = dashboard();
    await ui.run("loadModelRoutes()");
    ui.run(`modelRoutes.draft = ${draft};`);
    await assert.rejects(ui.run("saveModelRoutes()"), message);
    assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  }
});

test("usage shows the requested model beside the actual billed model", () => {
  const ui = dashboard();
  const routed = ui.run('usageModelCell({requested_model: "gpt-6-sol", model: "claude-sonnet-5-5", state: "completed"})');
  assert.equal(routed.className, "usage-model-routed");
  assert.equal(routed.textContent, "gpt-6-sol → claude-sonnet-5-5");
  const degraded = ui.run('usageModelCell({requested_model: "gpt-6-sol", model: "gpt-6-mini", state: "degraded"})');
  assert.equal(degraded.className, "usage-model-degraded");
  const plain = ui.run('usageModelCell({requested_model: "gpt-6-sol", model: "gpt-6-sol", state: "completed"})');
  assert.equal(plain.className, "");
  assert.equal(plain.textContent, "gpt-6-sol");
});
