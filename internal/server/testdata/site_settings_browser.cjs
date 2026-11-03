"use strict";

// Synthetic fixtures only: the QR-like images are generated noise, not real
// group or payment codes, and no account, balance or credential is used.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const zlib = require("node:zlib");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "http://127.0.0.1:8765";
const output = process.env.SCREENSHOT_DIR || "/tmp/gateway-site-settings-screenshots";

// A grayscale PNG with three finder squares and seeded noise. It only looks
// like a QR code and decodes to nothing.
function syntheticCode(seed, cell = 8) {
  const modules = 25, border = 2, cells = modules + border * 2, size = cells * cell;
  const finders = [[0, 0], [modules - 7, 0], [0, modules - 7]];
  let state = seed;
  const random = () => ((state = (state * 1103515245 + 12345) >>> 0) >>> 16) & 1;
  const dark = (x, y) => {
    for (const [fx, fy] of finders) {
      if (x < fx - 1 || y < fy - 1 || x > fx + 7 || y > fy + 7) continue;
      const dx = x - fx, dy = y - fy;
      if (dx < 0 || dy < 0 || dx > 6 || dy > 6) return false;
      return dx === 0 || dy === 0 || dx === 6 || dy === 6 || (dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4);
    }
    return random() === 1;
  };
  const grid = Array.from({length: cells}, (_, y) => Array.from({length: cells}, (_, x) => {
    const mx = x - border, my = y - border;
    return mx >= 0 && my >= 0 && mx < modules && my < modules && dark(mx, my);
  }));
  const raw = Buffer.alloc((size + 1) * size);
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) raw[y * (size + 1) + 1 + x] = grid[Math.floor(y / cell)][Math.floor(x / cell)] ? 24 : 255;
  }
  const chunk = (type, data) => {
    const body = Buffer.concat([Buffer.from(type), data]), length = Buffer.alloc(4), crc = Buffer.alloc(4);
    length.writeUInt32BE(data.length);
    crc.writeUInt32BE(zlib.crc32(body));
    return Buffer.concat([length, body, crc]);
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(size, 0);
  header.writeUInt32BE(size, 4);
  header[8] = 8;
  return Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk("IHDR", header), chunk("IDAT", zlib.deflateSync(raw)), chunk("IEND", Buffer.alloc(0))]);
}

const owner = {id: "synthetic-owner", username: "miao", display_name: "水源喵", role: "owner", status: "active"};
const member = {id: "synthetic-member", username: "lin", display_name: "林同学", role: "member", status: "active"};
const session = (user) => ({user, recently_verified: true, login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []});
const site = {revision: 1, updated_at: "2026-11-03T08:00:00Z", images: {}, settings: {
  community_enabled: false, community_title: "", community_qq_group: "", community_join_url: "", community_note: "",
  recharge_enabled: false, recharge_contact: "", recharge_note: "",
}};
const imageBytes = new Map(), writes = [];
const labels = {recharge_wechat: "微信支付", recharge_alipay: "支付宝"};

function communityView() {
  if (!site.settings.community_enabled) return null;
  const qr = site.images.community_qr;
  return {title: site.settings.community_title, qq_group: site.settings.community_qq_group, join_url: site.settings.community_join_url,
    note: site.settings.community_note, qr_image: qr ? {...qr, url: qr.url.replace("/admin/site/", "/site/")} : null};
}

function rechargeView() {
  if (!site.settings.recharge_enabled) return null;
  return {contact: site.settings.recharge_contact, note: site.settings.recharge_note,
    methods: Object.keys(labels).filter((slot) => site.images[slot]).map((slot) => ({slot, label: labels[slot], image: site.images[slot]}))};
}

