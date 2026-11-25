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

test("family scopes cover a brand including later models and never shrink silently", () => {
  const ui = dashboard(); ui.context.candidates = candidates;
  ui.run('renderModelPicker(byId("picker"), candidates, {families: true, selected: ["family:gpt", "opaque-one", "final-id", "family:unknown"]})');
  const host = ui.node("picker");
  const gptFamily = find(host, "GPT 全系"), claudeFamily = find(host, "Claude 全系"), gpt = find(host, "全选 GPT 模型");
  // Explicit IDs covered by a family are not submitted twice; unknown tokens are dropped.
  assert.deepEqual(ui.values(), ["family:gpt", "final-id"]);
  assert.equal(gptFamily.checked, true); assert.equal(claudeFamily.checked, false);
  const opaqueTwo = descendants(host).find((node) => node.value === "opaque-two");
  assert.equal(opaqueTwo.checked, true); assert.equal(opaqueTwo.disabled, true); assert.equal(gpt.disabled, true);
  assert.match(descendants(host).find((node) => node.className === "model-picker-count").textContent, /已选 3 \/ 4 · GPT 全系/);
  change(claudeFamily, true);
  assert.deepEqual(ui.values(), ["family:gpt", "family:claude"]);
  change(gptFamily, false);
  assert.deepEqual(ui.values(), ["family:claude", "opaque-one", "opaque-two"], "leaving a family keeps today's models");
  assert.equal(opaqueTwo.disabled, false);
  descendants(host).find((child) => child.textContent === "清空").listeners.click();
  assert.deepEqual(ui.values(), []);
  ui.run('byId("picker").modelPicker.set(["family:gemini", "opaque-one"])');
  assert.deepEqual(ui.values(), ["family:gemini", "opaque-one"]);
  assert.deepEqual(JSON.parse(ui.run('JSON.stringify(selectedPickerModels("picker"))')), ["family:gemini", "opaque-one"]);
});

test("family scopes are offered only to billing pickers", () => {
  const ui = dashboard(); ui.context.candidates = candidates;
  ui.run('renderModelPicker(byId("picker"), candidates, {selected: ["family:gpt", "opaque-one"]})');
  assert.equal(find(ui.node("picker"), "GPT 全系"), undefined);
  assert.deepEqual(ui.values(), ["opaque-one"]);
  ui.run('renderModelPicker(byId("single"), candidates, {single: true, families: true})');
  assert.equal(find(ui.node("single"), "GPT 全系"), undefined);
});

test("scope text names families first and hides the models they cover", () => {
  const ui = dashboard();
  assert.equal(ui.run('billingModelText(["claude-a", "family:claude", "family:gpt", "gpt-a", "gemini-x"])'), "GPT 全系、Claude 全系、gemini-x");
  assert.equal(ui.run('billingModelText(["gpt-a", "claude-b"])'), "gpt-a、claude-b");
  assert.equal(ui.run('billingModelText([])'), "");
  assert.equal(ui.run('modelFamilyGroup("gemini-4-argon-high") + modelFamilyGroup("o9") + modelFamilyGroup("codex-auto-review")'), "GeminiGPT");
});

