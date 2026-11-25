"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {}; this.className = "";
    this.value = ""; this.disabled = false; this.listeners = {}; this.text = ""; this.title = ""; this.href = "";
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => this.classList.toggle(name, true), remove: (name) => this.classList.toggle(name, false),
      toggle: (name, active) => { const set = new Set(this.className.split(/\s+/).filter(Boolean)); if (active) set.add(name); else set.delete(name); this.className = [...set].join(" "); },
    };
  }
  get textContent() { return this.text + this.children.map((child) => child.textContent).join(""); }
  set textContent(value) { this.text = value; this.children = []; }
  append(...children) { children.filter(Boolean).forEach((child) => { if (typeof child === "string") { const text = new Element("#text"); text.text = child; child = text; } child.parent = this; this.children.push(child); }); }
  replaceChildren(...children) { this.text = ""; this.children = []; this.append(...children); }
  setAttribute(key, value) { this.attributes[key] = value; }
  getAttribute(key) { return this.attributes[key]; }
  addEventListener(name, listener) { this.listeners[name] = listener; }
  matches(selector) { return selector.startsWith(".") ? this.classList.contains(selector.slice(1)) : this.tagName === selector; }
  querySelectorAll(selector) { return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest() { return null; }
  reset() {}
}

function dashboard(role = "member", marketplace = {enabled: true, max_accounts_per_user: 2, max_egress_servers_per_user: 1}) {
  const nodes = new Map(), root = new Element();
  const node = (id) => { if (!nodes.has(id)) { const value = new Element(); nodes.set(id, value); root.append(value); } return nodes.get(id); };
  const opened = [];
  const context = vm.createContext({
    document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    location: {hash: "#providers"}, URLSearchParams, URL, TextEncoder, DOMException, AbortController,
    window: {setTimeout: () => 0, clearTimeout: () => {}, open: (...args) => opened.push(args)},
  });
  const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
  vm.runInContext(source.slice(0, source.lastIndexOf("\nstart().catch")), context);
  const run = (code) => vm.runInContext(code, context);
  context.initialState = {user: {id: role === "owner" ? "owner-id" : "member-id", role}, recently_verified: true, marketplace};
  run("state = initialState; notice = () => {};");
  const calls = [];
  const responses = [];
  context.respond = async (requestPath, options = {}) => {
    calls.push({path: requestPath, method: options.method || "GET", body: options.body ? JSON.parse(options.body) : null});
    const next = responses.shift();
    if (!next) throw new Error(`unexpected ${requestPath}`);
    if (next instanceof Error) throw next;
    return typeof next === "function" ? next(requestPath, options) : next;
  };
  run("api = respond;");
  const form = (id, values) => {
    const host = node(id);
    host.elements = Object.fromEntries(Object.entries(values).map(([key, value]) => [key, {value}]));
    return host;
  };
  return {run, node, calls, responses, opened, form, context};
}

const listing = {
  max_accounts_per_user: 2, max_egress_servers_per_user: 1, member_suppliers: false, account_count: 1, providers: ["codex", "anthropic"],
  accounts: [{id: "a000000000000001", provider: "codex", credential_kind: "oauth", alias: "主力", email_masked: "a***@example.com", plan: "plus",
    status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available", admin_suspended: false,
    can_manage: true, concurrent_limit: 2, allocation_weight: 1, egress_server_id: "", request_count: 3, equivalent_cost_usd: "0.12"}],
};

test("members reach their providers and egress nodes only while the marketplace is enabled", () => {
  const member = dashboard();
  assert.equal(member.run('sectionAllowed("providers")'), true);
  assert.equal(member.run('sectionAllowed("egress-servers")'), true);
  assert.equal(member.run('sectionAllowed("tenant-providers")'), false);
  assert.equal(member.run('sectionAllowed("upstream-accounts")'), false);
  assert.equal(member.run("egressBase()"), "/admin/me/egress-servers");
  const closed = dashboard("member", {enabled: false});
  assert.equal(closed.run('sectionAllowed("providers")'), false);
  assert.equal(closed.run('sectionAllowed("egress-servers")'), false);
  assert.equal(closed.run("egressAllowed()"), false);
  const owner = dashboard("owner");
  assert.equal(owner.run('sectionAllowed("providers")'), false);
  assert.equal(owner.run('sectionAllowed("tenant-providers")'), true);
  assert.equal(owner.run('sectionAllowed("egress-servers")'), true);
  assert.equal(owner.run("egressBase()"), "/admin/egress-servers");
});

test("a member's egress page lists their own nodes", async () => {
  const ui = dashboard();
  ui.run("renderEgressServers = () => {}; syncUpstreamAccountControls = () => {};");
  ui.responses.push({servers: [{id: "n1", name: "home"}], version: 1, runtime: {}});
  await ui.run("loadEgressServers()");
  assert.equal(ui.calls[0].path, "/admin/me/egress-servers");
  assert.equal(ui.run("egressServers[0].id"), "n1");
});

