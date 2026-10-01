"use strict";
// Synthetic identities and credentials only. Standalone dashboard browser checks;
// the workbench suite exercises exchange and CORS with two live local origins.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
const initial = () => ({user: {id: "member", username: "lin", display_name: "林同学", role: "member", status: "active"},
  browser_client_enabled: true, recently_verified: true, login_methods: {password: true}, passkeys: [], projects: [],
  devices: [{id: "device", name: "开发工作站", status: "active", created_at: "2026-09-01"}],
  api_keys: [{id: "key", device_id: "device", name: "网页开发密钥", key_prefix: "gw_synthetic", status: "active", secret_available: true, expires_at: "2099-01-01"}]});
async function main() {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    for (const width of [1440, 390]) {
      const page = await browser.newPage({viewport: {width, height: width < 600 ? 844 : 1000}, locale: "zh-CN"});
      let state = initial(), handoffFailures = 0, creates = 0, signs = 0;
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.route("**/*", async (route) => {
        const url = new URL(route.request().url());
        const send = (value, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof value === "string" ? value : JSON.stringify(value)});
        if (url.origin === "https://ai.water555.com") return send("<h1>工作台</h1>", 200, "text/html");
        if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
        if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
        if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
        if (url.pathname === "/admin/state") return send(state);
        if (url.pathname === "/admin/devices") {
          const body = route.request().postDataJSON();
          const device = {id: "created-device", name: body.name, status: "active", created_at: new Date().toISOString()}; state.devices.push(device); return send(device, 201);
        }
        if (url.pathname === "/admin/api-keys") {
          creates++;
          const body = route.request().postDataJSON();
          assert.equal(body.name, "网页工作台"); assert.equal(body.expires_days, 90); assert.deepEqual(body.models, []);
          state.api_keys.push({id: "created-key", device_id: body.device_id, name: body.name, key_prefix: "gw_created", status: "active", secret_available: true, expires_at: "2099-01-01"});
          return send({id: "created-key", api_key: "synthetic_secret", prefix: "gw_created", expires_at: "2099-01-01"}, 201);
        }
        if (url.pathname === "/admin/browser-handoffs") {
          signs++;
          const body = route.request().postDataJSON(); assert.equal(body.remember_key, false);
          if (handoffFailures-- > 0) return send({error: {message: "模拟接入失败"}}, 503);
          return send({launch_url: "https://ai.water555.com/#handoff_version=1&code=synthetic", expires_at: "2099-01-01"}, 201);
        }
        if (url.pathname === "/admin/billing/me") return send({cash_balance_usd: "0", subscriptions: {}});
        if (url.pathname === "/admin/usage") return send({summary: {requests: 0, tokens: 0, error_rate: 0}, requests: []});
        throw new Error(`Unexpected dashboard request ${url.pathname}`);
      });
      const initialize = async (value) => page.evaluate((data) => {bindUI(); initializeDateFilters(); renderState(data);}, value);
      const open = async () => {await page.locator('[data-section="overview"] [data-browser-handoff-open]').click(); await page.waitForFunction(() => browserHandoffDraft?.resourcesReady && !browserHandoffDraft.busy);};
      await page.goto(`${origin}/#overview`); await initialize(state);
      await open(); assert.equal(await page.locator("#browser-handoff-remember").isChecked(), false);
      assert.match(await page.locator("#browser-handoff-key-detail").textContent(), /gw_synthetic.*2099/);
      await page.locator("#browser-handoff-dialog [data-close]").click();
      await page.evaluate(() => {location.hash = "guide";});
      await page.locator('[data-section="guide"] [data-browser-handoff-open]').click();
      await page.waitForFunction(() => browserHandoffDraft?.resourcesReady && !browserHandoffDraft.busy);
      assert.equal(await page.locator("#browser-handoff-dialog").isVisible(), true);
      await page.keyboard.press("Escape");
      state = {...initial(), devices: [], api_keys: [], recently_verified: false};
      await page.evaluate((value) => {renderState(value); location.hash = "overview";}, state);
      await open(); await page.locator("#browser-handoff-submit").click();
      await page.locator("#reauth-dialog").waitFor({state: "visible"});
      await page.locator("#reauth-dialog [data-close]").click();
      await page.waitForFunction(() => !browserHandoffDraft?.busy);
      assert.equal(creates, 0); assert.equal(signs, 0);
      assert.match(await page.locator("#browser-handoff-form .form-message").textContent(), /取消/);
      state.recently_verified = true;
      await page.locator("#browser-handoff-refresh").click(); await page.waitForFunction(() => !browserHandoffDraft?.busy);
      assert.equal(await page.locator("#browser-handoff-name").inputValue(), "网页工作台");
      assert.match(await page.locator("#browser-handoff-device-help").textContent(), /自动创建设备/);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `page fits ${width}`);
      assert.equal(await page.locator("#browser-handoff-dialog").evaluate((node) => node.scrollWidth <= node.clientWidth), true, `dialog fits ${width}`);
      if (process.env.SCREENSHOT_DIR) {
        fs.mkdirSync(process.env.SCREENSHOT_DIR, {recursive: true});
        await page.screenshot({path: path.join(process.env.SCREENSHOT_DIR, `gateway-handoff-${width}.png`), fullPage: true});
      }
      handoffFailures = 1;
      await page.locator("#browser-handoff-submit").click(); await page.waitForFunction(() => !browserHandoffDraft?.busy);
      assert.equal(creates, 1); assert.equal(signs, 1);
      assert.match(await page.locator("#browser-handoff-form .form-message").textContent(), /模拟接入失败/);
      assert.match(await page.locator("#browser-handoff-key-detail").textContent(), /gw_created/);
      await page.locator("#browser-handoff-submit").click(); await page.waitForURL("https://ai.water555.com/**");
      assert.equal(creates, 1); assert.equal(signs, 2); assert.deepEqual(errors, []);
      await page.close();
    }
    console.log("Gateway handoff desktop/mobile passed: shared entries, metadata, defaults, reauth cancellation, device/key creation, issuance retry, same-tab navigation, no overflow.");
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
