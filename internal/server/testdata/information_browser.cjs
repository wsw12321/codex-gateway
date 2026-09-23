"use strict";

// Optional real-browser regression suite. All traffic is intercepted at the
// synthetic origin; no account, database, or upstream service is contacted.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const initialState = {user: {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"},
  recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const cutoff = "2026-06-24T00:00:00Z";
const report = {retention_days: 90, cutoff,
  delete_counts: {usage_requests: 12408, billing_ledger_entries: 12317, billing_operations: 82, billing_charge_allocations: 12408, usage_daily: 450, audit_events: 82},
  retained_counts: {usage_requests: 2, billing_ledger_entries: 16, billing_operations: 16},
  retained_reasons: {current_balance: 12, active_subscription: 4, unsettled_request: 2}};
const candidates = [
  {id: "user-alpha", username: "alpha", display_name: "测试成员甲", status: "active", created_at: "2026-08-12T09:00:00Z"},
  {id: "user-beta", username: "beta", display_name: "测试成员乙", status: "disabled", created_at: "2026-08-13T09:00:00Z"},
];
const accounts = [
  {id: "a1b2c3d4e5f60001", email_masked: "al***@example.test", status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available", can_manage: true, plan: "Plus", allocation_weight: 1},
  {id: "a1b2c3d4e5f60002", email_masked: "be***@example.test", status: "unavailable", cliproxy_status: "active", gateway_manual_status: "manual_disabled", gateway_quota_status: "available", can_manage: true, plan: "Pro", allocation_weight: 2},
  {id: "a1b2c3d4e5f60003", email_masked: "ga***@example.test", status: "available", cliproxy_status: "active", gateway_manual_status: "enabled", gateway_quota_status: "available", can_manage: true, plan: "Plus", allocation_weight: 1},
].map((account) => ({last_synced_at: "2026-09-22T09:00:00Z", request_count: 384, input_tokens: 78000, output_tokens: 24500,
  equivalent_cost_usd: "18.732", rolling_cost_usd: "2.43", rolling_cost_share: "0.25", target_share: "0.25", ...account}));

async function main() {
  const browser = await chromium.launch({headless: true});
  try {
    fs.mkdirSync(screenshots, {recursive: true});
    const context = await browser.newContext({viewport: {width: 1440, height: 1100}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [], previews = [], jobs = [], deletions = [], events = [], confirms = [];
    let users = [...candidates], latestJob = null, failCreate = true, blockDeletion = false, jobReads = 0;
    let concurrencyReads = 0, failConcurrency = false;
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("dialog", async (dialog) => { confirms.push(dialog.message()); await dialog.accept(); });
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/information") return send({latest_job: latestJob, cleaned_before: latestJob?.status === "completed" ? cutoff : null});
      if (url.pathname === "/admin/information/preview") { previews.push(request.postDataJSON()); return send(report); }
      if (url.pathname === "/admin/information/jobs") {
        jobs.push(request.postDataJSON()); events.push("job");
        if (failCreate) return send({error: {message: "模拟网络结果未确认"}}, 503);
        latestJob = {id: "cleanup-test-1", retention_days: 90, cutoff, status: "running", report,
          created_at: "2026-09-22T09:00:00Z", updated_at: "2026-09-22T09:00:00Z"};
        return send(latestJob);
      }
      if (url.pathname === "/admin/information/jobs/cleanup-test-1") {
        jobReads++;
        latestJob.status = "completed";
        latestJob.updated_at = "2026-09-22T09:01:00Z";
        return send(latestJob);
      }
      if (url.pathname === "/admin/information/deletable-users") {
        const query = url.searchParams.get("q");
        return send({users: users.filter((user) => !query || `${user.username} ${user.display_name}`.includes(query)), limit: 100, offset: 0});
      }
      if (url.pathname === "/admin/information/users/delete") {
        const body = request.postDataJSON(); deletions.push(body);
        if (blockDeletion) {
          users = [candidates[1]];
          return send({error: {code: "users_not_deletable", message: "用户状态已变化，整批取消"},
            blockers: [{user_id: candidates[0].id, reasons: ["nonzero_balance"]}]}, 409);
        }
        users = users.filter((user) => !body.user_ids.includes(user.id));
        return send({deleted_count: body.user_ids.length, user_ids: body.user_ids});
      }
      if (url.pathname === "/auth/password/reauth") { events.push("verified"); return send({ok: true}); }
      if (url.pathname === "/admin/upstream-accounts") return send({accounts, all: true});
      if (url.pathname === "/admin/upstream-accounts/concurrency") {
        concurrencyReads++;
        if (failConcurrency) return send({error: {message: "旧兼容服务暂不支持"}}, 503);
        return send({sampled_at: new Date().toISOString(), accounts: [{id: accounts[0].id, active_requests: 3 + concurrencyReads}, {id: accounts[1].id, active_requests: 0}]});
      }
      throw new Error(`Unexpected mocked request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("http://127.0.0.1:8765/#information");
    await page.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, initialState);
    await page.waitForFunction(() => informationUsersReady);
    assert.equal(await page.locator("#information-user-rows tr").count(), 2);
    assert.equal(await page.locator('[name="retention_days"]').inputValue(), "90");
    for (const invalid of ["0", "-1", "1.5", "1e2", "", "9007199254740993"]) {
      await page.locator('[name="retention_days"]').fill(invalid);
      await page.locator("#information-preview-button").click();
      await page.waitForFunction(() => byId("information-preview-form").dataset.busy === "false");
      assert.match(await page.locator("#information-preview-form .form-message").textContent(), /正整数/);
    }
    assert.equal(previews.length, 0);
    await page.locator('[name="retention_days"]').fill("90");
    await page.locator("#information-preview-button").click();
    await page.locator("#information-preview").waitFor({state: "visible"});
    assert.deepEqual(previews, [{retention_days: 90}]);
    assert.match(await page.locator("#information-preview-cutoff").textContent(), /2026-06-24 00:00:00 UTC/);
    assert.match(await page.locator("#information-preview-report").textContent(), /支撑当前余额/);
    await page.evaluate(() => { state.recently_verified = false; });
    await page.locator("#information-create-job").click();
    await page.locator("#reauth-dialog").waitFor({state: "visible"});
    assert.equal(jobs.length, 0);
    await page.locator('#reauth-form input[name="password"]').fill("synthetic-test-password");
    await page.locator('#reauth-form button[type="submit"]').click();
    await page.waitForFunction(() => !informationOperation);
    assert.deepEqual(events.slice(0, 2), ["verified", "job"]);
    assert.match(confirms[0], /2026-06-24 00:00:00 UTC/);
    failCreate = false;
    await page.locator("#information-create-job").click();
    await page.waitForFunction(() => informationJob?.status === "running");
    assert.equal(jobs[0].operation_id, jobs[1].operation_id, "ambiguous result retry must reuse the operation ID");
    assert.equal(jobs[1].cutoff, cutoff);
    assert.equal(await page.locator("#information-preview-button").isDisabled(), true);
    // Execute the existing timer callback after its actual five-second interval.
    await page.waitForFunction(() => informationJob?.status === "completed", {timeout: 12000});
    assert.equal(jobReads, 1);
    assert.equal(await page.locator("#information-preview-button").isDisabled(), false);

    await page.locator("#information-select-all").check();
    assert.match(await page.locator("#information-selected-count").textContent(), /2 \/ 100/);
    blockDeletion = true;
    await page.locator("#information-delete-users").click();
    await page.waitForFunction(() => informationUsersReady && informationUsers.length === 1);
    assert.deepEqual(deletions[0].user_ids, ["user-alpha", "user-beta"]);
    assert.match(await page.locator("#information-message").textContent(), /整批删除未完成.*余额不为零/);
    assert.equal(await page.locator("#information-delete-users").isDisabled(), true);
    blockDeletion = false;
    await page.locator("#information-select-all").check();
    await page.locator("#information-delete-users").click();
    await page.waitForFunction(() => informationUsersReady && informationUsers.length === 0);
    assert.match(await page.locator("#information-message").textContent(), /已永久删除 1 位用户/);
    users = [...candidates];
    await page.locator("#information-refresh").click();
    await page.waitForFunction(() => informationUsersReady && informationUsers.length === 2);
    await page.locator("#information-preview-button").click();
    await page.locator("#information-preview").waitFor({state: "visible"});
    await page.evaluate(() => { hide("notice"); window.scrollTo(0, 0); });
    await page.screenshot({path: path.join(screenshots, "information-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.evaluate(() => window.scrollTo(0, 0));
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    await page.screenshot({path: path.join(screenshots, "information-mobile.png"), fullPage: true});

    await page.setViewportSize({width: 1440, height: 1100});
    await page.evaluate(async () => { location.hash = "upstream-accounts"; await loadUpstreamAccounts(new URLSearchParams("all=true")); });
    await page.waitForFunction(() => upstreamConcurrencySnapshot !== null);
    const card = (id) => page.locator(`[data-account-id="${id}"]`);
    assert.equal(await card(accounts[1].id).locator(".upstream-concurrency-count").textContent(), "0");
    assert.equal(await card(accounts[2].id).locator(".upstream-concurrency-count").textContent(), "暂不可用");
    await card(accounts[0].id).locator(".upstream-allocation-input").fill("37");
    const sampled = concurrencyReads;
    await page.waitForFunction((previous) => Number(document.querySelector(".upstream-concurrency-count").textContent) > previous + 3, sampled, {timeout: 12000});
    assert.equal(await card(accounts[0].id).locator(".upstream-allocation-input").inputValue(), "37");
    failConcurrency = true;
    await page.waitForFunction(() => document.querySelector(".upstream-concurrency-count").textContent === "暂不可用", {timeout: 12000});
    failConcurrency = false;
    await page.evaluate(() => { stopUpstreamConcurrency(); startUpstreamConcurrency(); });
    await page.waitForFunction(() => upstreamConcurrencySnapshot !== null);
    await page.screenshot({path: path.join(screenshots, "upstream-concurrency-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    await page.screenshot({path: path.join(screenshots, "upstream-concurrency-mobile.png"), fullPage: true});
    await page.evaluate(() => { location.hash = "usage"; });
    await page.waitForFunction(() => !upstreamConcurrencyPolling);
    const callsAfterLeave = concurrencyReads;
    await page.evaluate(() => new Promise((resolve) => setTimeout(resolve, 5100)));
    assert.equal(concurrencyReads, callsAfterLeave);

    await page.evaluate(() => renderPersonalUsage({summary: {p95_ttft_ms: null, p95_duration_ms: null}, requests: [
      {State: "failed", Model: "test-model", upstream_account_id: "a1b2c3d4e5f60001", upstream_masked_email: "al***@example.test"},
      {State: "completed", Model: "test-model", upstream_account_id: null, upstream_masked_email: null},
    ]}));
    assert.equal(await page.locator("#usage-rows tr").first().locator("td").count(), 9);
    assert.match(await page.locator("#usage-rows").textContent(), /al\*\*\*@example.test.*a1b2c3d4e5f60001.*未归因/);
    assert.equal(await page.locator("#metric-ttft").textContent(), "—");
    await page.evaluate((value) => {
      renderState({...value, user: {...value.user, role: "member"}});
      renderPersonalUsage({summary: {}, requests: [{State: "completed", upstream_account_id: "private-account", upstream_masked_email: "private-email"}]});
      location.hash = "information";
    }, initialState);
    await page.waitForFunction(() => location.hash === "#overview");
    assert.equal(await page.locator('nav a[href="#information"]').isVisible(), false);
    assert.equal(await page.locator("#usage-rows tr").first().locator("td").count(), 8);
    assert.doesNotMatch(await page.locator("#usage-rows").textContent(), /private-account|private-email/);
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: await browser.version(), previews: previews.length, jobs: jobs.length, deletions: deletions.length, concurrencyReads, errors}));
  } finally { await browser.close(); }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
