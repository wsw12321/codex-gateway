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

function dashboard() {
  const ids = ["model-multiplier-list", "model-multipliers-refresh", "model-multipliers-message", "model-multipliers-loading",
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
  const rows = [{model: "gpt-example", multiplier: "1", updated_at: null, editable: true},
    {model: "gemini-example", multiplier: "0.8", updated_at: "2026-09-28T10:00:00Z", editable: true},
    {model: "codex-auto-review", multiplier: "1", updated_at: null, editable: false}];
  const calls = [];
  context.api = async (url, options) => {
    calls.push({url, options});
    if (!options) return {models: rows.map((row) => ({...row}))};
    const row = rows.find((item) => url.endsWith(item.model));
    Object.assign(row, {multiplier: JSON.parse(options.body).multiplier, updated_at: "2026-09-28T12:00:00Z"});
    return {...row};
  };
  const form = (model = "gpt-example") => nodes.get("model-multiplier-list").children.find((card) => card.dataset.model === model)?.querySelector("form");
  const edit = (name, value, model = "gpt-example") => {
    const node = form(model).querySelectorAll("input").find((input) => input.attributes.name === name);
    node.value = value; node.dispatch("input");
  };
  const submit = (model = "gpt-example") => form(model).dispatch("submit");
  return {nodes, rows, calls, context, run, form, edit, submit};
}

const deferred = () => {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return {promise, resolve};
};

test("configured models show decimal strings, fixed internal model has no editor", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()");
  assert.match(ui.nodes.get("model-multiplier-list").textContent, /当前倍率1.0 ×/);
  assert.equal(ui.form("codex-auto-review"), null);
  assert.match(ui.nodes.get("model-multiplier-list").textContent, /固定 1.0，不可编辑/);
  assert.equal(ui.form().querySelector("input").value, "1.0");
  assert.equal(ui.nodes.get("model-multipliers-refresh").disabled, false);
});

test("invalid and zero decimals never reach the API", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()"); ui.edit("reason", "test");
  for (const value of ["0", "0.000000000000", "-1", "1e2", "Infinity", "NaN", ".5", "1.", "01", "1000000000000000000", "0.0000000000001"]) {
    ui.edit("multiplier", value); await ui.submit();
    assert.match(ui.form().textContent, /大于 0 的十进制数/, value);
  }
  assert.equal(ui.calls.length, 1);
  ui.edit("multiplier", "0.8"); ui.edit("reason", "  "); await ui.submit();
  assert.match(ui.form().textContent, /必须填写操作原因/);
  assert.equal(ui.calls.length, 1);
});

test("successful save retains exact digits, refreshes and preserves other models' drafts", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()");
  ui.edit("multiplier", "0.65", "gemini-example"); ui.edit("reason", "later", "gemini-example");
  ui.edit("multiplier", "999999999999999999.123456789012"); ui.edit("reason", "  review  ");
  await ui.submit();
  const body = JSON.parse(ui.calls.find((call) => call.options?.method === "PUT").options.body);
  assert.equal(body.multiplier, "999999999999999999.123456789012"); assert.equal(body.reason, "review");
  assert.match(body.operation_id, /^00000000-0000-4000-8000-/);
  assert.equal(ui.form().querySelector("input").value, body.multiplier);
  assert.equal(ui.form().querySelectorAll("input")[1].value, "");
  assert.equal(ui.form("gemini-example").querySelector("input").value, "0.65");
  assert.match(ui.nodes.get("model-multipliers-message").textContent, /已保存，仅影响新请求/);
  assert.equal(ui.calls.filter((call) => !call.options).length, 2);
});

test("failed saves retain input and reuse operation ID until the payload changes", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()");
  const bodies = []; ui.context.api = async (_url, options) => { bodies.push(JSON.parse(options.body)); throw new Error("database failed"); };
  ui.edit("multiplier", "0.75"); ui.edit("reason", "discount");
  await ui.submit(); await ui.submit();
  assert.match(ui.form().textContent, /database failed.*输入已保留/);
  assert.equal(ui.form().querySelector("input").value, "0.75");
  assert.deepEqual(bodies[0], bodies[1]);
  ui.edit("multiplier", "0.5"); await ui.submit();
  assert.notEqual(bodies[2].operation_id, bodies[1].operation_id);
});

