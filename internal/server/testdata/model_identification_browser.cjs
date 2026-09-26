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
    this._text = "";
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
  get textContent() { return this._text + this.children.map((child) => child.textContent).join(""); }
  get disabled() { return Object.hasOwn(this.attributes, "disabled"); }
  set disabled(value) { if (value) this.attributes.disabled = ""; else delete this.attributes.disabled; }
  get options() { return this.children.filter((child) => child.tagName === "option"); }
  get childElementCount() { return this.children.length; }
  setAttribute(name, value) { this.attributes[name] = String(value); if (name === "value") this.value = String(value); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) {
    this._text = ""; this.children = []; this.append(...children);
    if (this.tagName === "select") this.value = this.options[0]?.value || "";
  }
  querySelectorAll(selector) {
    return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  matches(selector) {
    return selector === this.tagName || selector === `#${this.id}` ||
      (selector === ".form-message" && this.classList.contains("form-message")) ||
      (selector === "button[type=submit]" && this.tagName === "button" && this.type === "submit");
  }
  closest() { return null; }
  addEventListener(name, handler) { this.listeners.set(name, handler); }
  dispatch(name) { return this.listeners.get(name)?.({currentTarget: this, preventDefault() {}}); }
}

function dashboard() {
  const ids = ["model-identification-form", "model-identification-account", "model-identification-model",
    "model-identification-run", "model-identification-refresh", "model-identification-version",
    "model-identification-run-status", "model-identification-progress", "model-identification-results", "model-identification-message"];
  const nodes = new Map(ids.map((id) => [id, new Element()]));
  for (const [id, node] of nodes) node.id = id;
  const form = nodes.get("model-identification-form");
  form.tagName = "form";
  form.dataset.busy = "false";
  nodes.get("model-identification-account").tagName = "select";
  nodes.get("model-identification-model").tagName = "select";
  nodes.get("model-identification-run").tagName = "button";
  nodes.get("model-identification-run").type = "submit";
  nodes.get("model-identification-message").className = "form-message";
  form.elements = {account_id: nodes.get("model-identification-account"), model: nodes.get("model-identification-model")};
  form.append(...["model-identification-account", "model-identification-model", "model-identification-run", "model-identification-message"].map((id) => nodes.get(id)));
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

test("all existing accounts can be selected regardless of daily routing status", async () => {
  const ui = dashboard();
  const calls = [];
  ui.context.api = async (url) => {
    calls.push(url);
    return {reference_version: "2026.09.5", accounts: [
      {id: "available-account", masked_email: "a***@example.com", status: "available"},
      {id: "unavailable-account", masked_email: "b***@example.com", status: "unavailable"},
      {id: "unknown-account"},
    ], models: ["gpt-6-sol"]};
  };
  await ui.run("loadModelIdentificationOptions()");
  const account = ui.nodes.get("model-identification-account");
  const model = ui.nodes.get("model-identification-model");
  const run = ui.nodes.get("model-identification-run");
  assert.equal(account.disabled, false);
  assert.equal(account.options[1].disabled, false, "available account must not have a disabled attribute, including disabled=\"false\"");
  assert.equal(account.options[2].disabled, false, "direct Owner diagnostics allow unavailable accounts");
  assert.equal(account.options[3].disabled, false, "daily routing status does not block diagnostics");
  assert.equal(model.disabled, true);
  assert.equal(run.disabled, true);

  account.value = "unavailable-account";
  await ui.run("loadModelIdentificationModels()");
  assert.deepEqual(calls, ["/admin/model-identifications/options", "/admin/model-identifications/options?account_id=unavailable-account"]);
  assert.equal(model.disabled, false);
  assert.equal(model.options[1].value, "gpt-6-sol");
  assert.equal(run.disabled, true, "a model must be selected before running");
  model.value = "gpt-6-sol";
  ui.run("syncModelIdentificationControls()");
  assert.equal(run.disabled, false);
});

test("refresh preserves selections across status changes and clears deleted accounts", async () => {
  const ui = dashboard();
  let status = "available";
  ui.context.api = async () => ({accounts: [{id: "account", status}], models: ["gpt-6-sol"]});
  await ui.run("loadModelIdentificationOptions()");
  const account = ui.nodes.get("model-identification-account");
  const model = ui.nodes.get("model-identification-model");
  account.value = "account";
  await ui.run("loadModelIdentificationModels()");
  model.value = "gpt-6-sol";

  await ui.run("loadModelIdentificationOptions()");
  assert.equal(account.value, "account");
  assert.equal(account.options[1].disabled, false);
  assert.equal(model.value, "gpt-6-sol");
  assert.equal(ui.nodes.get("model-identification-run").disabled, false);

  status = "unavailable";
  await ui.run("loadModelIdentificationOptions()");
  assert.equal(account.value, "account");
  assert.equal(account.options[1].disabled, false);
  assert.equal(model.value, "gpt-6-sol");
  assert.equal(ui.nodes.get("model-identification-run").disabled, false);

  ui.context.api = async () => ({accounts: []});
  await ui.run("loadModelIdentificationOptions()");
  assert.equal(account.value, "");
  assert.equal(model.value, "");
  assert.equal(model.disabled, true);
  assert.equal(ui.nodes.get("model-identification-run").disabled, true);
});

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
  ui.run('renderModelIdentifications([{account_id:"0123456789abcdef",requested_model:"gpt-6-sol",run_id:"id",run_actor_id:"owner",run_stage:"probing",run_probe_index:2,run_status:"failed",run_error_code:"model_identification_account_mismatch",run_progress:1,run_started_at:"2026-09-25T00:00:00Z",run_finished_at:"2026-09-25T00:01:00Z",conclusion:"claude-fable-5-1",closest_model:"claude-fable-5-1",match_level:"match",fit:0.36,margin:0.27,reference_version:"2026.09.5",completed_at:"2026-09-24T00:00:00Z",expires_at:"2026-10-24T00:00:00Z"}]);');
  assert.match(ui.nodes.get("model-identification-run-status").textContent, /本次失败.*不同的账号/);
  assert.match(ui.nodes.get("model-identification-results").textContent, /上次结论.*claude-fable-5-1.*2026.09.5/);
  assert.equal(ui.nodes.get("model-identification-progress").classList.contains("hidden"), false);
  assert.match(ui.nodes.get("model-identification-run-status").textContent, /第 2 题.*已通过 1 \/ 3 题.*耗时 60 秒.*运行 ID：id/);
});

