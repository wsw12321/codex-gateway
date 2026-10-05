"use strict";
// All requests and identities are synthetic. No service or account is required.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots/profile");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
const owner = {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"};
const member = {id: "member", username: "water5_0123456789abcdef0123456789abcdef", display_name: "吾水阁用户", role: "member", status: "active"};
const snapshot = (user, recent = false) => ({user: {...user}, recently_verified: recent,
  login_methods: {password: true, passkey: false, oidc: false}, passkeys: [], devices: [], projects: [], api_keys: [],
  external_identity: {enabled: true, linked: false}});
const waitUntil = async (test) => {
  const deadline = Date.now() + 10000;
  while (!test()) {
    assert.ok(Date.now() < deadline, "expected request did not arrive");
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
};

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1080}, locale: "zh-CN"});
    const errors = [], writes = [], invitations = [];
    let data = snapshot(owner), holdState = false, releaseState, holdWrite = false, releaseWrite;
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const req = route.request(), url = new URL(req.url());
      assert.equal(url.origin, origin);
      const send = (value, status = 200, contentType = "application/json") => route.fulfill({status, contentType,
        body: typeof value === "string" ? value : JSON.stringify(value)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/app.js") return send(application, 200, "application/javascript");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/billing/users") return send({users: [owner, member]});
      if (url.pathname === "/admin/state") {
        const value = structuredClone(data);
        if (holdState) await new Promise((resolve) => { releaseState = resolve; });
        return send(value);
      }
      if (url.pathname === "/auth/password/reauth") { data.recently_verified = true; return send({ok: true}); }
      if (url.pathname === "/auth/logout") return send({ok: true});
      if (url.pathname === "/admin/profile") {
        assert.equal(req.method(), "PATCH");
        const body = req.postDataJSON();
        writes.push(body);
        if (body.username === "taken") return send({error: {code: "username_taken", message: "用户名已被使用"}}, 409);
        data.user = {...data.user, ...body};
        const value = {user: {...data.user}};
        if (holdWrite) await new Promise((resolve) => { releaseWrite = resolve; });
        return send(value);
      }
      if (url.pathname === "/admin/invitations") {
        invitations.push(req.postDataJSON());
        return send({link: `${origin}/join#invitation=synthetic`, expires_at: "2099-01-01T00:00:00Z"});
      }
      throw new Error(`Unexpected fixture request ${req.method()} ${url.pathname}`);
    });
    await page.goto(`${origin}/#security`);
    await page.evaluate(async (value) => { bindUI(); initializeDateFilters(); renderState(value); await loadBillingUsers(); }, data);
    const username = page.locator('#profile-form input[name="username"]');
    const display = page.locator('#profile-form input[name="display_name"]');
    const submit = page.locator('#profile-form button[type="submit"]');
    const idle = () => page.waitForFunction(() => document.querySelector("#profile-form").dataset.busy !== "true");
    const noOverflow = async (width) => {
      await page.setViewportSize({width, height: width < 600 ? 844 : 1080});
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `overflow at ${width}`);
    };
    const capture = async (name) => {
      await page.evaluate(() => { document.activeElement?.blur(); hide("notice"); window.scrollTo(0, 0); });
      await page.screenshot({path: path.join(screenshots, name), fullPage: true});
    };
    assert.equal(await username.inputValue(), "owner");
    await display.fill("尚未保存的中文草稿");
    await page.evaluate(() => refreshState());
    assert.equal(await display.inputValue(), "尚未保存的中文草稿");
    await submit.click(); await idle();
    assert.deepEqual(writes.at(-1), {display_name: "尚未保存的中文草稿"});
    assert.equal(await page.locator("#reauth-dialog").isVisible(), false);
    assert.equal(await page.locator("#whoami").textContent(), "尚未保存的中文草稿");

    await username.fill("updated_owner");
    await submit.click();
    await page.locator("#reauth-dialog").waitFor({state: "visible"});
    await page.locator("#reauth-dialog [data-close]").click(); await idle();
    assert.equal(writes.length, 1);
    assert.equal(await username.inputValue(), "updated_owner");
    await submit.click();
    await page.locator('#reauth-form input[name="password"]').fill("synthetic-password");
    await page.locator('#reauth-form button[type="submit"]').click(); await idle();
    assert.deepEqual(writes.at(-1), {username: "updated_owner"});
    assert.match(await page.locator("#role").textContent(), /^updated_owner/);

    await username.fill("taken"); await display.fill("冲突时不能部分保存");
    await submit.click(); await idle();
    assert.match(await page.locator("#profile-form .form-message").textContent(), /用户名已被使用/);
    assert.equal(await username.inputValue(), "taken");
    assert.equal(await display.inputValue(), "冲突时不能部分保存");
    assert.equal(await page.evaluate(() => state.user.display_name), "尚未保存的中文草稿");

    // A snapshot begun before a successful write must not restore the old name.
    holdState = true;
    await page.evaluate(() => { window.profileRefresh = refreshState().catch((error) => error.code); });
    await waitUntil(() => releaseState);
    await username.fill("final_owner"); await display.fill("最终个人资料");
    await submit.click(); await idle();
    holdState = false; releaseState(); releaseState = null;
    await page.evaluate(() => window.profileRefresh);
    assert.equal(await page.evaluate(() => state.user.username), "final_owner");
    assert.equal(await display.inputValue(), "最终个人资料");
    await noOverflow(1440);
    await capture("profile-owner-desktop.png");

    // Recovery targets must be selected, and editing even back to the same text clears selection.
    const search = page.locator("#recovery-user-search");
    await search.fill("water5_");
    await page.locator('#recovery-user-search-results [role="option"]').click();
    assert.equal(await page.evaluate(() => recoveryUserSearch.selectedID()), member.id);
    await search.fill("unselected"); await search.fill(member.username);
    await search.press("Escape");
    await page.locator('#recovery-invite-form button[type="submit"]').click();
    await page.waitForFunction(() => document.querySelector("#recovery-invite-form").dataset.busy !== "true");
    assert.equal(invitations.length, 0);
    await search.fill("water5_"); await page.locator('#recovery-user-search-results [role="option"]').click();
    await page.locator('#recovery-invite-form button[type="submit"]').click();
    await page.locator("#secret-dialog").waitFor({state: "visible"});
    assert.deepEqual(invitations.at(-1), {kind: "recovery", target_user_id: member.id});
    await page.locator("#save-secret").click();

    // A delayed write cannot restore another account after an identity switch.
    holdWrite = true;
    await display.fill("旧账号迟到结果"); await submit.click();
    await waitUntil(() => releaseWrite);
    data = snapshot(member);
    await page.evaluate((value) => renderState(value), data);
    holdWrite = false; releaseWrite(); releaseWrite = null;
    await page.waitForFunction(() => profileOperation === null);
    assert.equal(await username.inputValue(), member.username);
    assert.equal(await display.inputValue(), member.display_name);
    await display.fill("中文会员名称"); await submit.click(); await idle();
    assert.deepEqual(writes.at(-1), {display_name: "中文会员名称"});
    assert.equal(await page.locator("#owner-tools").isVisible(), false);
    for (const width of [320, 390, 850, 851, 1440]) await noOverflow(width);
    await noOverflow(390);
    await capture("profile-member-mobile.png");

    // A pending refresh and an unsaved draft are both invalidated by logout.
    await display.fill("退出前草稿");
    holdState = true;
    await page.evaluate(() => { window.profileRefresh = refreshState().catch((error) => error.code); });
    await waitUntil(() => releaseState);
    await page.evaluate(() => handleUnauthorized());
    holdState = false; releaseState();
    assert.equal(await page.evaluate(() => window.profileRefresh), "stale_request");
    assert.equal(await display.inputValue(), "");
    assert.equal(await page.locator("#dashboard").isVisible(), false);
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: browser.version(), checks: "owner/member profile, legacy display-only edit, reauth/cancel, atomic conflict feedback, draft/refresh/write/account/logout races, stable recovery targets, 320/390/850/851/1440 layout", writes: writes.length, screenshots, errors}));
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
