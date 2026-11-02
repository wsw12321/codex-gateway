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
