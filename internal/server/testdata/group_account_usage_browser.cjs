"use strict";

// Optional browser regression for group account usage. Every request is
// intercepted; identities, groups and accounts are synthetic.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
const owner = {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"};
const member = {id: "alice", username: "alice", display_name: "Alice", role: "member", status: "active"};
const initial = (user) => ({user, recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []});
const group = {id: "research", name: "研究组", period: "month", custom_days: 0, limit_usd: "100.000000000000", used_usd: "42.500000000000",
  remaining_usd: "57.500000000000", member_limit_usd: null, member_count: 2, period_count: 0, current_period_number: 2, expires_at: null,
  starts_at: "2026-10-01T00:00:00Z", period_starts_at: "2026-11-01T00:00:00Z", period_ends_at: "2026-12-02T00:00:00Z",
  model_ids: ["gpt-5.6", "claude-sonnet-5"], account_usage_visible: false};
const other = {...group, id: "engineering", name: "工程组", account_usage_visible: false};
const poolAccounts = {
  codex: [{id: "0123456789abcdef", display_name: "团队主账号", email_masked: "a***@example.test", status: "available"},
    {id: "1123456789abcdef", display_name: "团队备用账号", email_masked: "b***@example.test", status: "available"}],
  antigravity: [{id: "2123456789abcdef", display_name: "Gemini 团队账号", email_masked: "g***@example.test", status: "available"}],
  anthropic: [{id: "3123456789abcdef", display_name: "claude-a", alias: "Claude 主力", email_masked: "c***@example.test", status: "available"},
    {id: "4123456789abcdef", display_name: "claude-b", email_masked: "d***@example.test", status: "available"},
    {id: "5123456789abcdef", display_name: "claude-c", email_masked: "e***@example.test", status: "available"}],
};
const pools = {anthropic: {mode: "selected", account_ids: ["3123456789abcdef", "4123456789abcdef"]}};
const groupPool = (id) => ({group_id: id, providers: ["codex", "antigravity", "anthropic"].map((provider) => {
  const policy = pools[provider] || {mode: "all", account_ids: []};
  return {group_id: id, provider, strategy: policy.mode, mode: policy.mode, account_ids: policy.account_ids, accounts: poolAccounts[provider]};
})});
const usage = {groups: [{group_id: "research", group_name: "研究组", accounts: [
  {provider: "anthropic", alias: "Claude 主力", source: "observed", updated_at: "2026-11-04T05:52:00Z", limited: false,
    five_hour: {used_percent: 42, reset_at: "2026-11-04T09:00:00Z"}, seven_day: {used_percent: 18, reset_at: "2026-11-09T02:00:00Z"}},
  {provider: "anthropic", alias: "", source: "observed", updated_at: "2026-11-04T05:40:00Z", limited: true, cooldown_until: "2026-11-04T07:30:00Z",
    five_hour: {used_percent: 100, reset_at: "2026-11-04T07:30:00Z"}, seven_day: {used_percent: 64, reset_at: "2026-11-08T12:00:00Z"}},
]}]};
const billing = (user) => ({user, cash_balance_usd: "9.876543", subscriptions: [], ledger_entries: [], pagination: {offset: 0, limit: 50, total: 0, has_more: false},
  groups: [{...group, account_usage_visible: true, priority: 10, member_used_usd: "12.500000000000", member_remaining_usd: null},
    {...other, priority: 20, member_used_usd: "0", member_remaining_usd: null}]});

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1080}, locale: "zh-CN", timezoneId: "Asia/Shanghai"});
    await page.addInitScript(() => { Date.now = () => new Date("2026-11-04T06:00:00Z").getTime(); });
    const errors = [], writes = [], unexpected = [];
    let identity = owner;
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, origin);
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/theme.js") return send(fs.readFileSync(path.join(assets, "theme.js"), "utf8"), 200, "application/javascript");
      if (url.pathname === "/static/favicon.svg") return send(fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), 200, "image/svg+xml");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/egress-servers") return send({servers: [], version: 1, runtime: {ready: true, applied_version: 1}});
      if (url.pathname === "/admin/billing/me") return send(billing(identity));
      if (url.pathname === "/admin/billing/me/account-usage") {
        assert.equal(identity.id, member.id, "only the signed-in member reads account usage");
        return send(usage);
      }
      if (url.pathname === "/admin/billing/plans") return send({plans: []});
      if (url.pathname === "/admin/billing/users") return send({users: [owner, member]});
      if (url.pathname === "/admin/model-catalog") return send({models: [{id: "gpt-5.6"}, {id: "claude-sonnet-5"}]});
      if (url.pathname === "/admin/usage") return send({summary: {requests: 0}, requests: []});
      if (url.pathname === "/admin/groups" && request.method() === "GET") return send({groups: [group, other]});
      if (url.pathname === "/admin/groups/research" && request.method() === "GET") return send({...group, members: []});
      if (url.pathname === "/admin/groups/research/upstream-access") return send(groupPool("research"));
      if (url.pathname === "/admin/groups/research/periods") return send({periods: []});
      if (url.pathname === "/admin/groups/research/account-usage-visibility" && request.method() === "PUT") {
        const body = request.postDataJSON(); writes.push({path: url.pathname, body});
        group.account_usage_visible = body.enabled;
        return send({...group, members: []});
      }
      unexpected.push(`${request.method()} ${url.pathname}`);
      return send({error: {type: "invalid_request_error", code: "not_found", message: "未模拟的请求"}}, 404);
    });
    const capture = async (selector, name) => {
      await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); });
      // Clip from a full-page capture so the sticky top bar cannot cover the section.
      const clip = await page.locator(selector).evaluate((node) => {
        const box = node.getBoundingClientRect();
        return {x: box.left + scrollX, y: box.top + scrollY, width: box.width, height: box.height};
      });
      await page.screenshot({path: path.join(screenshots, name), fullPage: true, clip});
    };
    const noOverflow = async (label) => assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, label);

    await page.goto(`${origin}/#groups`);
    await page.evaluate(async (value) => { bindUI(); initializeDateFilters(); renderState(value); await loadGroups("research"); }, initial(owner));
    await page.waitForFunction(() => /开启后成员可查看/.test(document.getElementById("group-account-usage-effect").textContent));
    assert.equal(await page.locator("#group-account-usage-enabled").isChecked(), false, "account usage starts disabled");
    assert.equal(await page.locator("#group-account-usage-effect").textContent(),
      "未开启。开启后成员可查看：Claude（指定 2 个账号）；不展示：Codex（全部账号）、Antigravity（不支持）。");
    await page.locator("#group-account-usage-enabled").check();
    await page.locator("#group-account-usage-form button[type=submit]").click();
    assert.match(await page.locator("#group-account-usage-form .form-message").textContent(), /必须填写操作原因/);
    assert.equal(writes.length, 0);
    await page.locator("#group-account-usage-form input[name=reason]").fill("让研究组成员查看 Claude 用量");
    await page.locator("#group-account-usage-form button[type=submit]").click();
    await page.waitForFunction(() => managedGroup?.account_usage_visible === true && !groupOperation && /已开启/.test(document.getElementById("group-account-usage-effect").textContent));
    assert.deepEqual({...writes[0].body, operation_id: undefined}, {enabled: true, reason: "让研究组成员查看 Claude 用量", operation_id: undefined});
    assert.match(writes[0].body.operation_id, /^[0-9a-f-]{36}$/);
    assert.equal(await page.locator("#group-account-usage-enabled").isChecked(), true);
    assert.equal(await page.locator("#group-account-usage-form input[name=reason]").inputValue(), "");
    assert.match(await page.locator("#groups-message").textContent(), /已允许成员查看用量/);
    await noOverflow("owner desktop");
    await capture(".group-upstream-settings", "group-account-usage-owner-desktop.png");
    await page.setViewportSize({width: 390, height: 844});
    await noOverflow("owner mobile");
    await capture(".group-upstream-settings", "group-account-usage-owner-mobile.png");

    identity = member;
    await page.setViewportSize({width: 1440, height: 1080});
    await page.goto(`${origin}/#billing`);
    await page.evaluate(async (value) => { bindUI(); initializeDateFilters(); renderState(value); routeFromHash(false); await loadBillingDashboard(); }, initial(member));
    await page.locator(".billing-account-usage-card").nth(1).waitFor();
    const research = page.locator('.billing-account-usage[data-group-id="research"]');
    assert.equal(await page.locator('.billing-account-usage[data-group-id="engineering"]').isVisible(), false);
    assert.match(await research.textContent(), /账号用量.*刷新用量/);
    const [primary, cooling] = [research.locator(".billing-account-usage-card").nth(0), research.locator(".billing-account-usage-card").nth(1)];
    assert.match(await primary.textContent(), /Claude 主力正常5 小时已用 42%.*每周已用 18%.*观测于/);
    assert.match(await cooling.textContent(), /Claude 账号 2冷却至.*5 小时已用 100%/);
    assert.doesNotMatch(await page.locator("#billing-group").textContent(), /3123456789abcdef|c\*\*\*@example\.test/);
    await noOverflow("member desktop");
    await capture("#billing-group", "group-account-usage-member-desktop.png");
    await page.setViewportSize({width: 390, height: 844});
    await noOverflow("member mobile");
    await capture("#billing-group", "group-account-usage-member-mobile.png");

    assert.deepEqual(errors, []);
    if (unexpected.length) console.log(`unmocked requests answered 404: ${[...new Set(unexpected)].join(", ")}`);
  } finally {
    await browser.close();
  }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
