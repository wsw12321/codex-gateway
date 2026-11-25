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
    DOMException, URLSearchParams, atob, TextDecoder, handler,
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

function egressForm(values) {
  const classes = () => { const set = new Set(); return {toggle: (name, on) => (on ? set.add(name) : set.delete(name)), contains: name => set.has(name)}; };
  const form = {dataset: {}, elements: Object.fromEntries(Object.entries(values).map(([key, value]) => [key, {value, checked: false, required: false, disabled: false, placeholder: ""}]))};
  return {form, cipherField: {classList: classes()}, usernameField: {classList: classes()}};
}

test("SOCKS5 nodes send protocol and username, never a cipher, and check credentials first", async () => {
  const calls = [];
  const ui = dashboard(async (url, options) => { calls.push({url, options}); return {}; });
  const {form} = egressForm({name: "socks", protocol: "socks5", server: "socks.example", port: "1080", cipher: "aes-256-gcm", username: "user", password: "secret", udp: ""});
  ui.nodes.set("egress-form", form);
  ui.nodes.set("egress-dialog", {close() {}});
  ui.context.event = {currentTarget: form};
  ui.run('egressDraftID = ""; loadEgressServers = async () => {}; egressMessage = () => {};');
  for (const [username, password] of [["", "orphan"], ["us\ner", "secret"], ["user", "x".repeat(256)], ["user", "密".repeat(86)]]) {
    form.elements.username.value = username;
    form.elements.password.value = password;
    await assert.rejects(ui.run("saveEgressServer(event)"), /SOCKS5/);
  }
  assert.equal(calls.length, 0);
  form.elements.username.value = "user";
  form.elements.password.value = "密".repeat(85);
  await ui.run("saveEgressServer(event)");
  assert.deepEqual(JSON.parse(calls[0].options.body), {name: "socks", protocol: "socks5", server: "socks.example", port: 1080, udp: false, username: "user", password: "密".repeat(85)});

  form.elements.username.value = "";
  form.elements.password.value = "";
  await ui.run("saveEgressServer(event)");
  const open = JSON.parse(calls[1].options.body);
  assert.equal(open.username, "");
  assert.equal("password" in open || "cipher" in open, false);
});

test("SS payloads keep the cipher and drop a SOCKS5 username", async () => {
  const calls = [];
  const ui = dashboard(async (url, options) => { calls.push({url, options}); return {}; });
  const {form} = egressForm({name: "ss", protocol: "ss", server: "ss.example", port: "8388", cipher: "aes-256-gcm", username: "stale", password: "pw", udp: ""});
  ui.nodes.set("egress-form", form);
  ui.nodes.set("egress-dialog", {close() {}});
  ui.context.event = {currentTarget: form};
  ui.run('egressDraftID = ""; loadEgressServers = async () => {}; egressMessage = () => {};');
  await ui.run("saveEgressServer(event)");
  assert.deepEqual(JSON.parse(calls[0].options.body), {name: "ss", protocol: "ss", server: "ss.example", port: 8388, udp: false, cipher: "aes-256-gcm", password: "pw"});
});

test("protocol switches toggle fields and require a new password", () => {
  const ui = dashboard(async () => ({}));
  const {form, cipherField, usernameField} = egressForm({name: "n", protocol: "socks5", server: "s.example", port: "1080", cipher: "aes-128-gcm", username: "", password: "typed", udp: ""});
  ui.nodes.set("egress-form", form);
  ui.nodes.set("egress-cipher-field", cipherField);
  ui.nodes.set("egress-username-field", usernameField);
  ui.nodes.set("egress-password-help", {textContent: ""});
  const sync = () => ui.run("syncEgressProtocolFields(byId('egress-form'))");
  const help = () => ui.nodes.get("egress-password-help").textContent;

  ui.run('egressDraftID = "";');
  sync();
  assert.equal(cipherField.classList.contains("hidden"), true);
  assert.equal(usernameField.classList.contains("hidden"), false);
  assert.equal(form.elements.password.disabled, true);
  assert.equal(form.elements.password.value, "");
  assert.match(help(), /无认证/);
  form.elements.username.value = "user";
  sync();
  assert.equal(form.elements.password.disabled, false);
  assert.equal(form.elements.password.required, true);

  // Editing an authenticated SOCKS5 node may keep its password.
  ui.run('egressDraftID = "node";');
  Object.assign(form.dataset, {savedProtocol: "socks5", savedPassword: "true", savedCipher: ""});
  sync();
  assert.equal(form.elements.password.required, false);
  assert.match(help(), /留空保留现有密码/);
  // An unauthenticated node gaining a username needs one.
  form.dataset.savedPassword = "";
  sync();
  assert.equal(form.elements.password.required, true);
  assert.match(help(), /重新填写密码/);

  // SOCKS5 to SS asks for the SS password again.
  form.dataset.savedPassword = "true";
  form.elements.protocol.value = "ss";
  sync();
  assert.equal(cipherField.classList.contains("hidden"), false);
  assert.equal(usernameField.classList.contains("hidden"), true);
  assert.equal(form.elements.password.disabled, false);
  assert.equal(form.elements.password.required, true);
  assert.match(help(), /切换协议需重新填写密码/);
  // SS to SOCKS5 with a username asks too.
  Object.assign(form.dataset, {savedProtocol: "ss", savedCipher: "aes-128-gcm"});
  form.elements.protocol.value = "socks5";
  sync();
  assert.equal(form.elements.password.required, true);
});