test("the provider list shows the member's accounts, limits and suppliers switch", async () => {
  const ui = dashboard();
  ui.responses.push(listing);
  await ui.run("loadMyProviders()");
  assert.equal(ui.calls[0].path, "/admin/me/providers");
  assert.match(ui.node("providers-summary").textContent, /已添加 1 \/ 2 个账号/);
  const cards = ui.node("providers-list").querySelectorAll(".provider-card");
  assert.equal(cards.length, 1);
  assert.match(cards[0].textContent, /主力/);
  assert.equal(ui.node("provider-add-provider").children.map((option) => option.attributes.value).join(","), "codex,anthropic");
  assert.equal(ui.node("provider-supplier-open").classList.contains("hidden"), true);
  // At the limit every add button is disabled.
  ui.responses.push({...listing, account_count: 2});
  await ui.run("loadMyProviders()");
  assert.equal(ui.node("provider-oauth-start").disabled, true);
  assert.equal(ui.node("provider-import-open").disabled, true);
});

test("a suspended account cannot be re-enabled by its member", async () => {
  const ui = dashboard();
  ui.responses.push({...listing, accounts: [{...listing.accounts[0], admin_suspended: true, gateway_manual_status: "manual_disabled"}]});
  await ui.run("loadMyProviders()");
  const card = ui.node("providers-list").querySelector(".provider-card");
  assert.match(card.textContent, /管理员已停用/);
  const toggle = card.querySelectorAll("button").find((button) => button.textContent === "启用");
  assert.equal(toggle.disabled, true);
});

test("logins start with the chosen egress, poll, and explain a refusal", async () => {
  const ui = dashboard();
  ui.form("provider-add-form", {provider: "codex", egress_server_id: "node-1"});
  ui.responses.push({id: "flow", url: "https://auth.openai.com/oauth/authorize?state=s", expires_in: 300});
  await ui.run("startProviderOAuth()");
  assert.deepEqual(ui.calls[0], {path: "/admin/me/providers/codex/oauth", method: "POST", body: {egress_server_id: "node-1"}});
  assert.equal(ui.node("provider-oauth-link").href, "https://auth.openai.com/oauth/authorize?state=s");
  assert.equal(ui.opened.length, 1);
  ui.responses.push({status: "wait"});
  await ui.run("pollProviderOAuth(providerOAuth)");
  assert.equal(ui.calls[1].path, "/admin/me/providers/oauth/flow/status");
  ui.responses.push({status: "error", error: "account_owned_elsewhere"});
  await ui.run("pollProviderOAuth(providerOAuth)");
  assert.match(ui.node("provider-oauth-status").textContent, /已归平台或其他用户所有/);
  // A foreign authorization host is never opened.
  ui.responses.push({id: "flow2", url: "https://auth.openai.com.attacker.example/?state=s", expires_in: 300});
  await assert.rejects(ui.run("startProviderOAuth()"), /授权地址无效/);
});

test("a completed login reloads the list", async () => {
  const ui = dashboard();
  ui.form("provider-add-form", {provider: "anthropic", egress_server_id: ""});
  ui.responses.push({id: "flow", url: "https://claude.ai/oauth/authorize?state=s", expires_in: 300});
  await ui.run("startProviderOAuth()");
  assert.deepEqual(ui.calls[0].body, {});
  ui.responses.push({status: "ok", account_id: "a000000000000001"}, listing);
  await ui.run("pollProviderOAuth(providerOAuth)");
  assert.equal(ui.calls.at(-1).path, "/admin/me/providers");
  assert.equal(ui.run("providerOAuth"), null);
});

test("callback addresses must be the local redirect with one code and state", () => {
  const ui = dashboard();
  assert.equal(ui.run('JSON.stringify(providerCallbackFields("http://localhost:1455/auth/callback?code=c&state=s"))'), '{"code":"c","state":"s"}');
  for (const bad of ["https://localhost/cb?code=c&state=s", "http://evil.example/cb?code=c&state=s", "http://localhost/cb?code=c",
    "http://localhost/cb?code=c&state=s&error=denied", "http://localhost/cb?code=c&code=d&state=s", "not a url"]) {
    assert.throws(() => ui.run(`providerCallbackFields(${JSON.stringify(bad)})`), /本地回调地址|授权码/, bad);
  }
});

