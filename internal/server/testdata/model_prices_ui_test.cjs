"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {};
    this.className = ""; this.value = ""; this.disabled = false; this._text = "";
    this.listeners = new Map();
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
  get textContent() { return this._text + this.children.map((child) => typeof child === "string" ? child : child.textContent).join(""); }
  get childNodes() {
    return [...(this._text ? [this._text] : []), ...this.children].map((child) => {
      if (typeof child !== "string") return child;
      const text = new Element("#text"); text._text = child; return text;
    });
  }
  cloneNode(deep = false) {
    const clone = new Element(this.tagName);
    clone.className = this.className; clone._text = this._text; clone.value = this.value; clone.disabled = this.disabled;
    clone.dataset = {...this.dataset}; clone.attributes = {...this.attributes};
    if (deep) clone.append(...this.children.map((child) => typeof child === "string" ? child : child.cloneNode(true)));
    return clone;
  }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this._text = ""; this.children = []; this.append(...children); }
  querySelectorAll(selector) {
    return this.children.filter((child) => typeof child !== "string").flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  matches(selector) { return selector === this.tagName || selector.startsWith(".") && this.classList.contains(selector.slice(1)); }
  closest() { return null; }
  addEventListener(name, handler) { this.listeners.set(name, handler); }
  dispatch(name) { return this.listeners.get(name)?.({currentTarget: this, preventDefault() {}}); }
}

const {rows: fixtureRows, clone} = require("./model_prices_fixtures.cjs");
const inputName = "standard.short.input_usd_per_million";

function dashboard() {
  const ids = ["model-price-list", "model-prices-refresh", "model-prices-message", "model-prices-loading", "model-prices-search",
    "billing-ledger-rows", "billing-ledger-page", "billing-ledger-prev", "billing-ledger-next"];
  const nodes = new Map(ids.map((id) => [id, new Element()]));
  let operationSequence = 0;
  const context = vm.createContext({
    document: {getElementById: (id) => nodes.get(id), createElement: (tag) => new Element(tag),
      querySelectorAll: (selector) => [...nodes.values()].flatMap((node) => node.querySelectorAll(selector))},
    window: {setTimeout: () => 0, clearTimeout() {}},
    crypto: {randomUUID: () => `00000000-0000-4000-8000-${String(++operationSequence).padStart(12, "0")}`},
    Intl, URLSearchParams,
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "owner", role: "owner"}, recently_verified: true};');
  const rows = fixtureRows(), calls = [];
  context.api = async (url, options) => {
    calls.push({url, options});
    if (!options) return {models: clone(rows)};
    const row = rows.find((item) => url.endsWith(item.model));
    const body = JSON.parse(options.body);
    Object.assign(row, {effective_price: clone(body.action === "save" ? body.price : row.configured_price),
      version: row.version + 1, source: body.action === "save" ? "override" : "config", conflict: false, updated_at: "2026-10-06T12:00:00Z"});
    return clone(row);
  };
  const card = (model = "gpt-example") => nodes.get("model-price-list").children.find((card) => card.dataset.model === model);
  const form = (model = "gpt-example") => card(model)?.querySelector("form");
  const input = (name = inputName, model = "gpt-example") => form(model).querySelectorAll("input").find((input) => input.attributes.name === name);
  const edit = (name, value, model = "gpt-example") => { const node = input(name, model); node.value = value; node.dispatch("input"); };
  const submit = (model = "gpt-example") => form(model).dispatch("submit");
  const action = (name, model = "gpt-example") => form(model).querySelectorAll("button").find((node) => node.dataset.action === name).dispatch("click");
  return {nodes, rows, calls, context, run, card, form, input, edit, submit, action};
}

const deferred = () => { let resolve; const promise = new Promise((done) => { resolve = done; }); return {promise, resolve}; };