function runRecord(overrides = {}) {
  return {account_id: "account", requested_model: "gpt-6-sol", run_id: "own-run", run_actor_id: "owner",
    run_status: "running", run_stage: "preflight", run_probe_index: 0, run_progress: 0,
    run_started_at: "2026-09-25T00:00:00Z", run_updated_at: "2026-09-25T00:00:00Z", ...overrides};
}

function prepareSubmission(ui) {
  ui.nodes.get("model-identification-account").value = "account";
  ui.nodes.get("model-identification-model").value = "gpt-6-sol";
  ui.context.sensitiveAction = async (operation) => operation();
  ui.run("bindModelIdentification()");
  return ui.nodes.get("model-identification-form");
}

test("an immediately failed submission stays bound to its returned run and shows diagnostics", async () => {
  const ui = dashboard();
  const form = prepareSubmission(ui);
  const created = runRecord();
  const failed = runRecord({run_status: "failed", run_stage: "probing", run_probe_index: 1,
    run_error_code: "model_identification_forbidden", run_error_source: "upstream", run_upstream_status: 403,
    run_finished_at: "2026-09-25T00:00:01Z", run_updated_at: "2026-09-25T00:00:01Z"});
  const other = runRecord({run_id: "other-run", run_actor_id: "another-owner", run_status: "succeeded",
    run_started_at: "2026-09-25T00:01:00Z"});
  const calls = [];
  ui.context.api = async (url, options) => {
    calls.push(url);
    if (options?.method === "POST") return {run_id: created.run_id, run: created};
    if (url === "/admin/model-identifications") return {identifications: [other, failed]};
    if (url === "/admin/model-identifications/runs/own-run") return {run: failed};
    throw new Error(`unexpected request ${url}`);
  };
  await form.dispatch("submit");
  assert.equal(ui.run("modelIdentificationActiveRunID"), "own-run");
  assert.match(ui.nodes.get("model-identification-run-status").textContent, /本次失败：上游拒绝.*第 1 题.*HTTP 403.*运行 ID：own-run/);
  assert.doesNotMatch(ui.nodes.get("model-identification-run-status").textContent, /other-run/);
  assert.equal(form.dataset.busy, "false", "bindAsync must receive a predicate factory and finish without a TypeError");
  assert.equal(ui.nodes.get("model-identification-run").disabled, false);
  assert.deepEqual(calls, ["/admin/model-identifications/runs", "/admin/model-identifications", "/admin/model-identifications/runs/own-run"]);
});

