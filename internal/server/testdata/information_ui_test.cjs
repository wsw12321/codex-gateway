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
  reset() {}
}

function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return {promise, resolve, reject}; }
const report = {retention_days: 90, cutoff: "2026-06-24T00:00:00Z", delete_counts: {usage_requests: 12}, retained_counts: {usage_requests: 2}, retained_reasons: {unsettled_request: 2}};
const job = {id: "job-1", status: "running", retention_days: 90, cutoff: report.cutoff, report};

function dashboard() {
  const nodes = new Map(), root = new Element(), timers = new Map(), confirms = [];
  let timerID = 0, operationID = 0;
  const node = (id) => { if (!nodes.has(id)) { const value = new Element(); nodes.set(id, value); root.append(value); } return nodes.get(id); };
  const form = node("information-preview-form");
  form.elements = {retention_days: new Element("input")}; form.elements.retention_days.value = "90";
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    location: {hash: "#information"}, URLSearchParams, DOMException, AbortController,
    crypto: {randomUUID: () => `operation-${++operationID}`},
    window: {setTimeout: (fn, delay) => { timers.set(++timerID, {fn, delay}); return timerID; }, clearTimeout: (id) => timers.delete(id), confirm: (message) => { confirms.push(message); return true; }}, report, job,
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "owner", role: "owner"}, recently_verified: true};');
  return {context, run, node, form, timers, confirms,
    api(handler) { context.handler = handler; run("api = handler;"); },
    preview: () => run('previewInformation({currentTarget: byId("information-preview-form")})'),
  };
}

test("preview accepts positive whole days and preserves the server UTC cutoff for confirmation", async () => {
  const ui = dashboard(); let calls = 0;
  ui.api(async (_, options) => { calls++; assert.deepEqual(JSON.parse(options.body), {retention_days: 90}); return report; });
  for (const value of ["0", "-1", "1.5", "1e2", "", "9007199254740993"]) {
    ui.form.elements.retention_days.value = value;
    await assert.rejects(ui.preview(), /正整数/);
  }
  assert.equal(calls, 0);
  ui.form.elements.retention_days.value = "90";
  await ui.preview();
  assert.match(ui.node("information-preview-cutoff").textContent, /2026-06-24 00:00:00 UTC/);
  assert.match(ui.node("information-preview-report").textContent, /请求尚未结算：2/);
  assert.equal(ui.node("information-create-job").disabled, false);
});

test("an obsolete preview cannot unlock confirmation after input or identity changes", async () => {
  for (const change of ["informationSequence++", "identityGeneration++", "loggingOut = true"]) {
    const ui = dashboard(), response = deferred(); ui.api(() => response.promise);
    const pending = ui.preview(); ui.run(change); response.resolve(report); await pending;
    assert.equal(ui.run("informationPreview"), null);
    assert.equal(ui.node("information-create-job").disabled, true);
  }
});

test("cleanup retries reuse an operation ID and verification precedes the write", async () => {
  const ui = dashboard(), verification = deferred(), writes = [];
  ui.run("informationPreview = report; state.recently_verified = false;");
  ui.context.verification = verification.promise; ui.run("reauthenticate = () => verification;");
  ui.api(async (_, options) => { writes.push(JSON.parse(options.body)); if (writes.length === 1) throw new Error("network result unknown"); return job; });
  const pending = ui.run("createInformationJob()");
  assert.equal(writes.length, 0);
  assert.equal(ui.run("informationOperation"), true);
  await ui.run("createInformationJob()");
  assert.equal(writes.length, 0);
  verification.resolve(); await assert.rejects(pending, /network result unknown/);
  await ui.run("createInformationJob()");
  assert.equal(writes.length, 2);
  assert.equal(writes[0].operation_id, writes[1].operation_id);
  assert.equal(writes[1].cutoff, report.cutoff);
  assert.match(ui.confirms[0], /2026-06-24 00:00:00 UTC/);
  assert.equal(ui.node("information-preview-button").disabled, true);
  assert.equal([...ui.timers.values()].some((timer) => timer.delay === 5000), true);
});

