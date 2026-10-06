"use strict";

// Optional browser checks use synthetic data and intercept every request.
// PLAYWRIGHT_MODULE may point to an existing Playwright or playwright-core package.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const {rows, clone} = require("./model_prices_fixtures.cjs");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const initialState = {user: {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"},
  recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const models = rows();

async function main() {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], writes = [], events = [];
    let failSave = false, holdSave = null, holdRead = null, failRead = false, conflictSave = false;
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/theme.js") return send(fs.readFileSync(path.join(assets, "theme.js"), "utf8"), 200, "application/javascript");
      if (url.pathname === "/static/favicon.svg") return send(fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), 200, "image/svg+xml");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/billing/me") return send({user: initialState.user, cash_balance_usd: "0", subscriptions: {}});
      if (url.pathname === "/admin/usage") return send({summary: {requests: 0, tokens: 0, error_rate: 0}, requests: []});
      if (url.pathname === "/admin/billing/model-prices") {
        const result = {models: clone(models)};
        if (holdRead) { const callback = holdRead; holdRead = null; callback(() => send(result)); return; }
        return failRead ? send({error: {message: "模拟读取失败"}}, 503) : send(result);
      }
      if (url.pathname === "/auth/password/reauth") { events.push("verified"); return send({ok: true}); }
      if (url.pathname.startsWith("/admin/billing/model-prices/") && request.method() === "PUT") {
        const model = decodeURIComponent(url.pathname.split("/").pop()), body = request.postDataJSON();
        const row = models.find((item) => item.model === model);
        assert.ok(row?.editable); writes.push(body); events.push("write");
        assert.match(body.operation_id, /^[0-9a-f-]{36}$/); assert.ok(body.reason);
        if (failSave) return send({error: {message: "模拟保存失败"}}, 503);
        if (conflictSave) return send({error: {message: "价格版本冲突"}}, 409);
        assert.equal(body.version, row.version); assert.equal(body.structure_id, row.structure_id);
        if (body.price) for (const tier of Object.values(body.price.service_tiers || {})) for (const price of Object.values(tier)) {
          assert.ok(Object.values(price).every((value) => typeof value === "string"));
        }
        const finish = () => {
          row.effective_price = clone(body.action === "save" ? body.price : row.configured_price);
          row.source = body.action === "save" ? "override" : "config"; row.conflict = false;
          row.updated_at = "2026-10-06T12:00:00Z"; row.version++;
          return send(clone(row));
        };
        if (holdSave) { const callback = holdSave; holdSave = null; callback(finish); return; }
        return finish();
      }
      throw new Error(`Unexpected request ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#model-pricing");
    await page.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, initialState);
    const card = (model = "gpt-example") => page.locator(`.model-price-card[data-model="${model}"]`);
    const input = () => card().locator('[name="standard.short.input_usd_per_million"]');
    const reason = () => card().locator('[name="reason"]');
    const settled = () => page.waitForFunction(() => !modelPriceLoading && !modelPriceOperation);
    const save = () => card().locator('button[type="submit"]').click();
    await settled();
    assert.equal(await card("codex-auto-review").locator("input, button").count(), 0);
    assert.equal(await card().locator("tbody tr").count(), 6);
    assert.equal(await card().locator("input").count(), 25);
    assert.equal(await card("gemini-example").locator("input").count(), 7);
    assert.equal(await card("legacy-example").locator("input").count(), 4);

    await input().fill("1e2"); await reason().fill("校验价格输入"); await save();
    assert.match(await card().locator(".form-message").textContent(), /大于或等于 0/); assert.equal(writes.length, 0);
    await page.evaluate(() => { state.recently_verified = false; });
    await input().fill("0"); await reason().fill("更新基础单价"); await save();
    await page.locator("#reauth-dialog").waitFor({state: "visible"}); assert.equal(writes.length, 0);
    await page.locator('#reauth-form input[name="password"]').fill("synthetic-password");
    await page.locator('#reauth-form button[type="submit"]').click(); await settled();
    assert.deepEqual(events.slice(0, 2), ["verified", "write"]);
    assert.equal(await input().inputValue(), "0"); assert.equal(await reason().inputValue(), "");
    assert.equal(writes.at(-1).price.service_tiers.standard.short.input_usd_per_million, "0");

    failSave = true; await input().fill("0.123456789012"); await reason().fill("调整基础费用"); await save(); await settled();
    assert.match(await card().locator(".form-message").textContent(), /模拟保存失败.*输入已保留/);
    assert.equal(await input().inputValue(), "0.123456789012"); const retryID = writes.at(-1).operation_id;
    failSave = false; await save(); await settled(); assert.equal(writes.at(-1).operation_id, retryID);

    let finishWrite;
    holdSave = (finish) => { finishWrite = finish; };
    await input().fill("1.25"); await reason().fill("标准服务调整"); await save();
    await page.waitForFunction(() => Boolean(modelPriceOperation));
    const count = writes.length;
    await card().locator("form").evaluate((form) => form.dispatchEvent(new Event("submit", {bubbles: true, cancelable: true})));
    assert.equal(writes.length, count); assert.equal(await input().isDisabled(), true);
    assert.equal(await page.locator("#model-prices-refresh").isDisabled(), true);
    while (!finishWrite) await new Promise((resolve) => setTimeout(resolve, 5));
    await finishWrite(); await settled();

    let finishRead;
    holdRead = (finish) => { finishRead = finish; };
    await page.locator("#model-prices-refresh").click();
    while (!finishRead) await new Promise((resolve) => setTimeout(resolve, 5));
    await input().fill("2.5"); await reason().fill("恢复基础单价"); await save(); await settled();
    await finishRead(); await page.waitForTimeout(50);
    assert.equal(await input().inputValue(), "2.5", "old GET cannot overwrite successful save");

    conflictSave = true; await input().fill("3"); await reason().fill("并发检查"); await save(); await settled();
    assert.match(await card().locator(".form-message").textContent(), /价格版本或配置结构已改变/);
    assert.equal(await input().inputValue(), "3");
    assert.equal(await card().locator('[data-action="save"]').isDisabled(), true);
    conflictSave = false; await card().locator('[data-action="reload"]').click(); await settled();
    assert.equal(await input().inputValue(), "2.5");

    await input().fill("bad-draft"); await reason().fill("使用部署配置价格");
    await card().locator('[data-action="restore"]').click(); await settled();
    assert.equal(writes.at(-1).action, "restore"); assert.equal(writes.at(-1).price, undefined);
    assert.match(await page.locator("#model-prices-message").textContent(), /已恢复配置价/);

    failRead = true; await input().fill("2.5"); await reason().fill("确认基础价格"); await save(); await settled();
    assert.match(await page.locator("#model-prices-message").textContent(), /已保存.*列表刷新失败/);
    failRead = false; await page.locator("#model-prices-refresh").click(); await settled();
    await input().fill("4"); await page.locator("#model-prices-search").fill("GEMINI");
    assert.equal(await page.locator(".model-price-card").count(), 1);
    await page.locator("#model-prices-search").fill(""); assert.equal(await input().inputValue(), "4");
    await card().locator('[data-action="reload"]').click(); await settled();

    await page.evaluate(() => { hide("notice"); modelPriceMessage(); });
    fs.mkdirSync(screenshots, {recursive: true});
    await page.evaluate(() => window.scrollTo(0, 0));
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    await page.screenshot({path: path.join(screenshots, "model-pricing-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.evaluate(() => window.scrollTo(0, 0));
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "narrow layout must not overflow");
    assert.equal(await card().locator("tbody td").first().evaluate((node) => getComputedStyle(node).display), "block");
    const originalRow = await page.evaluate(() => {
      const original = JSON.parse(JSON.stringify(modelPriceModels[0]));
      modelPriceModels[0].model = "m".repeat(128);
      modelPriceModels[0].multiplier = "999999999999999999.999999999999";
      renderModelPrices(); return original;
    });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "maximum model and multiplier must wrap");
    await page.evaluate((original) => { modelPriceDrafts.delete(modelPriceModels[0].model); modelPriceModels[0] = original; renderModelPrices(); }, originalRow);
    await page.screenshot({path: path.join(screenshots, "model-pricing-mobile.png"), fullPage: true});

    await page.evaluate(() => { renderState({...state, user: {...state.user, role: "member"}}); });
    assert.equal(await page.locator('nav [data-view="model-pricing"]').isVisible(), false);
    assert.equal(await page.locator('[data-section="model-pricing"]').isVisible(), false);
    assert.equal(await page.evaluate(() => location.hash), "#overview");
    assert.equal(await page.evaluate(() => modelPriceDrafts.size), 0);
    assert.deepEqual(errors, []);
    console.log("Model pricing: complete matrices, validation, reauthentication, retry, restore, conflicts, stale responses, Owner routing and responsive screenshots passed.");
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
