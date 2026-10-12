"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const test = require("node:test");
const source = fs.readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {}; this.listeners = {}; this.value = ""; this.className = "";
    this.classList = {add: (name) => { this.className += " " + name; }, toggle: () => {}};
  }
  set textContent(text) { this.text = text; this.children = []; }
  get textContent() { return (this.text || "") + this.children.map((child) => child.textContent).join(" "); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.text = ""; this.children = children; }
  setAttribute(name, value) { this.attributes[name] = value; if (name === "value") this.value = value; }
  addEventListener(name, fn) { this.listeners[name] = fn; }
  dispatchEvent(event) { return this.listeners[event.type]?.(event); }
}
function dashboard() {
  const nodes = new Map();
  const node = (id) => { if (!nodes.has(id)) nodes.set(id, new Element()); return nodes.get(id); };
  const context = vm.createContext({document: {getElementById: node, createElement: (tag) => new Element(tag)}, Event: class {constructor(type) { this.type = type; }}});
  vm.runInContext(app, context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "self", role: "owner"}}');
  return {run, node, context, values: (id = "picker") => JSON.parse(run(`JSON.stringify(byId("${id}").modelPicker.values())`))};
}
const candidates = [{id: "opaque-one", group: "GPT"}, {id: "opaque-two", group: "GPT"}, {id: "different-id", group: "Gemini"}, {id: "final-id", group: "Claude"}];
function descendants(node) { return node.children.flatMap((child) => [child, ...descendants(child)]); }
const find = (node, label) => descendants(node).find((child) => child.attributes["aria-label"] === label);
const change = (node, checked) => { node.checked = checked; node.listeners.change(); };

test("brand groups come from catalog metadata; full and partial states use explicit model IDs", () => {
  const ui = dashboard(); ui.context.candidates = candidates;
  ui.run('renderModelPicker(byId("picker"), candidates, {selected: ["opaque-one"]})');
  const host = ui.node("picker"), gpt = find(host, "全选 GPT 模型"), all = find(host, "全选当前目录模型");
  assert.equal(gpt.indeterminate, true); assert.equal(all.indeterminate, true);
  assert.equal(gpt.attributes["aria-checked"], "mixed");
  change(gpt, true); assert.deepEqual(ui.values(), ["opaque-one", "opaque-two"]);
  assert.equal(gpt.checked, true); assert.equal(gpt.indeterminate, false); assert.equal(all.indeterminate, true);
  change(all, true); assert.deepEqual(ui.values(), candidates.map((item) => item.id));
  candidates.push({id: "future-model", group: "GPT"});
  assert.equal(ui.values().includes("future-model"), false, "all means the saved candidate set"); candidates.pop();
  descendants(host).find((child) => child.textContent === "清空").listeners.click();
  assert.deepEqual(ui.values(), []); assert.throws(() => ui.run('selectedPickerModels("picker")'), /至少选择一个/);
});

test("search preserves selections and single mode cannot accumulate selections", () => {
  const ui = dashboard(); ui.context.candidates = candidates;
  ui.run('renderModelPicker(byId("picker"), candidates, {single: true, selected: ["opaque-one", "opaque-two"]})');
  assert.deepEqual(ui.values(), ["opaque-one"]);
  const host = ui.node("picker");
  assert.equal(find(host, "全选 GPT 模型"), undefined);
  change(descendants(host).find((node) => node.value === "different-id"), true);
  assert.deepEqual(ui.values(), ["different-id"]);
  ui.run('byId("picker").modelPicker.filter("opaque")');
  assert.deepEqual(ui.values(), ["different-id"]);
  assert.equal(descendants(host).find((node) => node.dataset.brand === "Gemini").hidden, true);
  ui.run('byId("picker").modelPicker.filter("")');
  assert.equal(descendants(host).find((node) => node.dataset.brand === "Gemini").hidden, false);
});

test("late catalog response after replacing a form or identity cannot overwrite the current selection", async () => {
  const ui = dashboard(); let resolve;
  ui.context.request = () => new Promise((done) => { resolve = done; });
  ui.run('api = request');
  const pending = ui.run('loadFormModelPicker("picker", "manageable")');
  ui.run('identityGeneration++; state.user.id = "next-user"');
  resolve({models: candidates}); await pending;
  assert.equal(ui.node("picker").modelPicker, null);
  ui.context.request = async () => ({models: candidates}); ui.run('api = request');
  await ui.run('loadFormModelPicker("picker", "manageable", ["final-id"])');
  assert.deepEqual(ui.values(), ["final-id"]);
});

test("same-period subscriptions remain independent; expired and stopped records are hidden", () => {
  const ui = dashboard();
  ui.context.detail = {subscriptions: [
    {id: "one", name: "First", tier: "day", enabled: true, expires_at: "2099-01-01", charge_enabled: false},
    {id: "two", name: "Second", tier: "day", enabled: true, expires_at: "2099-01-01"},
    {id: "old", enabled: true, expires_at: "2000-01-01"}, {id: "stopped", enabled: false},
  ]};
  assert.equal(ui.run('billingSubscriptions(detail).filter(billingSubscriptionEnabled).length'), 2);
  assert.equal(ui.run('billingSubscription(detail, "two").name'), "Second");
  assert.equal(ui.run('billingSubscription(detail, "day")'), null);
});

test("custom cycles validate integer days and renewal uses the purchased plan length", () => {
  const ui = dashboard();
  ui.context.form = {elements: {tier: {value: "custom"}, period_days: {value: "3650"}}};
  assert.equal(ui.run('billingFormPeriodDays(form)'), 3650);
  for (const invalid of ["0", "3651", "1.5", "1e2", "-1"]) {
    ui.context.form.elements.period_days.value = invalid;
    assert.throws(() => ui.run('billingFormPeriodDays(form)'), /1–3650/);
  }
  assert.equal(ui.run('billingRenewalExpiry({expires_at: "2099-01-01T00:00:00Z"}, {tier: "custom", period_days: 10}, 2)'), "2099-01-21T00:00:00.000Z");
});

test("restricted API Key creation rejects empty selections before any write", async () => {
  const ui = dashboard(); let writes = 0;
  ui.context.form = {elements: {model_scope: {value: "restricted"}}};
  ui.context.FormData = class { *[Symbol.iterator]() { yield ["name", "limited"]; yield ["expires_days", "90"]; } };
  ui.context.request = async () => { writes++; return {}; };
  ui.run('api = request; sensitiveAction = async (action) => action()');
  await assert.rejects(ui.run('createKey({currentTarget: form})'), /至少选择一个/);
  assert.equal(writes, 0);
});

test("late API Key creation cannot expose a key to a replacement identity", async () => {
  const ui = dashboard(); let resolve, shown = 0;
  ui.context.form = {elements: {model_scope: {value: "unrestricted"}}};
  ui.context.FormData = class { *[Symbol.iterator]() { yield ["name", "unrestricted"]; yield ["expires_days", "90"]; } };
  ui.context.request = () => new Promise((done) => { resolve = done; });
  ui.context.secret = () => { shown++; };
  ui.run('api = request; sensitiveAction = async (action) => action(); showSecret = secret');
  const pending = ui.run('createKey({currentTarget: form})');
  ui.run('identityGeneration++; state.user.id = "replacement"');
  resolve({api_key: "synthetic-secret"}); await pending;
  assert.equal(shown, 0);
});
