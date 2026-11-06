"use strict";

// Optional browser validation of quota freshness labels, refresh buttons and
// quota line bars. Install Playwright outside the application and set
// PLAYWRIGHT_MODULE when it is not on Node's module path. Every request is
// intercepted; identities, groups, accounts and amounts are synthetic.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots/quota-visualization");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
// The production policy from internal/httpx: no inline styles or scripts.
const csp = "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'";
const now = Date.parse("2026-11-06T06:00:00Z");
const hour = 60 * 60 * 1000;
const at = (offset) => new Date(now + offset).toISOString();
const owner = {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"};
const member = {id: "alice", username: "alice", display_name: "Alice", role: "member", status: "active"};
const initial = (user) => ({user, recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []});

const group = {id: "research", name: "研究组", period: "month", custom_days: 0, limit_usd: "100.000000000000", used_usd: "62.400000000000",
  remaining_usd: "37.600000000000", member_limit_usd: "40.000000000000", member_count: 4, period_count: 0, current_period_number: 2, expires_at: null,
  starts_at: "2026-10-01T00:00:00Z", period_id: "period-2", period_starts_at: "2026-11-01T00:00:00Z", period_ends_at: "2026-12-02T00:00:00Z",
  model_ids: ["gpt-5.6", "claude-sonnet-5"], account_usage_visible: true, created_at: "2026-10-01T00:00:00Z", updated_at: "2026-11-01T00:00:00Z"};
const members = [
  {user_id: "alice", username: "alice", display_name: "Alice", used_usd: "24.100000000000", remaining_usd: "15.900000000000"},
  {user_id: "bob", username: "bob", display_name: "Bob", used_usd: "18.300000000000", remaining_usd: "21.700000000000"},
  {user_id: "carol", username: "carol", display_name: "陈卡罗", used_usd: "9.000000000000", remaining_usd: "31.000000000000"},
  {user_id: "dave", username: "dave", display_name: "", used_usd: "0", remaining_usd: "40.000000000000"},
];
const subscriptions = [
  {id: "sub-day", name: "日卡", enabled: true, status: "active", tier: "day", quota_usd: "10", remaining_usd: "6.25", priority: 1, charge_enabled: true,
    period_ends_at: at(18 * hour), expires_at: at(18 * hour), period_count: 1, current_period_number: 1, model_ids: ["gpt-5.6"]},
  {id: "sub-month", name: "月卡", enabled: true, status: "active", tier: "month", quota_usd: "100", remaining_usd: "71.5", priority: 2, charge_enabled: true,
    period_ends_at: at(20 * 24 * hour), expires_at: null, period_count: 0, current_period_number: 3, model_ids: ["gpt-5.6", "claude-sonnet-5"]},
];
const billing = (user) => ({user, cash_balance_usd: "9.876543", cash_preferences: {priority: 100, charge_enabled: true}, subscriptions, ledger_entries: [],
  pagination: {offset: 0, limit: 50, total: 0, has_more: false},
  groups: [{...group, priority: 10, member_used_usd: "24.100000000000", member_remaining_usd: "15.900000000000"}]});
const accountBase = {plan: "Plus", status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available",
  can_manage: true, allocation_weight: 1, concurrent_limit: 2, rolling_cost_usd: "8.20", rolling_cost_share: "0.3", target_share: "0.5",
  request_count: 96, input_tokens: 210000, output_tokens: 12000, error_count: 0, equivalent_cost_usd: "40.1", last_synced_at: at(-hour)};
const codexAccounts = [
  {...accountBase, id: "0123456789abcdef", alias: "团队主账号", email_masked: "a***@example.test",
    quota_guard: {status: "available", source: "auto", observed_at: at(-7 * 60 * 1000), next_check_at: at(40 * 60 * 1000),
      five_hour: {used_percent: 38, reset_at: at(3 * hour)}, seven_day: {used_percent: 64, reset_at: at(100 * hour)}}},
  {...accountBase, id: "1123456789abcdef", alias: "团队备用账号", email_masked: "b***@example.test", plan: "Pro",
    quota_guard: {status: "limited", source: "auto", limited_window: "five_hour", reset_at: at(2 * hour), restore_at: at(2 * hour + 5 * 60 * 1000),
      observed_at: at(-3 * 60 * 1000), five_hour: {used_percent: 97, reset_at: at(2 * hour)}, seven_day: {used_percent: 41, reset_at: at(80 * hour)}}},
  {...accountBase, id: "2123456789abcdef", email_masked: "c***@example.test", quota_guard: {status: "available", source: "auto"}},
];

const usageModels = {from: at(-24 * hour), until: at(1000), generated_at: at(1000), truncated: false,
  total: {requests: 57, tokens: 1873400, charged_usd: "6.418200000000"},
  models: [
    {model: "gpt-5.6", requests: 31, errors: 1, in_progress: 0, tokens: 1204000, charged_usd: "3.912000000000"},
    {model: "claude-sonnet-5", requests: 12, errors: 0, in_progress: 1, tokens: 402000, charged_usd: "1.640700000000"},
    {model: "gpt-5.6-mini", requests: 9, errors: 0, in_progress: 0, tokens: 198000, charged_usd: "0.511500000000"},
    {model: "gemini-3-pro", requests: 4, errors: 2, in_progress: 0, tokens: 69400, charged_usd: "0.354000000000"},
    {model: "gpt-5.6-nano", requests: 1, errors: 0, in_progress: 0, tokens: 0, charged_usd: "0"},
  ]};
const usageRequests = Array.from({length: 4}, (_, index) => ({request_id: `req-${index}`, requested_at: at(-index * 17 * 60 * 1000),
  model: ["gpt-5.6", "claude-sonnet-5", "gpt-5.6-mini", "gemini-3-pro"][index], state: "completed", http_status: 200,
  input_tokens: 1200 * (index + 1), output_tokens: 300, device_id: "", api_key_id: "", key_prefix: "cgk_v1_demo"}));

const periodRefs = [{id: "period-2", starts_at: group.period_starts_at, ends_at: group.period_ends_at},
  {id: "period-1", starts_at: "2026-10-01T00:00:00Z", ends_at: "2026-11-01T00:00:00Z"}];
const groupBreakdown = {group_id: "research", current_period: true, data_complete: true, period: periodRefs[0], periods: periodRefs,
  limit_usd: group.limit_usd, member_limit_usd: group.member_limit_usd, used_usd: group.used_usd, remaining_usd: group.remaining_usd,
  members: [
    ...members.map(({user_id, username, display_name, used_usd}) => ({user_id, username, display_name, current_member: true, group_used_usd: used_usd})),
    {user_id: "erin", username: "erin", display_name: "Erin", current_member: false, group_used_usd: "11.000000000000"},
  ].sort((a, b) => Number(b.group_used_usd) - Number(a.group_used_usd)),
  accounts: [
    {account_id: "0123456789abcdef", provider: "codex", alias: "团队主账号", in_pool: true, total_cost_usd: "71.300000000000", member_cost_usd: "52.900000000000",
      non_member_cost_usd: "18.400000000000", group_charged_usd: "48.200000000000",
      members: [{user_id: "alice", cost_usd: "24.100000000000"}, {user_id: "bob", cost_usd: "18.300000000000"}, {user_id: "carol", cost_usd: "6.500000000000"}, {user_id: "erin", cost_usd: "4.000000000000"}]},
    {account_id: "1123456789abcdef", provider: "codex", alias: "团队备用账号", in_pool: true, total_cost_usd: "16.700000000000", member_cost_usd: "6.500000000000",
      non_member_cost_usd: "10.200000000000", group_charged_usd: "6.500000000000",
      members: [{user_id: "carol", cost_usd: "2.500000000000"}, {user_id: "erin", cost_usd: "4.000000000000"}]},
  ]};
const previousBreakdown = {...groupBreakdown, current_period: false, period: periodRefs[1], used_usd: "91.000000000000", remaining_usd: "9.000000000000",
  members: groupBreakdown.members.map((member) => ({...member, group_used_usd: member.user_id === "alice" ? "40.000000000000" : member.group_used_usd})), accounts: []};
const breakdownQueries = [];

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1000}, locale: "zh-CN", timezoneId: "Asia/Shanghai", deviceScaleFactor: 2});
    // Pin both Date.now() and new Date() so data times in screenshots are stable.
    await page.addInitScript((fixed) => {
      const RealDate = Date;
      globalThis.Date = class extends RealDate {
        constructor(...args) { super(...(args.length ? args : [fixed])); }
        static now() { return fixed; }
      };
    }, now);
    const errors = [], unexpected = [], reads = [];
    let identity = owner;
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => { if (/Content Security Policy/i.test(message.text())) errors.push(message.text()); });
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, origin, "all traffic must stay on the mocked origin");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") {
        return route.fulfill({status: 200, contentType: "text/html", body: fs.readFileSync(path.join(assets, "index.html"), "utf8"),
          headers: {"Content-Security-Policy": csp}});
      }
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/theme.js") return send(fs.readFileSync(path.join(assets, "theme.js"), "utf8"), 200, "application/javascript");
      if (url.pathname === "/static/favicon.svg") return send(fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), 200, "image/svg+xml");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      reads.push(url.pathname);
      if (url.pathname === "/admin/egress-servers") return send({servers: [], version: 1, runtime: {ready: true, applied_version: 1}});
      if (url.pathname === "/admin/billing/me") return send(billing(identity));
      if (url.pathname === "/admin/billing/me/account-usage") return send({groups: []});
      if (url.pathname === "/admin/billing/plans") return send({plans: []});
      if (url.pathname === "/admin/billing/users") return send({users: [owner, member]});
      if (url.pathname === "/admin/model-catalog") return send({models: [{id: "gpt-5.6"}, {id: "claude-sonnet-5"}]});
      if (url.pathname === "/admin/usage") return send({summary: {requests: 12, tokens: 345678, error_rate: 0.02}, requests: usageRequests});
      if (url.pathname === "/admin/usage/models") return send(usageModels);
      if (url.pathname === "/admin/usage/global") return send({summary: {usage: {tokens: 1234567, actual_cost_usd: "88.12", requests: 345}, active_users: 4, total_users: 9, pricing_coverage: 1}, rows: []});
      if (["/admin/upstream-accounts", "/admin/anthropic-accounts", "/admin/antigravity-accounts"].includes(url.pathname)) {
        return send({all: true, until: at(0), allocation_from: at(-24 * hour), allocation_until: at(0), quota_guard_safety_percent: 5,
          accounts: url.pathname === "/admin/upstream-accounts" ? codexAccounts : []});
      }
      if (url.pathname === "/admin/upstream-accounts/concurrency") return send({sampled_at: at(0), accounts: []});
      if (url.pathname === "/admin/alerts") return send({alerts: []});
      if (url.pathname === "/admin/groups" && request.method() === "GET") return send({groups: [group]});
      if (url.pathname === "/admin/groups/research" && request.method() === "GET") return send({...group, members});
      if (url.pathname === "/admin/groups/research/upstream-access") {
        return send({group_id: "research", providers: ["codex", "antigravity", "anthropic"].map((provider) => ({group_id: "research", provider,
          strategy: provider === "codex" ? "selected" : "all", mode: provider === "codex" ? "selected" : "all",
          account_ids: provider === "codex" ? ["0123456789abcdef", "1123456789abcdef"] : [],
          accounts: provider === "codex" ? codexAccounts.map(({id, alias, email_masked, status}) => ({id, alias, email_masked, status})) : []}))});
      }
      if (url.pathname === "/admin/groups/research/periods") return send({periods: [{id: "period-2", starts_at: group.period_starts_at, ends_at: group.period_ends_at, used_usd: group.used_usd, group_charged_usd: group.used_usd}]});
      if (url.pathname === "/admin/groups/research/account-stats") return send({group_id: "research", accounts: [], data_complete: true});
      if (url.pathname === "/admin/groups/research/usage-breakdown") {
        breakdownQueries.push(url.search);
        return send(url.searchParams.get("period_id") === "period-1" ? previousBreakdown : groupBreakdown);
      }
      unexpected.push(`${request.method()} ${url.pathname}`);
      return send({error: {type: "invalid_request_error", code: "not_found", message: "未模拟的请求"}}, 404);
    });
    const capture = async (selector, name) => {
      await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); });
      // Clip from a full-page capture so the sticky top bar cannot cover the section.
      const clip = await page.locator(selector).first().evaluate((node) => {
        const box = node.getBoundingClientRect();
        return {x: box.left + scrollX, y: box.top + scrollY, width: box.width, height: box.height};
      });
      await page.screenshot({path: path.join(screenshots, name), fullPage: true, clip});
    };
    const noOverflow = async (label) => assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, label);
    const open = async (hash, user, load) => {
      identity = user;
      // A distinct query string forces a fresh document, so no state or
      // listener from the previous scene survives the hash change.
      await page.goto(`${origin}/?scene=${encodeURIComponent(hash)}-${user.id}#${hash}`);
      await page.evaluate(async ([value, script]) => {
        bindUI(); initializeDateFilters(); renderState(value); routeFromHash(false);
        await (0, eval)(script)();
        setConnection("已连接", "ok");
      }, [initial(user), `async () => { ${load} }`]);
    };
    const dataTime = /^数据时间：\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}$/;

    // Overview: both balance cards show when the balance was read and refresh alone.
    await open("overview", member, "await loadOverview();");
    assert.match(await page.locator("#overview-billing-updated").textContent(), dataTime);
    assert.match(await page.locator("#overview-subscriptions-updated").textContent(), dataTime);
    const before = reads.length;
    await page.locator("[data-overview-billing-refresh]").first().click();
    await page.waitForFunction(() => /^数据时间/.test(document.getElementById("overview-billing-updated").textContent));
    assert.deepEqual(reads.slice(before), ["/admin/billing/me"], "the balance refresh reads only billing");
    await noOverflow("overview desktop");
    await capture(".overview-personal-metrics", "overview-quota-freshness-desktop.png");

    // Billing: cash, subscriptions and group cards share one data time.
    await open("billing", member, "await loadBillingDashboard();");
    await page.locator(".billing-group-card").waitFor();
    for (const id of ["#billing-cash-time", "#billing-subscriptions-time"]) assert.match(await page.locator(id).textContent(), dataTime);
    assert.match(await page.locator("#billing-group .quota-freshness-time").textContent(), dataTime);
    await page.locator("#billing-subscriptions-time + .quota-refresh").click();
    await page.waitForFunction(() => !billingDetailLoading && /^数据时间/.test(document.getElementById("billing-subscriptions-time").textContent));
    await noOverflow("billing desktop");
    await capture('[data-section="billing"]', "billing-quota-freshness-desktop.png");
    await page.setViewportSize({width: 390, height: 844});
    await noOverflow("billing mobile");
    await capture(".billing-period-panel", "billing-quota-freshness-mobile.png");
    await page.setViewportSize({width: 1440, height: 1000});

    // Groups: the list and the selected group each show their read time.
    await open("groups", owner, 'await loadGroups("research");');
    await page.waitForFunction(() => document.getElementById("group-detail-time").dataset.state === "ready");
    assert.match(await page.locator("#groups-list-time").textContent(), dataTime);
    assert.match(await page.locator("#group-detail-time").textContent(), dataTime);
    await page.locator("#group-detail-refresh").click();
    await page.locator("#groups-message").filter({hasText: "本群组额度已刷新"}).waitFor();
    await page.waitForFunction(() => document.getElementById("group-usage-time").dataset.state === "ready");
    await noOverflow("groups desktop");
    await capture(".groups-layout", "groups-quota-freshness-desktop.png");

    // Group usage card: members' share of the group quota and of each pooled account.
    const quotaBar = page.locator("#group-usage-body .share-bar").first();
    assert.equal(await quotaBar.locator(".share-bar-segment").count(), 4, "members with usage plus the removed member");
    await quotaBar.locator(".share-bar-segment").first().hover();
    assert.match(await quotaBar.locator(".share-bar-detail").textContent(), /^Alice：本期群组支付 US\$24\.10，占群组额度 24\.1%，占已用 \d+\.\d%$/);
    const mainAccount = page.locator('.group-usage-account[data-account-id="0123456789abcdef"]');
    assert.match(await mainAccount.textContent(), /总成本 US\$71\.30 · 群组成员 74\.2% · 非群组用户 25\.8%/);
    const outside = mainAccount.locator('.share-bar-segment[data-tone="outside"]');
    await outside.click();
    assert.match(await mainAccount.locator(".share-bar-detail").textContent(), /^非群组用户：在此账号本周期成本 US\$18\.40，占 25\.8%$/);
    const tones = await page.locator("#group-usage-body .share-bar").evaluateAll((bars) => bars.map((bar) => [...bar.querySelectorAll(".share-bar-segment")].map((node) => node.dataset.tone)));
    assert.equal(tones[1][0], tones[0][0], "Alice keeps one color across bars");
    await noOverflow("group usage desktop");
    await capture("#group-usage-visual", "group-usage-visual-desktop.png");
    await page.setViewportSize({width: 390, height: 844});
    await noOverflow("group usage mobile");
    await capture("#group-usage-visual", "group-usage-visual-mobile.png");
    await page.setViewportSize({width: 1440, height: 1000});
    await page.locator("#group-usage-period").selectOption("period-1");
    await page.waitForFunction(() => /历史周期/.test(document.getElementById("group-usage-body").textContent));
    assert.equal(breakdownQueries.at(-1), "?period_id=period-1");
    await page.locator("#group-usage-period").selectOption("period-2");
    await page.waitForFunction(() => /当前周期/.test(document.getElementById("group-usage-body").textContent));
    assert.equal(breakdownQueries.at(-1), "", "the newest period follows the group");

    // Usage: the 24-hour per-model charges come first; the request list is opt-in.
    const usageStart = reads.length;
    await open("usage", member, "");
    await page.waitForFunction(() => document.getElementById("usage-models-time").dataset.state === "ready");
    assert.equal(await page.locator("#usage-details").isVisible(), false, "request list starts collapsed");
    assert.equal(reads.slice(usageStart).includes("/admin/usage"), false, "the request list is not read until expanded");
    assert.equal(await page.locator("#usage-models-charged").textContent(), "US$6.4182");
    assert.equal(await page.locator("#usage-models-share .share-bar-segment").count(), 4, "models without charges get no segment");
    const widths = await page.locator("#usage-models-share .share-bar-segment").evaluateAll((nodes) => nodes.map((node) => Math.round(node.getBoundingClientRect().width)));
    assert.ok(widths[0] > widths[1] && widths[1] > widths[2], `segment widths follow charges: ${widths}`);
    await page.locator("#usage-models-share .share-bar-segment").nth(1).hover();
    assert.match(await page.locator("#usage-models-share .share-bar-detail").textContent(), /^claude-sonnet-5：US\$1\.6407，占 25\.6%$/);
    await page.locator("#usage-models-share .share-bar-segment").nth(1).click();
    await noOverflow("usage desktop");
    await capture("#usage-models-panel", "usage-models-desktop.png");
    await page.setViewportSize({width: 390, height: 844});
    await noOverflow("usage mobile");
    await capture("#usage-models-panel", "usage-models-mobile.png");
    await page.setViewportSize({width: 1440, height: 1000});
    await page.locator("#usage-details-toggle").click();
    await page.locator("#usage-rows tr").nth(3).waitFor();
    assert.equal(await page.locator("#usage-details-toggle").textContent(), "收起请求明细");
    assert.equal(reads.slice(usageStart).filter((value) => value === "/admin/usage").length, 1);
    await capture('[data-section="usage"]', "usage-details-expanded-desktop.png");
    await page.locator("#usage-details-toggle").click();
    assert.equal(await page.locator("#usage-details").isVisible(), false);

    // Upstream accounts: the list states when its status and costs were read.
    await open("upstream-accounts", owner, 'byId("upstream-account-filter").elements.range.value = "all"; syncUpstreamAccountRange(); await loadUpstreamAccounts(upstreamAccountQueryFromForm());');
    assert.match(await page.locator("#upstream-account-list-time").textContent(), dataTime);
    await page.locator("#upstream-account-list-refresh").click();
    await page.waitForFunction(() => !upstreamAccountListLoading);
    await noOverflow("upstream desktop");
    await capture("#upstream-account-manager", "upstream-accounts-freshness-desktop.png");

    assert.deepEqual(errors, []);
    if (unexpected.length) console.log(`unmocked requests answered 404: ${[...new Set(unexpected)].join(", ")}`);
  } finally {
    await browser.close();
  }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