test("node cards describe the protocol and authentication", () => {
  const ui = dashboard(async () => ({}));
  assert.equal(ui.run('egressProtocolSummary({protocol: "ss", cipher: "aes-256-gcm"})'), "SS · aes-256-gcm");
  assert.equal(ui.run('egressProtocolSummary({cipher: "aes-128-gcm"})'), "SS · aes-128-gcm");
  assert.equal(ui.run('egressProtocolSummary({protocol: "socks5", cipher: "", has_password: true})'), "SOCKS5 · 用户名/密码认证");
  assert.equal(ui.run('egressProtocolSummary({protocol: "socks5", cipher: "", has_password: false})'), "SOCKS5 · 无认证");
});

test("HTTPS sends SNI with credentials and advanced protocols send only their option object", async () => {
  const calls = [];
  const ui = dashboard(async (_url, request) => { calls.push(JSON.parse(request.body)); return {}; });
  const {form} = egressForm({name: "node", protocol: "https", server: "192.0.2.1", port: "443",
    cipher: "stale", username: "user", password: "sample", udp: "", tls_server_name: "proxy.example", options: ""});
  ui.context.event = {currentTarget: form};
  ui.nodes.set("egress-dialog", {close() {}});
  ui.run('egressDraftID = ""; loadEgressServers = async () => {};');
  await ui.run("saveEgressServer(event)");
  assert.deepEqual(calls[0], {name: "node", protocol: "https", server: "192.0.2.1", port: 443,
    udp: false, tls_server_name: "proxy.example", username: "user", password: "sample"});
  form.elements.protocol.value = "trojan";
  form.elements.options.value = '{"password":"sample"}';
  await ui.run("saveEgressServer(event)");
  assert.deepEqual(calls[1], {name: "node", protocol: "trojan", server: "192.0.2.1", port: 443, udp: false, options: {password:"sample"}});
});

// Field lists copied from internal/egress/outbound.go; imports must stay inside them.
const protocolFields = {
  vless: "uuid flow encryption tls servername network alpn client-fingerprint ws-opts grpc-opts h2-opts http-opts xhttp-opts reality-opts packet-encoding",
  vmess: "uuid alterId cipher tls servername network alpn client-fingerprint ws-opts grpc-opts h2-opts http-opts reality-opts packet-encoding",
  trojan: "password network sni alpn client-fingerprint ws-opts grpc-opts reality-opts",
  hysteria2: "password up down obfs obfs-password sni alpn",
  tuic: "uuid password token sni alpn reduce-rtt udp-relay-mode congestion-controller",
  anytls: "password sni alpn client-fingerprint",
};

function importer(role = "owner") {
  const ui = dashboard(async () => ({}));
  ui.context.fields = Object.fromEntries(Object.entries(protocolFields).map(([key, value]) => [key, value.split(" ")]));
  ui.run(`egressListing = {protocol_fields: fields}; state = {user: {id: "u", role: "${role}"}};`);
  const parse = (text, current = "") => {
    ui.context.input = text;
    return JSON.parse(ui.run(`JSON.stringify(parseEgressImport(input, ${JSON.stringify(current)}))`));
  };
  const fails = (text, pattern) => {
    ui.context.input = text;
    assert.throws(() => ui.run("parseEgressImport(input)"), pattern);
  };
  return {ui, parse, fails};
}