test("all service/context/cache prices, structure, configured references and multiplier render", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()");
  assert.equal(ui.form().querySelectorAll("input").length, 25);
  assert.equal(ui.form().querySelectorAll("tbody")[0].children.length, 6);
  assert.match(ui.card().textContent, /管理员覆盖/);
  assert.match(ui.card().textContent, /长上下文阈值：200,000 tokens.*最大输入：1,048,576/);
  assert.match(ui.card().textContent, /当前倍率：0.8 ×/);
  assert.match(ui.card().textContent, /USD \/ 百万 tokens/);
  for (const tier of ["standard", "fast", "flex"]) for (const context of ["short", "long"]) {
    for (const field of ["input", "cached_input", "cache_write", "output"]) {
      const name = `${tier}.${context}.${field}_usd_per_million`;
      assert.equal(ui.input(name).value, ui.rows[0].effective_price.service_tiers[tier][context][`${field}_usd_per_million`]);
    }
  }
  assert.equal(ui.form("gemini-example").querySelectorAll("input").length, 7);
  assert.match(ui.card("gemini-example").textContent, /包含在输入单价中/);
  assert.equal(ui.form("legacy-example").querySelectorAll("input").length, 4);
  assert.match(ui.card("legacy-example").textContent, /旧版三价配置/);
  assert.equal(ui.form("codex-auto-review"), null);
  assert.match(ui.card("codex-auto-review").textContent, /固定零价，不可编辑/);
});

test("zero and maximum decimal precision remain strings and complete readonly structure round trips", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()");
  const original = clone(ui.rows[0].effective_price);
  ui.edit(inputName, "0"); ui.edit("fast.long.cache_write_usd_per_million", "999999999999999999.123456789012");
  ui.edit("reason", "  cost update  "); await ui.submit();
  const body = JSON.parse(ui.calls.find((call) => call.options).options.body);
  assert.equal(body.action, "save"); assert.equal(body.version, 4); assert.equal(body.structure_id, "structure-gpt-example");
  assert.equal(body.price.service_tiers.standard.short.input_usd_per_million, "0");
  assert.equal(body.price.service_tiers.fast.long.cache_write_usd_per_million, "999999999999999999.123456789012");
  original.service_tiers.standard.short.input_usd_per_million = "0";
  original.service_tiers.fast.long.cache_write_usd_per_million = "999999999999999999.123456789012";
  assert.deepEqual(body.price, original); assert.equal(body.reason, "cost update");
  assert.equal(ui.input("reason").value, ""); assert.match(ui.nodes.get("model-prices-message").textContent, /覆盖价已保存，仅影响新请求/);
});

test("invalid decimals and missing reasons prevent both save and restore", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()");
  await ui.submit(); await ui.action("restore"); assert.match(ui.form().textContent, /必须填写操作原因/);
  ui.edit("reason", "update");
  for (const value of ["-1", "1e2", "Infinity", "NaN", ".5", "1.", "01", "1000000000000000000", "0.0000000000001"]) {
    ui.edit(inputName, value); await ui.submit(); assert.match(ui.form().textContent, /单价必须是大于或等于 0/);
  }
  assert.equal(ui.calls.length, 1);
  // Restore does not require a valid price draft, only an operation reason.
  await ui.action("restore");
  const body = JSON.parse(ui.calls.find((call) => call.options).options.body);
  assert.equal(body.action, "restore"); assert.equal(body.price, undefined); assert.equal(body.reason, "update");
  assert.match(ui.nodes.get("model-prices-message").textContent, /已恢复配置价/);
});

test("legacy and included cache saves never introduce separate write prices", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()");
  for (const model of ["legacy-example", "gemini-example"]) {
    ui.edit("reason", "update", model); await ui.submit(model);
    const body = JSON.parse(ui.calls.filter((call) => call.options).at(-1).options.body);
    assert.deepEqual(body.price, ui.rows.find((row) => row.model === model).configured_price);
    assert.doesNotMatch(JSON.stringify(body.price), /cache_write_usd_per_million/);
  }
});

test("failed writes retain all drafts, messages and retry ID until the payload changes", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()");
  const bodies = []; ui.context.api = async (_url, options) => { bodies.push(JSON.parse(options.body)); throw new Error("database failed"); };
  ui.edit(inputName, "0.75"); ui.edit("reason", "cost"); await ui.submit(); await ui.submit();
  assert.deepEqual(bodies[0], bodies[1]); assert.equal(ui.input().value, "0.75");
  ui.run("renderModelPrices()"); assert.match(ui.form().textContent, /database failed.*输入已保留/);
  ui.edit(inputName, "0.5"); await ui.submit(); assert.notEqual(bodies[2].operation_id, bodies[1].operation_id);
  await ui.action("restore"); assert.notEqual(bodies[3].operation_id, bodies[2].operation_id);
});

