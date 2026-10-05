"use strict";

// Synthetic account data; every network request is intercepted. No OAuth
// credential, public management service or live account is used by this test.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const origin = "http://127.0.0.1:8765";
const assets = path.join(__dirname, "../assets/cpa");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots");
const csp = "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'";

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE });
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 1100 }, locale: "zh-CN" });
    const errors = [], writes = [];
    let importFails = false, oauthDone = false, deleted = false;
    page.on("pageerror", error => errors.push(error.message));
    page.on("console", event => {
      if (event.type() === "error" && event.text() !== "Failed to load resource: the server responded with a status of 502 (Bad Gateway)") errors.push(event.text());
    });
    const state = "a".repeat(32);
    const account = provider => ({ id: "0123456789abcdef", display_name: provider === "codex" ? "Codex 团队账号" : "Gemini 团队账号", email_masked: "te***@example.test", status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available" });
    await page.route("**/*", async route => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, origin);
      const send = (body, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
      if (url.pathname === "/static/theme.js") return route.fulfill({ contentType: "application/javascript", body: fs.readFileSync(path.join(assets, "../theme.js")) });
      if (url.pathname === "/static/favicon.svg") return route.fulfill({ contentType: "image/svg+xml", body: fs.readFileSync(path.join(assets, "../favicon.svg")) });
      if (url.pathname === "/admin/cpa/") return route.fulfill({ status: 200, contentType: "text/html", headers: { "Content-Security-Policy": csp }, body: fs.readFileSync(path.join(assets, "index.html")) });
      if (url.pathname.startsWith("/admin/cpa/assets/")) {
        const name = path.basename(url.pathname);
        assert.ok(["panel.js", "panel.css"].includes(name));
        return route.fulfill({ status: 200, contentType: name.endsWith("js") ? "application/javascript" : "text/css", body: fs.readFileSync(path.join(assets, name)) });
      }
      if (/\/api\/(codex|antigravity)\/accounts$/.test(url.pathname)) return send({ accounts: deleted ? [] : [account(url.pathname.includes("/codex/") ? "codex" : "antigravity")] });
      if (request.method() !== "GET") writes.push({ path: url.pathname, method: request.method(), body: request.postDataJSON() });
      if (url.pathname.endsWith("/oauth")) return send({ id: "flow-one", url: `https://accounts.google.com/o/oauth2/auth?state=${state}`, expires_in: 300 });
      if (url.pathname === "/admin/cpa/api/oauth/flow-one/status") return send({ status: oauthDone ? "ok" : "wait" });
      if (url.pathname === "/admin/cpa/api/oauth/flow-one/callback") { oauthDone = true; return send({ status: "submitted" }); }
      if (url.pathname.endsWith("/credentials")) return importFails ? send({ error: { message: "凭据验证未完成" } }, 502) : send({ status: "ok" });
      if (url.pathname.endsWith("/quota")) return send({ status: "ok", quota: [{ model: "gemini-3-flash", remaining_fraction: 0.82, reset_time: "2026-10-01T00:00:00Z" }] });
      if (url.pathname.endsWith("/refresh") || url.pathname.endsWith("/status")) return send({ status: "ok" });
      if (request.method() === "DELETE" && url.pathname.endsWith("/accounts/0123456789abcdef")) { deleted = true; return send({ status: "deleted" }); }
      throw new Error(`Unexpected request ${request.method()} ${url.pathname}`);
    });
    await page.goto(origin + "/admin/cpa/");
    await page.getByRole("heading", { name: "Codex 团队账号" }).waitFor();
    assert.match(await page.title(), /水源喵/);
    await page.locator('[data-theme-choice="dark"]').click();
    assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
    await page.reload();
    await page.getByRole("heading", { name: "Codex 团队账号" }).waitFor();
    assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
    await page.locator('[data-theme-choice="light"]').click();
    assert.equal(await page.locator("html").getAttribute("data-theme"), "light");
    await page.getByRole("button", { name: "Antigravity", exact: true }).click();
    await page.getByRole("heading", { name: "Gemini 团队账号" }).waitFor();
    await page.getByRole("button", { name: "停用", exact: true }).click();
    await page.getByRole("status").filter({ hasText: "操作完成" }).waitFor();
    assert.deepEqual(writes.at(-1).body, { enabled: false });

    await page.getByRole("button", { name: "新增 / 重新授权" }).click();
    await page.getByRole("link", { name: "打开供应商授权页面 ↗" }).waitFor();
    await page.getByLabel("授权回调地址").fill(`http://localhost:51121/oauth-callback?code=synthetic-code&state=${state}`);
    await page.getByRole("button", { name: "提交回调" }).click();
    await page.waitForFunction(() => document.querySelector("#callback")?.value === "" || !document.querySelector("#callback"));
    await page.getByRole("status").filter({ hasText: "授权完成" }).waitFor();
    assert.deepEqual(writes.find(write => write.path.endsWith("/callback")).body, { code: "synthetic-code", state });

    for (const failure of [false, true]) {
      importFails = failure;
      await page.locator('input[type="file"]').setInputFiles({ name: "fixture.json", mimeType: "application/json", buffer: Buffer.from(JSON.stringify({ refresh_token: "synthetic-refresh", access_token: "synthetic-access", proxy_url: "http://ignored.invalid", priority: 999 })) });
      await page.getByRole("status").filter({ hasText: failure ? "凭据验证未完成" : "操作完成" }).waitFor();
      const uploaded = writes.filter(write => write.path.endsWith("/credentials")).at(-1).body;
      assert.deepEqual(uploaded, { refresh_token: "synthetic-refresh", access_token: "synthetic-access" });
      assert.equal(await page.locator('input[type="file"]').inputValue(), "");
      assert.deepEqual(await page.evaluate(() => Object.keys(localStorage)), ["shuiyuan-theme"]);
      assert.equal(await page.evaluate(() => sessionStorage.length), 0);
      assert.ok(!(await page.locator("body").textContent()).includes("synthetic-refresh"));
    }
    await page.getByRole("button", { name: "查询额度" }).click();
    await page.getByText("gemini-3-flash：剩余 82.0%", { exact: false }).waitFor();
    fs.mkdirSync(screenshots, { recursive: true });
    await page.screenshot({ path: path.join(screenshots, "cpa-admin-desktop.png"), fullPage: true, animations: "disabled" });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: path.join(screenshots, "cpa-admin-mobile.png"), fullPage: true, animations: "disabled" });
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await page.locator('[data-theme-choice="dark"]').click();
    await page.screenshot({ path: path.join(screenshots, "cpa-admin-dark-mobile.png"), fullPage: true, animations: "disabled" });
    await page.setViewportSize({ width: 320, height: 844 });
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "CPA dark mode fits at 320px");
    await page.setViewportSize({ width: 1440, height: 1100 });
    await page.screenshot({ path: path.join(screenshots, "cpa-admin-dark-desktop.png"), fullPage: true, animations: "disabled" });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole("button", { name: "删除凭据", exact: true }).click();
    await page.getByRole("button", { name: "确认删除", exact: true }).click();
    await page.getByText("暂无已登记账号").waitFor();
    assert.equal(writes.at(-1).method, "DELETE");
    assert.deepEqual(errors, []);
    console.log("CPA panel: provider isolation, OAuth, import clearing, status/delete, desktop/mobile and CSP passed");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