async function routes(page) {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url());
    assert.equal(url.origin, origin);
    const send = (body, status = 200, contentType = "application/json") => route.fulfill({status, contentType,
      body: Buffer.isBuffer(body) || typeof body === "string" ? body : JSON.stringify(body)});
    if (url.pathname === "/") return send(fs.readFileSync(path.join(assets, "index.html"), "utf8"), 200, "text/html");
    if (url.pathname === "/static/app.js") return send(app, 200, "application/javascript");
    for (const [name, contentType] of [["style.css", "text/css"], ["theme.js", "application/javascript"], ["favicon.svg", "image/svg+xml"]]) {
      if (url.pathname === `/static/${name}`) return send(fs.readFileSync(path.join(assets, name), "utf8"), 200, contentType);
    }
    if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
    const image = url.pathname.match(/^\/(?:admin\/)?site\/images\/([a-z_]+)$/);
    if (image && request.method() === "GET") {
      return imageBytes.has(image[1]) ? send(imageBytes.get(image[1]), 200, "image/png") : send({error: {code: "site_image_not_found"}}, 404);
    }
    if (url.pathname === "/site/community") return send({community: communityView()});
    if (url.pathname === "/admin/site") return send({community: communityView(), recharge: rechargeView()});
    if (url.pathname === "/admin/site-settings" && request.method() === "GET") return send(site);
    if (url.pathname === "/admin/site-settings" && request.method() === "PUT") {
      const {revision, ...settings} = request.postDataJSON();
      writes.push({path: url.pathname, body: {revision, ...settings}});
      assert.equal(revision, site.revision);
      Object.assign(site, {revision: site.revision + 1, settings});
      return send(site);
    }
    const upload = url.pathname.match(/^\/admin\/site-settings\/images\/([a-z_]+)$/);
    if (upload && request.method() === "PUT") {
      assert.equal(request.headers()["content-type"], "image/png");
      const bytes = request.postDataBuffer();
      writes.push({path: url.pathname, bytes: bytes.length});
      imageBytes.set(upload[1], bytes);
      site.revision++;
      site.images[upload[1]] = {url: `/admin/site/images/${upload[1]}?v=${String(site.revision).padStart(16, "0")}`, width: 232, height: 232, updated_at: site.updated_at};
      return send(site);
    }
    if (url.pathname === "/admin/billing/me") {
      return send({user: member, subscriptions: [], cash_preferences: {priority: 100, charge_enabled: true}, cash_balance_usd: "3.5", ledger_entries: [], pagination: {offset: 0, limit: 50, has_more: false}});
    }
    if (url.pathname === "/admin/usage") return send({summary: {requests: 42, tokens: 182340, error_rate: 0.02}, requests: []});
    if (request.method() === "GET") return send({});
    throw new Error(`Unexpected fixture request ${request.method()} ${url.pathname}`);
  });
  return errors;
}

