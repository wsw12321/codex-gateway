"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.className = "";
    this.value = "";
    this.disabled = false;
    this.options = this.children;
    this._text = "";
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => this.classList.toggle(name, true),
      remove: (name) => this.classList.toggle(name, false),
      toggle: (name, enabled) => {
        const names = new Set(this.className.split(/\s+/).filter(Boolean));
        if (enabled === undefined) enabled = !names.has(name);
        if (enabled) names.add(name); else names.delete(name);
        this.className = [...names].join(" ");
      },
    };
  }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map((child) => child.textContent).join(""); }
  setAttribute(name, value) { this.attributes[name] = String(value); if (name === "value") this.value = String(value); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this._text = ""; this.children = []; this.append(...children); }
  querySelectorAll(selector) {
    return this.children.flatMap((child) => [...(selector === "option" && child.tagName === "option" ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
}

function dashboard() {
  const ids = ["model-identification-form", "model-identification-account", "model-identification-model",
    "model-identification-run", "model-identification-refresh", "model-identification-version",
    "model-identification-run-status", "model-identification-progress", "model-identification-results"];
  const nodes = new Map(ids.map((id) => [id, new Element()]));
  nodes.get("model-identification-form").dataset.busy = "false";
  nodes.get("model-identification-account").tagName = "select";
  nodes.get("model-identification-model").tagName = "select";
  const timeouts = [];
  const context = vm.createContext({
    document: {visibilityState: "visible", getElementById: (id) => nodes.get(id),
      createElement: (tag) => new Element(tag), querySelectorAll: () => []},
    window: {setTimeout: (callback, delay) => { timeouts.push({callback, delay}); return timeouts.length; }, clearTimeout: () => {}},
    location: {hash: "#model-identification"},
    Intl, URLSearchParams,
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "owner", role: "owner"}}; modelIdentificationOptionsReady = true;');
  return {nodes, context, run, timeouts};
}

test("owner page polls while visible and stops after leaving", async () => {
  const ui = dashboard();
  ui.context.calls = 0;
  ui.run("loadModelIdentifications = async () => { calls++; }; startModelIdentification();");
  await Promise.resolve();
  assert.equal(ui.context.calls, 1);
  assert.equal(ui.timeouts[0].delay, 3000);
  ui.run('location.hash = "#overview"; syncVisiblePolling();');
  ui.timeouts[0].callback();
  await Promise.resolve();
  assert.equal(ui.context.calls, 1);
  ui.run('location.hash = "#model-identification"; state.user.role = "member"; syncVisiblePolling();');
  assert.equal(ui.context.calls, 1);
});

test("failed rerun displays failure and retained valid conclusion", () => {
  const ui = dashboard();
  const account = ui.nodes.get("model-identification-account");
  const option = new Element("option");
  option.value = "0123456789abcdef";
  option.textContent = "u***@example.com · 可用";
  account.append(option);
  account.value = option.value;
  ui.run('renderModelIdentifications([{account_id:"0123456789abcdef",requested_model:"gpt-6-sol",run_id:"id",run_status:"failed",run_error_code:"model_identification_account_mismatch",run_progress:1,run_started_at:"2026-09-25T00:00:00Z",run_finished_at:"2026-09-25T00:01:00Z",conclusion:"claude-fable-5-1",closest_model:"claude-fable-5-1",match_level:"match",fit:0.36,margin:0.27,reference_version:"2026.09.5",completed_at:"2026-09-24T00:00:00Z",expires_at:"2026-10-24T00:00:00Z"}]);');
  assert.match(ui.nodes.get("model-identification-run-status").textContent, /本次失败.*不同的账号/);
  assert.match(ui.nodes.get("model-identification-results").textContent, /上次结论.*claude-fable-5-1.*2026.09.5/);
  assert.equal(ui.nodes.get("model-identification-progress").classList.contains("hidden"), true);
});