test("reauthentication retries use identical payload and pending submissions cannot duplicate a write", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()");
  ui.edit(inputName, "2"); ui.edit("reason", "update");
  const verification = deferred(); let authCalls = 0, writes = [];
  ui.context.reauthenticate = async () => { authCalls++; await verification.promise; };
  const api = ui.context.api;
  ui.context.api = async (url, options) => {
    if (options) {
      writes.push(options.body);
      if (writes.length === 1) throw Object.assign(new Error("verify"), {code: "recent_identity_verification_required"});
    }
    return api(url, options);
  };
  const saving = ui.submit(); await Promise.resolve(); await Promise.resolve(); await ui.submit();
  assert.equal(ui.input().disabled, true); assert.equal(ui.nodes.get("model-prices-refresh").disabled, true);
  verification.resolve(); await saving;
  assert.equal(authCalls, 1); assert.equal(writes.length, 2); assert.equal(writes[0], writes[1]);
});

test("out of order GETs and a GET started before a save cannot replace a newer price", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()");
  const old = deferred(), fresh = deferred(); const api = ui.context.api; let reads = 0;
  ui.context.api = () => ++reads === 1 ? old.promise : fresh.promise;
  const first = ui.run("loadModelPrices()"), second = ui.run("loadModelPrices()");
  const updated = clone(ui.rows); updated[0].effective_price.service_tiers.standard.short.input_usd_per_million = "3";
  fresh.resolve({models: updated}); await second; old.resolve({models: clone(ui.rows)}); await first;
  assert.equal(ui.input().value, "3");
  const beforeSave = deferred(); ui.context.api = () => beforeSave.promise;
  const stale = ui.run("loadModelPrices()"); ui.context.api = api;
  ui.edit(inputName, "4"); ui.edit("reason", "update"); await ui.submit();
  beforeSave.resolve({models: updated}); await stale; assert.equal(ui.input().value, "4");
});

test("refresh preserves dirty drafts and changed versions require explicit reload", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()");
  ui.edit(inputName, "0.75"); ui.edit("reason", "update");
  ui.edit("standard.short.output_usd_per_million", "8", "gemini-example");
  ui.rows[0].version++; ui.rows[0].effective_price.service_tiers.standard.short.input_usd_per_million = "3";
  await ui.run("loadModelPrices()");
  assert.equal(ui.input().value, "0.75"); assert.match(ui.form().textContent, /价格已被其他操作修改/);
  await ui.submit(); assert.equal(ui.calls.filter((call) => call.options).length, 0);
  await ui.action("reload"); assert.equal(ui.input().value, "3"); assert.equal(ui.input("reason").value, "");
  assert.equal(ui.input("standard.short.output_usd_per_million", "gemini-example").value, "8");
});

test("409 marks the draft stale and preserves edits until explicit reload", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()"); const api = ui.context.api;
  ui.context.api = async () => { throw Object.assign(new Error("version conflict"), {status: 409}); };
  ui.edit(inputName, "0.25"); ui.edit("reason", "update"); await ui.submit();
  assert.equal(ui.input().value, "0.25"); assert.match(ui.form().textContent, /价格版本或配置结构已改变/);
  assert.equal(ui.form().querySelectorAll("button").find((button) => button.dataset.action === "save").disabled, true);
  ui.context.api = api; await ui.action("reload"); assert.equal(ui.input().value, "2.5");
});

test("structure conflicts show current configuration and both resolution actions remain available", async () => {
  const ui = dashboard(); ui.rows[0].conflict = true; ui.rows[0].source = "conflict"; ui.rows[0].effective_price = null;
  await ui.run("loadModelPrices()"); assert.equal(ui.input().value, "2.5");
  assert.match(ui.card().textContent, /新请求已暂停计费准入/);
  assert.equal(ui.form().querySelectorAll("button").some((button) => button.disabled), false);
  ui.edit("reason", "accept config structure"); await ui.action("restore");
  assert.doesNotMatch(ui.card().textContent, /新请求已暂停计费准入/);
});

test("search filters without losing drafts and removing a model clears its draft", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()"); ui.edit(inputName, "8");
  ui.nodes.get("model-prices-search").value = "GeMiNi"; ui.run("renderModelPrices()");
  assert.equal(ui.card(), undefined); assert.ok(ui.card("gemini-example"));
  ui.nodes.get("model-prices-search").value = ""; ui.run("renderModelPrices()"); assert.equal(ui.input().value, "8");
  ui.rows.shift(); await ui.run("loadModelPrices()"); assert.equal(ui.card(), undefined);
  assert.equal(ui.run('modelPriceDrafts.has("gpt-example")'), false);
});

