"use strict";

// Synthetic fixtures only: no real OAuth tokens, keys or egress credentials.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8766";
const output = process.env.SCREENSHOT_DIR || "/tmp/gateway-tenant-provider-screenshots";
const marketplace = {enabled: true, member_suppliers: true, max_accounts_per_user: 5, max_egress_servers_per_user: 2};
const node = {id: "44444444-4444-4444-8444-444444444444", name: "家里的出口", protocol: "ss", server: "203.0.113.20", port: 8388, cipher: "aes-256-gcm",
  username: "", udp: false, has_password: true, revision: 3, bound_accounts: 1};
const accounts = [
  {id: "a000000000000001", provider: "codex", credential_kind: "oauth", alias: "主力 Plus", display_name: "", email_masked: "a***@example.test", plan: "plus",
    status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available", admin_suspended: false, can_manage: true,
    concurrent_limit: 2, allocation_weight: 1, egress_server_id: node.id, egress_server_name: node.name, request_count: 128, equivalent_cost_usd: "3.42"},
  {id: "a000000000000002", provider: "anthropic", credential_kind: "oauth", alias: "", display_name: "", email_masked: "c***@example.test", plan: "unknown",
    status: "unavailable", cliproxy_status: "active", gateway_manual_status: "manual_disabled", gateway_quota_status: "available", admin_suspended: true, can_manage: true,
    concurrent_limit: 1, allocation_weight: 1, egress_server_id: "", egress_server_name: "", request_count: 0, equivalent_cost_usd: "0"},
  {id: "a000000000000003", provider: "codex", credential_kind: "supplier", alias: "备用 API", display_name: "supplier-a000000000000003", email_masked: "",
    plan: "unknown", status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available", admin_suspended: false,
    can_manage: true, concurrent_limit: 4, allocation_weight: 1, egress_server_id: "", egress_server_name: "", supplier_models: ["gpt-5.4", "gpt-5.4-mini"],
    request_count: 12, equivalent_cost_usd: "0.31"},
];
const tenantAccounts = [
  {id: "a000000000000001", provider: "codex", credential_kind: "oauth", owner_user_id: "alice-id", owner_username: "alice", owner_display_name: "Alice",
    display_name: "", alias: "主力 Plus", email_masked: "a***@example.test", plan: "plus", status: "available", admin_suspended: false,
    egress_server_id: node.id, egress_server_name: node.name},
  {id: "b000000000000002", provider: "anthropic", credential_kind: "oauth", owner_user_id: "bob-id", owner_username: "bob", owner_display_name: "Bob",
    display_name: "", alias: "", email_masked: "b***@example.test", plan: "unknown", status: "unavailable", admin_suspended: true,
    egress_server_id: "", egress_server_name: ""},
];

async function dashboard(browser, viewport, user, hash, handler) {
  const page = await browser.newPage({viewport, locale: "zh-CN"});
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.route("**/*", async route => {
    const request = route.request(), url = new URL(request.url());
    assert.equal(url.origin, origin);
    const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
    if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
    if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
    for (const [name, contentType] of [["style.css", "text/css"], ["theme.js", "application/javascript"], ["favicon.svg", "image/svg+xml"]]) {
      if (url.pathname === `/static/${name}`) return send(fs.readFileSync(path.join(assets, name), "utf8"), 200, contentType);
    }
    if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
    const answer = handler(request, url);
    if (answer !== undefined) return send(answer);
    // Other dashboard panels are outside this fixture; the provider routes are not.
    if (url.pathname.startsWith("/admin/me/") || url.pathname.startsWith("/admin/tenant-providers")) {
      throw new Error(`Unexpected fixture request ${request.method()} ${url.pathname}`);
    }
    return send({error: {message: "不在本测试夹具中", code: "fixture_missing"}}, 404);
  });
  await page.goto(`${origin}/#${hash}`);
  await page.evaluate(value => {window.open = () => null; bindUI(); initializeDateFilters(); renderState(value);},
    {user, recently_verified: true, marketplace: user.role === "member" ? marketplace : {enabled: false}, devices: [], projects: [], api_keys: [], passkeys: []});
  return {page, errors};
}

