"use strict";

// Optional browser regression. All requests are intercepted and contain only
// synthetic users/accounts; no upstream service or real database is contacted.
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
const users = [
  {id: "alice", username: "alice", display_name: "设计组 · Alice", group_id: "design"},
  {id: "bob", username: "bob", display_name: "设计组 · Bob", group_id: null},
  {id: "carol", username: "carol", display_name: "研发组 · Carol", group_id: "engineering"},
];
const groups = [{id: "design", name: "产品设计团队", period: "month", custom_days: 0, limit_usd: "100.000000000000", used_usd: "42.500000000000", remaining_usd: "57.500000000000", member_count: 1,
  member_limit_usd: "100.000000000000",
  starts_at: "2026-09-01T08:00:00Z", period_starts_at: "2026-09-01T08:00:00Z", period_ends_at: "2026-10-02T08:00:00Z", members: []}];
const account = {id: "a1b2c3d4e5f60718", email_masked: "a***@example.test", plan: "Pro", status: "available", can_manage: true,
  access_mode: "exclusive", authorized_user_ids: ["alice"], allocation_weight: 1, rolling_cost_usd: "42.5", rolling_cost_share: "1", target_share: "1"};

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1080}, locale: "zh-CN"});
    const errors = [], writes = [], idempotency = new Map();
    let failCreateOnce = false, failAccounts = false, delayNextGroupRead = false, releaseGroupRead;
    let delayOldList = false, releaseOldList;
    const memberUsage = {design: {alice: "12.500000000000"}};
    const detail = (group) => ({...group, members: users.filter((u) => u.group_id === group.id).map((u) => {
      const used = memberUsage[group.id]?.[u.id] || "0";
      return {user_id: u.id, username: u.username, display_name: u.display_name, used_usd: used,
        remaining_usd: group.member_limit_usd == null ? null : Math.max(0, Number(group.member_limit_usd) - Number(used)).toFixed(12)};
    })});
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("dialog", (dialog) => dialog.accept());
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
      if (url.pathname === "/admin/usage") return send({summary: {requests: 0}, requests: []});
      if (url.pathname === "/admin/upstream-accounts/concurrency") return send({sampled_at: new Date().toISOString(), accounts: [{id: account.id, active_requests: 0}]});
      if (url.pathname === "/admin/billing/users") return send({users});
      if (url.pathname === "/admin/upstream-accounts") return failAccounts ? send({error: {message: "模拟刷新失败"}}, 503) : send({accounts: [account], all: true});
      if (url.pathname.endsWith("/access")) {
        const body = request.postDataJSON(); writes.push({path: url.pathname, body});
        account.access_mode = body.mode; account.authorized_user_ids = body.user_ids;
        return send({mode: body.mode, user_ids: body.user_ids});
      }
      if (url.pathname === "/admin/groups" && request.method() === "GET") {
        if (delayOldList) {
          delayOldList = false;
          await new Promise((resolve) => { releaseOldList = resolve; });
          return send({error: {code: "invalid_session", message: "旧身份已过期"}}, 401);
        }
        return send({groups});
      }
      if (url.pathname.startsWith("/admin/groups")) {
        const id = url.pathname.split("/")[3];
        const group = groups.find((g) => g.id === id);
        if (request.method() === "GET") {
          if (delayNextGroupRead) {
            delayNextGroupRead = false;
            const snapshot = JSON.parse(JSON.stringify(detail(group)));
            await new Promise((resolve) => { releaseGroupRead = resolve; });
            return send(snapshot);
          }
          return send(detail(group));
        }
        const body = request.postDataJSON(); writes.push({path: url.pathname, body});
        if (idempotency.has(body.operation_id)) return send(idempotency.get(body.operation_id));
        let result;
        if (url.pathname.endsWith("/members")) {
          body.user_ids.forEach((id) => { users.find((u) => u.id === id).group_id = body.action === "add" ? group.id : null; });
          group.member_count = users.filter((u) => u.group_id === group.id).length;
          result = detail(group);
        } else if (request.method() === "POST") {
          const created = {...groups[0], ...body, id: "new-team", used_usd: "0", remaining_usd: body.limit_usd, member_count: 0, members: []};
          groups.push(created); result = detail(created);
        } else if (request.method() === "DELETE") {
          group.archived_at = "2026-09-21T08:00:00Z"; result = detail(group);
        } else {
          const resets = body.period !== group.period || body.custom_days !== group.custom_days || body.starts_at;
          Object.assign(group, body);
          if (resets) {
            group.used_usd = "0";
            memberUsage[group.id] = {};
            group.period_starts_at = "2026-09-21T08:00:00Z";
            group.period_ends_at = "2026-10-05T08:00:00Z";
          }
          group.remaining_usd = Math.max(0, Number(group.limit_usd) - Number(group.used_usd)).toFixed(12);
          result = detail(group);
        }
        idempotency.set(body.operation_id, result);
        if (failCreateOnce && request.method() === "POST") { failCreateOnce = false; return send({error: {message: "模拟提交后响应丢失"}}, 503); }
        return send(result);
      }
      throw new Error(`Unexpected mocked request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#groups");
    await page.evaluate(async (value) => { bindUI(); initializeDateFilters(); renderState(value); await loadGroups(); }, initialState);
    assert.match(await page.locator("#group-member-list").textContent(), /本期群组已用 US\$12\.50.*上限剩余 US\$87\.50/);
    await page.getByLabel("选择用户 alice", {exact: true}).check();
    await page.locator("#group-member-search").fill("bob");
    await page.getByLabel("选择用户 bob", {exact: true}).check();
    assert.match(await page.locator("#group-selected-count").textContent(), /2/);
    await page.locator("#group-member-search").fill("");
    assert.equal(await page.getByLabel("选择用户 carol", {exact: true}).isDisabled(), true);
    await page.locator("#group-members-form input[name=reason]").fill("加入产品团队");
    await page.locator("#group-members-add").click();
    await page.waitForFunction(() => managedGroup?.member_count === 2 && !groupOperation);
    assert.deepEqual(writes[0].body.user_ids, ["bob"]);
    assert.equal(await page.locator("#group-archive").isDisabled(), true);

    await page.locator("#group-edit").click();
    assert.equal(await page.locator("#group-form input[name=member_limit_usd]").inputValue(), "100");
    await page.locator("#group-form input[name=reason]").fill("调整团队预算");
    await page.locator("#group-form input[name=member_limit_usd]").fill("-1");
    const beforeInvalidCap = writes.length;
    await page.locator("#group-form button[type=submit]").click();
    assert.equal(writes.length, beforeInvalidCap);
    assert.match(await page.locator("#group-form .form-message").textContent(), /成员上限必须/);
    await page.locator("#group-form input[name=member_limit_usd]").fill("50");
    await page.locator("#group-form input[name=limit_usd]").fill("125.50");
    await page.locator("#group-form input[name=reason]").fill("调整团队预算");
    await page.locator("#group-form button[type=submit]").click();
    await page.waitForFunction(() => !byId("group-dialog").open);
    assert.equal(writes.at(-1).body.limit_usd, "125.50");
    assert.equal(writes.at(-1).body.member_limit_usd, "50");
    assert.equal("starts_at" in writes.at(-1).body, false);
    assert.equal(groups[0].used_usd, "42.500000000000");
    await page.waitForFunction(() => managedGroup?.id === "design" && managedGroup.member_limit_usd === "50");
    assert.match(await page.locator("#group-member-list").textContent(), /本期群组已用 US\$12\.50.*上限剩余 US\$37\.50/);

    await page.locator("#group-edit").click();
    await page.locator("#group-form input[name=member_limit_usd]").fill("");
    await page.locator("#group-form select[name=period]").selectOption("custom");
    await page.locator("#group-form input[name=custom_days]").fill("14");
    await page.locator("#group-form input[name=reason]").fill("采用双周周期");
    await page.locator("#group-form button[type=submit]").click();
    await page.waitForFunction(() => !byId("group-dialog").open);
    assert.equal(writes.at(-1).body.custom_days, 14);
    assert.equal(writes.at(-1).body.member_limit_usd, null);
    await page.waitForFunction(() => managedGroup?.period === "custom" && managedGroup.member_limit_usd === null);
    assert.match(await page.locator("#group-member-list").textContent(), /本期群组已用 US\$0\.00.*上限剩余 不限/);

    failCreateOnce = true;
    await page.locator("#group-create").click();
    await page.locator("#group-form input[name=name]").fill("新项目团队");
    await page.locator("#group-form input[name=limit_usd]").fill("200");
    await page.locator("#group-form input[name=reason]").fill("新项目预算");
    await page.locator("#group-form button[type=submit]").click();
    await page.waitForFunction(() => byId("group-form").dataset.busy === "false");
    const operationID = writes.at(-1).body.operation_id;
    assert.match(await page.locator("#group-form .form-message").textContent(), /响应丢失/);
    await page.locator("#group-form button[type=submit]").click();
    await page.waitForFunction(() => !byId("group-dialog").open);
    assert.equal(writes.at(-1).body.operation_id, operationID);
    assert.equal(groups.filter((g) => g.id === "new-team").length, 1);
    assert.equal(writes.at(-1).body.member_limit_usd, null);

    await page.waitForFunction(() => managedGroup?.id === "new-team");
    await page.locator("#group-edit").click();
    await page.locator("#group-form input[name=limit_usd]").fill("0");
    await page.locator("#group-form input[name=member_limit_usd]").fill("0");
    await page.locator("#group-form input[name=reason]").fill("暂停新团队用量");
    await page.locator("#group-form button[type=submit]").click();
    await page.waitForFunction(() => !byId("group-dialog").open);
    assert.equal(writes.at(-1).body.limit_usd, "0");
    assert.equal(writes.at(-1).body.member_limit_usd, "0");
    await page.waitForFunction(() => managedGroup?.id === "new-team" && managedGroup.limit_usd === "0" && managedGroup.member_limit_usd === "0");

    await page.evaluate(async () => { await loadGroups("design"); });
    delayNextGroupRead = true;
    await page.evaluate(() => { window.pendingGroupRead = loadGroupDetail("design"); });
    await page.locator("#group-edit").click();
    await page.locator("#group-form input[name=limit_usd]").fill("150");
    await page.locator("#group-form input[name=member_limit_usd]").fill("100");
    await page.locator("#group-form input[name=reason]").fill("调整双周预算");
    await page.locator("#group-form button[type=submit]").click();
    await page.waitForFunction(() => managedGroup?.id === "design" && managedGroup.limit_usd === "150" && managedGroup.member_limit_usd === "100" && !byId("group-dialog").open);
    assert.equal(typeof releaseGroupRead, "function");
    releaseGroupRead();
    await page.evaluate(async () => { await window.pendingGroupRead; });
    assert.equal(await page.evaluate(() => managedGroup.limit_usd), "150", "late reads cannot overwrite a completed mutation");

    await page.evaluate(async () => { await loadGroupDetail("design"); });
    await page.screenshot({path: path.join(screenshots, "groups-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    await page.screenshot({path: path.join(screenshots, "groups-mobile.png"), fullPage: true});

    await page.setViewportSize({width: 1440, height: 1080});
    await page.locator("#group-edit").click();
    assert.equal(await page.getByLabel("单个成员周期用量上限（USD）").inputValue(), "100");
    await page.screenshot({path: path.join(screenshots, "groups-form-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.getByLabel("单个成员周期用量上限（USD）").isVisible(), true);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    await page.screenshot({path: path.join(screenshots, "groups-form-mobile.png"), fullPage: true});
    await page.locator("#group-form [data-close]").click();

    await page.evaluate(() => {
      location.hash = "billing"; routeFromHash(false);
      renderBillingDetail({user: state.user, cash_balance_usd: "0", subscriptions: {}, group: {...managedGroup, used_usd: "95", remaining_usd: "55"},
        group_member_used_usd: "95", group_member_remaining_usd: "5", ledger_entries: [
          {entry_type: "usage_charge", request_id: "split-request", actual_cost_usd: "20", charged_usd: "18", group_charged_usd: "5", personal_charged_usd: "13", uncovered_usd: "2"},
          {entry_type: "usage_charge", request_id: "group-only", actual_cost_usd: "3", charged_usd: "3", group_charged_usd: "3", personal_charged_usd: "0", uncovered_usd: "0"},
          {entry_type: "cash_adjustment", amount_usd: "10", group_charged_usd: null, personal_charged_usd: null},
        ]});
    });
    assert.match(await page.locator("#billing-group").textContent(), /本用户本期群组已用US\$95\.00.*本用户本期上限剩余US\$5\.00/);
    assert.match(await page.locator(".billing-order-card").textContent(), /群组 → 个人日 → 周 → 月 → 现金/);
    await page.locator("#billing-ledger-rows .record-details > summary").first().click();
    assert.match(await page.locator("#billing-ledger-rows tr").first().textContent(), /群组支付：US\$5\.00.*个人支付：US\$13\.00.*未覆盖：US\$2\.00/);
    assert.match(await page.locator("#billing-ledger-rows tr").nth(1).textContent(), /个人支付：US\$0\.00/);
    assert.doesNotMatch(await page.locator("#billing-ledger-rows tr").nth(2).textContent(), /群组支付|个人支付/);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    await page.screenshot({path: path.join(screenshots, "groups-billing-mobile.png"), fullPage: true});
    await page.setViewportSize({width: 1440, height: 1080});
    await page.screenshot({path: path.join(screenshots, "groups-billing-desktop.png"), fullPage: true});

    await page.setViewportSize({width: 1440, height: 1080});
    await page.evaluate(async () => { location.hash = "upstream-accounts"; routeFromHash(false); await loadUpstreamAccounts(new URLSearchParams({all: "true"})); });
    await page.locator(".upstream-account-details > summary").click();
    await page.locator(".upstream-access-button").click();
    await page.locator("#upstream-access-dialog").waitFor({state: "visible"});
    assert.equal(await page.getByLabel("授权用户 alice", {exact: true}).isChecked(), true);
    await page.locator("#upstream-access-search").fill("bob");
    await page.getByLabel("授权用户 bob", {exact: true}).check();
    await page.locator("#upstream-access-search").fill("");
    assert.equal(await page.getByLabel("授权用户 alice", {exact: true}).isChecked(), true);
    await page.locator("#upstream-access-form input[name=reason]").fill("产品团队专属账号");
    await page.screenshot({path: path.join(screenshots, "upstream-exclusive-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.screenshot({path: path.join(screenshots, "upstream-exclusive-mobile.png"), fullPage: true});
    await page.locator("#upstream-access-clear").click();
    const before = writes.length;
    await page.locator("#upstream-access-form button[type=submit]").click();
    assert.equal(writes.length, before);
    assert.match(await page.locator("#upstream-access-form .form-message").textContent(), /至少/);
    await page.getByLabel("授权用户 bob", {exact: true}).check();
    await page.locator("#upstream-access-form button[type=submit]").click();
    await page.waitForFunction(() => !upstreamAccountOperation);
    assert.deepEqual(account.authorized_user_ids, ["bob"]);

    await page.locator(".upstream-access-button").click();
    await page.locator("#upstream-access-dialog").waitFor({state: "visible"});
    await page.locator("#upstream-access-form select[name=mode]").selectOption("shared");
    await page.locator("#upstream-access-form input[name=reason]").fill("恢复共享");
    failAccounts = true;
    await page.locator("#upstream-access-form button[type=submit]").click();
    await page.waitForFunction(() => !upstreamAccountOperation);
    assert.equal(account.access_mode, "shared");
    assert.deepEqual(account.authorized_user_ids, []);
    assert.match(await page.locator("#upstream-account-refresh-message").textContent(), /权限已保存.*刷新失败/);
    await page.evaluate(() => { openGroupEditor(managedGroup); });
    assert.equal(await page.locator("#group-dialog").isVisible(), true);
    delayOldList = true;
    const oldReadStarted = page.waitForRequest((request) => new URL(request.url()).pathname === "/admin/groups");
    await page.evaluate(() => { window.oldGroupsRead = loadGroups().catch(() => {}); });
    await oldReadStarted;
    await page.evaluate((value) => { renderState({...value, user: {...value.user, role: "member"}}); }, initialState);
    assert.equal(typeof releaseOldList, "function");
    releaseOldList();
    await page.evaluate(async () => { await window.oldGroupsRead; });
    assert.equal(await page.evaluate(() => state?.user?.role), "member", "old 401 must not clear replacement identity");
    assert.equal(await page.locator("#group-dialog").isVisible(), false);
    assert.equal(await page.evaluate(() => managedGroup), null);
    assert.equal(await page.locator("#group-member-list").textContent(), "");
    assert.equal(await page.locator("#upstream-access-user-list").textContent(), "");
    await page.evaluate(() => handleUnauthorized());
    assert.equal(await page.locator("#group-member-list").textContent(), "");
    assert.equal(await page.locator("#upstream-access-user-list").textContent(), "");
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: await browser.version(), writes: writes.length, checks: "groups, nullable/zero/invalid member caps, independent member usage, split ledger, idempotent retry, membership, period reset, exclusivity, refresh failure, desktop/mobile forms, logout", errors}));
  } finally { await browser.close(); }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