test("identity invalidation during recent verification prevents cleanup writes", async () => {
  const ui = dashboard(), verification = deferred(); let calls = 0;
  ui.context.verification = verification.promise;
  ui.run("informationPreview = report; state.recently_verified = false; reauthenticate = () => verification;");
  ui.api(async () => { calls++; return job; });
  const pending = ui.run("createInformationJob()"); ui.run("identityGeneration++"); verification.resolve();
  await assert.rejects(pending, /身份已变化/);
  assert.equal(calls, 0);
});

test("batch conflicts report blockers and discard a selection that no longer qualifies", async () => {
  const ui = dashboard(), writes = [];
  ui.run('informationUsers = [{id: "one", username: "甲"}, {id: "two", username: "乙"}]; informationUsersReady = true; informationSelectedUsers = new Set(["one", "two"]);');
  ui.api(async (_, options) => {
    if (options?.method === "POST") { writes.push(JSON.parse(options.body)); throw Object.assign(new Error("状态已变化"), {status: 409, blockers: [{user_id: "one", reasons: ["nonzero_balance"]}]}); }
    return {users: [{id: "two", username: "乙"}]};
  });
  await assert.rejects(ui.run("deleteInformationUsers()"), /状态已变化/);
  assert.deepEqual(writes[0].user_ids, ["one", "two"]);
  assert.match(ui.node("information-message").textContent, /整批删除未完成.*甲：余额不为零/);
  assert.equal(ui.run("informationSelectedUsers.size"), 0);
  assert.equal(ui.node("information-delete-users").disabled, true);
});

test("candidate search and pagination preserve selections outside the visible page", async () => {
  const ui = dashboard();
  ui.run('informationUsers = [{id:"one",username:"alpha",display_name:"张三"}]; informationUsersReady = true; informationSelectedUsers.add("one"); informationSelectedDetails.set("one", informationUsers[0]);');
  ui.node("information-user-search").value = "lisi";
  ui.api(async () => ({users: [{id: "two", username: "beta", display_name: "李四"}]}));
  await ui.run("loadInformationUsers(0)");
  assert.equal(ui.run("JSON.stringify([...informationSelectedUsers])"), '["one"]');
  assert.equal(ui.run('informationSelectedDetails.get("one").display_name'), "张三");
  assert.match(ui.node("information-selected-count").textContent, /已选 1/);
});

test("member and oversized batch submissions never call the delete API", async () => {
  for (const setup of ['state.user.role = "member"; informationSelectedUsers = new Set(["one"]);', 'informationSelectedUsers = new Set(Array.from({length: 101}, (_, i) => String(i)));']) {
    const ui = dashboard(); let calls = 0; ui.api(async () => { calls++; return {}; });
    ui.run(`informationUsersReady = true; ${setup}`); await ui.run("deleteInformationUsers()");
    assert.equal(calls, 0);
  }
});

test("a candidate refresh failure never changes a confirmed deletion into a failed deletion", async () => {
  const ui = dashboard();
  ui.run('informationUsers = [{id:"one",username:"甲"}]; informationUsersReady = true; informationSelectedUsers = new Set(["one"]);');
  ui.api(async (_, options) => { if (options?.method === "POST") return {deleted_count: 1}; throw new Error("候选服务暂不可用"); });
  await ui.run("deleteInformationUsers()");
  assert.match(ui.node("information-message").textContent, /已永久删除 1 位用户，但候选列表刷新失败/);
  assert.doesNotMatch(ui.node("information-message").textContent, /整批删除未完成/);
  assert.equal(ui.node("information-delete-users").disabled, true);
});

test("pending job monitoring pauses while hidden and resumes immediately after returning", () => {
  const ui = dashboard(); ui.run("renderInformationJob(job); scheduleInformationJob();");
  assert.equal(ui.timers.size, 1);
  ui.run('document.visibilityState = "hidden"; syncVisiblePolling();');
  assert.equal(ui.timers.size, 0);
  ui.run('document.visibilityState = "visible"; syncVisiblePolling();');
  assert.equal(ui.timers.size, 1);
  ui.run("resetInformation();"); assert.equal(ui.timers.size, 0);
});

test("request attribution never fabricates a missing ID and renders provided text safely", () => {
  const ui = dashboard();
  assert.equal(ui.run('requestUpstreamCell({upstream_masked_email: "unknown@example.test"}).textContent'), "未归因");
  assert.equal(ui.run('requestUpstreamCell({upstream_account_id: "stable-id", upstream_masked_email: "<script>"}).textContent'), "<script>stable-id");
});