const b64 = (text) => Buffer.from(text).toString("base64");

test("VLESS Reality and WebSocket links become Mihomo options", () => {
  const {parse} = importer();
  const reality = parse("vless://1b2c3d4e-0000-4000-8000-000000000001@203.0.113.10:443?encryption=none&flow=xtls-rprx-vision&security=reality&sni=www.example.com&fp=chrome&pbk=PUBkey_-1&sid=6ba85179&spx=%2F&type=tcp&headerType=none#%E9%A6%99%E6%B8%AF%2001");
  assert.deepEqual(reality, {name: "香港 01", server: "203.0.113.10", port: 443, dropped: [], protocol: "vless", options: {
    uuid: "1b2c3d4e-0000-4000-8000-000000000001", flow: "xtls-rprx-vision", tls: true, servername: "www.example.com",
    "client-fingerprint": "chrome", "reality-opts": {"public-key": "PUBkey_-1", "short-id": "6ba85179"}}});
  const ws = parse("vless://u-1@cdn.example.com:443?type=ws&security=tls&path=%2Fray&host=cdn.example.com&sni=cdn.example.com&alpn=h2%2Chttp%2F1.1&allowInsecure=1#ws");
  assert.deepEqual(ws.options, {uuid: "u-1", "ws-opts": {path: "/ray", headers: {Host: "cdn.example.com"}}, network: "ws", tls: true,
    servername: "cdn.example.com", alpn: ["h2", "http/1.1"]});
  assert.deepEqual(ws.dropped, ["allowInsecure"]);
  for (const result of [reality, ws]) for (const key of Object.keys(result.options)) assert.ok(protocolFields.vless.split(" ").includes(key), key);
});

test("VMess base64 JSON, Trojan, Hysteria2 and TUIC links parse", () => {
  const {parse} = importer();
  const vmess = parse("vmess://" + b64(JSON.stringify({v: "2", ps: "节点", add: "203.0.113.1", port: "8443", id: "uuid-v", aid: "0", scy: "auto", net: "grpc", path: "svc", tls: "tls", sni: "g.example.com"})));
  assert.deepEqual(vmess, {name: "节点", server: "203.0.113.1", port: 8443, dropped: [], protocol: "vmess", options: {
    uuid: "uuid-v", alterId: 0, cipher: "auto", "grpc-opts": {"grpc-service-name": "svc"}, network: "grpc", tls: true, servername: "g.example.com"}});
  const trojan = parse("trojan://p%40ss@203.0.113.2:443?security=tls&type=ws&path=%2Ft&sni=t.example.com#t");
  assert.deepEqual(trojan.options, {password: "p@ss", "ws-opts": {path: "/t"}, network: "ws", sni: "t.example.com"});
  const hy2 = parse("hy2://secret@203.0.113.5:8443/?sni=h.example.com&obfs=salamander&obfs-password=x&insecure=1#h");
  assert.equal(hy2.protocol, "hysteria2");
  assert.deepEqual(hy2.options, {password: "secret", sni: "h.example.com", obfs: "salamander", "obfs-password": "x"});
  assert.deepEqual(hy2.dropped, ["insecure"]);
  const tuic = parse("tuic://uuid-t:pw@203.0.113.6:443?congestion_control=bbr&udp_relay_mode=native&alpn=h3&sni=x.example.com");
  assert.deepEqual(tuic.options, {uuid: "uuid-t", password: "pw", sni: "x.example.com", alpn: ["h3"], "congestion-controller": "bbr", "udp-relay-mode": "native"});
});

test("Shadowsocks links fill cipher and password and refuse what the runtime lacks", () => {
  const {parse, fails} = importer();
  assert.deepEqual(parse(`ss://${b64("aes-256-gcm:pa ss").replace(/=+$/, "")}@203.0.113.3:8388#ss`),
    {name: "ss", server: "203.0.113.3", port: 8388, dropped: [], protocol: "ss", cipher: "aes-256-gcm", password: "pa ss"});
  const key = "AAAAAAAAAAAAAAAAAAAAAA==";
  assert.equal(parse(`ss://2022-blake3-aes-128-gcm:${encodeURIComponent(key)}@203.0.113.3:443`).password, key);
  const legacy = parse(`ss://${b64("chacha20-ietf-poly1305:pw@203.0.113.4:8388")}#old`);
  assert.deepEqual([legacy.cipher, legacy.password, legacy.server, legacy.port], ["chacha20-ietf-poly1305", "pw", "203.0.113.4", 8388]);
  fails(`ss://${b64("rc4-md5:pw")}@203.0.113.3:8388`, /不支持加密方式 rc4-md5/);
  fails(`ss://${b64("aes-128-gcm:pw")}@203.0.113.3:8388/?plugin=obfs-local`, /插件/);
});

