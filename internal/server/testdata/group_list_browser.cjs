"use strict";

// Optional browser regression for the group list filters and rename dialog.
// Every request is intercepted; groups and users are synthetic.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots/group-management");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
const initialState = {user: {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"},
  recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const base = {period: "month", custom_days: 0, limit_usd: "100.000000000000", used_usd: "20.000000000000", remaining_usd: "80.000000000000",
  member_limit_usd: null, member_count: 0, period_count: 0, current_period_number: 1, expires_at: null, model_ids: [],
  starts_at: "2026-09-01T08:00:00Z", period_starts_at: "2026-09-01T08:00:00Z", period_ends_at: "2026-10-02T08:00:00Z"};
const groups = [
  {...base, id: "design", name: "产品设计团队", pinyin_full: "chanpinshejituandui", pinyin_initials: "cpsjtd", member_count: 2},
  {...base, id: "research", name: "研发效能组", pinyin_full: "yanfaxiaonengzu", pinyin_initials: "yfxnz"},
  {...base, id: "ops", name: "Ops On-call", pinyin_full: "opson-call", pinyin_initials: "opsoncall"},
  {...base, id: "legacy", name: "旧版试点团队", pinyin_full: "jiubanshidiantuandui", pinyin_initials: "jbsdtd", archived_at: "2026-09-10T08:00:00Z"},
  {...base, id: "pilot", name: "春季活动组", pinyin_full: "chunjihuodongzu", pinyin_initials: "cjhdz", archived_at: "2026-09-15T08:00:00Z"},
];
const users = [{id: "alice", username: "alice", display_name: "Alice", group_ids: ["design"]}, {id: "bob", username: "bob", display_name: "Bob", group_ids: ["design"]}];
const detail = (group) => ({...group, members: users.filter((user) => user.group_ids.includes(group.id)).map((user) => ({user_id: user.id, username: user.username, display_name: user.display_name, used_usd: "10", remaining_usd: null}))});
const pool = (id) => ({group_id: id, providers: ["codex", "antigravity", "anthropic"].map((provider) => ({group_id: id, provider, strategy: "all", mode: "all", account_ids: [], accounts: []}))});

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1000}, locale: "zh-CN", colorScheme: process.env.BROWSER_COLOR_SCHEME || "light"});
    await page.addInitScript(() => { Date.now = () => new Date("2026-09-21T08:00:00Z").getTime(); });
    const errors = [], writes = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => { if (message.type() === "error") errors.push(message.text()); });
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, origin);
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      const files = {"/": ["index.html", "text/html"], "/static/style.css": ["style.css", "text/css"], "/static/theme.js": ["theme.js", "application/javascript"], "/static/favicon.svg": ["favicon.svg", "image/svg+xml"]};
      if (files[url.pathname]) return send(fs.readFileSync(path.join(assets, files[url.pathname][0]), "utf8"), 200, files[url.pathname][1]);
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/billing/users") return send({users});
      if (url.pathname === "/admin/model-catalog") return send({models: [{id: "gpt-5.6"}]});
      if (url.pathname === "/admin/groups" && request.method() === "GET") return send({groups});
      const match = url.pathname.match(/^\/admin\/groups\/([^/]+)(\/.*)?$/);
      if (match) {
        const group = groups.find((item) => item.id === match[1]);
        if (match[2] === "/upstream-access") return send(pool(group.id));
        if (match[2] === "/periods") return send({periods: []});
        if (match[2] === "/name" && request.method() === "PUT") {
          const body = request.postDataJSON(); writes.push({path: url.pathname, body});
          if (group.archived_at) return send({error: {code: "group_operation_conflict", message: "群组已归档"}}, 409);
          group.name = body.name; group.pinyin_full = ""; group.pinyin_initials = "";
          return send(detail(group));
        }
        if (!match[2] && request.method() === "GET") return send(detail(group));
      }
      throw new Error(`Unexpected fixture request: ${request.method()} ${url.pathname}`);
    });
    const names = () => page.locator("#group-list .group-select strong").allTextContents();
    const noOverflow = async (label) => assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, label);

    await page.goto(`${origin}/#groups`);
    await page.evaluate(async (value) => { bindUI(); initializeDateFilters(); renderState(value); await loadGroups(); }, initialState);
    assert.deepEqual(await names(), ["产品设计团队", "研发效能组", "Ops On-call"], "archived groups are hidden by default");
    assert.equal(await page.locator("#group-show-archived").isChecked(), false);
    assert.equal(await page.locator("#group-list-status").textContent(), "显示 3 / 5 个群组 · 已隐藏 2 个已归档");
    assert.equal(await page.locator("#group-detail-name").textContent(), "产品设计团队");
    await noOverflow("desktop default list");
    await page.screenshot({path: path.join(screenshots, "group-list-desktop.png"), fullPage: true});

    await page.getByLabel("显示已归档群组").check();
    assert.deepEqual(await names(), ["产品设计团队", "研发效能组", "Ops On-call", "旧版试点团队", "春季活动组"]);
    assert.equal(await page.locator('#group-list [data-archived="true"]').count(), 2);
    await page.locator("#group-search").fill("cj");
    assert.deepEqual(await names(), ["春季活动组"], "initials find archived groups once shown");
    await page.locator('#group-list .group-select').first().click();
    await page.waitForFunction(() => managedGroup?.id === "pilot");
    assert.equal(await page.locator("#group-rename").isDisabled(), true, "archived groups cannot be renamed");
    assert.equal(await page.locator("#group-edit").isDisabled(), true);
    await page.screenshot({path: path.join(screenshots, "group-list-archived-desktop.png"), fullPage: true});

    await page.locator("#group-search").fill("");
    await page.getByLabel("显示已归档群组").uncheck();
    await page.waitForFunction(() => managedGroup?.id === "design");
    assert.deepEqual(await names(), ["产品设计团队", "研发效能组", "Ops On-call"]);
    await page.locator("#group-search").fill("yanfa");
    assert.deepEqual(await names(), ["研发效能组"], "full pinyin matches");
    await page.locator("#group-search").fill("ops on");
    assert.deepEqual(await names(), ["Ops On-call"], "every term must match");
    await page.locator("#group-search").fill("不存在");
    assert.match(await page.locator("#group-list").textContent(), /没有名称匹配“不存在”的未归档群组/);
    await page.locator("#group-search").fill("");

    await page.locator("#group-rename").click();
    const dialog = page.locator("#group-rename-dialog");
    assert.equal(await dialog.isVisible(), true);
    assert.equal(await dialog.locator('input[name="name"]').inputValue(), "产品设计团队");
    assert.match(await page.locator("#group-rename-current").textContent(), /当前名称：产品设计团队/);
    await dialog.locator('input[name="name"]').fill("  设计与体验团队  ");
    await dialog.locator('input[name="reason"]').fill("部门调整");
    await page.screenshot({path: path.join(screenshots, "group-rename-desktop.png")});
    await dialog.locator('button[type="submit"]').click();
    await page.waitForFunction(() => !byId("group-rename-dialog").open && managedGroup?.name === "设计与体验团队");
    assert.equal(writes.length, 1);
    assert.equal(writes[0].path, "/admin/groups/design/name");
    assert.deepEqual(Object.keys(writes[0].body).sort(), ["name", "operation_id", "reason"]);
    assert.equal(writes[0].body.name, "设计与体验团队");
    assert.match(await page.locator("#groups-message").textContent(), /群组已重命名为“设计与体验团队”/);
    assert.equal(await page.locator("#group-detail-name").textContent(), "设计与体验团队");
    assert.deepEqual(await names(), ["设计与体验团队", "研发效能组", "Ops On-call"]);

    await page.locator("#group-rename").click();
    await dialog.locator('input[name="reason"]').fill("无变化");
    await dialog.locator('button[type="submit"]').click();
    assert.match(await dialog.locator(".form-message").textContent(), /新名称与当前名称相同/);
    assert.equal(writes.length, 1, "unchanged names are not sent");
    await dialog.locator("[data-close]").click();

    for (const width of [390, 320]) {
      await page.setViewportSize({width, height: 844});
      await noOverflow(`${width} group list`);
    }
    await page.setViewportSize({width: 390, height: 844});
    await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); });
    await page.screenshot({path: path.join(screenshots, "group-list-mobile.png")});
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: browser.version(), checks: "archived hidden by default, archived toggle and reselection, name/pinyin/initials search, rename dialog payload and refresh, unchanged and archived rename guards, 320/390/1440 overflow", screenshots}));
  } finally {
    await browser.close();
  }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
