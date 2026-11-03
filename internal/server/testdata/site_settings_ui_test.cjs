"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const source = readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));

class Element {
  constructor(tag = "div") {
    this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {};
    this.className = ""; this.listeners = {}; this.value = ""; this.disabled = false; this.checked = false;
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      toggle: (name, on) => {
        const names = new Set(this.className.split(/\s+/).filter(Boolean));
        if (on === undefined) on = !names.has(name);
        if (on) names.add(name); else names.delete(name);
        this.className = [...names].join(" ");
      },
      add: (name) => this.classList.toggle(name, true), remove: (name) => this.classList.toggle(name, false),
    };
  }
  get childElementCount() { return this.children.length; }
  set textContent(value) { this.text = String(value); this.children = []; }
  get textContent() { return (this.text || "") + this.children.map((child) => child.textContent).join(""); }
  append(...children) {
    for (const child of children) {
      if (typeof child === "string") { const text = new Element("#text"); text.text = child; this.children.push(text); continue; }
      child.parentElement = this; this.children.push(child);
    }
  }
  replaceChildren(...children) { this.text = ""; this.children = []; this.append(...children.filter(Boolean)); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  reset() { for (const control of Object.values(this.elements || {})) { control.value = ""; control.checked = false; } }
  matches(selector) {
    if (selector.includes(",")) return selector.split(",").some((item) => this.matches(item.trim()));
    if (selector.startsWith(".")) return this.classList.contains(selector.slice(1));
    const data = selector.match(/^\[data-([a-z-]+)(?:="([^"]*)")?\]$/);
    if (data) {
      const key = data[1].replace(/-([a-z])/g, (_, letter) => letter.toUpperCase());
      return key in this.dataset && (data[2] === undefined || this.dataset[key] === data[2]);
    }
    if (selector === "button[type=submit]") return this.tagName === "button" && this.type === "submit";
    return this.tagName === selector;
  }
  querySelectorAll(selector) { return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) { return this.matches(selector) ? this : this.parentElement?.closest(selector) || null; }
}

const fields = {community_enabled: true, community_title: "水源喵交流群", community_qq_group: "123456789", community_join_url: "",
  community_note: "", recharge_enabled: true, recharge_contact: "群主", recharge_note: ""};

function dashboard({role = "member", handler} = {}) {
  const root = new Element(), nodes = new Map(), calls = [];
  const node = (id, tag = "div") => {
    if (!nodes.has(id)) { const value = new Element(tag); value.id = id; nodes.set(id, value); root.append(value); }
    return nodes.get(id);
  };
  const form = node("site-settings-form", "form");
  form.elements = {};
  for (const name of Object.keys(fields)) {
    const input = new Element("input");
    input.type = name.endsWith("_enabled") ? "checkbox" : "text";
    form.elements[name] = input; form.append(input);
  }
  const message = new Element("p"); message.className = "form-message"; form.append(message);
  for (const slot of ["community_qr", "recharge_wechat", "recharge_alipay"]) {
    const host = new Element(); host.dataset.siteImageSlot = slot;
    for (const marker of ["siteImagePreview", "siteImageFile", "siteImageMessage", "siteImageUpload", "siteImageRemove"]) {
      const child = new Element(marker === "siteImageFile" ? "input" : marker.endsWith("Message") ? "p" : marker.endsWith("Preview") ? "div" : "button");
      child.dataset[marker] = ""; host.append(child);
    }
    root.append(host);
  }
  const context = vm.createContext({
    role, calls,
    document: {getElementById: node, createElement: (tag) => new Element(tag), querySelectorAll: (selector) => root.querySelectorAll(selector)},
    window: {setTimeout, clearTimeout}, location: {hash: "#site-settings"}, URLSearchParams,
    request: async (url, options = {}) => {
      calls.push({url, options});
      if (handler) return handler(url, options);
      return {};
    },
  });
  vm.runInContext(application, context, {filename: "assets/app.js"});
  const run = (code) => vm.runInContext(code, context);
  run(`state = {user: {id: "self", username: "lin", role}, recently_verified: true}; api = request; notice = () => {};`);
  return {run, node, form, calls, context, slot: (name) => root.querySelectorAll(`[data-site-image-slot="${name}"]`)[0]};
}

function links(element) {
  return element.querySelectorAll("a");
}

function images(element) {
  return element.querySelectorAll("img");
}

test("community card renders only HTTPS links and same-origin images as plain text", () => {
  const page = dashboard();
  const host = page.node("auth-community");
  host.className = "hidden";
  page.context.community = {title: "<b>群</b>", qq_group: "123456789", join_url: "javascript:alert(1)", note: "<img src=x onerror=alert(1)>",
    qr_image: {url: "https://evil.example/qr.png", width: 8, height: 8}};
  page.run('renderSiteCommunity(byId("auth-community"), community)');
  assert.equal(host.classList.contains("hidden"), false);
  assert.equal(links(host).length, 0);
  assert.equal(images(host).length, 0);
  assert.match(host.textContent, /<b>群<\/b>/);
  assert.match(host.textContent, /123456789/);

  for (const url of ["//evil.example/qr.png", "data:image/png;base64,AA=="]) {
    page.context.community.qr_image.url = url;
    page.run('renderSiteCommunity(byId("auth-community"), community)');
    assert.equal(images(host).length, 0, url);
  }

  page.context.community = {title: "", qq_group: "", join_url: "https://qm.qq.com/q/abc", note: "", qr_image: {url: "/site/images/community_qr?v=0123456789abcdef", width: 8, height: 8}};
  page.run('renderSiteCommunity(byId("auth-community"), community)');
  const [link] = links(host);
  assert.equal(link.attributes.href, "https://qm.qq.com/q/abc");
  assert.equal(link.attributes.target, "_blank");
  assert.equal(link.attributes.rel, "noopener noreferrer");
  assert.equal(images(host)[0].attributes.src, "/site/images/community_qr?v=0123456789abcdef");
  assert.match(host.textContent, /加入交流群/);

  page.run('renderSiteCommunity(byId("auth-community"), null)');
  assert.equal(host.classList.contains("hidden"), true);
  assert.equal(host.children.length, 0);
});

test("recharge card shows the signed-in username only on the user's own billing view", () => {
  const page = dashboard({role: "owner"});
  page.run(`siteContent = {community: {qq_group: "123456789"}, recharge: {contact: "群主", note: "最低 10 元",
    methods: [{slot: "recharge_wechat", label: "微信支付", image: {url: "/admin/site/images/recharge_wechat?v=0123456789abcdef", width: 8, height: 8}}]}};
    billingUserID = "self"; renderSiteRecharge();`);
  const card = page.node("billing-recharge");
  assert.equal(card.classList.contains("hidden"), false);
  assert.equal(page.node("plans-recharge-hint").classList.contains("hidden"), false);
  assert.equal(page.node("billing-recharge-username").textContent, "lin");
  assert.match(page.node("billing-recharge-contact").textContent, /客服：群主。交流群号：123456789。/);
  assert.equal(page.node("billing-recharge-note").textContent, "最低 10 元");
  assert.equal(images(page.node("billing-recharge-methods")).length, 1);

  page.run('billingUserID = "someone-else"; renderSiteRecharge();');
  assert.equal(card.classList.contains("hidden"), true);
  assert.equal(page.node("plans-recharge-hint").classList.contains("hidden"), true);

  page.run('billingUserID = "self"; resetSiteContent();');
  assert.equal(card.classList.contains("hidden"), true);
  assert.equal(page.node("billing-recharge-methods").children.length, 0);
});

test("a late site content response cannot repopulate another login", async () => {
  let resolve;
  const pending = new Promise((done) => { resolve = done; });
  const page = dashboard({handler: () => pending});
  const request = page.run("loadSiteContent()");
  page.run('identityGeneration++; state = {user: {id: "other", username: "other", role: "member"}};');
  resolve({community: {qq_group: "123456789"}, recharge: {methods: []}});
  await request;
  assert.equal(page.run("siteContent"), null);
});

test("saving sends the loaded revision with every trimmed field", async () => {
  const page = dashboard({role: "owner", handler: (url, options) => options.method ? {revision: 8, updated_at: "2026-11-03T00:00:00Z", settings: JSON.parse(options.body), images: {}} : {}});
  page.context.loaded = {revision: 7, updated_at: "2026-11-03T00:00:00Z", settings: {...fields}, images: {}};
  page.run("applySiteSettings(loaded, true); siteSettingsLoaded = true; loadSiteContent = async () => {};");
  page.form.elements.community_title.value = "  新标题  ";
  page.form.elements.recharge_enabled.checked = false;
  page.context.event = {currentTarget: page.form};
  await page.run("saveSiteSettings(event)");
  const write = page.calls.find((call) => call.options.method === "PUT");
  assert.equal(write.url, "/admin/site-settings");
  const body = JSON.parse(write.options.body);
  assert.deepEqual(Object.keys(body).sort(), ["revision", ...Object.keys(fields)].sort());
  assert.equal(body.revision, 7);
  assert.equal(body.community_title, "新标题");
  assert.equal(body.recharge_enabled, false);
  assert.equal(page.run("siteSettingsRevision"), 8);

  page.form.elements.community_qq_group.value = "";
  await assert.rejects(page.run("saveSiteSettings(event)"), /至少填写 QQ 群号或加群链接/);
});

test("an image write adopts the new revision only while text fields are unchanged", () => {
  const page = dashboard({role: "owner"});
  page.context.loaded = {revision: 3, updated_at: "2026-11-03T00:00:00Z", settings: {...fields}, images: {}};
  page.run("applySiteSettings(loaded, true);");
  page.context.next = {revision: 4, settings: {...fields}, images: {recharge_wechat: {url: "/admin/site/images/recharge_wechat?v=0123456789abcdef", width: 8, height: 8}}};
  page.run("applySiteSettings(next, false);");
  assert.equal(page.run("siteSettingsRevision"), 4);
  assert.equal(images(page.slot("recharge_wechat")).length, 1);
  assert.equal(page.slot("recharge_wechat").querySelector("[data-site-image-remove]").disabled, false);

  page.context.concurrent = {revision: 6, settings: {...fields, community_title: "另一个标签页"}, images: {}};
  page.run("applySiteSettings(concurrent, false);");
  assert.equal(page.run("siteSettingsRevision"), 4);
  assert.match(page.node("site-settings-status").textContent, /其他页面修改/);
  assert.equal(page.slot("recharge_wechat").querySelector("[data-site-image-remove]").disabled, true);
});

test("uploads send only PNG or JPEG bodies with their own content type", async () => {
  const page = dashboard({role: "owner", handler: () => ({revision: 2, settings: {...fields}, images: {}})});
  page.context.loaded = {revision: 1, updated_at: "2026-11-03T00:00:00Z", settings: {...fields}, images: {}};
  page.run("applySiteSettings(loaded, true); siteSettingsLoaded = true; loadSiteContent = async () => {};");
  const host = page.slot("recharge_alipay");
  const input = host.querySelector("[data-site-image-file]");
  page.context.host = host;

  input.files = [{type: "image/svg+xml", size: 10}];
  await assert.rejects(page.run("uploadSiteImage(host)"), /仅支持 PNG 或 JPEG/);
  input.files = [{type: "image/png", size: 5 * 1024 * 1024 + 1}];
  await assert.rejects(page.run("uploadSiteImage(host)"), /不能超过 5 MiB/);
  assert.equal(page.calls.length, 0);

  const file = {type: "image/jpeg", size: 1024};
  input.files = [file];
  await page.run("uploadSiteImage(host)");
  assert.equal(page.calls.length, 1);
  assert.equal(page.calls[0].url, "/admin/site-settings/images/recharge_alipay");
  assert.equal(page.calls[0].options.method, "PUT");
  assert.equal(page.calls[0].options.body, file);
  assert.equal(page.calls[0].options.headers["Content-Type"], "image/jpeg");
  assert.equal(input.value, "");
});