function memberAPI(writes) {
  return (request, url) => {
    if (request.method() !== "GET") writes.push({path: url.pathname, method: request.method(), body: request.postData() ? request.postDataJSON() : null});
    if (url.pathname === "/admin/me/egress-servers") return {servers: [node], version: 3, runtime: {ready: true, applied_version: 3, node_revisions: {[node.id]: 3}}};
    if (url.pathname === "/admin/me/providers") return {...marketplace, member_suppliers: true, account_count: accounts.length, providers: ["codex", "anthropic", "antigravity"],
      until: "2026-11-24T10:00:00Z", all: false, accounts};
    if (url.pathname === "/admin/me/providers/codex/oauth") return {id: "flow", url: "https://auth.openai.com/oauth/authorize?client_id=app&state=synthetic-state-0123", expires_in: 300};
    if (url.pathname === "/admin/me/providers/oauth/flow/status") return {status: "wait"};
    return undefined;
  };
}

(async () => {
  fs.mkdirSync(output, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const member = {id: "alice-id", role: "member", username: "alice", display_name: "Alice"};
    for (const [name, viewport] of [["desktop", {width: 1440, height: 1080}], ["mobile", {width: 390, height: 844}]]) {
      const writes = [];
      const {page, errors} = await dashboard(browser, viewport, member, "providers", memberAPI(writes));
      await page.locator(".provider-card", {hasText: "主力 Plus"}).waitFor();
      assert.equal(await page.locator(".provider-card").count(), 3);
      await page.locator(".provider-card", {hasText: "管理员已停用"}).waitFor();
      await page.getByRole("button", {name: "授权登录", exact: true}).click();
      await page.locator("#provider-oauth").waitFor({state: "visible"});
      assert.deepEqual(writes[0], {path: "/admin/me/providers/codex/oauth", method: "POST", body: {}});
      await page.screenshot({path: path.join(output, `tenant-providers-member-${name}.png`), fullPage: true});
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await page.evaluate(() => {location.hash = "egress-servers";});
      await page.getByRole("heading", {name: node.name}).waitFor();
      assert.equal(await page.locator("#page-title").textContent(), "我的出口");
      await page.locator("#egress-member-note").waitFor({state: "visible"});
      if (name === "desktop") await page.screenshot({path: path.join(output, "tenant-providers-member-egress-desktop.png"), fullPage: true});
      // The Owner's sections stay closed to a member.
      await page.evaluate(() => {location.hash = "tenant-providers";});
      await page.waitForFunction(() => location.hash === "#overview");
      assert.deepEqual(errors, []);
      await page.close();
    }

    const owner = {id: "owner-id", role: "owner", username: "owner", display_name: "管理员"};
    for (const [name, viewport] of [["desktop", {width: 1440, height: 1080}], ["mobile", {width: 390, height: 844}]]) {
      const writes = [];
      const {page, errors} = await dashboard(browser, viewport, owner, "tenant-providers", (request, url) => {
        if (request.method() !== "GET") writes.push({path: url.pathname, method: request.method(), body: request.postDataJSON()});
        if (url.pathname === "/admin/tenant-providers") return {accounts: tenantAccounts, egress_servers: [{...node, owner_user_id: "alice-id", owner_username: "alice", created_at: "2026-11-24T08:00:00Z"}]};
        if (url.pathname === "/admin/billing/users") return {users: [{id: "alice-id", username: "alice", display_name: "Alice", role: "member", status: "active"},
          {id: "bob-id", username: "bob", display_name: "Bob", role: "member", status: "active"}, {id: "owner-id", username: "owner", display_name: "管理员", role: "owner", status: "active"}]};
        return undefined;
      });
      await page.locator("#tenant-providers-body tr", {hasText: "Alice"}).waitFor();
      assert.equal(await page.locator("#tenant-providers-body tr").count(), 2);
      await page.screenshot({path: path.join(output, `tenant-providers-owner-${name}.png`), fullPage: true});
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      if (name === "desktop") {
        await page.locator("#tenant-providers-body tr", {hasText: "Alice"}).getByRole("button", {name: "转移归属"}).click();
        await page.locator("#owner-transfer-dialog").waitFor({state: "visible"});
        await page.locator("#owner-transfer-user-search").click();
        await page.locator("#owner-transfer-user-search-results [role=option]", {hasText: "Bob"}).waitFor();
        assert.equal(await page.locator("#owner-transfer-user-search-results [role=option]").count(), 1);
        await page.screenshot({path: path.join(output, "tenant-providers-owner-transfer.png")});
      }
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log("Tenant providers browser: member list, OAuth start, member egress page, section gating, Owner moderation table and transfer dialog, desktop/mobile passed");
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