test("status restoration ignores another actor's and legacy records", () => {
  const ui = dashboard();
  ui.context.records = [runRecord({run_id: "someone-else", run_actor_id: "other-owner"}), runRecord({run_id: "legacy", run_actor_id: undefined})];
  ui.run("renderModelIdentifications(records)");
  assert.equal(ui.run("modelIdentificationActiveRunID"), "");
  assert.match(ui.nodes.get("model-identification-run-status").textContent, /尚无本人的运行记录/);
  assert.match(ui.nodes.get("model-identification-results").textContent, /someone-else.*legacy/);
  assert.equal(ui.nodes.get("model-identification-run").disabled, true, "the global slot still prevents a second run");
});

test("stale list and run responses cannot replace a newer bound run", async () => {
  const ui = dashboard();
  const form = prepareSubmission(ui);
  const old = runRecord({run_id: "old-run", run_status: "failed"});
  ui.context.records = [old];
  ui.run("renderModelIdentifications(records)");
  let resolveOldRun;
  let resolveOldList;
  ui.context.api = (url) => new Promise((resolve) => {
    if (url === "/admin/model-identifications") resolveOldList = resolve;
    else resolveOldRun = resolve;
  });
  const oldRunRequest = ui.run("loadModelIdentificationRun()");
  const oldListRequest = ui.run("loadModelIdentifications()");
  const newer = runRecord({run_id: "new-run", run_updated_at: "2026-09-25T00:02:00Z"});
  ui.context.api = async (url, options) => options?.method === "POST" ? {run_id: newer.run_id, run: newer} :
    url === "/admin/model-identifications" ? {identifications: [newer]} : {run: newer};
  await form.dispatch("submit");
  resolveOldRun({run: old});
  resolveOldList({identifications: [old]});
  await Promise.all([oldRunRequest, oldListRequest]);
  assert.equal(ui.run("modelIdentificationActiveRunID"), "new-run");
  assert.match(ui.nodes.get("model-identification-run-status").textContent, /运行 ID：new-run/);
  assert.doesNotMatch(ui.nodes.get("model-identification-results").textContent, /old-run/);
});

test("a replaced run refreshes the list without binding the replacement", async () => {
  const ui = dashboard();
  ui.context.records = [runRecord()];
  ui.run("renderModelIdentifications(records)");
  const replacement = runRecord({run_id: "replacement", run_actor_id: "other-owner"});
  const calls = [];
  ui.context.api = async (url) => {
    calls.push(url);
    if (url === "/admin/model-identifications/runs/own-run") {
      const error = new Error("not found"); error.status = 404; error.code = "model_identification_run_not_found"; throw error;
    }
    return {identifications: [replacement]};
  };
  await ui.run("loadModelIdentificationRun()");
  assert.equal(ui.run("modelIdentificationActiveRunID"), "");
  assert.match(ui.nodes.get("model-identification-run-status").textContent, /本次运行已被新的鉴别替代/);
  assert.match(ui.nodes.get("model-identification-results").textContent, /replacement/);
  assert.deepEqual(calls, ["/admin/model-identifications/runs/own-run", "/admin/model-identifications"]);
});

test("repeated submit events send one POST and a failed status read keeps the slot locked", async () => {
  const ui = dashboard();
  const form = prepareSubmission(ui);
  let resolvePost;
  let posts = 0;
  ui.context.api = (url, options) => {
    if (options?.method === "POST") {
      posts++;
      return new Promise((resolve) => { resolvePost = resolve; });
    }
    throw new Error("状态读取失败");
  };
  const first = form.dispatch("submit");
  await form.dispatch("submit");
  assert.equal(posts, 1);
  assert.equal(ui.nodes.get("model-identification-run").disabled, true);
  resolvePost({run_id: "own-run", run: runRecord()});
  await first;
  assert.equal(form.dataset.busy, "false");
  assert.equal(ui.nodes.get("model-identification-run").disabled, true);
  assert.match(ui.nodes.get("model-identification-message").textContent, /状态读取失败/);
  await form.dispatch("submit");
  assert.equal(posts, 1);
});

