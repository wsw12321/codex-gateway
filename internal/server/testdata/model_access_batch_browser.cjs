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
  const browser = await chromium.launch({headless: true});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [];
    let failWrite = false;
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/model-access/models") return send({models: models.map((model) => ({model, default_enabled: defaults.get(model), enabled_user_count: users.filter((user) => permissions.get(`${model}/${user.user_id}`)).length, disabled_user_count: users.filter((user) => !permissions.get(`${model}/${user.user_id}`)).length}))});
      if (url.pathname === "/admin/model-access/users" && request.method() === "GET") {
        const selected = url.searchParams.getAll("models");
        return send({models: selected, users: users.flatMap((user) => selected.map((model) => ({...user, model, enabled: permissions.get(`${model}/${user.user_id}`)})))});
      }
      if (request.method() === "PUT" && ["/admin/model-access/users", "/admin/model-access/defaults"].includes(url.pathname)) {
        const body = request.postDataJSON();
        writes.push({path: url.pathname, body});
        if (failWrite) return send({error: {message: "模拟批量操作失败"}}, 503);
        let changed = 0, target = 0;
        for (const model of body.models) {
          if (url.pathname.endsWith("/defaults")) { target++; changed += defaults.get(model) !== body.enabled ? 1 : 0; defaults.set(model, body.enabled); continue; }
          for (const user of users) {
            if (body.scope !== "all" && !body.user_ids.includes(user.user_id)) continue;
            const key = `${model}/${user.user_id}`;
            target++; changed += permissions.get(key) !== body.enabled ? 1 : 0;
            permissions.set(key, body.enabled);
          }
        }
        return send({target_count: target, changed_count: changed, results: []});
      }
      throw new Error(`Unexpected request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#model-access");
    await page.evaluate(async (value) => { bindUI(); initializeDateFilters(); renderState(value); await loadModelAccess(); }, initialState);
    const row = (id) => page.locator("#model-access-user-rows tr").filter({has: page.locator(`input[value="${id}"]`)});
    const userCheck = (id) => page.locator(`#model-access-user-rows input[value="${id}"]`);
    const ready = () => page.waitForFunction(() => byId("model-access-user-rows").closest("table").parentElement.getAttribute("aria-busy") === "false");
    await userCheck("user-1").check();
    await page.locator('.model-access-model-checkbox[value="gpt-beta"]').check(); await ready();
    assert.equal(await userCheck("user-1").isChecked(), true, "model changes preserve users");
    assert.match(await row("user-1").textContent(), /部分启用/);
    assert.match(await page.locator("#model-access-selected-count").textContent(), /2 个模型 × 1 位用户.*2 项权限/);
    assert.equal(await page.locator("#model-access-default-enabled").evaluate((input) => input.indeterminate), true);
    await page.locator("#model-access-model-search").fill("gamma");
    assert.equal(await page.locator(".model-access-model-choice:visible").count(), 1);
    assert.match(await page.locator("#model-access-model-count").textContent(), /已选 2/);
    await page.locator("#model-access-model-search").fill("");
    await page.locator('#model-access-users-form input[name="reason"]').fill("Enable selected pairs");
    await page.locator("#model-access-enable-selected").click();
    await page.waitForFunction(() => !byId("model-access-users-form").elements.reason.value); await ready();
    assert.deepEqual(writes[0].body, {models: ["gpt-alpha", "gpt-beta"], enabled: true, scope: "selected", reason: "Enable selected pairs", user_ids: ["user-1"]});
    assert.equal(permissions.get("gpt-beta/user-2"), false, "unselected users are unaffected");
    assert.equal(await userCheck("user-1").isChecked(), true, "successful refresh preserves users");
    assert.equal(await page.locator(".model-access-model-checkbox:checked").count(), 2);
    await page.locator("#model-access-model-clear").click(); await ready();
    assert.equal(await page.locator("#model-access-enable-selected").isDisabled(), true);
    await page.locator("#model-access-model-all").click(); await ready();
    assert.equal(await userCheck("user-1").isChecked(), true, "clearing models preserves users");
    await page.locator('#model-access-users-form input[name="reason"]').fill("Disable all selected models");
    page.once("dialog", (dialog) => dialog.accept());
    await page.locator("#model-access-disable-all").click();
    await page.waitForFunction(() => !byId("model-access-users-form").elements.reason.value); await ready();
    assert.equal(writes[1].body.scope, "all");
    assert.equal("user_ids" in writes[1].body, false);
    assert.equal(writes[1].body.models.length, 3);
    await page.locator("#model-access-default-enabled").check();
    await page.locator('#model-access-default-form input[name="reason"]').fill("Enable future users");
    await page.locator('#model-access-default-form button[type="submit"]').click();
    await page.waitForFunction(() => !byId("model-access-default-form").elements.reason.value); await ready();
    assert.equal(writes[2].path, "/admin/model-access/defaults");
    assert.ok([...defaults.values()].every(Boolean));
    assert.ok([...permissions.values()].every((enabled) => !enabled), "defaults do not change existing users");
    failWrite = true;
    await page.locator('#model-access-users-form input[name="reason"]').fill("Failure preserves selection");
    await page.locator("#model-access-enable-selected").click();
    await page.waitForFunction(() => !byId("model-access-enable-selected").disabled);
    assert.equal(await userCheck("user-1").isChecked(), true);
    assert.equal(await page.locator(".model-access-model-checkbox:checked").count(), 3);
    assert.match(await row("user-1").textContent(), /全部禁用/);
    await page.evaluate(async () => {
      await loadModelAccess();
      setLocalMessage(byId("model-access-users-form"));
      setLocalMessage(byId("model-access-default-form"));
      byId("model-access-users-form").elements.reason.value = "";
      byId("model-access-default-form").elements.reason.value = "";
      hide("notice");
    });
    assert.equal(await userCheck("user-1").isChecked(), true, "manual refresh preserves users");
    const screenshots = process.env.SCREENSHOT_DIR || "/tmp/codex-gateway-feature-screenshots";
    fs.mkdirSync(screenshots, {recursive: true});
    await page.screenshot({path: path.join(screenshots, "model-access-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.screenshot({path: path.join(screenshots, "model-access-mobile.png"), fullPage: true});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile should not overflow viewport");
    assert.deepEqual(errors, []);
    console.log("Model batch browser regression passed; screenshots:", screenshots);
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
