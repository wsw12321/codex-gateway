"use strict";

// Optional real-browser regression. All HTTP traffic is intercepted locally.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const models = ["gpt-alpha", "gpt-beta", "gpt-gamma"];
const users = [
  {user_id: "user-1", username: "alice", display_name: "Alice", role: "owner", status: "active"},
  {user_id: "user-2", username: "bob", display_name: "Bob", role: "member", status: "disabled"},
];
const permissions = new Map(models.flatMap((model) => users.map((user) => [`${model}/${user.user_id}`, model !== "gpt-beta"])));
const defaults = new Map(models.map((model) => [model, model !== "gpt-beta"]));
const initialState = {user: {id: "user-1", username: "alice", display_name: "Alice", role: "owner", status: "active"}, recently_verified: true,
  login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};

async function main() {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  const output = process.env.SCREENSHOT_DIR || "/tmp/gateway-protocol-conversion-screenshots";
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1000}, locale: "zh-CN", deviceScaleFactor: 2});
    const errors = [], writes = [];
    let settings = {claude_responses_enabled: false, revision: 1, updated_at: "2026-11-09T08:00:00Z"};
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("dialog", (dialog) => dialog.accept());
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      for (const [name, contentType] of [["style.css", "text/css"], ["theme.js", "application/javascript"], ["favicon.svg", "image/svg+xml"]]) {
        if (url.pathname === `/static/${name}`) return send(fs.readFileSync(path.join(assets, name), "utf8"), 200, contentType);
      }
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/model-catalog") return send({models: models.map((id) => ({id, group: "GPT"}))});
      if (url.pathname === "/admin/model-access/models") return send({models: models.map((model) => ({model, default_enabled: defaults.get(model), enabled_user_count: 1, disabled_user_count: 1}))});
      if (url.pathname === "/admin/model-access/users") return send({models: url.searchParams.getAll("models"), users: []});
      if (url.pathname === "/admin/protocol-conversion" && request.method() === "GET") return send(settings);
      if (url.pathname === "/admin/protocol-conversion" && request.method() === "PUT") {
        const body = request.postDataJSON();
        writes.push(body);
        assert.equal(body.revision, settings.revision);
        settings = {claude_responses_enabled: body.claude_responses_enabled, revision: settings.revision + 1, updated_at: "2026-11-09T09:30:00Z"};
        return send(settings);
      }
      throw new Error(`Unexpected request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#model-access");
    await page.evaluate(async (value) => { bindUI(); initializeDateFilters(); renderState(value); await loadModelAccess(); }, initialState);
    const card = page.locator(".protocol-conversion");
    await card.locator("#protocol-conversion-status", {hasText: "已禁用"}).waitFor();
    fs.mkdirSync(output, {recursive: true});
    await card.screenshot({path: path.join(output, "protocol-conversion-disabled.png")});
    await card.getByRole("button", {name: "开启转换"}).click();
    await card.locator("#protocol-conversion-status", {hasText: "已开启"}).waitFor();
    await card.screenshot({path: path.join(output, "protocol-conversion-enabled.png")});
    await card.getByRole("button", {name: "禁用转换"}).click();
    await card.locator("#protocol-conversion-status", {hasText: "已禁用"}).waitFor();
    assert.deepEqual(writes, [{claude_responses_enabled: true, revision: 1}, {claude_responses_enabled: false, revision: 2}]);
    await page.setViewportSize({width: 390, height: 844});
    await card.scrollIntoViewIfNeeded();
    await card.screenshot({path: path.join(output, "protocol-conversion-mobile.png")});
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "mobile should not overflow viewport");
    assert.deepEqual(errors, []);
    console.log("Protocol conversion browser: default disabled, enable, confirmed disable, revisions, desktop/mobile passed");
  } finally { await browser.close(); }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