test("imports send only the tokens", () => {
  const ui = dashboard();
  const codex = ui.run(`JSON.stringify(providerImportFields('{"refresh_token":" rt ","email":"x@example.com","proxy_url":"http://evil"}', "codex"))`);
  assert.equal(codex, '{"refresh_token":"rt"}');
  const nested = ui.run(`JSON.stringify(providerImportFields('{"tokens":{"refresh_token":"rt","id_token":"id"}}', "codex"))`);
  assert.equal(nested, '{"refresh_token":"rt","id_token":"id"}');
  const claude = ui.run(`JSON.stringify(providerImportFields('{"claudeAiOauth":{"refreshToken":"r","accessToken":"a","subscriptionType":"max"}}', "anthropic"))`);
  assert.equal(claude, '{"refresh_token":"r","access_token":"a"}');
  assert.throws(() => ui.run(`providerImportFields('{"access_token":"a"}', "codex")`), /refresh_token/);
  assert.throws(() => ui.run(`providerImportFields('[1]', "codex")`), /JSON 对象/);
});

test("the Owner suspends and transfers tenant accounts", async () => {
  const ui = dashboard("owner");
  ui.responses.push({accounts: [{id: "a000000000000001", provider: "codex", owner_user_id: "alice-id", owner_username: "alice", owner_display_name: "Alice",
    alias: "", display_name: "", email_masked: "a***@example.com", status: "available", admin_suspended: false, egress_server_name: ""}],
  egress_servers: [{id: "n1", name: "home", server: "8.8.8.8", port: 8388, protocol: "ss", owner_username: "alice", bound_accounts: 1}]});
  await ui.run("loadTenantProviders()");
  assert.equal(ui.calls[0].path, "/admin/tenant-providers");
  assert.equal(ui.node("tenant-providers-body").children.length, 1);
  assert.match(ui.node("tenant-egress-body").textContent, /8\.8\.8\.8:8388/);
  ui.responses.push({}, {accounts: [], egress_servers: []});
  await ui.run('setTenantProviderSuspension(tenantProviders.accounts[0], true)');
  assert.deepEqual(ui.calls[1], {path: "/admin/tenant-providers/a000000000000001/suspension", method: "PUT", body: {suspended: true}});
  // Transfer to the platform.
  ui.run('ownerTransferTarget = {provider: "anthropic", id: "b000000000000002", owner: "alice-id", label: "x"};');
  const form = ui.form("owner-transfer-form", {target: "platform"});
  ui.node("owner-transfer-dialog").close = () => {};
  ui.context.event = {currentTarget: form};
  ui.responses.push({egress_reset: true});
  await ui.run("submitOwnerTransfer(event)");
  assert.deepEqual(ui.calls.at(-1), {path: "/admin/anthropic-accounts/b000000000000002/owner", method: "PUT", body: {owner_user_id: null, expected_owner_user_id: "alice-id"}});
  // A member target needs a chosen member.
  ui.run('ownerTransferTarget = {provider: "codex", id: "c000000000000003", owner: "", label: "x"}; ownerTransferUserSearch = {selectedID: () => ""};');
  form.elements.target.value = "member";
  await assert.rejects(ui.run("submitOwnerTransfer(event)"), /请选择新主人/);
  ui.run('ownerTransferUserSearch = {selectedID: () => "bob-id"};');
  ui.responses.push({egress_reset: false});
  await ui.run("submitOwnerTransfer(event)");
  assert.deepEqual(ui.calls.at(-1), {path: "/admin/upstream-accounts/c000000000000003/owner", method: "PUT", body: {owner_user_id: "bob-id", expected_owner_user_id: null}});
});

test("the Owner deletes a disabled member's egress node after confirming", async () => {
  const ui = dashboard("owner");
  const node = {id: "n2", name: "office", server: "8.8.4.4", port: 8388, protocol: "ss", owner_username: "carol", owner_status: "disabled", bound_accounts: 0};
  ui.responses.push({accounts: [], egress_servers: [node]});
  await ui.run("loadTenantProviders()");
  const body = ui.node("tenant-egress-body");
  assert.match(body.textContent, /成员已停用/);
  const remove = body.querySelectorAll("button").find((button) => button.dataset.tenantEgressDelete === "n2");
  // The first click only asks for confirmation.
  remove.listeners.click();
  assert.equal(remove.textContent, "确认删除");
  assert.equal(ui.calls.length, 1);
  ui.responses.push({deleted: true}, {accounts: [], egress_servers: []});
  await ui.run("deleteTenantEgressServer(tenantProviders.egress_servers[0])");
  assert.deepEqual(ui.calls[1], {path: "/admin/tenant-providers/egress-servers/n2", method: "DELETE", body: {}});
  assert.equal(ui.calls[2].path, "/admin/tenant-providers");
  assert.match(ui.node("tenant-providers-message").textContent, /已删除 carol 的出口服务器“office”/);
  assert.match(ui.node("tenant-egress-body").textContent, /成员还没有添加出口服务器/);
});