test("repeated submit cannot create duplicate writes while authentication or save is pending", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()");
  ui.edit("multiplier", "2"); ui.edit("reason", "markup");
  const verification = deferred(); let writes = 0;
  ui.context.sensitiveAction = async (operation) => { await verification.promise; return operation(); };
  const originalAPI = ui.context.api;
  ui.context.api = async (...args) => { if (args[1]?.method === "PUT") writes++; return originalAPI(...args); };
  const saving = ui.submit(); await ui.submit();
  assert.equal(ui.form().querySelector("button").disabled, true); assert.equal(writes, 0);
  assert.equal(ui.nodes.get("model-multipliers-refresh").disabled, true);
  verification.resolve(); await saving;
  assert.equal(writes, 1); assert.equal(ui.form().querySelector("button").disabled, false);
});

test("a GET started before save cannot replace the newly saved value", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()");
  const read = deferred(); const api = ui.context.api;
  ui.context.api = () => read.promise;
  const stale = ui.run("loadModelMultipliers()");
  ui.context.api = api; ui.edit("multiplier", "1.5"); ui.edit("reason", "increase"); await ui.submit();
  read.resolve({models: [{...ui.rows[0], multiplier: "0.1"}]}); await stale;
  assert.equal(ui.form().querySelector("input").value, "1.5");
  assert.match(ui.nodes.get("model-multipliers-message").textContent, /已保存/);
});

test("out of order reads retain the newest response and fresh untouched inputs", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()");
  const old = deferred(), fresh = deferred(); let calls = 0;
  ui.context.api = () => ++calls === 1 ? old.promise : fresh.promise;
  const first = ui.run("loadModelMultipliers()"), second = ui.run("loadModelMultipliers()");
  fresh.resolve({models: [{...ui.rows[0], multiplier: "2"}]}); await second;
  old.resolve({models: [{...ui.rows[0], multiplier: "3"}]}); await first;
  assert.equal(ui.form().querySelector("input").value, "2.0");
});

test("identity reset prevents late save responses from restoring old private UI", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()");
  const write = deferred(); ui.context.api = () => write.promise;
  ui.edit("multiplier", "2"); ui.edit("reason", "pending"); const saving = ui.submit();
  ui.run('identityGeneration++; state = {user: {id: "member", role: "member"}}; resetModelMultipliers();');
  write.resolve({...ui.rows[0], multiplier: "2"}); await saving;
  assert.equal(ui.run("modelMultiplierModels.length"), 0);
  assert.equal(ui.run("modelMultiplierDrafts.size"), 0);
  assert.doesNotMatch(ui.nodes.get("model-multiplier-list").textContent, /gpt-example/);
});

test("successful write with failed refresh reports success and does not retain a retry operation", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()"); const api = ui.context.api;
  ui.context.api = async (url, options) => { if (!options) throw new Error("read unavailable"); return api(url, options); };
  ui.edit("multiplier", "2"); ui.edit("reason", "markup"); await ui.submit();
  assert.match(ui.nodes.get("model-multipliers-message").textContent, /已保存.*列表刷新失败/);
  assert.equal(ui.form().querySelector("input").value, "2.0");
  assert.equal(ui.run('modelMultiplierDrafts.get("gpt-example").pending'), null);
});

test("a failed refresh preserves a draft and a removed model loses its editor", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()"); const api = ui.context.api;
  ui.edit("multiplier", "0.25"); ui.edit("reason", "new discount");
  ui.context.api = async () => { throw new Error("read unavailable"); };
  await ui.run("loadModelMultipliers()");
  assert.equal(ui.form().querySelector("input").value, "0.25");
  assert.match(ui.nodes.get("model-multipliers-message").textContent, /倍率加载失败/);
  ui.rows.shift(); ui.context.api = api; await ui.run("loadModelMultipliers()");
  assert.equal(ui.form(), undefined);
  assert.equal(ui.run('modelMultiplierDrafts.has("gpt-example")'), false);
});

test("members neither load settings nor submit writes", async () => {
  const ui = dashboard(); await ui.run("loadModelMultipliers()"); const form = ui.form();
  ui.run('state.user.role = "member";'); await ui.run("loadModelMultipliers()");
  await form.dispatch("submit"); assert.equal(ui.calls.length, 1);
  assert.equal(ui.run('ownerOnlySections.has("model-multipliers")'), true);
});

test("ledger displays request multiplier with legacy fallback", () => {
  const ui = dashboard();
  ui.run('renderBillingLedger({ledger_entries: [{entry_type:"usage", request_id:"request", model:"gpt-example", pricing_multiplier:"0.750000000000", amount_usd:"0.000000000001"}]})');
  assert.match(ui.nodes.get("billing-ledger-rows").textContent, /请求倍率：0.75/);
  ui.run('renderBillingLedger({ledger_entries: [{entry_type:"usage", request_id:"legacy", model:"gpt-example", amount_usd:"1"}]})');
  assert.match(ui.nodes.get("billing-ledger-rows").textContent, /请求倍率：1.0/);
});