async function snapshot(page, name, fullPage = true) {
  // Full-page captures of a scrolled page misplace the sticky sidebar.
  await page.evaluate(() => { hide("notice"); document.activeElement?.blur(); scrollTo(0, 0); });
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${name} overflows horizontally`);
  await page.screenshot({path: path.join(output, `${name}.png`), fullPage});
}

(async () => {
  fs.mkdirSync(output, {recursive: true});
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const viewport = {viewport: {width: 1440, height: 1000}, locale: "zh-CN", timezoneId: "Asia/Shanghai"};

    // The Owner edits both cards and uploads three synthetic codes.
    const ownerPage = await browser.newPage(viewport);
    const ownerErrors = await routes(ownerPage);
    await ownerPage.goto(origin + "/#site-settings");
    await ownerPage.evaluate((value) => { bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok"); }, session(owner));
    await ownerPage.waitForFunction(() => siteSettingsLoaded);
    const form = ownerPage.locator("#site-settings-form");
    await form.getByLabel("在登录页和概览页显示交流群卡片").check();
    await form.getByLabel("卡片标题").fill("水源喵交流群");
    await form.getByLabel("QQ 群号").fill("123456789");
    await form.getByLabel("加群链接（可选）").fill("https://qm.qq.com/q/example");
    await form.getByLabel("说明（可选）", {exact: true}).fill("使用问题、公告与充值客服都在群里。");
    await form.getByLabel("在额度与订阅页显示充值卡片").check();
    await form.getByLabel("客服联系方式（可选）").fill("群主 @水源喵");
    await form.getByLabel("补充说明（可选）").fill("最低充值 ¥10，通常 24 小时内到账。\n到账以结算流水为准。");
    await form.getByRole("button", {name: "保存文字设置"}).click();
    await form.getByText("已保存，各页面立即显示新内容。").waitFor();
    assert.equal(writes[0].body.revision, 1);
    assert.equal(writes[0].body.community_qq_group, "123456789");
    for (const [slot, seed] of [["community_qr", 7], ["recharge_wechat", 11], ["recharge_alipay", 23]]) {
      const host = ownerPage.locator(`[data-site-image-slot="${slot}"]`);
      await host.locator("[data-site-image-file]").setInputFiles({name: `${slot}.png`, mimeType: "image/png", buffer: syntheticCode(seed)});
      await host.getByRole("button", {name: "上传"}).click();
      await host.getByText("图片已上传并立即生效。").waitFor();
      await host.locator("img").waitFor();
    }
    assert.equal(writes.filter((write) => write.bytes).length, 3);
    await ownerPage.waitForFunction(() => !siteSettingsOperation && siteSettingsRevision === siteSettingsResult.revision);
    await snapshot(ownerPage, "site-settings-desktop");
    await ownerPage.setViewportSize({width: 390, height: 844});
    await snapshot(ownerPage, "site-settings-mobile");
    assert.deepEqual(ownerErrors, []);

    // Anonymous visitors see the community card on the login page.
    const loginPage = await browser.newPage(viewport);
    const loginErrors = await routes(loginPage);
    await loginPage.goto(origin + "/");
    await loginPage.evaluate(async () => { bindUI(); show("login-view"); await loadPublicCommunity(); });
    const loginCard = loginPage.locator("#auth-community");
    await loginCard.getByText("123456789").waitFor();
    assert.equal(await loginCard.getByRole("link", {name: /一键加群/}).getAttribute("rel"), "noopener noreferrer");
    await loginCard.locator("img").waitFor();
    await snapshot(loginPage, "site-community-login-desktop", false);
    await loginPage.setViewportSize({width: 390, height: 844});
    await snapshot(loginPage, "site-community-login-mobile");
    assert.deepEqual(loginErrors, []);

    // A member sees the community card on the overview and recharge on billing.
    const memberPage = await browser.newPage(viewport);
    const memberErrors = await routes(memberPage);
    await memberPage.goto(origin + "/#overview");
    await memberPage.evaluate(async (value) => {
      bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok");
      await Promise.all([loadSiteContent(), loadBillingDetail()]);
    }, session(member));
    await memberPage.locator("#overview-community").getByText("水源喵交流群").waitFor();
    await snapshot(memberPage, "site-community-overview-desktop", false);
    await memberPage.evaluate(() => { location.hash = "billing"; });
    const recharge = memberPage.locator("#billing-recharge");
    await recharge.locator("#billing-recharge-username").getByText("lin", {exact: true}).waitFor();
    assert.equal(await recharge.locator("img").count(), 2);
    assert.match(await recharge.textContent(), /充值需要管理员确认后才会到账/);
    assert.match(await recharge.textContent(), /客服：群主 @水源喵。交流群号：123456789。/);
    await snapshot(memberPage, "site-recharge-billing-desktop", false);
    await memberPage.setViewportSize({width: 390, height: 844});
    await snapshot(memberPage, "site-recharge-billing-mobile");
    assert.equal(await memberPage.locator("#plans-recharge-hint").evaluate((node) => node.classList.contains("hidden")), false);
    assert.deepEqual(memberErrors, []);
    console.log("Site settings browser: owner edit/upload, login, overview and billing cards, desktop/mobile passed");
  } finally {
    await browser.close();
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
