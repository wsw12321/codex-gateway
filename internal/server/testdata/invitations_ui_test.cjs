"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));

class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {}; this.className = "";
    this.listeners = {}; this.value = ""; this.checked = false; this.disabled = false; this.open = false; this.elements = {};
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => this.classList.toggle(name, true), remove: (name) => this.classList.toggle(name, false),
      toggle: (name, enabled) => {
        const names = new Set(this.className.split(/\s+/).filter(Boolean));
        if (enabled === undefined) enabled = !names.has(name);
        if (enabled) names.add(name); else names.delete(name);
        this.className = [...names].join(" ");
      },
    };
  }
  set textContent(value) { this.text = String(value); this.children = []; }
  get textContent() { return this.text || this.children.map((child) => child.textContent || "").join(" "); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.text = ""; this.children = children; }
  setAttribute(name, value) { this.attributes[name] = value; }
  getAttribute(name) { return this.attributes[name]; }
  addEventListener(name, fn) { this.listeners[name] = fn; }
  querySelector() { return this.control ||= new Element("button"); }
  querySelectorAll() { return []; }
  closest() { return null; }
  matches(selector) { return selector === "button" && this.tagName === "button"; }
  reset() { this.wasReset = true; }
  close() { this.open = false; }
}
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => {resolve = yes; reject = no;}); return {promise, resolve, reject}; }
const invitation = {id: "invite-one", kind: "member", max_uses: 5, used_count: 2, requires_approval: true, expires_at: "2099-10-01T12:00:00Z"};
const pending = (id = "app-one") => ({id, invitation_id: invitation.id, user_id: `user-${id}`, username: `name-${id}`, display_name: "申请人", status: "pending", registered_at: "2099-09-01T12:00:00Z", applied_at: "2099-09-01T12:00:00Z"});
function dashboard(handler = () => ({})) {
  const nodes = new Map(), calls = [], redirects = [], secrets = [];
  const node = (id) => { if (!nodes.has(id)) nodes.set(id, new Element()); return nodes.get(id); };
  for (const id of ["invitations-filter", "invitation-form"]) {
    for (const name of ["kind", "group_id", "expires_at", "max_uses", "requires_approval"]) node(id).elements[name] = new Element("input");
  }
  const context = vm.createContext({URL, URLSearchParams, FormData: class {constructor(form) {this.data = form.mockData || [];} [Symbol.iterator]() {return this.data[Symbol.iterator]();}},
    document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: () => []},
    window: {confirm: () => true, setTimeout: () => 0, clearTimeout: () => {}},
    history: {replaceState: (...args) => redirects.push(args.at(-1))}, location: {origin: "https://gateway.test", href: "https://gateway.test/join", assign: (value) => redirects.push(value)},
    request: async (pathname, options, current) => {calls.push({pathname, options, current}); return handler(pathname, options, current);},
    secret: (...args) => secrets.push(args),
  });
  vm.runInContext(application, context);
  const run = (code) => vm.runInContext(code, context);
  run('state = {user: {id: "owner", role: "owner"}, recently_verified: true}; api = request; showSecret = secret;');
  return {node, run, calls, redirects, secrets, context};
}

test("both registration methods preserve credentials and show codes before awaiting approval", async () => {
  for (const method of ["password", "passkey"]) {
    const ui = dashboard((pathname) => pathname.endsWith("/begin") ? {flow_id: "flow"} : {status: "pending", recovery_codes: ["recovery-code"]});
    ui.run('invitationToken = "private-token"; createPasskey = async () => ({id: "credential"});');
    ui.node("join-form").mockData = [["username", "applicant"], ["display_name", "Applicant"], ["login_method", method], ["password", "password123"], ["password_confirmation", "password123"]];
    await ui.run('register({currentTarget: byId("join-form")})');
    assert.equal(ui.run("invitationToken"), "");
    assert.equal(ui.secrets.length, 1);
    assert.equal(ui.secrets[0][1], "recovery-code");
    assert.equal(ui.redirects.length, 0);
    ui.secrets[0][3]();
    assert.equal(ui.node("registration-pending-view").classList.contains("hidden"), false);
    assert.equal(ui.node("join-view").classList.contains("hidden"), true);
    assert.equal(ui.redirects.length, 0);
    const body = JSON.parse(ui.calls[0].options.body);
    assert.equal(body.invitation_token, "private-token");
    assert.equal(body.username, "applicant");
    assert.equal("password_confirmation" in body, false);
    assert.equal(ui.node("join-form").wasReset, true);
  }
});