test("Mihomo JSON splits endpoint fields and reports ignored keys", () => {
  const {parse, fails} = importer();
  const node = parse(JSON.stringify({name: "jp", type: "vless", server: "203.0.113.7", port: 443, udp: true, uuid: "u", tls: true,
    servername: "s.example.com", "skip-cert-verify": true, "reality-opts": {"public-key": "k", "short-id": "1", "spider-x": "/"}}));
  assert.deepEqual(node, {name: "jp", server: "203.0.113.7", port: 443, dropped: ["skip-cert-verify", "reality-opts.spider-x"], udp: true,
    options: {uuid: "u", tls: true, servername: "s.example.com", "reality-opts": {"public-key": "k", "short-id": "1"}}, protocol: "vless"});
  const http = parse(JSON.stringify({name: "h", type: "http", server: "203.0.113.8", port: 443, tls: true, sni: "p.example.com", username: "a", password: "b"}));
  assert.deepEqual([http.protocol, http.tls_server_name, http.username, http.password], ["https", "p.example.com", "a", "b"]);
  const only = parse('{"password": "x", "sni": "t.example.com"}', "trojan");
  assert.deepEqual([only.protocol, only.optionsOnly, only.options], ["trojan", true, {password: "x", sni: "t.example.com"}]);
  fails("{bad", /JSON 格式无效/);
  fails("hello", /无法识别/);
  fails("vless://u@host-without-port", /服务器地址或端口/);
  fails("trojan://p@203.0.113.2:443?type=xhttp&path=%2Fx", /不支持该传输方式/);
  fails(JSON.stringify({type: "wireguard", server: "203.0.113.9", port: 51820}), /当前不支持 WIREGUARD/);
});

test("importing fills the form and saving rejects keys the protocol lacks", async () => {
  const calls = [];
  const ui = dashboard(async (_url, request) => { calls.push(JSON.parse(request.body)); return {}; });
  ui.context.fields = Object.fromEntries(Object.entries(protocolFields).map(([key, value]) => [key, value.split(" ")]));
  ui.run('egressListing = {protocol_fields: fields}; state = {user: {id: "m", role: "member"}, recently_verified: true, marketplace: {enabled: true}}; egressDraftID = ""; loadEgressServers = async () => {};');
  const {form} = egressForm({name: "", protocol: "ss", server: "", port: "", cipher: "aes-128-gcm", username: "", password: "", udp: "", tls_server_name: "", options: ""});
  const result = {classList: {remove() {}, add() {}}, dataset: {}, textContent: ""};
  ui.nodes.set("egress-form", form);
  ui.nodes.set("egress-import-text", {value: "trojan://pw@proxy.example.com:443?sni=proxy.example.com#东京"});
  ui.nodes.set("egress-import-result", result);
  ui.nodes.set("egress-dialog", {close() {}});
  ui.run("applyEgressImport(byId('egress-form'))");
  assert.deepEqual([form.elements.protocol.value, form.elements.name.value, form.elements.server.value, form.elements.port.value], ["trojan", "东京", "proxy.example.com", "443"]);
  assert.deepEqual(JSON.parse(form.elements.options.value), {password: "pw", sni: "proxy.example.com"});
  assert.equal(result.dataset.kind, "warn");
  assert.match(result.textContent, /公网 IPv4/);

  ui.context.event = {currentTarget: form};
  form.elements.server.value = "203.0.113.2";
  form.elements.options.value = '{"name": "x", "password": "pw"}';
  await assert.rejects(ui.run("saveEgressServer(event)"), /不支持的字段：name.*快速导入/);
  assert.equal(calls.length, 0);
  form.elements.options.value = '{"password": "pw"}';
  await ui.run("saveEgressServer(event)");
  assert.deepEqual(calls[0].options, {password: "pw"});
});
