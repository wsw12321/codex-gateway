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
  {id: "member-3", username: "wangwu", display_name: "王五", pinyin_full: "wangwu", pinyin_initials: "ww", role: "member", status: "pending"},
  initialState.user,
];
const providers = [
  {provider: "anthropic", accounts: [{id: "h1", display_name: "Claude 团队账号", email_masked: "cl***@example.test", status: "available"}]},
  {provider: "codex", accounts: [
    {id: "c1", display_name: "团队主账号", email_masked: "al***@example.test", status: "available"},
    {id: "c2", display_name: "团队备用账号", email_masked: "be***@example.test", status: "unavailable"},
    {id: "c3", display_name: "新登记账号", email_masked: "ne***@example.test", status: "available"},
  ]},
  {provider: "antigravity", sync_warning: "upstream_account_sync_unavailable", accounts: [
    {id: "a1", display_name: "Gemini 团队账号", email_masked: "ge***@example.test", status: "available"},
    {id: "a2", display_name: "Gemini 备用账号", email_masked: "ba***@example.test", status: "unknown"},
  ]},
];
const rules = [{user_id: "member-2", provider: "codex", mode: "selected", account_ids: ["c2"]}];

// A minimal model of the server's batch semantics.
function applyBatch(body) {
  const owners = new Map(providers.flatMap((row) => row.accounts.map((account) => [account.id, row.provider])));
  const targets = body.action === "all" ? body.providers : [...new Set(body.account_ids.map((id) => owners.get(id)))].sort();
  const access = [];
  let changed = 0;
  for (const userID of body.user_ids) {
    for (const provider of targets) {
      const index = rules.findIndex((rule) => rule.user_id === userID && rule.provider === provider);
      const previous = index >= 0 ? rules[index] : {user_id: userID, provider, mode: "all", account_ids: []};
      const chosen = body.account_ids.filter((id) => owners.get(id) === provider);
      const registered = providers.find((row) => row.provider === provider).accounts.map((account) => account.id);
      let next = {user_id: userID, provider, mode: "selected", account_ids: []};
      if (body.action === "all" || body.action === "enable" && previous.mode === "all") next.mode = "all";
      else if (body.action === "enable") next.account_ids = [...new Set([...previous.account_ids, ...chosen])].sort();
      else next.account_ids = (previous.mode === "all" ? registered : previous.account_ids).filter((id) => !chosen.includes(id)).sort();
      if (next.mode !== previous.mode || next.account_ids.join() !== previous.account_ids.join()) {
        changed++;
        if (index >= 0) rules[index] = next; else rules.push(next);
      } else next = previous;
      access.push(next);
    }
  }
  return {access, target_count: access.length, changed_count: changed};
}

