"use strict";

// All API requests use synthetic fixtures. No real credentials or models are called.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
const output = process.env.SCREENSHOT_DIR || "/tmp/gateway-model-routes-screenshots";
const catalog = ["claude-haiku-5-5", "claude-opus-5-5", "claude-sonnet-5-5", "gpt-6-mini", "gpt-6-sol"].map(id => ({id, group: id.startsWith("claude-") ? "Claude" : "GPT"}));

(async () => {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1000}, locale: "zh-CN", deviceScaleFactor: 2});
    const errors = [], writes = [];
    let routes = [
      {source_model: "gpt-6-sol", target_model: "claude-sonnet-5-5", available: true},
      {source_model: "gpt-6-legacy", target_model: "claude-retired-4", available: false},
    ];
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
      if (url.pathname === "/admin/model-catalog") return send({models: catalog});
      if (url.pathname === "/admin/model-routes" && request.method() === "GET") return send({routes, max_routes: 32});
      if (url.pathname === "/admin/model-routes" && request.method() === "PUT") {
        const body = request.postDataJSON();
        writes.push(body);
        routes = body.routes.map(item => ({...item, available: catalog.some(model => model.id === item.target_model)}));
        return send({routes, max_routes: 32});
      }
      throw new Error(`Unexpected fixture request ${request.method()} ${url.pathname}`);
    });
    await page.goto(origin + "/#keys");
    await page.evaluate(value => {bindUI(); initializeDateFilters(); renderState(value); return loadModelRoutes();}, {
      user: {id: "member", role: "member", display_name: "成员"}, recently_verified: true,
      devices: [{id: "device-1", name: "MacBook", status: "active"}], projects: [],
      api_keys: [{id: "key-1", name: "Codex CLI", key_prefix: "cgk_v1_demo", status: "active", device_id: "device-1", model_allowlist: [], secret_available: true, created_at: "2026-11-01T08:00:00Z", expires_at: "2027-11-01T08:00:00Z"}],
      passkeys: [],
    });
    const card = page.locator(".model-routes");
    await card.locator(".model-route-warning:not(.hidden)", {hasText: "目标模型当前不可用"}).waitFor();
    assert.equal(await card.locator(".model-route-warning:not(.hidden)").count(), 1);
    await card.getByRole("button", {name: "删除第 2 条规则"}).click();
    await card.getByRole("button", {name: "添加规则"}).click();
    await card.getByLabel("第 2 条规则的原模型").fill("gpt-6-mini");
    await card.getByLabel("第 2 条规则的目标模型").selectOption("claude-haiku-5-5");
    await card.getByRole("button", {name: "保存路由"}).click();
    await page.getByText("已保存 2 条模型路由，新请求立即生效。").waitFor();
    assert.deepEqual(writes, [{routes: [
      {source_model: "gpt-6-sol", target_model: "claude-sonnet-5-5"},
      {source_model: "gpt-6-mini", target_model: "claude-haiku-5-5"},
    ]}]);
    await card.getByRole("button", {name: "添加规则"}).click();
    await card.getByLabel("第 3 条规则的原模型").fill("gpt-6-mini");
    await card.getByLabel("第 3 条规则的目标模型").selectOption("claude-opus-5-5");
    await card.getByRole("button", {name: "保存路由"}).click();
    await card.locator(".form-message", {hasText: "原模型 gpt-6-mini 重复，每个原模型只能有一条规则。"}).waitFor();
    assert.equal(writes.length, 1);
    await card.getByRole("button", {name: "删除第 3 条规则"}).click();
    assert.equal(await card.locator(".form-message").isHidden(), true);

    fs.mkdirSync(output, {recursive: true});
    await card.screenshot({path: path.join(output, "model-routes-desktop.png")});
    await page.setViewportSize({width: 390, height: 844});
    await card.scrollIntoViewIfNeeded();
    await card.screenshot({path: path.join(output, "model-routes-mobile.png")});
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    assert.deepEqual(errors, []);
    console.log("Model routes browser: load, unavailable target warning, add/remove, save payload, duplicate rejection, desktop/mobile passed");
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
