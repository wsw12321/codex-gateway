"use strict";

// All API requests use synthetic fixtures. No real SS or OAuth credentials.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
const output = process.env.SCREENSHOT_DIR || "/tmp/gateway-egress-screenshots";

(async () => {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1000}, locale: "zh-CN"});
    const errors = [], writes = [];
    let servers = [], version = 1, binding = null;
    const id = "11111111-1111-4111-8111-111111111111";
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
      if (request.method() !== "GET") writes.push({path: url.pathname, method: request.method(), body: request.postData() ? request.postDataJSON() : null});
      const runtime = () => ({ready: true, applied_version: version, node_revisions: Object.fromEntries(servers.map(node => [node.id, node.revision]))});
      if (url.pathname === "/admin/egress-servers" && request.method() === "GET") return send({servers, version, runtime: runtime()});
      if (url.pathname === "/admin/egress-servers" && request.method() === "POST") {
        const {password, ...node} = request.postDataJSON();
        assert.equal(password, '  synthetic$"\\password  ');
        servers.push({...node, id, revision: ++version, has_password: true});
        return send({server: servers[0], runtime: runtime()});
      }
      if (url.pathname === `/admin/egress-servers/${id}` && request.method() === "PUT") {
        const node = request.postDataJSON(); assert.equal(Object.hasOwn(node, "password"), false);
        servers[0] = {...servers[0], ...node, revision: ++version};
        return send({server: servers[0], runtime: runtime()});
      }
      if (url.pathname === `/admin/egress-servers/${id}` && request.method() === "DELETE") {
        assert.equal(binding, null); servers = []; version++; return send({deleted: true});
      }
      if (url.pathname === "/admin/upstream-accounts") return send({all: true, accounts: [{
        id: "0123456789abcdef", email_masked: "te***@example.test", plan: "Plus", status: "available", can_manage: true,
        cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available", allocation_weight: 1,
        egress_server_id: binding, concurrent_limit: 1, rolling_cost_usd: "0", rolling_cost_share: "0", target_share: "1",
      }]});
      if (url.pathname === "/admin/upstream-accounts/concurrency") return send({sampled_at: new Date().toISOString(), accounts: [{id: "0123456789abcdef", active_requests: 0}]});
      if (url.pathname === "/admin/upstream-accounts/0123456789abcdef/egress") { binding = request.postDataJSON().egress_server_id; return send({id: "0123456789abcdef", egress_server_id: binding}); }
      throw new Error(`Unexpected fixture request ${request.method()} ${url.pathname}`);
    });
    await page.goto(origin + "/#egress-servers");
    await page.evaluate(value => {bindUI(); initializeDateFilters(); renderState(value);}, {user: {id: "owner", role: "owner", display_name: "管理员"}, recently_verified: true, devices: [], projects: [], api_keys: [], passkeys: []});
    await page.getByRole("button", {name: "新增节点", exact: true}).click();
    const dialog = page.locator("#egress-dialog");
    await dialog.getByLabel("节点名称").fill("华东出口");
    await dialog.getByLabel("服务器地址").fill("ss.example.test");
    await dialog.getByLabel("端口", {exact: true}).fill("8388");
    await dialog.getByLabel("加密方式").selectOption("aes-256-gcm");
    await dialog.locator('input[name="password"]').fill('  synthetic$"\\password  ');
    await dialog.getByLabel("允许 UDP").check();
    await dialog.getByRole("button", {name: "保存节点"}).click();
    await page.getByRole("heading", {name: "华东出口"}).waitFor();
    assert.equal(await dialog.locator('input[name="password"]').inputValue(), "");
    await page.getByRole("button", {name: "编辑", exact: true}).click();
    assert.equal(await dialog.locator('input[name="password"]').inputValue(), "");
    await dialog.getByLabel("节点名称").fill("华东主出口");
    await dialog.getByRole("button", {name: "保存节点"}).click();
    await page.getByRole("heading", {name: "华东主出口"}).waitFor();
    fs.mkdirSync(output, {recursive: true});
    await page.screenshot({path: path.join(output, "egress-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.screenshot({path: path.join(output, "egress-mobile.png"), fullPage: true});
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await page.setViewportSize({width: 1440, height: 1000});
    await page.evaluate(() => {location.hash = "upstream-accounts";});
    const card = page.locator('[data-account-id="0123456789abcdef"]');
    await card.locator("summary").click();
    await card.getByLabel("出口服务器", {exact: true}).selectOption(id);
    await card.getByRole("button", {name: "保存出口", exact: true}).click();
    await page.waitForFunction(() => !upstreamAccountOperation);
    assert.equal(binding, id);
    await card.getByLabel("出口服务器", {exact: true}).selectOption("");
    await card.getByRole("button", {name: "保存出口", exact: true}).click();
    await page.waitForFunction(() => !upstreamAccountOperation);
    assert.equal(binding, null);
    await page.evaluate(() => {location.hash = "egress-servers";});
    await page.getByRole("button", {name: "删除", exact: true}).click();
    await page.getByRole("button", {name: "确认删除", exact: true}).click();
    await page.getByText("尚未配置出口服务器。新增 SS 节点后，可在上游账号设置中绑定。").waitFor();
    assert.deepEqual(errors, []);
    assert.equal(writes.filter(write => write.path.endsWith("/egress")).length, 2);
    console.log("Egress browser: CRUD, secret clearing, per-account binding, unbind, desktop/mobile passed");
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