test("group invitation cannot enter either registration endpoint", async () => {
  const ui = dashboard();
  ui.run('invitationToken = "group-token"; invitationKind = "group";');
  await assert.rejects(ui.run('register({currentTarget: byId("join-form")})'), /已有账号/);
  assert.equal(ui.calls.length, 0);
});

test("password and Passkey login retain the group token until explicit confirmation", async () => {
  for (const method of ["password", "passkey"]) {
    const ui = dashboard((pathname) => {
      if (pathname.endsWith("/begin")) return {flow_id: "flow"};
      if (pathname === "/admin/state") return {user: {id: "member", username: "alice", display_name: "Alice"}};
      if (pathname === "/auth/invitations/join") return {status: "pending"};
      return {};
    });
    ui.run(`state = null; invitationToken = "group-token"; invitationKind = "group"; inspectedInvitation = ${JSON.stringify({...invitation, kind: "group", group_name: "产品团队"})}; getPasskey = async () => ({id: "credential"});`);
    ui.node("password-login-form").mockData = [["username", "alice"], ["password", "password123"]];
    await ui.run(method === "password" ? 'passwordLogin({currentTarget: byId("password-login-form")})' : "login()");
    assert.equal(ui.run("invitationToken"), "group-token");
    assert.equal(ui.redirects.length, 0);
    assert.match(ui.node("group-invitation-account").textContent, /Alice（alice）/);
    assert.equal(ui.calls.some((call) => call.pathname === "/auth/invitations/join"), false);
    await ui.run("joinInvitedGroup()");
    assert.equal(JSON.parse(ui.calls.at(-1).options.body).invitation_token, "group-token");
    assert.equal(ui.run("invitationToken"), "");
    assert.match(ui.node("group-invitation-title").textContent, /等待管理员审核/);
    assert.equal(ui.node("group-invitation-join").classList.contains("hidden"), true);
  }
});

test("logout invalidates delayed group confirmation and prevents secret presentation", async () => {
  const result = deferred();
  const ui = dashboard(() => result.promise);
  ui.run('invitationToken = "group-token"; invitationKind = "group"; groupInvitationUser = {id: "member"};');
  const work = ui.run("joinInvitedGroup()");
  ui.run('loggingOut = true; clearInvitationContext();');
  result.resolve({status: "approved"});
  await assert.rejects(work, (error) => error.code === "stale_request");
  assert.equal(ui.run("invitationToken"), "");
  assert.equal(ui.node("group-invitation-title").textContent, "");
  assert.equal(ui.secrets.length, 0);
});

test("pagination defaults to 50 and late list responses cannot restore a previous identity", async () => {
  const result = deferred();
  const ui = dashboard(() => result.promise);
  ui.node("invitations-filter").elements.kind.value = "group";
  ui.node("invitations-filter").elements.group_id.value = "group-id";
  const work = ui.run("loadInvitations(50)");
  assert.match(ui.calls[0].pathname, /limit=50&offset=50&kind=group&group_id=group-id/);
  ui.run("identityGeneration++; state = null; resetInvitationManagement();");
  result.resolve({invitations: [invitation]});
  await assert.rejects(work, (error) => error.code === "stale_request");
  assert.equal(ui.run("invitationList.length"), 0);
  assert.match(ui.node("invitation-list").textContent, /登录后/);
});

test("review uses immutable application IDs and refreshes released capacity", async () => {
  let applications = [pending(), pending("app-two")];
  const ui = dashboard((pathname, options) => {
    if (pathname.endsWith("/review")) { applications = []; return {updated_count: 2}; }
    if (pathname.includes("/applications?")) return {applications};
    if (pathname.startsWith("/admin/invitations?")) return {invitations: [{...invitation, used_count: 0}]};
    throw new Error(pathname);
  });
  ui.run(`selectedInvitation = ${JSON.stringify(invitation)}; invitationApplications = ${JSON.stringify(applications)};`);
  await ui.run('reviewInvitationApplications("reject", ["app-one", "app-two", "app-one"])');
  const body = JSON.parse(ui.calls[0].options.body);
  assert.deepEqual(body, {application_ids: ["app-one", "app-two"], decision: "reject"});
  assert.equal(ui.run("invitationApplications.length"), 0);
  assert.match(ui.node("invitation-detail-message").textContent, /名额已释放/);
  await assert.rejects(ui.run('reviewInvitationApplications("approve", Array.from({length: 101}, (_, i) => String(i)))'), /100/);
});

