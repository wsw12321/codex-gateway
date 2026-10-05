"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));
const deferred = () => {let resolve; const promise = new Promise((value) => {resolve = value;}); return {promise, resolve};};
class Element {
  constructor() {
    this.elements = {}; this.listeners = {}; this.dataset = {}; this.attributes = {}; this.value = ""; this.disabled = false; this.children = [];
    this.classList = {add() {}, remove() {}, toggle() {}, contains: () => false};
  }
  addEventListener(name, handler) {this.listeners[name] = handler;}
  querySelectorAll() {return [];}
  querySelector() {return null;}
  closest() {return null;}
  setAttribute(name, value) {this.attributes[name] = value;}
  getAttribute(name) {return this.attributes[name];}
  removeAttribute(name) {delete this.attributes[name];}
  replaceChildren(...children) {this.children = children;}
  append(...children) {this.children.push(...children);}
  reset() {for (const input of Object.values(this.elements)) input.value = "";}
  focus() {}
}
function dashboard(handler = () => ({}), user = {id: "member", username: "member", display_name: "会员", role: "member"}) {
  const nodes = new Map(), calls = [];
  const node = (id) => {if (!nodes.has(id)) nodes.set(id, new Element()); return nodes.get(id);};
  node("profile-form").elements = {username: new Element(), display_name: new Element()};
  const context = vm.createContext({URL, URLSearchParams,
    document: {getElementById: node, createElement: () => new Element(), querySelectorAll: () => [], addEventListener() {}},
    window: {}, location: {hash: "#security"},
    fetch: async (pathname, options) => {
      const call = {pathname, method: options.method, body: options.body ? JSON.parse(options.body) : undefined}; calls.push(call);
      const value = await handler(call);
      return {ok: !value.httpStatus || value.httpStatus < 400, status: value.httpStatus || 200, json: async () => value};
    },
  });
  const run = (code) => vm.runInContext(code, context);
  run(application);
  run(`setLocalMessage = (form, message) => {form.message = message || "";}; setBusy = () => {}; notice = () => {};
    setConnection = () => {}; state = {user: ${JSON.stringify(user)}, recently_verified: true}; renderProfile();`);
  const form = node("profile-form");
  return {run, node, form, calls, context, save: () => run('saveProfile({currentTarget: byId("profile-form")})')};
}
function stubStateRendering(ui) {
  ui.run(`for (const name of ["resetBrowserHandoff", "closeNavigationDrawer", "resetOverview", "clearUpstreamAccountUIState",
    "stopUpstreamConcurrency", "stopMonitoring", "stopModelIdentification", "resetModelIdentification", "resetMonitoring", "resetInformation",
    "cancelReauthentication", "resetGroupManagement", "resetInvitationManagement", "resetBillingUserSearch", "resetModelMultipliers",
    "renderDevices", "renderProjects", "renderAPIKeys", "renderPasskeys", "renderLoginMethods", "renderIdentityLink", "renderSelects",
    "renderBrowserHandoff", "renderResourceSummary", "renderOnboarding", "routeFromHash"]) globalThis[name] = () => {};`);
}

test("historical SSO usernames allow display-only changes without reauthentication", async () => {
  const user = {id: "member", username: "water5_" + "a".repeat(32), display_name: "吾水阁用户", role: "member"};
  const ui = dashboard(() => ({user: {...user, display_name: "中文名称"}}), user);
  ui.run('state.recently_verified = false; reauthenticate = async () => {throw new Error("display-only must not reauthenticate");};');
  ui.form.elements.display_name.value = " 中文名称 ";
  await ui.save();
  assert.deepEqual(ui.calls, [{pathname: "/admin/profile", method: "PATCH", body: {display_name: "中文名称"}}]);
  assert.equal(ui.run("state.user.id"), "member");
  assert.equal(ui.node("whoami").textContent, "中文名称");
  assert.equal(ui.form.elements.username.value, user.username);
  assert.equal(ui.form.elements.display_name.value, "中文名称");
});