async function main() {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [], events = [], dialogs = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("dialog", (dialog) => { dialogs.push(dialog.message()); return dialog.accept(); });
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
      if (url.pathname === "/admin/upstream-access/users" && request.method() === "GET") return send({providers, rules});
      if (url.pathname === "/admin/upstream-access/users" && request.method() === "PUT") {
        const body = request.postDataJSON();
        writes.push(body); events.push("write");
        return send(applyBatch(body));
      }
      throw new Error(`Unexpected request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#user-upstream-access");
    await page.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, initialState);
    await page.waitForFunction(() => userUpstreamReady && !userUpstreamLoading);
    const userRow = (id) => page.locator(`.user-upstream-user[data-user-id="${id}"]`);
    const accountRow = (id) => page.locator(`.user-upstream-account[data-account-id="${id}"]`);
    assert.equal(await userRow("member-3").count(), 0, "pending users are not listed");
    assert.match(await userRow("member-2").textContent(), /Codex 1\/3/);
    assert.match(await page.locator('.user-upstream-account-group[data-provider="antigravity"]').textContent(), /同步失败.*本地快照/s);

    // Pinyin search keeps earlier selections.
    await page.locator("#user-upstream-user-search").fill("zs");
    assert.equal(await page.locator(".user-upstream-user").count(), 1);
    await userRow("member-1").locator("input").check();
    await page.locator("#user-upstream-user-search").fill("");
    await userRow("member-2").locator("input").check();
    assert.equal(await page.locator("#user-upstream-user-count").textContent(), "已选 2 人");
    assert.equal(await userRow("member-1").locator("input").isChecked(), true);

    // Enabling only extends selected scopes and needs no confirmation.
    await accountRow("c3").locator("input").check();
    assert.match(await page.locator("#user-upstream-preview").textContent(), /将修改 1 项设置（1 人）；1 项已是目标状态，将跳过。/);
    await page.locator("#user-upstream-reason").fill("为李四补充新登记账号");
    await page.locator("#user-upstream-submit").click();
    await page.waitForFunction(() => !userUpstreamSaving);
    assert.deepEqual(writes[0], {user_ids: ["member-1", "member-2"], action: "enable", account_ids: ["c3"], providers: [], reason: "为李四补充新登记账号"});
    assert.equal(dialogs.length, 0);

    // Disabling across providers converts all-account users after confirming,
    // and re-verification locks the editor until the single write completes.
    await accountRow("c3").locator("input").uncheck();
    await page.locator("#user-upstream-account-search").fill("ge***");
    assert.equal(await accountRow("c1").count(), 0);
    await accountRow("a1").locator("input").check();
    await page.locator("#user-upstream-account-search").fill("");
    await accountRow("c1").locator("input").check();
    await page.locator("#user-upstream-action").selectOption("disable");
    assert.match(await page.locator("#user-upstream-preview").textContent(), /其中 3 项将从「全部账号」改为「仅指定账号」/);
    assert.equal(await page.locator("#user-upstream-matrix .user-upstream-cell").count(), 4);
    await page.locator("#user-upstream-reason").fill("停用团队主账号与 Gemini 主账号");
    await page.evaluate(() => { state.recently_verified = false; });
    await page.locator("#user-upstream-submit").click();
    await page.locator("#reauth-dialog").waitFor({state: "visible"});
    assert.match(dialogs[0], /今后新增账号不会自动加入/);
    assert.equal(await page.locator("#user-upstream-submit").isDisabled(), true);
    assert.equal(await userRow("member-1").locator("input").isDisabled(), true);
    await page.locator('#reauth-form input[name="password"]').fill("synthetic-test-password");
    await page.locator('#reauth-form button[type="submit"]').click();
    await page.waitForFunction(() => !userUpstreamSaving);
    assert.deepEqual(events, ["write", "verified", "write"]);
    assert.deepEqual(writes[1], {user_ids: ["member-1", "member-2"], action: "disable", account_ids: ["a1", "c1"], providers: [], reason: "停用团队主账号与 Gemini 主账号"});
    assert.deepEqual(await page.locator("#user-upstream-matrix .user-upstream-cell").allTextContents(), ["禁用", "禁用", "禁用", "禁用"]);
    assert.match(await userRow("member-1").textContent(), /Codex 2\/3 · Antigravity 1\/2/);
    // Show a mixed matrix in the screenshots.
    await accountRow("c2").locator("input").check();
    await accountRow("h1").locator("input").check();
    await page.locator("#user-upstream-action").selectOption("enable");

    fs.mkdirSync(screenshots, {recursive: true});
    await page.evaluate(() => {
      hide("notice");
      for (const id of ["user-upstream-user-list", "user-upstream-account-list"]) byId(id).scrollTop = 0;
      window.scrollTo(0, 0);
    });
    await page.screenshot({path: path.join(screenshots, "user-upstream-access-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.evaluate(() => window.scrollTo(0, 0));
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile must not overflow horizontally");
    await page.screenshot({path: path.join(screenshots, "user-upstream-access-mobile.png"), fullPage: true});
    await page.evaluate(() => { resetUserUpstreamAccess(); identityGeneration++; state.user.role = "member"; loadOverview = () => {}; routeFromHash(false); });
    assert.equal(await page.locator('[data-section="user-upstream-access"]').isVisible(), false);
    assert.equal(await page.evaluate(() => userUpstreamSelectedUsers.size + userUpstreamSelectedAccounts.size), 0);
    assert.deepEqual(errors, []);
    console.log("User account access batch browser validation passed; desktop/mobile screenshots saved.");
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