test("unsupported protocol disables submission even with previously selected options", async () => {
  const ui = dashboard();
  const form = prepareSubmission(ui);
  let posts = 0;
  ui.context.api = async (url, options) => {
    if (options?.method === "POST") posts++;
    const error = new Error("版本不兼容"); error.status = 503; error.code = "model_identification_protocol_unsupported"; throw error;
  };
  await assert.rejects(ui.run("loadModelIdentificationOptions()"), /版本不兼容/);
  assert.equal(ui.nodes.get("model-identification-run").disabled, true);
  await form.dispatch("submit");
  assert.equal(posts, 0);
});

test("older snapshots do not move a bound run backwards or clear terminal diagnostics", () => {
  const ui = dashboard();
  ui.context.records = [runRecord({run_status: "failed", run_stage: "validating", run_probe_index: 2,
    run_progress: 1, run_error_code: "model_identification_invalid_answer", run_updated_at: "2026-09-25T00:00:02Z"})];
  ui.run("renderModelIdentifications(records)");
  ui.context.records = [runRecord()];
  ui.run("renderModelIdentifications(records)");
  assert.match(ui.nodes.get("model-identification-run-status").textContent, /本次失败.*第 2 题.*已通过 1 \/ 3 题/);
  assert.equal(ui.run("modelIdentificationRunning"), false);
});

test("backend diagnostic failure scenarios keep specific labels and safe upstream details", () => {
  const ui = dashboard();
  const scenarios = [
    ["request_invalid", /鉴别请求或身份信息无效/],
    ["account_not_found", /所选账号已不存在/],
    ["unavailable", /鉴别服务暂不可用/],
    ["unexpected_tool", /上游返回了工具调用/],
    ["empty_output", /上游未返回可用于鉴别的文本回答/],
    ["upstream_rejected", /上游拒绝了鉴别请求/, 400],
    ["upstream_rejected", /上游拒绝了鉴别请求/, 403],
    ["upstream_rejected", /上游拒绝了鉴别请求/, 307],
    ["forbidden", /上游拒绝访问该账号或模型/, 403],
    ["redirect", /上游返回了不支持的重定向/, 307],
    ["auth_failed", /上游认证失败/, 401],
    ["rate_limited", /上游限流或额度不足/, 429],
    ["credential_missing", /账号缺少有效凭据/],
    ["model_unavailable", /所选模型已不可用/],
    ["account_mismatch", /上游返回了不同的账号/],
    ["network_failed", /无法连接上游/],
    ["upstream_failed", /上游服务执行失败/, 503],
    ["incomplete", /上游回答未完整生成/],
    ["invalid_response", /上游响应格式不符合鉴别协议/],
    ["response_too_large", /上游响应超过大小限制/],
    ["timeout", /探针或整项运行超时/],
    ["interrupted", /运行已中断/],
    ["invalid_answer", /本题回答未通过输入校验/],
    ["storage_failed", /保存运行结果失败/],
    ["reference_unavailable", /参考库暂不可用/],
    ["protocol_unsupported", /鉴别服务版本不兼容/],
    ["probe_failed", /探针执行失败，请根据运行 ID 查询诊断日志/],
  ];
  for (const [suffix, expected, status] of scenarios) {
    ui.context.records = [runRecord({run_status: "failed", run_error_code: `model_identification_${suffix}`,
      run_upstream_status: status || 0, run_retry_after: suffix === "rate_limited" ? 12 : 0,
      run_updated_at: "2026-09-25T00:00:01Z"})];
    ui.run("renderModelIdentifications(records)");
    const text = ui.nodes.get("model-identification-run-status").textContent;
    assert.match(text, expected, suffix);
    if (status) assert.match(text, new RegExp(`上游 HTTP ${status}`), suffix);
    if (suffix === "rate_limited") assert.match(text, /建议 12 秒后重试/);
  }
});

test("snapshots within one millisecond cannot move a running task to an earlier phase", () => {
  const ui = dashboard();
  ui.context.records = [runRecord({run_stage: "validating", run_probe_index: 2, run_progress: 1,
    run_updated_at: "2026-09-25T00:00:01.000900Z"})];
  ui.run("renderModelIdentifications(records)");
  ui.context.records = [runRecord({run_stage: "probing", run_probe_index: 2, run_progress: 1,
    run_updated_at: "2026-09-25T00:00:01.000100Z"})];
  ui.run("renderModelIdentifications(records)");
  assert.match(ui.nodes.get("model-identification-run-status").textContent, /第 2 题 · 校验回答 · 已通过 1 \/ 3 题/);
  assert.match(ui.nodes.get("model-identification-results").textContent, /第 2 题 · 校验回答/);
});