test("username changes normalize the submitted fields and wait for recent verification", async () => {
  const pending = deferred();
  const ui = dashboard(() => ({user: {id: "member", username: "new_name", display_name: "新名称", role: "member"}}));
  ui.context.verify = () => pending.promise;
  ui.run("state.recently_verified = false; reauthenticate = verify;");
  ui.form.elements.username.value = " New_Name "; ui.form.elements.display_name.value = " 新名称 ";
  const save = ui.save();
  assert.equal(ui.calls.length, 0);
  pending.resolve(); await save;
  assert.deepEqual(ui.calls[0].body, {username: "new_name", display_name: "新名称"});
  assert.match(ui.node("role").textContent, /^new_name/);
});

test("cancelled verification preserves the draft and cannot send a profile mutation", async () => {
  const ui = dashboard();
  ui.run('state.recently_verified = false; reauthenticate = async () => {throw Object.assign(new Error("cancelled"), {code: "reauth_cancelled"});};');
  ui.form.elements.username.value = "new_name";
  await assert.rejects(ui.save(), {code: "reauth_cancelled"});
  assert.equal(ui.calls.length, 0);
  assert.equal(ui.form.elements.username.value, "new_name");
  assert.equal(ui.run("profileOperation"), null);
});

test("conflict failure retains both fields and cannot partially update the displayed identity", async () => {
  const ui = dashboard(() => ({httpStatus: 409, error: {code: "username_taken", message: "occupied"}}));
  ui.form.elements.username.value = "taken"; ui.form.elements.display_name.value = "待保存";
  await assert.rejects(ui.save(), {code: "username_taken"});
  assert.equal(ui.run("state.user.display_name"), "会员");
  assert.equal(ui.form.elements.username.value, "taken");
  assert.equal(ui.form.elements.display_name.value, "待保存");
});

test("ordinary state refresh preserves drafts and updates untouched fields", () => {
  const ui = dashboard();
  ui.form.elements.display_name.value = "未保存";
  ui.run('state.user = {...state.user, username: "renamed_elsewhere", display_name: "服务器名称"}; renderProfile();');
  assert.equal(ui.form.elements.username.value, "renamed_elsewhere");
  assert.equal(ui.form.elements.display_name.value, "未保存");
  ui.form.elements.display_name.value = "服务器名称";
  ui.run('state.user.display_name = "再次更新"; renderProfile();');
  assert.equal(ui.form.elements.display_name.value, "再次更新");
});

test("a late successful save preserves edits typed after submission and prevents duplicate writes", async () => {
  const pending = deferred(); const ui = dashboard(() => pending.promise);
  ui.form.elements.display_name.value = "第一稿";
  const save = ui.save(); await ui.save();
  ui.form.elements.display_name.value = "第二稿";
  pending.resolve({user: {id: "member", username: "member", display_name: "第一稿", role: "member"}}); await save;
  assert.equal(ui.calls.length, 1);
  assert.equal(ui.node("whoami").textContent, "第一稿");
  assert.equal(ui.form.elements.display_name.value, "第二稿");
});

test("a state read started before a successful write cannot restore old names", async () => {
  const pending = deferred();
  const ui = dashboard(({pathname}) => pathname === "/admin/state" ? pending.promise : {user: {id: "member", username: "new_member", display_name: "新会员", role: "member"}});
  const read = ui.run('api("/admin/state")');
  ui.form.elements.username.value = "new_member"; ui.form.elements.display_name.value = "新会员";
  await ui.save();
  pending.resolve({user: {id: "member", username: "member", display_name: "会员", role: "member"}});
  const result = await read;
  assert.equal(result.user.username, "new_member"); assert.equal(result.user.display_name, "新会员");
});