test("new group scopes default to every family; other defaults stay explicit", async () => {
  const ui = dashboard();
  ui.context.request = async () => ({models: candidates}); ui.run('api = request');
  await ui.run('loadFormModelPicker("group", "manageable", undefined, {defaultAll: true, families: true})');
  assert.deepEqual(ui.values("group"), ["family:gpt", "family:gemini", "family:claude"]);
  await ui.run('loadFormModelPicker("plain", "manageable", undefined, {defaultAll: true})');
  assert.deepEqual(ui.values("plain"), candidates.map((item) => item.id));
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

test("groups start collapsed and each summary carries the group's select-all and count", () => {
  const ui = dashboard(); ui.context.candidates = candidates;
  ui.run('renderModelPicker(byId("picker"), candidates, {selected: ["opaque-one"]})');
  const host = ui.node("picker");
  const groups = descendants(host).filter((node) => node.className === "model-picker-brand");
  assert.deepEqual(groups.map((group) => group.open), [false, false, false]);
  const summaries = groups.map((group) => group.children.find((child) => child.tagName === "summary"));
  const gpt = find(summaries[0], "全选 GPT 模型");
  assert.ok(gpt, "GPT select-all is in the summary");
  assert.match(summaries[0].textContent, /已选 1 \/ 2/);
  change(gpt, true);
  assert.deepEqual(ui.values(), ["opaque-one", "opaque-two"]);
  assert.match(summaries[0].textContent, /已选 2 \/ 2/);
  assert.equal(groups[0].open, false, "selecting a whole group leaves it collapsed");
});

const repo = path.join(__dirname, "../../..");
const pricedModels = Object.keys(JSON.parse(fs.readFileSync(path.join(repo, "internal/config/data/pricing.json"), "utf8")).models)
  .filter((id) => id !== "codex-auto-review");
const group = (id) => id.startsWith("claude-") ? "Claude" : id.startsWith("gemini-") ? "Gemini" : "GPT";

test("display names use x.x versions and the catalog sorts newest first by tier", () => {
  const ui = dashboard();
  const name = (id) => ui.run(`modelDisplayName(${JSON.stringify(id)})`);
  assert.deepEqual(["claude-opus-5-5", "claude-haiku-4-5-20251001", "gpt-6-astra", "gpt-6.2-sol", "gpt-5.4-mini", "gemini-3-flash", "gemini-4-neon-xhigh", "opaque-one"].map(name),
    ["Claude Opus 5.5", "Claude Haiku 4.5", "GPT-6.0 Astra", "GPT-6.2 Sol", "GPT-5.4 Mini", "Gemini 3.0 Flash", "Gemini 4.0 Neon XHigh", "opaque-one"]);
  ui.context.ids = pricedModels;
  const sorted = JSON.parse(ui.run("JSON.stringify([...ids].sort(compareModels))"));
  const byGroup = (brand) => sorted.filter((id) => group(id) === brand);
  assert.deepEqual(byGroup("GPT"), ["gpt-6.2-astra", "gpt-6.2-sol", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna",
    "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.4", "gpt-5.4-mini"]);
  assert.deepEqual(byGroup("Claude"), ["claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-2", "claude-fable-5-1", "claude-haiku-4-5-20251001"]);
  assert.deepEqual(byGroup("Gemini"), ["gemini-4-argon-high", "gemini-3.8-flash-high", "gemini-3.5-flash-lite", "gemini-3.1-flash-lite", "gemini-3-flash"]);
});

test("pickers hide dated snapshots unless chosen and list unpriced Gemini models as unselectable", () => {
  const ui = dashboard();
  ui.context.catalog = pricedModels.map((id) => ({id, group: group(id)}));
  ui.run('renderModelPicker(byId("picker"), catalog, {})');
  const host = ui.node("picker");
  const choice = (id) => descendants(host).find((node) => node.dataset?.model === id);
  assert.equal(choice("claude-haiku-4-5-20251001"), undefined, "dated snapshot hidden");
  const pending = choice("gemini-4-neon-xhigh");
  assert.ok(pending, "pending model listed");
  assert.match(pending.className, /model-picker-pending/);
  assert.match(pending.textContent, /Gemini 4\.0 Neon XHigh.*gemini-4-neon-xhigh.*待定价/);
  assert.equal(pending.children[0].attributes.disabled, "");
  change(find(host, "全选 Gemini 模型"), true);
  assert.deepEqual(ui.values().sort(), pricedModels.filter((id) => group(id) === "Gemini").sort(), "select-all never picks pending models");
  ui.run('renderModelPicker(byId("picker"), catalog, {selected: ["claude-haiku-4-5-20251001"]})');
  assert.ok(choice("claude-haiku-4-5-20251001"), "a chosen snapshot stays visible so saving keeps it");
  assert.deepEqual(ui.values(), ["claude-haiku-4-5-20251001"]);
});

test("pending models are exactly unpriced models that CPA still excludes", () => {
  const ui = dashboard();
  const pending = JSON.parse(ui.run("JSON.stringify(modelsPendingPricing)"));
  const entrypoint = fs.readFileSync(path.join(repo, "deploy/codex-compat/plugin-entrypoint.sh"), "utf8");
  for (const id of pending) {
    assert.ok(entrypoint.includes(`- "${id}"`), `${id} is no longer excluded by CPA; remove it from modelsPendingPricing`);
    assert.equal(pricedModels.includes(id), false, `${id} now has a price; remove it from modelsPendingPricing`);
  }
});
