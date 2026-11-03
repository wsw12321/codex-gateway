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
const accounts = [
  {id: "0123456789abcdef", alias: "主力 Plus", email_masked: "al***@example.test", plan: "Plus", allocation_weight: 5, rolling_cost_usd: "12.345678"},
  {id: "1123456789abcdef", alias: "", email_masked: "be***@example.test", plan: "Pro", allocation_weight: 20, rolling_cost_usd: "37.037034"},
  {id: "2123456789abcdef", alias: "", display_name: "team-slot-3", email_masked: "ga***@example.test", plan: "Plus", allocation_weight: 1, rolling_cost_usd: "0"},
].map((account) => ({status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available",
  can_manage: true, concurrent_limit: 1, rolling_cost_share: "0.3", target_share: "0.3", request_count: 12, equivalent_cost_usd: "3.4",
  last_synced_at: "2026-11-03T08:00:00Z", ...account}));
const users = [
  {id: "member-1", username: "zhangsan", display_name: "张三", role: "member", status: "active"},
  {id: "member-2", username: "lisi", display_name: "李四", role: "member", status: "active"},
  initialState.user,
];

async function serve(page, writes) {
  await page.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url());
    assert.equal(url.origin, "http://127.0.0.1:8765", "all traffic must stay on the mocked origin");
    const send = (body, contentType = "application/json") => route.fulfill({status: 200, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
    if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), "text/html");
    const staticFiles = {"/static/app.js": [app, "application/javascript"],
      "/static/style.css": [fs.readFileSync(path.join(assets, "style.css"), "utf8"), "text/css"],
      "/static/theme.js": [fs.readFileSync(path.join(assets, "theme.js"), "utf8"), "application/javascript"],
      "/static/favicon.svg": [fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), "image/svg+xml"]};
    if (staticFiles[url.pathname]) return send(...staticFiles[url.pathname]);
    if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
    if (url.pathname === "/admin/egress-servers") return send({servers: [], version: 1, runtime: {ready: true, applied_version: 1}});
    if (url.pathname === "/admin/upstream-accounts/concurrency") return send({sampled_at: new Date().toISOString(), accounts: accounts.map((account) => ({id: account.id, active_requests: 0}))});
    if (url.pathname === "/admin/upstream-accounts") return send({all: true, until: "2026-11-03T08:00:00Z", allocation_from: "2026-11-02T08:00:00Z", allocation_until: "2026-11-03T08:00:00Z", accounts});
    const alias = url.pathname.match(/^\/admin\/upstream-accounts\/([0-9a-f]{16})\/alias$/);
    if (alias && request.method() === "PUT") {
      const body = request.postDataJSON(), account = accounts.find((item) => item.id === alias[1]);
      assert.ok(account);
      writes.push({id: account.id, body});
      account.alias = body.alias;
      return send({id: account.id, alias: account.alias});
    }
    if (url.pathname === "/admin/billing/users") return send({users});
    if (url.pathname === "/admin/upstream-access/users") return send({providers: [
      {provider: "codex", accounts: accounts.map(({id, alias, display_name, email_masked, status}) => ({id, alias, display_name: display_name || "", email_masked, status}))},
      {provider: "antigravity", accounts: []}, {provider: "anthropic", accounts: []},
    ], rules: []});
    throw new Error(`Unexpected mocked request: ${request.method()} ${url.pathname}`);
  });
}

async function capture(page, file) {
  await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); });
  await page.screenshot({path: path.join(screenshots, file), fullPage: true});
}

async function main() {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await serve(page, writes);
    await page.goto("http://127.0.0.1:8765/#upstream-accounts");
    await page.evaluate(async (value) => {
      bindUI(); initializeDateFilters(); renderState(value);
      byId("upstream-account-filter").elements.range.value = "all"; syncUpstreamAccountRange();
      await loadUpstreamAccounts(upstreamAccountQueryFromForm()); setConnection("已连接", "ok");
    }, initialState);
    const card = (id) => page.locator(`.upstream-account-card[data-account-id="${id}"]`);
    const settled = () => page.waitForFunction(() => !upstreamAccountOperation && !upstreamAccountListLoading);
    assert.equal(await card(accounts[0].id).locator(".upstream-account-identity h3").textContent(), "主力 Plus");
    assert.equal(await card(accounts[0].id).locator(".upstream-account-identity p").textContent(), "al***@example.test");

    // Unsafe aliases never leave the browser.
    await card(accounts[1].id).locator(".upstream-account-details > summary").click();
    const input = card(accounts[1].id).locator(".upstream-alias-input");
    await input.fill("be@example.test");
    await card(accounts[1].id).locator(".upstream-alias-save").click();
    assert.equal(await input.getAttribute("aria-invalid"), "true");
    assert.equal(writes.length, 0);

    await input.fill("  备用 Pro  ");
    await card(accounts[1].id).locator(".upstream-alias-save").click();
    await settled();
    assert.deepEqual(writes, [{id: accounts[1].id, body: {alias: "备用 Pro"}}]);
    assert.equal(await card(accounts[1].id).locator(".upstream-account-identity h3").textContent(), "备用 Pro");
    assert.equal(await page.locator("#upstream-account-action-message").textContent(), "别名已保存：备用 Pro（be***@example.test）。");
    assert.equal(await card(accounts[1].id).locator(".upstream-account-details").getAttribute("open"), "", "the edited card stays expanded after refresh");

    await page.locator("#upstream-account-search").fill("备用");
    assert.equal(await card(accounts[0].id).isVisible(), false);
    assert.equal(await card(accounts[1].id).isVisible(), true);
    await page.locator("#upstream-account-search").fill("");
    fs.mkdirSync(screenshots, {recursive: true});
    await capture(page, "upstream-account-alias-desktop.png");

    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "no horizontal scroll at phone width");
    // Keep the card heading clear of the sticky mobile top bar.
    await card(accounts[1].id).evaluate((node) => {
      document.activeElement?.blur();
      window.scrollTo(0, node.getBoundingClientRect().top + window.scrollY - document.querySelector(".workspace-header").getBoundingClientRect().height - 12);
    });
    await page.screenshot({path: path.join(screenshots, "upstream-account-alias-mobile.png")});

    // Pickers show the alias beside the label they showed before.
    const picker = await context.newPage();
    picker.on("pageerror", (error) => errors.push(error.message));
    await serve(picker, writes);
    await picker.setViewportSize({width: 1440, height: 1080});
    await picker.goto("http://127.0.0.1:8765/#user-upstream-access");
    await picker.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, initialState);
    await picker.waitForFunction(() => userUpstreamReady && !userUpstreamLoading);
    const names = await picker.locator(".user-upstream-account strong").allTextContents();
    assert.deepEqual(names, ["主力 Plus（al***@example.test）", "备用 Pro（be***@example.test）", "team-slot-3"]);
    await picker.locator("#user-upstream-account-search").fill("主力");
    assert.equal(await picker.locator(".user-upstream-account").count(), 1);
    await picker.locator("#user-upstream-account-search").fill("");
    await picker.locator('.user-upstream-user[data-user-id="member-1"] input').check();
    await picker.locator(`.user-upstream-account[data-account-id="${accounts[0].id}"] input`).check();
    await capture(picker, "upstream-account-alias-picker-desktop.png");

    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
