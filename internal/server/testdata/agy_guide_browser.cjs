"use strict";

// Optional browser verification using synthetic state and locally served assets.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
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
  api_keys: [{id: "key", name: "Gemini", key_prefix: "cgk_v1_example", status: "active", model_allowlist: ["gemini-pro-agent"]}],
  passkeys: [],
};

async function verifyLauncher(command, client, expectedOrigin) {
  assert.ok(command.startsWith('node -e "') && command.endsWith('"'));
  const script = command.slice(9, -1);
  assert.ok(!/["\\$`!%\r\n]/.test(script), "launcher must be safe inside CMD and POSIX double quotes");
  assert.ok(!script.includes("cgk_"), "copied launcher must not contain a key");
  for (const outcome of ["success", "http-failure", "download-failure", "spawn-failure", "login-failure", "cancel"]) {
    const temporary = "/tmp/中文 用户/gateway-setup-example";
    const target = `${temporary}/configure-client.cjs`;
    const calls = [];
    const fakeProcess = {execPath: "/usr/bin/node", exitCode: 0};
    const fsMock = {
      mkdtempSync(prefix) { assert.equal(prefix, "/tmp/中文 用户/gateway-setup-"); calls.push("create"); return temporary; },
      writeFileSync(file, body, options) {
        assert.equal(file, target); assert.equal(body, "/* configurator */");
        assert.deepEqual(JSON.parse(JSON.stringify(options)), {mode: 384, flag: "wx"}); calls.push("write");
      },
      rmSync(directory, options) {
        assert.equal(directory, temporary);
        assert.deepEqual(JSON.parse(JSON.stringify(options)), {recursive: true, force: true}); calls.push("cleanup");
      },
    };
    const cpMock = {spawnSync(executable, args, options) {
      assert.equal(executable, fakeProcess.execPath);
      assert.deepEqual(Array.from(args), [target, client, expectedOrigin]);
      assert.equal(options.stdio, "inherit", "interactive key input must keep the terminal");
      calls.push("spawn");
      return outcome === "spawn-failure" ? {error: new Error("spawn failed")} :
        {status: outcome === "cancel" ? null : outcome === "login-failure" ? 7 : 0};
    }};
    await vm.runInNewContext(script, {
      Buffer, AbortSignal, process: fakeProcess,
      require(name) { return {"node:fs": fsMock, "node:os": {tmpdir: () => "/tmp/中文 用户"}, "node:path": path.posix, "node:child_process": cpMock}[name]; },
      async fetch(url, options) {
        assert.equal(url, `${expectedOrigin}/setup/configure-client.cjs`);
        assert.equal(options.redirect, "error"); assert.ok(options.signal);
        calls.push("download");
        if (outcome === "download-failure") throw new Error("download failed");
        return {ok: outcome !== "http-failure", status: 503, text: async () => "/* configurator */"};
      },
      console: {error() { calls.push("error"); }},
    });
    assert.ok(calls.includes("cleanup"), `${outcome} must clean temporary files`);
    assert.equal(fakeProcess.exitCode, outcome === "success" ? 0 : outcome === "login-failure" ? 7 : 1);
    if (outcome === "http-failure" || outcome === "download-failure") assert.ok(!calls.includes("spawn"));
  }
}

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
    const guide = page.locator('[data-section="guide"]');
    const agyGuide = page.locator("#guide-agy");
    await guide.waitFor({state: "visible"});
    assert.equal(await page.locator("#guide-base-url").textContent(), origin);
    assert.equal(await page.locator("#guide-codex-install-code").textContent(), "npm install -g @openai/codex");
    assert.equal(await page.locator("#guide-agy-install-windows-code").textContent(), "curl -fsSL https://antigravity.google/cli/install.cmd -o install.cmd && install.cmd && del install.cmd");
    assert.equal(await page.locator("#guide-agy-install-unix-code").textContent(), "curl -fsSL https://antigravity.google/cli/install.sh | bash");
    assert.equal(await page.locator("#guide-codex-start-code").textContent(), "codex");
    assert.equal(await page.locator("#guide-agy-start-code").textContent(), "agy --model gemini-pro-agent");
    for (const client of ["codex", "agy"]) {
      const command = await page.locator(`#guide-${client}-configure-code`).textContent();
      await verifyLauncher(command, client, origin);
    }
    const guideText = await guide.textContent();
    for (const text of ["公共准备", "Codex CLI", "agy CLI", "Win+R", "cmd", "Node.js LTS", "CODEX_HOME", "0600", "CPA 原生模型 ID", "重新打开终端", "辅助标题请求也需使用已授权模型"]) {
      assert.ok(guideText.includes(text), `guide missing ${text}`);
    }
    for (const model of ["gemini-pro-agent"]) {
      assert.ok(guideText.includes(model));
    }
    assert.equal(await guide.locator('a[download]').count(), 0);
    assert.ok(!guideText.includes("本机 AGY"));
    assert.ok(!guideText.includes("configure-codex.bat"));
    await page.evaluate(() => { window.guideCopied = ""; navigator.clipboard.writeText = async (value) => { window.guideCopied = value; }; });
    for (const button of await guide.locator("[data-copy-target]").all()) {
      const target = await button.getAttribute("data-copy-target");
      await button.click();
      assert.equal(await page.evaluate(() => window.guideCopied), await page.locator(`#${target}`).textContent());
    }
    assert.deepEqual(await page.evaluate(() => [localStorage.length, sessionStorage.length]), [0, 0]);
    // Let the copy button's transient feedback settle before documenting the UI.
    await page.waitForTimeout(1600);
    fs.mkdirSync(screenshots, {recursive: true});
    // Capture the guide panel without sticky navigation overlapping a tall crop.
    const screenshotStyle = ".app > aside, .skip-link { visibility: hidden !important; }";
    await guide.screenshot({path: path.join(screenshots, "usage-guide-desktop.png"), style: screenshotStyle});
    await agyGuide.screenshot({path: path.join(screenshots, "agy-guide-desktop.png"), style: screenshotStyle});
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile guide must not overflow horizontally");
    await guide.screenshot({path: path.join(screenshots, "usage-guide-mobile.png"), style: screenshotStyle});
    await agyGuide.screenshot({path: path.join(screenshots, "agy-guide-mobile.png"), style: screenshotStyle});
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: await browser.version(), errors, desktop: "1440x1080", mobile: "390x844", screenshots}));
  } finally {
    await browser.close();
  }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
