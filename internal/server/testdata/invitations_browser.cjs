"use strict";

// Synthetic browser fixtures only; no database or upstream account is contacted.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const owner = {user: {id: "owner", username: "owner", display_name: "团队管理员", role: "owner", status: "active"}, recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const member = {...owner, user: {id: "member", username: "alice", display_name: "林舟", role: "member", status: "active"}};
const group = {id: "design", name: "产品设计团队", members: [], period: "month", limit_usd: "100", used_usd: "0", remaining_usd: "100"};
const base = {kind: "member", group_id: null, group_name: "", created_at: "2026-09-29T12:00:00Z", expires_at: "2099-10-01T12:00:00Z", max_uses: 20, used_count: 3, requires_approval: true};
const makeApplication = (index) => ({id: `application-${index}`, invitation_id: "invite-member", user_id: `user-${index}`, username: `applicant-${index}`, display_name: `新同事 ${index}`, registered_at: "2026-09-29T13:00:00Z", applied_at: "2026-09-29T13:02:00Z", status: "pending"});

async function main() {
  fs.mkdirSync(screenshots, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1080}, locale: "zh-CN"});
    const errors = [], writes = [];
    const invitations = [{...base, id: "invite-member"}, {...base, id: "invite-group", kind: "group", group_id: group.id, group_name: group.name, used_count: 0}];
    let applications = [makeApplication(1), makeApplication(2), makeApplication(3)];
    let authenticated = false, currentKind = "group", reviewDelay = false, joinDelay = false, releaseReview, releaseJoin;
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("dialog", (dialog) => dialog.accept());
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      assert.equal(url.origin, "http://127.0.0.1:8765");
      const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType, body: typeof body === "string" ? body : JSON.stringify(body)});
      if (["/", "/join"].includes(url.pathname)) return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
      if (url.pathname === "/static/style.css") return send(fs.readFileSync(path.join(assets, "style.css"), "utf8"), 200, "text/css");
      if (url.pathname === "/static/theme.js") return send(fs.readFileSync(path.join(assets, "theme.js"), "utf8"), 200, "application/javascript");
      if (url.pathname === "/static/favicon.svg") return send(fs.readFileSync(path.join(assets, "favicon.svg"), "utf8"), 200, "image/svg+xml");
      if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      if (url.pathname === "/admin/state") return authenticated ? send(member) : send({error: {code: "session_required", message: "请登录"}}, 401);
      if (url.pathname === "/admin/groups") return send({groups: [group]});
      if (url.pathname === "/admin/invitations" && request.method() === "GET") {
        assert.equal(url.searchParams.get("limit"), "50");
        return send({invitations: invitations.filter((item) => !url.searchParams.get("kind") || item.kind === url.searchParams.get("kind")), limit: 50, offset: Number(url.searchParams.get("offset"))});
      }
      if (url.pathname === "/admin/invitations" && request.method() === "POST") {
        const body = request.postDataJSON(); writes.push({path: url.pathname, body});
        const created = {...base, ...body, id: "invite-created", group_name: body.group_id ? group.name : "", used_count: 0};
        invitations.push(created);
        return send({...created, token: "only-once-invite-token", link: "http://127.0.0.1:8765/join#token=only-once-invite-token"});
      }
      if (/\/admin\/invitations\/[^/]+\/applications/.test(url.pathname)) {
        assert.equal(url.searchParams.get("limit"), "50");
        return send({applications: url.pathname.includes("invite-member") ? applications : [], limit: 50, offset: 0});
      }
      if (url.pathname.endsWith("/review")) {
        const body = request.postDataJSON(); writes.push({path: url.pathname, body});
        if (reviewDelay) await new Promise((resolve) => { releaseReview = resolve; });
        applications = body.decision === "approve" ? applications.map((item) => body.application_ids.includes(item.id) ? {...item, status: "approved", reviewed_at: "2026-09-30T08:00:00Z"} : item) : applications.filter((item) => !body.application_ids.includes(item.id));
        invitations[0].used_count = applications.length;
        return send({updated_count: body.application_ids.length});
      }
      if (url.pathname.endsWith("/revoke")) {
        writes.push({path: url.pathname, body: request.postDataJSON()});
        invitations.find((item) => url.pathname.includes(item.id)).revoked_at = "2026-09-30T08:00:00Z";
        return send({ok: true});
      }
      if (url.pathname === "/auth/invitations/inspect") {
        assert.ok(request.postDataJSON().invitation_token);
        return send({...base, kind: currentKind, group_id: currentKind === "group" ? group.id : null, group_name: currentKind === "group" ? group.name : ""});
      }
      if (url.pathname === "/auth/password/login" || url.pathname === "/auth/login/finish") {
        writes.push({path: url.pathname, body: request.postDataJSON()}); authenticated = true; return send({ok: true});
      }
      if (url.pathname === "/auth/login/begin" || url.pathname === "/auth/register/begin") return send({flow_id: "fake-passkey-flow"});
      if (url.pathname === "/auth/password/register" || url.pathname === "/auth/register/finish") {
        writes.push({path: url.pathname, body: request.postDataJSON()});
        return send({status: "pending", requires_approval: true, recovery_codes: ["SAVE-THIS-RECOVERY-CODE"]});
      }
      if (url.pathname === "/auth/invitations/join") {
        writes.push({path: url.pathname, body: request.postDataJSON()});
        if (joinDelay) await new Promise((resolve) => { releaseJoin = resolve; });
        return send({status: "pending", application: {id: "joined-application", status: "pending"}});
      }
      if (url.pathname === "/auth/logout") { authenticated = false; return send({ok: true}); }
      throw new Error(`Unexpected mocked request: ${request.method()} ${url.pathname}`);
    });

    await page.goto("http://127.0.0.1:8765/#invitations");
    await page.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); }, owner);
    await page.waitForFunction(() => invitationList.length === 2 && !invitationListLoading);
    await page.locator("#invitation-create").click();
    await page.locator('#invitation-form input[name="max_uses"]').fill("6");
    await page.locator('#invitation-form input[name="requires_approval"]').check();
    await page.locator('#invitation-form input[name="expires_at"]').fill("2099-10-01T12:00");
    await page.locator('#invitation-form button[type="submit"]').click();
    await page.locator("#secret-dialog[open]").waitFor();
    assert.match(await page.locator("#secret-value").textContent(), /only-once-invite-token/);
    assert.equal(writes[0].body.max_uses, 6);
    assert.equal(writes[0].body.requires_approval, true);
    await page.locator("#save-secret").click();
    await page.waitForFunction(() => byId("secret-value").textContent === "");
    assert.equal(await page.locator("#secret-value").textContent(), "");
    assert.equal((await page.locator("body").innerText()).includes("only-once-invite-token"), false);

    await page.getByRole("button", {name: "查看 成员注册 invite-member 的申请", exact: true}).click();
    await page.getByLabel("选择 applicant-1 的申请", {exact: true}).check();
    await page.getByLabel("选择 applicant-2 的申请", {exact: true}).check();
    await page.locator("#invitation-approve").click();
    await page.waitForFunction(() => invitationApplications.filter((item) => item.status === "approved").length === 2 && !invitationOperation);
    assert.deepEqual(writes.at(-1).body.application_ids, ["application-1", "application-2"]);
    await page.getByRole("button", {name: "拒绝 applicant-3 的申请", exact: true}).click();
    await page.waitForFunction(() => invitationApplications.length === 2 && !invitationOperation);
    assert.match(await page.locator("#invitation-detail-summary").textContent(), /2 \/ 20/);
    await page.locator("#invitation-revoke").click();
    await page.waitForFunction(() => Boolean(selectedInvitation.revoked_at) && !invitationOperation);
    assert.equal(await page.locator("#invitation-revoke").isDisabled(), true);

    // Restock fixture applications for useful approval screenshots, including after expiry/revocation.
    applications.push(makeApplication(4), makeApplication(5));
    await page.evaluate(() => loadInvitationApplications(selectedInvitation));
    await page.evaluate(() => {byId("content").focus({preventScroll: true}); window.scrollTo(0, 0);});
    await page.screenshot({path: path.join(screenshots, "invitations-desktop.png"), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.screenshot({path: path.join(screenshots, "invitations-mobile.png"), fullPage: true});
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1));
    await page.setViewportSize({width: 1440, height: 1080});
    await page.evaluate((value) => {managedGroup = value; location.hash = "groups"; show("group-detail"); syncGroupControls();}, group);
    await page.locator("#group-invitation-create").click();
    await page.locator("#invitation-dialog[open]").waitFor();
    assert.equal(await page.locator('#invitation-form select[name="kind"]').inputValue(), "group");
    assert.equal(await page.locator('#invitation-form select[name="group_id"]').inputValue(), group.id);
    assert.equal(await page.locator('#invitation-form select[name="group_id"]').isDisabled(), true);
    await page.locator('#invitation-form button[data-close]').click();

    // Late review responses must not repopulate a cleared owner session.
    await page.evaluate(() => {location.hash = "invitations";});
    await page.getByRole("button", {name: "查看 成员注册 invite-member 的申请", exact: true}).click();
    reviewDelay = true;
    await page.getByRole("button", {name: "批准 applicant-4 的申请", exact: true}).click();
    await page.waitForFunction(() => invitationOperation);
    await page.evaluate(() => {loggingOut = true; identityGeneration++; resetInvitationManagement(); state = null;});
    while (!releaseReview) await new Promise((resolve) => setTimeout(resolve, 10));
    releaseReview();
    await page.waitForTimeout(100);
    assert.equal(await page.evaluate(() => invitationApplications.length), 0);
    assert.equal(await page.locator("#invitation-applications").textContent(), "");
    reviewDelay = false;

    for (const method of ["password", "passkey"]) {
      authenticated = false; currentKind = "group";
      await page.goto(`http://127.0.0.1:8765/join?scenario=group-${method}#token=group-private-token&kind=member`);
      await page.evaluate(async () => {bindUI(); getPasskey = async () => ({id: "credential"}); await configureInvitationView();});
      assert.equal(new URL(page.url()).hash, "");
      assert.equal(await page.evaluate(() => invitationToken), "group-private-token");
      assert.equal(await page.locator("#join-view").isVisible(), false);
      assert.equal(await page.locator("#login-view").isVisible(), true);
      if (method === "password") {
        await page.locator('#password-login-form input[name="username"]').fill("alice");
        await page.locator('#password-login-form input[name="password"]').fill("correct-password");
        await page.locator('#password-login-form button[type="submit"]').click();
      } else await page.locator("#login").click();
      await page.locator("#group-invitation-join").waitFor({state: "visible"});
      assert.match(await page.locator("#group-invitation-account").textContent(), /alice/);
      assert.equal(await page.evaluate(() => invitationToken), "group-private-token");
      assert.equal(await page.locator("#login-view").isVisible(), false);
      if (method === "password") {
        await page.setViewportSize({width: 390, height: 844});
        await page.screenshot({path: path.join(screenshots, "group-invitation-mobile.png"), fullPage: true});
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1));
      }
      await page.locator("#group-invitation-join").click();
      await page.waitForFunction(() => invitationToken === "");
      assert.equal(writes.at(-1).body.invitation_token, "group-private-token");
      assert.match(await page.locator("#group-invitation-title").textContent(), /等待管理员审核/);
    }

    for (const method of ["password", "passkey"]) {
      authenticated = false; currentKind = "member";
      await page.goto(`http://127.0.0.1:8765/join?scenario=register-${method}#token=member-private-token`);
      await page.evaluate(async () => {bindUI(); createPasskey = async () => ({id: "credential"}); await configureInvitationView();});
      await page.locator('#join-form input[name="username"]').fill("new-member");
      await page.locator('#join-form input[name="display_name"]').fill("新同事");
      await page.locator('#join-form select[name="login_method"]').selectOption(method);
      if (method === "password") {
        await page.locator('#join-form input[name="password"]').fill("new-password");
        await page.locator('#join-form input[name="password_confirmation"]').fill("new-password");
      }
      await page.locator('#join-form button[type="submit"]').click();
      await page.locator("#secret-dialog[open]").waitFor();
      assert.match(await page.locator("#secret-value").textContent(), /SAVE-THIS-RECOVERY-CODE/);
      await page.locator("#save-secret").click();
      await page.waitForFunction(() => byId("secret-value").textContent === "");
      assert.equal(await page.locator("#registration-pending-view").isVisible(), true);
      assert.equal(await page.locator("#login-view").isVisible(), false);
      assert.equal(await page.locator("#secret-value").textContent(), "");
      assert.equal(new URL(page.url()).pathname, "/join");
    }

    // A join response arriving after explicit logout cannot restore invitation context.
    currentKind = "group"; authenticated = true; joinDelay = true;
    await page.goto("http://127.0.0.1:8765/join?scenario=logout#token=old-group-token");
    await page.evaluate(async () => {bindUI(); await configureInvitationView();});
    await page.locator("#group-invitation-join").click();
    while (!releaseJoin) await new Promise((resolve) => setTimeout(resolve, 10));
    await page.locator("#group-invitation-logout").click();
    await page.waitForURL("http://127.0.0.1:8765/");
    releaseJoin();
    await page.waitForTimeout(100);
    assert.equal(await page.evaluate(() => invitationToken), "");
    assert.equal(await page.locator("#group-invitation-view").isVisible(), false);
    assert.deepEqual(errors, []);
    console.log("Invitation browser regressions passed (desktop, mobile, approval, both authentication methods, and stale responses).");
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