test("failed list read preserves draft; confirmed write survives failed followup read", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()"); const api = ui.context.api;
  ui.context.api = async (url, options) => { if (!options) throw new Error("read unavailable"); return api(url, options); };
  ui.edit(inputName, "0.75"); ui.edit("reason", "cost"); await ui.run("loadModelPrices()");
  assert.equal(ui.input().value, "0.75"); await ui.submit();
  assert.match(ui.nodes.get("model-prices-message").textContent, /覆盖价已保存.*列表刷新失败/);
  assert.equal(ui.input().value, "0.75"); assert.equal(ui.run('modelPriceDrafts.get("gpt-example").pending'), null);
});

test("logout cancels late saves and reads without resurrecting private data", async () => {
  for (const save of [false, true]) {
    const ui = dashboard(); await ui.run("loadModelPrices()"); const pending = deferred(); ui.context.api = () => pending.promise;
    ui.edit(inputName, "2"); ui.edit("reason", "pending");
    const request = save ? ui.submit() : ui.run("loadModelPrices()");
    ui.run('identityGeneration++; state = null; resetModelPrices();');
    pending.resolve(save ? clone(ui.rows[0]) : {models: clone(ui.rows)}); await request;
    assert.equal(ui.run("modelPriceModels.length"), 0); assert.equal(ui.run("modelPriceDrafts.size"), 0);
    assert.doesNotMatch(ui.nodes.get("model-price-list").textContent, /gpt-example/);
  }
});

test("members cannot fetch, submit or route to model pricing", async () => {
  const ui = dashboard(); await ui.run("loadModelPrices()"); const form = ui.form();
  ui.run('state.user.role = "member";'); await ui.run("loadModelPrices()"); await form.dispatch("submit");
  assert.equal(ui.calls.length, 1); assert.equal(ui.run('ownerOnlySections.has("model-pricing")'), true);
});


test("ledger displays immutable model price action, version and full matrix", () => {
  const ui = dashboard();
  ui.context.ledgerSnapshot = {model_price: ui.rows[0], action: "save", previous_price: null};
  ui.run('renderBillingLedger({ledger_entries: [{entry_type:"model_price", model:"gpt-example", reason:"update", transaction_snapshot:ledgerSnapshot}]})');
  const text = ui.nodes.get("billing-ledger-rows").textContent;
  assert.match(text, /模型基础定价调整/); assert.match(text, /保存覆盖价 · 版本 4 · 倍率 0.8 ×/);
  assert.match(text, /fast \/ long · 输入 10 · 缓存读取 1 · 缓存写入 12.5 · 输出 30/);
  ui.context.ledgerSnapshot.action = "restore";
  ui.run('renderBillingLedger({ledger_entries: [{entry_type:"model_price", transaction_snapshot:ledgerSnapshot}]})');
  assert.match(ui.nodes.get("billing-ledger-rows").textContent, /恢复配置价/);
});

test('Claude TTL prices expose and preserve distinct five-minute and one-hour values', async () => {
  const ui = dashboard();
  const {row} = require('./model_prices_fixtures.cjs');
  const model = 'claude-sonnet-exact';
  const price = {cache_write_mode: 'separate_by_ttl', max_input_tokens: 200000, long_context_threshold_tokens: 100000,
    service_tiers: {standard: {short: {input_usd_per_million: '3', cached_input_usd_per_million: '0.3', cache_write_5m_usd_per_million: '3.75', cache_write_1h_usd_per_million: '6', output_usd_per_million: '15'}}}};
  ui.rows.push(row(model, price));
  await ui.run('loadModelPrices()');
  assert.match(ui.card(model).textContent, /5 分钟 \/ 1 小时独立计费/);
  assert.equal(ui.input('standard.short.cache_write_5m_usd_per_million', model).value, '3.75');
  assert.equal(ui.input('standard.short.cache_write_1h_usd_per_million', model).value, '6');
  assert.equal(ui.input('standard.short.cache_write_usd_per_million', model), undefined);
  ui.edit('standard.short.cache_write_5m_usd_per_million', '4.1', model);
  ui.edit('standard.short.cache_write_1h_usd_per_million', '6.2', model);
  ui.edit('reason', 'adjust TTL prices', model); await ui.submit(model);
  const saved = JSON.parse(ui.calls.find(call => call.options).options.body).price.service_tiers.standard.short;
  assert.equal(saved.cache_write_5m_usd_per_million, '4.1'); assert.equal(saved.cache_write_1h_usd_per_million, '6.2');
  assert.equal(saved.cache_write_usd_per_million, undefined);
});