test("a stale detail page cannot replace the selected invite or expose its applicants", async () => {
  const result = deferred();
  const ui = dashboard((pathname) => pathname.includes("invite-one") ? result.promise : {applications: [pending("new")]});
  const old = ui.run(`loadInvitationApplications(${JSON.stringify(invitation)})`);
  await ui.run(`loadInvitationApplications(${JSON.stringify({...invitation, id: "invite-two"})})`);
  result.resolve({applications: [pending("old")]});
  await assert.rejects(old, (error) => error.code === "stale_request");
  assert.equal(ui.run("selectedInvitation.id"), "invite-two");
  assert.equal(ui.run("invitationApplications[0].id"), "new");
});

test("failed bulk review leaves the entire selection available for retry", async () => {
  const ui = dashboard(() => {throw new Error("群组已归档");});
  ui.run(`selectedInvitation = ${JSON.stringify({...invitation, kind: "group"})}; invitationSelectedApplications = new Set(["app-one", "app-two"]);`);
  await ui.run('runInvitationReview("approve")');
  assert.equal(ui.run("invitationSelectedApplications.size"), 2);
  assert.equal(ui.run("invitationOperation"), false);
  assert.match(ui.node("invitation-detail-message").textContent, /群组已归档/);
});

test("Owner bootstrap and recovery links keep both original credential flows", async () => {
  for (const kind of ["owner_bootstrap", "recovery"]) {
    for (const method of ["password", "passkey"]) {
      const ui = dashboard((pathname) => {
        if (pathname.endsWith("/inspect")) return {...invitation, kind};
        if (pathname.endsWith("/begin")) return {flow_id: "flow"};
        return {status: "approved", recovery_codes: ["bootstrap-recovery-code"]};
      });
      ui.context.location.hash = "#token=legacy-token&kind=group";
      ui.run('createPasskey = async () => ({id: "credential"});');
      await ui.run("configureInvitationView()");
      assert.equal(ui.run("invitationKind"), kind);
      assert.equal(ui.node("join-form").querySelector().disabled, false);
      ui.node("join-form").mockData = [["login_method", method], ["password", "new-password"], ["password_confirmation", "new-password"]];
      if (kind === "owner_bootstrap") ui.node("join-form").mockData.push(["username", "first-owner"], ["display_name", "Owner"]);
      await ui.run('register({currentTarget: byId("join-form")})');
      const firstRegistration = ui.calls[1];
      assert.equal(firstRegistration.pathname, method === "passkey" ? "/auth/register/begin" : kind === "recovery" ? "/auth/password/recovery" : "/auth/password/register");
      assert.equal(JSON.parse(firstRegistration.options.body).invitation_token, "legacy-token");
      ui.secrets[0][3]();
      assert.equal(ui.redirects.at(-1), "/#overview");
    }
  }
});

test("full, expired and revoked group invitations retain confirmation for an existing application", async () => {
  for (const status of ["full", "expired", "revoked"]) {
    const ui = dashboard((pathname) => {
      if (pathname.endsWith("/inspect")) return {...invitation, kind: "group", status};
      if (pathname === "/admin/state") return {user: {id: "member", username: "alice"}};
      return {status: "pending"};
    });
    ui.context.location.hash = "#token=used-group-token";
    await ui.run("configureInvitationView()");
    assert.equal(ui.node("group-invitation-join").classList.contains("hidden"), false);
    assert.match(ui.node("group-invitation-help").textContent, /无法提交新申请/);
    await ui.run("joinInvitedGroup()");
    assert.match(ui.node("group-invitation-title").textContent, /等待管理员审核/);
  }
});

test("legacy consumed recovery and Owner invitation status follows server metadata", () => {
  const ui = dashboard();
  for (const kind of ["owner_bootstrap", "recovery"]) assert.equal(ui.run(`invitationStatus(${JSON.stringify({...invitation, kind, used_count: 0, status: "full"})})`), "名额已满");
});
