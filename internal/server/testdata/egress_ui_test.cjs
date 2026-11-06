"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return {promise, resolve};
}

function dashboard(handler) {
  const nodes = new Map();
  const context = vm.createContext({
    document: {getElementById: id => nodes.get(id), querySelectorAll: () => []},
    window: {setTimeout, clearTimeout}, location: {hash: "#upstream-accounts"},
    DOMException, URLSearchParams, handler,
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = text => vm.runInContext(text, context);
  run(`state = {user: {id: "owner", role: "owner"}, recently_verified: true};
    api = handler; syncUpstreamAccountControls = () => {}; setLocalMessage = () => {};
    renderEgressServers = () => {};`);
  return {context, run, nodes};
}

test("late node response cannot repopulate another login's configuration", async () => {
  const pending = deferred();
  const ui = dashboard(() => pending.promise);
  const request = ui.run("loadEgressServers()");
  ui.run("identityGeneration++; state = null;");
  pending.resolve({servers: [{id: "private-node", name: "private"}]});
  await request;
  assert.equal(ui.run("egressServers.length"), 0);
  assert.equal(ui.run("egressLoaded"), false);
});

test("newer node list wins when refreshes complete out of order", async () => {
  const first = deferred(), second = deferred();
  let count = 0;
  const ui = dashboard(() => (++count === 1 ? first : second).promise);
  const older = ui.run("loadEgressServers()");
  const newer = ui.run("loadEgressServers()");
  second.resolve({servers: [{id: "new", name: "new"}]});
  await newer;
  first.resolve({servers: [{id: "old", name: "old"}]});
  await older;
  assert.equal(ui.run("egressServers[0].id"), "new");
});

test("editing preserves an empty password and transmits replacement exactly", async () => {
  const calls = [];
  const ui = dashboard(async (url, options) => { calls.push({url, options}); return {}; });
  const form = {elements: Object.fromEntries(Object.entries({name: " exit ", server: "exit.example", port: "8388", cipher: "aes-256-gcm", password: "", udp: ""}).map(([key, value]) => [key, {value, checked: key === "udp"}]))};
  ui.nodes.set("egress-form", form);
  ui.nodes.set("egress-dialog", {close() {}});
  ui.context.event = {currentTarget: form};
  ui.run('egressDraftID = "node-id"; loadEgressServers = async () => {};');
  await ui.run("saveEgressServer(event)");
  assert.equal(calls[0].url, "/admin/egress-servers/node-id");
  let payload = JSON.parse(calls[0].options.body);
  assert.equal(Object.hasOwn(payload, "password"), false);
  assert.equal(payload.name, "exit");
  assert.equal(payload.udp, true);
  form.elements.password.value = '  secret$"\\  ';
  await ui.run("saveEgressServer(event)");
  payload = JSON.parse(calls[1].options.body);
  assert.equal(payload.password, '  secret$"\\  ');
  assert.equal(form.elements.password.value, "");
});

test("account unbinding sends null to the captured provider route", async () => {
  const calls = [];
  const ui = dashboard(async (url, options) => { calls.push({url, options}); return {}; });
  ui.context.form = {};
  ui.run('egressLoaded = true; upstreamAccountSyncHealthy = true; upstreamAccountProvider = "anthropic"; upstreamAccounts = [{id: "0123456789abcdef", can_manage: true}];');
  await ui.run('saveUpstreamEgress(upstreamAccounts[0], form, "")');
  assert.equal(calls[0].url, "/admin/anthropic-accounts/0123456789abcdef/egress");
  assert.deepEqual(JSON.parse(calls[0].options.body), {egress_server_id: null});
});

test("removed or unsynchronized accounts can release their durable exit binding", async () => {
  const calls = [];
  const ui = dashboard(async (url, options) => { calls.push({url, options}); return {}; });
  ui.context.form = {};
  ui.run('egressLoaded = true; upstreamAccountSyncHealthy = false; upstreamAccounts = [{id: "0123456789abcdef", can_manage: false, egress_server_id: "node-id"}];');
  await ui.run('saveUpstreamEgress(upstreamAccounts[0], form, "")');
  assert.equal(calls.length, 1);
  assert.deepEqual(JSON.parse(calls[0].options.body), {egress_server_id: null});
  assert.equal(ui.run("upstreamAccounts[0].egress_server_id"), null);
});

test("changing provider during verification prevents the delayed binding write", async () => {
  let writes = 0;
  const pending = deferred();
  const ui = dashboard(async () => { writes++; return {}; });
  ui.context.form = {};
  ui.context.pending = pending;
  ui.run('egressLoaded = true; upstreamAccountSyncHealthy = true; upstreamAccounts = [{id: "0123456789abcdef", can_manage: true}]; state.recently_verified = false; reauthenticate = () => pending.promise;');
  const save = ui.run('saveUpstreamEgress(upstreamAccounts[0], form, "node-id")');
  ui.run('upstreamAccountProvider = "antigravity"; upstreamAccountOperation = null;');
  pending.resolve();
  await assert.rejects(save);
  assert.equal(writes, 0);
});

test("connectivity test posts the selected target and records the tested revision", async () => {
  const calls = [];
  const ui = dashboard(async (url, options) => { calls.push({url, options}); return {ok: true, code: "ok", latency_ms: 142, target: "api.anthropic.com", tested_at: "2026-11-06T08:00:00Z"}; });
  ui.nodes.set("egress-probe-target", {value: "anthropic"});
  ui.run('egressServers = [{id: "node/id", name: "a", revision: 4}];');
  await ui.run("testEgressServer(egressServers[0])");
  assert.equal(calls[0].url, "/admin/egress-servers/node%2Fid/test");
  assert.equal(calls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(calls[0].options.body), {target: "anthropic"});
  const status = ui.run("egressProbeStatus(egressServers[0])");
  assert.equal(status.kind, "ok");
  assert.match(status.text, /^连通正常 · api\.anthropic\.com · 142 ms/);
  ui.run("egressServers[0].revision = 5;");
  assert.equal(ui.run("egressProbeStatus(egressServers[0])").kind, "stale");
});

test("unknown selections fall back to the Codex target", async () => {
  const calls = [];
  const ui = dashboard(async (url, options) => { calls.push(JSON.parse(options.body)); return {ok: true, code: "ok"}; });
  ui.nodes.set("egress-probe-target", {value: "evil.invalid"});
  ui.run('egressServers = [{id: "n", revision: 1}];');
  await ui.run("testEgressServer(egressServers[0])");
  assert.deepEqual(calls[0], {target: "codex"});
});

test("failed connectivity tests show a stable explanation without raw details", async () => {
  let reply = {ok: false, code: "handshake_failed", latency_ms: 10000};
  const ui = dashboard(async () => { if (reply instanceof Error) throw reply; return reply; });
  ui.run('egressServers = [{id: "n", revision: 1}];');
  await ui.run("testEgressServer(egressServers[0])");
  let status = ui.run("egressProbeStatus(egressServers[0])");
  assert.equal(status.kind, "error");
  assert.match(status.text, /^连通失败 · chatgpt\.com：未能与目标完成 TLS 握手/);
  reply = Object.assign(new Error("节点配置尚未应用到出口服务，请稍后再测试"), {code: "egress_node_not_applied", status: 409});
  await ui.run("testEgressServer(egressServers[0])");
  status = ui.run("egressProbeStatus(egressServers[0])");
  assert.equal(status.kind, "error");
  assert.match(status.text, /节点配置尚未应用到出口服务/);
});

test("a late connectivity result cannot appear after another login", async () => {
  const pending = deferred();
  const ui = dashboard(() => pending.promise);
  ui.run('egressServers = [{id: "n", revision: 1}];');
  const request = ui.run("testEgressServer(egressServers[0])");
  assert.equal(ui.run("egressProbeStatus(egressServers[0]).kind"), "pending");
  ui.run("resetEgressServers(); identityGeneration++; state = {user: {id: 'other', role: 'owner'}};");
  pending.resolve({ok: true, code: "ok"});
  await request;
  assert.equal(ui.run("egressProbes.size"), 0);
});

test("testing all nodes skips unapplied revisions and bounds concurrency", async () => {
  let active = 0, peak = 0;
  const tested = [];
  const ui = dashboard(async (url) => {
    active++; peak = Math.max(peak, active);
    await new Promise(done => setTimeout(done, 5));
    active--; tested.push(url);
    return {ok: true, code: "ok"};
  });
  ui.run(`egressServers = Array.from({length: 7}, (_, index) => ({id: "n" + index, revision: index === 6 ? 9 : 1}));
    egressListing = {version: 8, runtime: {ready: true, applied_version: 7, node_revisions: Object.fromEntries(egressServers.slice(0, 6).map(node => [node.id, 1]))}};
    egressMessage = () => {};`);
  await ui.run("testAllEgressServers()");
  assert.equal(tested.length, 6);
  assert.equal(tested.includes("/admin/egress-servers/n6/test"), false);
  assert.ok(peak <= 3, `peak concurrency ${peak}`);
});

test("SS2022 keys are checked before any request and switching asks for a key", async () => {
  const calls = [];
  const ui = dashboard(async (url, options) => { calls.push({url, options}); return {}; });
  const values = {name: "ss2022", server: "exit.example", port: "8388", cipher: "2022-blake3-aes-128-gcm", password: "not-base64", udp: ""};
  const form = {dataset: {}, elements: Object.fromEntries(Object.entries(values).map(([key, value]) => [key, {value, checked: false, required: false, placeholder: ""}]))};
  ui.nodes.set("egress-form", form);
  ui.nodes.set("egress-dialog", {close() {}});
  ui.nodes.set("egress-password-help", {textContent: ""});
  ui.context.event = {currentTarget: form};
  ui.run('egressDraftID = ""; loadEgressServers = async () => {};');
  for (const bad of ["not-base64", "AAECAwQFBgcICQoLDA0ODw", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", "AAECAwQFBgcICQoLDA0ODw==:"]) {
    form.elements.password.value = bad;
    await assert.rejects(ui.run("saveEgressServer(event)"), /Base64/);
  }
  assert.equal(calls.length, 0);
  form.elements.password.value = "AAECAwQFBgcICQoLDA0ODw==:EBESExQVFhcYGRobHB0eHw==";
  await ui.run("saveEgressServer(event)");
  assert.equal(JSON.parse(calls[0].options.body).cipher, "2022-blake3-aes-128-gcm");

  ui.run('egressDraftID = "node-id";');
  form.dataset.savedCipher = "aes-256-gcm";
  ui.run("syncEgressPasswordHelp(byId('egress-form'))");
  assert.equal(form.elements.password.required, true);
  assert.match(ui.nodes.get("egress-password-help").textContent, /重新填写密钥/);
  form.dataset.savedCipher = "2022-blake3-aes-128-gcm";
  ui.run("syncEgressPasswordHelp(byId('egress-form'))");
  assert.equal(form.elements.password.required, false);
  assert.match(ui.nodes.get("egress-password-help").textContent, /留空保留现有密钥/);
  form.elements.cipher.value = "aes-128-gcm";
  ui.run("syncEgressPasswordHelp(byId('egress-form'))");
  assert.equal(form.elements.password.required, false);
  assert.equal(ui.nodes.get("egress-password-help").textContent, "留空保留现有密码。");
});
