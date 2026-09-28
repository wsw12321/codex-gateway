"use strict";

// Optional browser verification using synthetic state and locally served assets.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const assets = path.join(__dirname, "../assets");
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, "../../../docs/screenshots");
const source = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const app = source.slice(0, source.lastIndexOf("\nstart().catch("));
const origin = "https://gateway.example.test";
const initialState = {
  user: {id: "member", username: "lin", display_name: "林同学", role: "member", status: "active"},
  recently_verified: true, login_methods: {password: true},
  devices: [{id: "device", name: "工作电脑", status: "active"}], projects: [],
  api_keys: [{id: "key", name: "Gemini", key_prefix: "cgk_v1_example", status: "active", model_allowlist: ["gemini-3.1-pro-high"]}],
  passkeys: [],
};

async function main() {
  const browser = await chromium.launch({headless: true});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const url = new URL(route.request().url());
      assert.equal(url.origin, origin);
      if (url.pathname === "/") return route.fulfill({contentType: "text/html", body: fs.readFileSync(path.join(assets, "index.html"), "utf8")});
      if (url.pathname === "/static/style.css") return route.fulfill({contentType: "text/css", body: fs.readFileSync(path.join(assets, "style.css"), "utf8")});
      if (url.pathname === "/static/app.js") return route.fulfill({contentType: "application/javascript", body: app});
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      throw new Error(`Unexpected synthetic request: ${url.pathname}`);
    });
    await page.goto(`${origin}/#guide`);
    await page.evaluate((value) => {
      bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok");
    }, initialState);
    const guide = page.locator("#guide-agy");
    await guide.waitFor({state: "visible"});
    assert.equal(await page.locator("#guide-base-url").textContent(), `${origin}/v1`);
    assert.equal(await page.locator("#guide-agy-base-url").textContent(), origin);
    assert.deepEqual(JSON.parse(await page.locator("#guide-agy-config-code").textContent()), {
      model: "gemini-3.1-pro-high", input: "Reply with exactly OK.", store: false,
    });
    const shell = await page.locator("#guide-agy-shell-code").textContent();
    assert.ok(shell.includes(`export GATEWAY_BASE_URL='${origin}'`));
    assert.ok(shell.includes("read -r -s -p 'Gateway API Key: ' GATEWAY_API_KEY"));
    assert.ok(shell.includes("--config -"), "read the key through stdin, not process arguments");
    assert.ok(shell.includes('"model":"gemini-3.1-pro-high"'));
    assert.ok(shell.endsWith('"$GATEWAY_BASE_URL/v1/responses"'));
    assert.ok(!shell.includes("agy --model"));
    const guideText = await guide.textContent();
    for (const family of ["3.8", "3.7", "3.6"]) {
      for (const level of ["high", "medium"]) assert.ok(guideText.includes(`gemini-${family}-flash-${level}`));
    }
    assert.ok(guideText.includes("默认禁用"));
    assert.ok(guideText.includes("不能直接采用该配置连接 Gateway"));
    await page.evaluate(() => { window.guideCopied = ""; navigator.clipboard.writeText = async (value) => { window.guideCopied = value; }; });
    await page.locator('[data-copy-target="guide-agy-shell-code"]').click();
    assert.equal(await page.evaluate(() => window.guideCopied), shell);
    await page.locator('[data-copy-target="guide-agy-base-url"]').click();
    assert.equal(await page.evaluate(() => window.guideCopied), origin);
    // Let the copy button's transient feedback settle before documenting the UI.
    await page.waitForTimeout(1600);
    fs.mkdirSync(screenshots, {recursive: true});
    // Capture the guide panel without sticky navigation overlapping a tall crop.
    const screenshotStyle = ".app > aside, .skip-link { visibility: hidden !important; }";
    await guide.screenshot({path: path.join(screenshots, "agy-guide-desktop.png"), style: screenshotStyle});
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile guide must not overflow horizontally");
    await guide.screenshot({path: path.join(screenshots, "agy-guide-mobile.png"), style: screenshotStyle});
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: await browser.version(), errors, desktop: "1440x1080", mobile: "390x844", screenshots}));
  } finally {
    await browser.close();
  }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
