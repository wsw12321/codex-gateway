"use strict";

// Optional browser checks use synthetic data and intercept every request.
// PLAYWRIGHT_MODULE may point to an existing Playwright or playwright-core package.
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
const models = [
  {model: "gpt-6", multiplier: "1", updated_at: null, editable: true},
  {model: "gpt-6-sol", multiplier: "0.8", updated_at: "2026-09-28T08:30:00Z", editable: true},
  {model: "gemini-2.5-pro", multiplier: "1.25", updated_at: "2026-09-28T09:15:00Z", editable: true},
  {model: "gemini-2.5-flash", multiplier: "1", updated_at: null, editable: true},
  {model: "codex-auto-review", multiplier: "1", updated_at: null, editable: false},
];

async function main() {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [], events = [];
    let failSave = false, holdSave = null, holdRead = null, failRead = false;
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765", "every request stays on the mock origin");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/theme.js") return send(fs.readFileSync(path.join(assets, "theme.js"), "utf8"), 200, "application/javascript");
      if (url.pathname === "/static/favicon.svg") return send(fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), 200, "image/svg+xml");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      // Losing Owner privileges redirects to the member overview.
      if (url.pathname === "/admin/billing/me") return send({user: initialState.user, cash_balance_usd: "0", subscriptions: {}});
      if (url.pathname === "/admin/usage") return send({summary: {requests: 0, tokens: 0, error_rate: 0}, requests: []});
      if (url.pathname === "/admin/billing/model-multipliers") {
        const result = {models: models.map((row) => ({...row}))};
        if (holdRead) { const callback = holdRead; holdRead = null; callback(() => send(result)); return; }
        return failRead ? send({error: {message: "模拟读取失败"}}, 503) : send(result);
      }
      if (url.pathname === "/auth/password/reauth") { events.push("verified"); return send({ok: true}); }
      if (url.pathname.startsWith("/admin/billing/model-multipliers/") && request.method() === "PUT") {
        const model = decodeURIComponent(url.pathname.split("/").pop()), body = request.postDataJSON();
        const row = models.find((item) => item.model === model);
        assert.ok(row?.editable); writes.push(body); events.push("write");
        assert.equal(typeof body.multiplier, "string");
        assert.match(body.operation_id, /^[0-9a-f-]{36}$/); assert.ok(body.reason);
        if (failSave) return send({error: {message: "模拟保存失败"}}, 503);
        const finish = () => { row.multiplier = body.multiplier; row.updated_at = "2026-09-28T12:00:00Z"; return send({...row}); };
        if (holdSave) { const callback = holdSave; holdSave = null; callback(finish); return; }
        return finish();
      }
      throw new Error(`Unexpected request ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#model-multipliers");
    await page.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, initialState);
    const card = (model = "gpt-6") => page.locator(`.model-multiplier-card[data-model="${model}"]`);
    const input = () => card().locator('[name="multiplier"]');
    const reason = () => card().locator('[name="reason"]');
    const settled = () => page.waitForFunction(() => !modelMultiplierLoading && !modelMultiplierOperation);
    const save = () => card().locator('button[type="submit"]').click();
    await settled();
    assert.equal(await card("codex-auto-review").locator("input, button").count(), 0);
    assert.equal(await input().inputValue(), "1.0");
    await input().fill("0"); await reason().fill("校验倍率输入"); await save();
    assert.match(await card().locator(".form-message").textContent(), /大于 0/); assert.equal(writes.length, 0);

    await page.evaluate(() => { state.recently_verified = false; });
    await input().fill("0.750000000001"); await reason().fill("团队折扣调整"); await save();
    await page.locator("#reauth-dialog").waitFor({state: "visible"}); assert.equal(writes.length, 0);
    await page.locator('#reauth-form input[name="password"]').fill("synthetic-password");
    await page.locator('#reauth-form button[type="submit"]').click(); await settled();
    assert.deepEqual(events.slice(0, 2), ["verified", "write"]);
    assert.equal(await input().inputValue(), "0.750000000001");
    assert.equal(await reason().inputValue(), "");
    assert.match(await page.locator("#model-multipliers-message").textContent(), /已保存，仅影响新请求/);

    failSave = true;
    await input().fill("0.8"); await reason().fill("调整为八折"); await save(); await settled();
    assert.equal(await input().inputValue(), "0.8"); assert.equal(await reason().inputValue(), "调整为八折");
    assert.match(await card().locator(".form-message").textContent(), /模拟保存失败.*输入已保留/);
    const retryID = writes.at(-1).operation_id;
    failSave = false; await save(); await settled();
    assert.equal(writes.at(-1).operation_id, retryID);

    let finishWrite;
    holdSave = (finish) => { finishWrite = finish; };
    await input().fill("1.25"); await reason().fill("标准服务加价"); await save();
    await page.waitForFunction(() => Boolean(modelMultiplierOperation));
    const count = writes.length;
    await card().locator("form").evaluate((form) => form.dispatchEvent(new Event("submit", {bubbles: true, cancelable: true})));
    assert.equal(writes.length, count);
    assert.equal(await input().isDisabled(), true);
    assert.equal(await page.locator("#model-multipliers-refresh").isDisabled(), true);
    while (!finishWrite) await new Promise((resolve) => setTimeout(resolve, 5));
    await finishWrite(); await settled();

    let finishRead;
    holdRead = (finish) => { finishRead = finish; };
    await page.locator("#model-multipliers-refresh").click();
    while (!finishRead) await new Promise((resolve) => setTimeout(resolve, 5));
    await input().fill("0.9"); await reason().fill("新的折扣规则"); await save(); await settled();
    await finishRead();
    await page.waitForTimeout(50);
    assert.equal(await input().inputValue(), "0.9", "old read cannot replace successful write");

    failRead = true;
    await input().fill("1.0"); await reason().fill("恢复标准倍率"); await save(); await settled();
    assert.match(await page.locator("#model-multipliers-message").textContent(), /已保存.*列表刷新失败/);
    assert.equal(await input().inputValue(), "1.0");
    failRead = false; await page.locator("#model-multipliers-refresh").click(); await settled();

    await page.evaluate(() => hide("notice"));
    fs.mkdirSync(screenshots, {recursive: true});
    await page.screenshot({path: path.join(screenshots, "model-multipliers-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.evaluate(() => window.scrollTo(0, 0));
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "narrow layout must not overflow");
    const originalRow = await page.evaluate(() => {
      const original = {...modelMultiplierModels[0]};
      modelMultiplierModels[0] = {...original, model: "m".repeat(128), multiplier: "999999999999999999.999999999999"};
      renderModelMultipliers();
      return original;
    });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "maximum model and multiplier must not overflow the narrow viewport");
    assert.equal(await page.locator(".model-multiplier-current strong").first().evaluate((node) => node.scrollWidth <= node.clientWidth), true, "maximum multiplier must wrap within its card");
    await page.evaluate((original) => {
      modelMultiplierDrafts.delete(modelMultiplierModels[0].model);
      modelMultiplierModels[0] = original;
      renderModelMultipliers();
    }, originalRow);
    await page.screenshot({path: path.join(screenshots, "model-multipliers-mobile.png"), fullPage: true});

    await page.evaluate(() => { renderState({...state, user: {...state.user, role: "member"}}); });
    assert.equal(await page.locator('nav [data-view="model-multipliers"]').isVisible(), false);
    assert.equal(await page.locator('[data-section="model-multipliers"]').isVisible(), false);
    assert.equal(await page.evaluate(() => location.hash), "#overview");
    assert.deepEqual(errors, []);
    console.log("Model multipliers: validation, reauthentication, retry, concurrency, stale responses, owner routing and responsive screenshots passed.");
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