test("late profile responses cannot affect a switched account or a logged-out page", async () => {
  for (const logout of [false, true]) {
    const pending = deferred(); const ui = dashboard(() => pending.promise);
    stubStateRendering(ui);
    ui.form.elements.display_name.value = "旧账号草稿";
    const save = ui.save();
    if (logout) ui.run("loggingOut = true; identityGeneration++; resetProfile();");
    else ui.run('renderState({user: {id: "another", username: "another", display_name: "另一账号", role: "member"}})');
    pending.resolve({user: {id: "member", username: "member", display_name: "旧账号草稿", role: "member"}});
    await assert.rejects(save, {code: "stale_request"});
    assert.equal(ui.form.elements.display_name.value, logout ? "" : "另一账号");
    if (!logout) assert.equal(ui.run("state.user.id"), "another");
  }
});

test("late dashboard reads cannot restore a switched account or a logged-out page", async () => {
  for (const logout of [false, true]) {
    const pending = deferred(); const ui = dashboard(() => pending.promise);
    stubStateRendering(ui);
    const read = ui.run("refreshState()");
    if (logout) ui.run("loggingOut = true; identityGeneration++; resetProfile();");
    else ui.run('renderState({user: {id: "another", username: "another", display_name: "另一账号", role: "member"}})');
    pending.resolve({user: {id: "member", username: "member", display_name: "旧名称", role: "member"}});
    await assert.rejects(read, {code: "stale_request"});
    assert.equal(ui.form.elements.display_name.value, logout ? "" : "另一账号");
  }
});

test("only modified fields are validated and Unicode display limits count characters", async () => {
  for (const value of ["ab", "a".repeat(33), "_invalid", "含中文", "has space"]) {
    const ui = dashboard(); ui.form.elements.username.value = value;
    await assert.rejects(ui.save(), /用户名/); assert.equal(ui.calls.length, 0);
  }
  for (const value of [" ", "中".repeat(81), "😀".repeat(81)]) {
    const ui = dashboard(); ui.form.elements.display_name.value = value;
    await assert.rejects(ui.save(), /显示名称/); assert.equal(ui.calls.length, 0);
  }
  const displayName = "😀".repeat(80);
  const ui = dashboard(() => ({user: {id: "member", username: "member", display_name: displayName, role: "member"}}));
  ui.form.elements.username.value = " MEMBER "; ui.form.elements.display_name.value = displayName;
  await ui.save(); assert.deepEqual(ui.calls[0].body, {display_name: displayName});
});

test("recovery invites use the selected stable ID and editing the query clears selection", async () => {
  const ui = dashboard(() => ({link: "https://gateway.test/join#token=secret"}), {id: "owner", username: "owner", display_name: "Owner", role: "owner"});
  ui.run('showSecret = () => {}; globalThis.picker = createUserSearch("recovery-user-search"); picker.setUsers([{id: "original-user", username: "old_name", display_name: "旧用户"}]);');
  await ui.run("picker.choose(picker.users[0])");
  assert.equal(ui.run("picker.selectedID()"), "original-user");
  // A rename and reuse of the visible old name must not change the selected target.
  ui.run('picker.setUsers([{id: "original-user", username: "new_name"}, {id: "new-holder", username: "old_name"}]);');
  await ui.run('invite("recovery", picker.selectedID())');
  assert.deepEqual(ui.calls[0].body, {kind: "recovery", target_user_id: "original-user"});
  ui.node("recovery-user-search").value = "other";
  ui.node("recovery-user-search").listeners.input();
  assert.equal(ui.run("picker.selectedID()"), "");
  await assert.rejects(ui.run('invite("recovery", picker.selectedID())'), /选择待恢复用户/);
  assert.equal(ui.calls.length, 1);
});

test("changing a recovery selection while verifying identity cannot issue an invitation", async () => {
  const pending = deferred();
  const ui = dashboard(() => ({}), {id: "owner", username: "owner", role: "owner"});
  ui.context.verify = () => pending.promise;
  ui.run('state.recently_verified = false; reauthenticate = verify; globalThis.target = "original-user";');
  const invitation = ui.run('invite("recovery", target, () => target === "original-user")');
  ui.run('target = "";');
  pending.resolve();
  await assert.rejects(invitation, {code: "stale_request"});
  assert.equal(ui.calls.length, 0);
});
