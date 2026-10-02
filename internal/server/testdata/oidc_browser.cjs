"use strict";
// Real cross-site HTTPS navigation and Strict cookies, synthetic backend and
// identities. This checks browser behavior, not a real Supabase integration.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const https = require("node:https");
const os = require("node:os");
const path = require("node:path");
const {execFileSync} = require("node:child_process");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots/oidc");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));
const cookie = (name, value) => `${name}=${value}; Path=/; Secure; HttpOnly; SameSite=Strict`;
const initial = () => ({user: {id: "synthetic-user", username: "lin", display_name: "林同学", role: "member", status: "active"},
  recently_verified: false, login_methods: {password: true}, passkeys: [], devices: [], projects: [], api_keys: [],
  external_identity: {enabled: true, linked: false, masked_email: "", linked_at: null}});
const csp = "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; connect-src 'self'";
const tick = () => new Promise((resolve) => setTimeout(resolve, 10));
const waitForSignal = async (ready) => {
  const deadline = Date.now() + 10000;
  while (!ready()) {assert.ok(Date.now() < deadline, "expected fixture request did not arrive"); await tick();}
};

async function main() {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "gateway-oidc-browser-"));
  const key = path.join(directory, "key.pem"), cert = path.join(directory, "cert.pem");
  execFileSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key,
    "-out", cert, "-days", "1", "-subj", "/CN=gateway.localhost"], {stdio: "ignore"});
  const requests = [], errors = [];
  let gateway, center, state = initial(), enabled = true, completeMode = "confirm", confirmFailure = false;
  let holdComplete = false, releaseComplete, holdBegin = false, releaseBegin;
  const server = https.createServer({key: fs.readFileSync(key), cert: fs.readFileSync(cert)}, async (req, res) => {
    try {
      const url = new URL(req.url, `https://${req.headers.host}`);
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const text = Buffer.concat(chunks).toString();
      const body = text ? JSON.parse(text) : null;
      requests.push({host: url.hostname, path: url.pathname, query: url.search, method: req.method, cookie: req.headers.cookie || "", origin: req.headers.origin, body});
      res.setHeader("Cache-Control", "no-store");
      res.setHeader("Content-Security-Policy", csp);
      res.setHeader("Referrer-Policy", "no-referrer");
      const send = (value, status = 200, type = "application/json") => {
        res.writeHead(status, {"Content-Type": type});
        res.end(typeof value === "string" ? value : JSON.stringify(value));
      };
      const file = (name, type) => send(fs.readFileSync(path.join(assets, name), "utf8"), 200, type);
      if (url.hostname === "accounts.localhost") {
        const query = url.searchParams.get("query") || "state=synthetic-state&code=synthetic-code";
        return send(`<html lang="zh-CN"><a id="authorize" href="${gateway}/auth/oidc/callback?${query.replaceAll("&", "&amp;")}">同意并返回网关</a></html>`, 200, "text/html");
      }
      if (url.pathname === "/seed") {
        res.setHeader("Set-Cookie", [cookie("__Host-cg_oidc", "synthetic-transaction"), cookie("__Host-cg_session", "synthetic-local-session")]);
        return send(`<a href="${center}/authorize" id="center">前往账号中心</a>`, 200, "text/html");
      }
      if (url.pathname === "/") return file("index.html", "text/html");
      if (url.pathname === "/static/app.js") return send(application, 200, "application/javascript");
      if (url.pathname === "/static/style.css") return file("style.css", "text/css");
      if (url.pathname === "/static/oidc-callback.js") return file("oidc-callback.js", "application/javascript");
      if (url.pathname === "/auth/oidc/callback") return file("oidc-callback.html", "text/html");
      if (url.pathname === "/auth/oidc/config") return send({enabled});
      if (url.pathname === "/auth/oidc/login") return send({authorization_url: `${center}/authorize`});
      if (url.pathname === "/auth/oidc/complete") {
        if (holdComplete) await new Promise((resolve) => {releaseComplete = resolve;});
        if (body.error) return send({error: {message: "授权已取消，请重新开始。"}}, 400);
        if (!req.headers.cookie?.includes("__Host-cg_oidc=")) return send({error: {message: "授权事务无效，请重新开始。"}}, 400);
        if (completeMode === "login") return send({result: "login"});
        return send({result: "confirm", flow_id: "synthetic-preview", user: {username: "lin"}, masked_email: "l***@example.com", expires_at: "2099-10-03T10:05:00Z"});
      }
      if (url.pathname === "/admin/identity-link/confirm") {
        if (confirmFailure) return send({error: {message: "原登录会话已失效，请重新开始。"}}, 401);
        state.external_identity = {enabled: true, linked: true, masked_email: "l***@example.com", linked_at: "2026-10-03T10:00:00Z"};
        return send({ok: true});
      }
      if (url.pathname === "/admin/identity-link/cancel") return send({ok: true});
      if (url.pathname === "/admin/identity-link/begin") {
        if (holdBegin) await new Promise((resolve) => {releaseBegin = resolve;});
        return send({authorization_url: `${center}/authorize`});
      }
      if (url.pathname === "/admin/identity-link") {
        state.external_identity = initial().external_identity;
        return send({ok: true, logged_out: true});
      }
      if (url.pathname === "/auth/password/reauth") return send({ok: true});
      if (url.pathname === "/auth/logout") return send({ok: true});
      if (url.pathname === "/admin/state") return send(state);
      if (url.pathname === "/admin/billing/me") return send({cash_balance_usd: "48.75", subscriptions: {}});
      if (url.pathname === "/admin/usage") return send({summary: {requests: 0, tokens: 0}, requests: []});
      if (url.pathname === "/favicon.ico") return send("", 204);
      throw new Error(`Unexpected request ${url.pathname}`);
    } catch (error) {errors.push(error.message); res.writeHead(500); res.end("fixture error");}
  });
  let browser;
  try {
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const port = server.address().port;
    gateway = `https://gateway.localhost:${port}`; center = `https://accounts.localhost:${port}`;
    browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE,
      args: ["--no-proxy-server", "--host-resolver-rules=MAP gateway.localhost 127.0.0.1, MAP accounts.localhost 127.0.0.1"]});
    const context = await browser.newContext({ignoreHTTPSErrors: true, viewport: {width: 1440, height: 1000}, locale: "zh-CN"});
    await context.addInitScript(() => {
      const original = window.fetch;
      window.oidcLocationsAtFetch = [];
      window.fetch = (...args) => {window.oidcLocationsAtFetch.push(location.href); return original(...args);};
    });
    const page = await context.newPage();
    page.on("pageerror", (error) => errors.push(error.message));
    const count = (endpoint) => requests.filter((request) => request.path === endpoint).length;
    const goCallback = async (query = "state=synthetic-state&code=synthetic-code", seed = true) => {
      if (seed) await page.goto(`${gateway}/seed`);
      await page.goto(`${center}/authorize?query=${encodeURIComponent(query)}`);
      await page.locator("#authorize").click();
      await page.waitForURL(`${gateway}/auth/oidc/callback`);
    };
    const preview = () => page.locator("#oidc-preview").waitFor({state: "visible"});
    const initialize = async () => {await page.goto(`${gateway}/#security`); await page.evaluate((data) => {bindUI(); initializeDateFilters(); renderState(data); setConnection("已连接", "ok");}, state);};
    const reauth = async () => {
      await page.locator("#reauth-dialog").waitFor({state: "visible"});
      await page.locator('#reauth-form input[name="password"]').fill("synthetic-password");
      await page.locator('#reauth-form button[type="submit"]').click();
    };
    fs.mkdirSync(screenshots, {recursive: true});
    for (const width of [1440, 390]) {
      await page.setViewportSize({width, height: width < 600 ? 844 : 1000});
      state = initial();
      await initialize();
      assert.equal(await page.locator("#identity-link-begin").isVisible(), true);
      const starts = count("/admin/identity-link/begin");
      await page.locator("#identity-link-begin").click();
      await page.locator("#reauth-dialog [data-close]").click();
      await page.waitForFunction(() => !document.getElementById("identity-link-begin").disabled);
      assert.equal(count("/admin/identity-link/begin"), starts, "cancelled reauth cannot begin linking");
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      await page.evaluate(() => {hide("notice"); document.activeElement?.blur(); window.scrollTo(0, 0);});
      await page.screenshot({path: path.join(screenshots, `security-${width}.png`), fullPage: true});
      await goCallback(); await preview();
      const callback = requests.filter((r) => r.path === "/auth/oidc/callback").at(-1);
      const complete = requests.filter((r) => r.path === "/auth/oidc/complete").at(-1);
      assert.equal(callback.cookie, "", "cross-site callback GET omits Strict cookies");
      assert.match(complete.cookie, /__Host-cg_oidc=synthetic-transaction/);
      assert.match(complete.cookie, /__Host-cg_session=synthetic-local-session/);
      assert.equal(complete.origin, gateway);
      const cookies = await context.cookies(gateway);
      assert.equal(cookies.length, 2);
      assert.equal(cookies.every((value) => value.secure && value.httpOnly && value.sameSite === "Strict"), true);
      assert.equal(await page.evaluate(() => document.cookie), "", "JavaScript cannot read transaction or session cookie");
      assert.deepEqual(await page.evaluate(() => window.oidcLocationsAtFetch), [`${gateway}/auth/oidc/callback`]);
      assert.equal(await page.evaluate(() => localStorage.length + sessionStorage.length), 0);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      assert.match(await page.locator("#oidc-accounts").textContent(), /lin.*l\*\*\*@example.com/);
      await page.screenshot({path: path.join(screenshots, `confirmation-${width}.png`), fullPage: true});
      const cancels = count("/admin/identity-link/cancel"), confirms = count("/admin/identity-link/confirm");
      await page.locator("#oidc-cancel").click();
      await page.waitForFunction(() => document.getElementById("oidc-message").textContent === "已取消绑定。");
      assert.equal(count("/admin/identity-link/cancel"), cancels + 1);
      assert.equal(count("/admin/identity-link/confirm"), confirms);
      await goCallback(); await preview();
      await page.locator("#oidc-confirm").dblclick();
      await page.waitForFunction(() => document.getElementById("oidc-message").textContent.startsWith("绑定成功"));
      assert.equal(count("/admin/identity-link/confirm"), confirms + 1, "confirmation cannot be submitted twice");
      await initialize();
      assert.equal(await page.locator("#identity-link-unlink").isVisible(), true);
      await page.screenshot({path: path.join(screenshots, `linked-${width}.png`), fullPage: true});
    }

    await page.setViewportSize({width: 1440, height: 1000});
    page.once("dialog", (dialog) => dialog.dismiss());
    await page.locator("#identity-link-unlink").click();
    assert.equal(await page.locator("#reauth-dialog").isVisible(), false, "cancelled unlink cannot request proof or change binding");
    assert.equal(count("/admin/identity-link"), 0);
    page.once("dialog", (dialog) => dialog.accept());
    await page.locator("#identity-link-unlink").click(); await reauth();
    await page.waitForURL(`${gateway}/`);
    assert.equal(count("/admin/identity-link"), 1, "unlink submits once after local proof and returns logged-out sessions to login");

    await goCallback("state=synthetic-state&error=access_denied");
    await page.waitForFunction(() => document.getElementById("oidc-message").textContent.includes("授权已取消"));
    assert.equal(await page.locator("#oidc-preview").isVisible(), false);
    const completions = count("/auth/oidc/complete");
    await goCallback("state=one&state=two&code=synthetic-code");
    await page.waitForFunction(() => document.getElementById("oidc-message").textContent.includes("参数无效"));
    assert.equal(count("/auth/oidc/complete"), completions, "duplicate query parameters cannot be exchanged");
    await context.clearCookies();
    await goCallback("state=synthetic-state&code=synthetic-code", false);
    await page.waitForFunction(() => document.getElementById("oidc-message").textContent.includes("事务无效"));
    confirmFailure = true;
    await goCallback(); await preview(); await page.locator("#oidc-confirm").click();
    await page.waitForFunction(() => document.getElementById("oidc-message").textContent.includes("会话已失效"));
    assert.equal(await page.locator("#oidc-preview").isVisible(), false);
    confirmFailure = false;

    holdComplete = true; releaseComplete = undefined;
    await goCallback(); await waitForSignal(() => releaseComplete);
    await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent("pagehide")));
    releaseComplete(); holdComplete = false;
    await page.waitForLoadState("networkidle");
    assert.equal(await page.locator("#oidc-preview").isVisible(), false, "late completion after pagehide cannot restore preview");
    await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent("pageshow", {persisted: true})));
    assert.match(await page.locator("#oidc-message").textContent(), /页面已失效/);

    state = initial(); holdBegin = true; releaseBegin = undefined;
    await page.setViewportSize({width: 1440, height: 1000});
    await initialize(); await page.locator("#identity-link-begin").click(); await reauth();
    await waitForSignal(() => releaseBegin);
    await page.locator("#logout").click(); await page.waitForURL(`${gateway}/`);
    releaseBegin(); holdBegin = false;
    await page.waitForLoadState("networkidle");
    assert.equal(page.url(), `${gateway}/`, "late begin after logout cannot redirect to account center");

    await page.evaluate(() => {bindUI(); show("login-view"); return loadOIDCConfig();});
    assert.equal(await page.locator("#oidc-login").isVisible(), true);
    for (const width of [1440, 390]) {
      await page.setViewportSize({width, height: width < 600 ? 844 : 1000});
      await page.screenshot({path: path.join(screenshots, `login-${width}.png`), fullPage: true});
    }
    enabled = false; await page.evaluate(() => loadOIDCConfig());
    assert.equal(await page.locator("#oidc-login").isVisible(), false);
    enabled = true; completeMode = "login";
    await goCallback(); await page.waitForURL(`${gateway}/#overview`);
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: browser.version(), checks: "cross-site HTTPS Strict cookie GET/POST, URL removal before fetch, no browser storage, desktop/mobile login/security/confirmation, reauth cancel, explicit single-use confirm/cancel, unlink cancel/proof/logout, missing transaction, denied/duplicate callback, stale session, late callback/begin after pagehide/logout, bfcache invalidation, login return", screenshots}));
  } finally {
    releaseComplete?.(); releaseBegin?.();
    await browser?.close();
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    fs.rmSync(directory, {recursive: true, force: true});
  }
}
main().catch((error) => {console.error(error); process.exitCode = 1;});
