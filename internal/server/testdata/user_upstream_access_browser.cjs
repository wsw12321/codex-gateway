"use strict";

// Optional browser validation; all traffic is intercepted at a synthetic origin.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const initialState = {user: {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"},
  recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const users = [
  {id: "member-1", username: "zhangsan", display_name: "张三", pinyin_full: "zhangsan", pinyin_initials: "zs", role: "member", status: "active"},
  {id: "member-2", username: "lisi", display_name: "李四", pinyin_full: "lisi", pinyin_initials: "ls", role: "member", status: "active"},
  initialState.user,
];
const providers = [
  {provider: "codex", mode: "all", account_ids: [], accounts: [
    {id: "c1", display_name: "团队主账号", email_masked: "al***@example.test", status: "available"},
    {id: "c2", display_name: "团队备用账号", email_masked: "be***@example.test", status: "unavailable"},
    {id: "c3", display_name: "新登记账号", email_masked: "ne***@example.test", status: "available"},
  ]},
  {provider: "antigravity", mode: "selected", account_ids: [], sync_warning: "upstream_account_sync_unavailable", accounts: [
    {id: "a1", display_name: "Gemini 团队账号", email_masked: "ge***@example.test", status: "available"},
    {id: "a2", display_name: "Gemini 备用账号", email_masked: "ba***@example.test", status: "unknown"},
  ]},
];

async function main() {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [], events = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765");
      const send = (body, contentType = "application/json") => route.fulfill({status: 200, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), "text/html");
      const staticFiles = {"/static/app.js": [app, "application/javascript"],
        "/static/style.css": [fs.readFileSync(path.join(assets, "style.css"), "utf8"), "text/css"],
        "/static/theme.js": [fs.readFileSync(path.join(assets, "theme.js"), "utf8"), "application/javascript"],
        "/static/favicon.svg": [fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), "image/svg+xml"]};
      if (staticFiles[url.pathname]) return send(...staticFiles[url.pathname]);
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/billing/users") return send({users});
      if (url.pathname === "/auth/password/reauth") { events.push("verified"); return send({ok: true}); }
      const match = url.pathname.match(/^\/admin\/users\/([^/]+)\/upstream-access(?:\/(codex|antigravity))?$/);
      if (match && request.method() === "GET") return send({user_id: match[1], providers});
      if (match && request.method() === "PUT") {
        const body = request.postDataJSON(), provider = providers.find((row) => row.provider === match[2]);
        writes.push({path: url.pathname, body}); events.push("write");
        Object.assign(provider, {mode: body.mode, account_ids: body.account_ids});
        return send({user_id: match[1], provider: match[2], mode: body.mode, account_ids: body.account_ids});
      }
      throw new Error(`Unexpected request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#user-upstream-access");
    await page.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, initialState);
    const picker = page.locator("#user-upstream-user-search");
    await picker.waitFor({state: "visible"});
    await page.waitForFunction(() => userUpstreamUsersReady);
    await picker.fill("zs");
    assert.equal(await page.locator('#user-upstream-user-search-results [role="option"]').count(), 1);
    await page.locator('#user-upstream-user-search-results [role="option"]').click();
    await page.waitForFunction(() => !userUpstreamLoading && userUpstreamProviders.length === 2);
    const card = (provider) => page.locator(`.user-upstream-card[data-provider="${provider}"]`);
    const codex = card("codex"), agy = card("antigravity");
    assert.match(await page.locator("#user-upstream-target").textContent(), /张三/);
    assert.match(await agy.textContent(), /同步失败.*本地快照/s);
    assert.match(await agy.locator(".user-upstream-selection").textContent(), /该类型全部禁用/);
    await codex.locator("select").selectOption("selected");
    await codex.locator('input[value="c1"]').check();
    await codex.locator('input[value="c2"]').check();
    await codex.locator('input[name="reason"]').fill("为团队成员分配固定 Codex 账号");
    await codex.locator('input[type="search"]').fill("be***");
    assert.equal(await codex.locator('[data-account-id="c1"]').isVisible(), false);
    await codex.locator('input[type="search"]').fill("");
    assert.equal(await codex.locator('input[value="c1"]').isChecked(), true);
    await agy.locator('input[name="reason"]').fill("暂时禁用 Gemini 请求");
    await agy.locator('button[type="submit"]').click();
    await page.waitForFunction(() => userUpstreamOperations.size === 0);
    assert.deepEqual(writes[0].body, {mode: "selected", account_ids: [], reason: "暂时禁用 Gemini 请求"});
    assert.equal(await codex.locator('input[name="reason"]').inputValue(), "为团队成员分配固定 Codex 账号");
    await page.evaluate(() => { state.recently_verified = false; });
    await codex.locator('button[type="submit"]').click();
    await page.locator("#reauth-dialog").waitFor({state: "visible"});
    assert.equal(await picker.isDisabled(), true);
    assert.equal(await codex.locator("select").isDisabled(), true);
    assert.equal(await agy.locator("select").isDisabled(), false);
    await page.locator('#reauth-form input[name="password"]').fill("synthetic-test-password");
    await page.locator('#reauth-form button[type="submit"]').click();
    await page.waitForFunction(() => userUpstreamOperations.size === 0);
    assert.deepEqual(events, ["write", "verified", "write"]);
    assert.deepEqual(writes[1].body.account_ids, ["c1", "c2"]);
    assert.match(writes[1].path, /\/member-1\/upstream-access\/codex$/);
    fs.mkdirSync(screenshots, {recursive: true});
    await page.evaluate(() => { hide("notice"); window.scrollTo(0, 0); });
    await page.screenshot({path: path.join(screenshots, "user-upstream-access-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.evaluate(() => window.scrollTo(0, 0));
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile must not overflow horizontally");
    await page.screenshot({path: path.join(screenshots, "user-upstream-access-mobile.png"), fullPage: true});
    await page.evaluate(() => { resetUserUpstreamAccess(); identityGeneration++; state.user.role = "member"; loadOverview = () => {}; routeFromHash(false); });
    assert.equal(await page.locator('[data-section="user-upstream-access"]').isVisible(), false);
    assert.equal(await page.locator("#user-upstream-target").textContent(), "尚未选择用户");
    assert.deepEqual(errors, []);
    console.log("User account access browser validation passed; desktop/mobile screenshots saved.");
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
