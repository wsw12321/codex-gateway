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
  api_keys: [{id: "key", name: "Gemini", key_prefix: "cgk_v1_example", status: "active", model_allowlist: ["gemini-3.8-flash-high"]}],
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
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const context = await browser.newContext({viewport: {width: 1440, height: 1080}, deviceScaleFactor: 1, locale: "zh-CN"});
    const page = await context.newPage();
    const errors = [];
    const setupModels = [
      {id: "claude-sonnet-5-5", group: "Claude"},
      {id: "gpt-6.2-astra", group: "GPT"},
      {id: "gpt-6.2-sol", group: "GPT"},
      {id: "gpt-6-astra", group: "GPT"},
      {id: "gemini-3.8-flash-high", group: "Gemini"},
    ];
    let catalogFails = true;
    let catalogEmpty = false;
    let downloadFails = true;
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const url = new URL(route.request().url());
      assert.equal(url.origin, origin);
      if (url.pathname === "/") return route.fulfill({contentType: "text/html", body: fs.readFileSync(path.join(assets, "index.html"), "utf8")});
      if (url.pathname === "/static/style.css") return route.fulfill({contentType: "text/css", body: fs.readFileSync(path.join(assets, "style.css"), "utf8")});
      if (url.pathname === "/static/theme.js") return route.fulfill({contentType: "application/javascript", body: fs.readFileSync(path.join(assets, "theme.js"), "utf8")});
      if (url.pathname === "/static/favicon.svg") return route.fulfill({contentType: "image/svg+xml", body: fs.readFileSync(path.join(assets, "favicon.svg"), "utf8")});
      if (url.pathname === "/static/app.js") return route.fulfill({contentType: "application/javascript", body: app});
      if (url.pathname === "/admin/model-catalog") {
        assert.equal(url.searchParams.get("scope"), "client_setup");
        if (catalogFails) return route.fulfill({status: 503, json: {error: {message: "模型目录暂不可用"}}});
        return route.fulfill({json: {models: catalogEmpty ? [] : [
          ...setupModels, {id: "unsupported-model", group: "其他"},
        ]}});
      }
      if (url.pathname === "/setup/configure-opencode.ps1") {
        assert.equal(url.search, "");
        if (downloadFails) return route.fulfill({status: 503});
        return route.fulfill({contentType: "text/plain; charset=utf-8", body: fs.readFileSync(path.join(assets, "configure-opencode.ps1"), "utf8")});
      }
      if (url.pathname === "/favicon.ico") return route.fulfill({status: 204});
      throw new Error(`Unexpected synthetic request: ${url.pathname}`);
    });
    await page.goto(`${origin}/#guide`);
    await page.evaluate((value) => {
      bindUI(); initializeDateFilters(); renderState(value); setConnection("已连接", "ok");
    }, initialState);
    const guide = page.locator('[data-section="guide"]');
    await guide.waitFor({state: "visible"});
    // The initial OS tab follows the browser platform.
    const detected = await page.evaluate(() => detectGuideOS(navigator));
    assert.equal(await page.locator(`#guide-os-${detected}`).getAttribute("aria-selected"), "true");
    assert.equal(await page.locator("#guide-tab-opencode").getAttribute("aria-selected"), "true");
    assert.equal(await page.locator("#guide-prepare").isVisible(), false);
    assert.equal(await page.locator("#guide-os-switcher").isVisible(), false);
    assert.equal(await page.locator("#guide-opencode-base-url").textContent(), origin + "/v1");
    const openSetup = page.locator("#guide-opencode-download");
    const setupDialog = page.locator("#opencode-download-dialog");
    const confirmSetup = page.locator("#opencode-download-confirm");
    const retrySetup = page.locator("#opencode-download-retry");
    const planModel = page.locator("#opencode-plan-model");
    const buildModel = page.locator("#opencode-build-model");
    await openSetup.click();
    await retrySetup.waitFor({state: "visible"});
    assert.equal(await confirmSetup.isDisabled(), true);
    catalogFails = false;
    catalogEmpty = true;
    await retrySetup.click();
    await setupDialog.getByText("网关目录没有可配置的模型。").waitFor();
    assert.equal(await confirmSetup.isDisabled(), true);
    await page.keyboard.press("Escape");
    await setupDialog.waitFor({state: "hidden"});
    catalogEmpty = false;
    await openSetup.click();
    await confirmSetup.waitFor({state: "visible"});
    await planModel.locator('option[value="gpt-6.2-astra"]').waitFor({state: "attached"});
    assert.equal(await planModel.inputValue(), "gpt-6.2-astra");
    assert.equal(await buildModel.inputValue(), "gpt-6-astra");
    assert.equal(await planModel.locator("option").count(), setupModels.length);
    assert.equal(await buildModel.locator('option[value="gpt-6.2-sol"]').count(), 1);
    assert.equal(await planModel.locator('option[value="unsupported-model"]').count(), 0);
    await planModel.selectOption("claude-sonnet-5-5");
    await buildModel.selectOption("gemini-3.8-flash-high");
    await confirmSetup.click();
    await setupDialog.getByText("下载配置脚本失败，请重试。").waitFor();
    assert.equal(await planModel.inputValue(), "claude-sonnet-5-5");
    downloadFails = false;
    const [download] = await Promise.all([page.waitForEvent("download"), confirmSetup.click()]);
    assert.equal(download.suggestedFilename(), "configure-opencode.ps1");
    const script = fs.readFileSync(await download.path(), "utf8");
    assert.equal(script.charCodeAt(0), 0xFEFF, "PowerShell 5.1 needs a UTF-8 BOM");
    assert.ok(!script.includes("__OPENCODE_SETUP_PAYLOAD__"));
    assert.ok(!script.includes(initialState.api_keys[0].key_prefix));
    const payload = {
      origin, plan: {id: "claude-sonnet-5-5", group: "Claude"}, build: {id: "gemini-3.8-flash-high", group: "Gemini"},
      models: setupModels,
    };
    assert.ok(script.includes(Buffer.from(JSON.stringify(payload), "utf8").toString("base64")));
    await setupDialog.waitFor({state: "hidden"});
    assert.equal(await page.locator("#guide-base-url").textContent(), origin);
    assert.equal(await page.locator("#guide-codex-install-code").textContent(), "npm install -g @openai/codex");
    assert.equal(await page.locator("#guide-codex-desktop-install-windows-code").textContent(), "winget install --id 9PLM9XGG6VKS -s msstore");
    assert.equal(await page.locator("#guide-claude-install-windows-code").textContent(), "curl -fsSL https://claude.ai/install.cmd -o install.cmd && install.cmd && del install.cmd");
    assert.equal(await page.locator("#guide-claude-install-unix-code").textContent(), "curl -fsSL https://claude.ai/install.sh | bash");
    assert.equal(await page.locator("#guide-agy-install-windows-code").textContent(), "curl -fsSL https://antigravity.google/cli/install.cmd -o install.cmd && install.cmd && del install.cmd");
    assert.equal(await page.locator("#guide-agy-install-unix-code").textContent(), "curl -fsSL https://antigravity.google/cli/install.sh | bash");
    assert.equal(await page.locator("#guide-codex-start-code").textContent(), "codex");
    assert.equal(await page.locator("#guide-claude-start-code").textContent(), "claude");
    assert.equal(await page.locator("#guide-agy-start-code").textContent(), "agy --model gemini-3.8-flash-high");
    for (const client of ["codex", "agy", "claude"]) {
      const command = await page.locator(`#guide-${client}-configure-code`).textContent();
      await verifyLauncher(command, client, origin);
    }
    const guideText = await guide.textContent();
    for (const text of ["公共准备", "Codex CLI", "Codex 桌面端", "Codex IDE 扩展", "Claude Code", "CC Switch", "Claude Code IDE 扩展", "agy CLI",
      "Win+R", "cmd", "Node.js LTS", "CODEX_HOME", "0600", "CPA 原生模型 ID", "重新打开终端", "辅助标题请求也需使用已授权模型",
      "Windows 已验证", "farion1231/cc-switch", "claudeCode.disableLoginPrompt"]) {
      assert.ok(guideText.includes(text), `guide missing ${text}`);
    }
    assert.equal(await guide.locator('a[href="https://ccswitch.io"]').count(), 1);
    assert.equal(await guide.locator('a[download]').count(), 0);
    assert.ok(!guideText.includes("本机 AGY"));
    assert.ok(!guideText.includes("configure-codex.bat"));

    const cards = {codex: ["#guide-codex", "#guide-codex-desktop", "#guide-codex-ide"], claude: ["#guide-claude", "#guide-claude-desktop", "#guide-claude-ide"], agy: ["#guide-agy"]};
    const osCode = {
      codex: {windows: ["#guide-codex-desktop-install-windows-code"], unix: []},
      claude: {windows: ["#guide-claude-install-windows-code"], unix: ["#guide-claude-install-unix-code"]},
      agy: {windows: ["#guide-agy-install-windows-code"], unix: ["#guide-agy-install-unix-code"]},
    };
    await page.evaluate(() => { window.guideCopied = ""; navigator.clipboard.writeText = async (value) => { window.guideCopied = value; }; });
    for (const provider of Object.keys(cards)) {
      await page.locator(`#guide-tab-${provider}`).click();
      for (const platform of ["windows", "macos", "linux"]) {
        await page.locator(`#guide-os-${platform}`).click();
        assert.equal(await page.locator(`#guide-os-${platform}`).getAttribute("aria-selected"), "true");
        for (const [other, ids] of Object.entries(cards)) {
          for (const id of ids) assert.equal(await page.locator(id).isVisible(), other === provider, `${id} on ${provider}/${platform}`);
        }
        const windows = platform === "windows";
        for (const id of osCode[provider].windows) assert.equal(await page.locator(id).isVisible(), windows, `${id} on ${platform}`);
        for (const id of osCode[provider].unix) assert.equal(await page.locator(id).isVisible(), !windows, `${id} on ${platform}`);
        for (const button of await guide.locator("[data-copy-target]:visible").all()) {
          const target = await button.getAttribute("data-copy-target");
          await button.click();
          assert.equal(await page.evaluate(() => window.guideCopied), await page.locator(`#${target}`).textContent());
        }
      }
    }
    // Keyboard navigation follows the WAI-ARIA tabs pattern with automatic activation.
    await page.locator("#guide-tab-codex").click();
    await page.keyboard.press("ArrowRight");
    assert.equal(await page.evaluate(() => [guideProvider, document.activeElement.id].join()), "claude,guide-tab-claude");
    await page.keyboard.press("End");
    assert.equal(await page.evaluate(() => [guideProvider, document.activeElement.id].join()), "agy,guide-tab-agy");
    await page.keyboard.press("ArrowRight");
    assert.equal(await page.evaluate(() => guideProvider), "opencode");
    await page.locator("#guide-tab-codex").click();
    await page.locator("#guide-os-linux").click();
    await page.keyboard.press("Home");
    assert.equal(await page.evaluate(() => [guideOS, document.activeElement.id].join()), "windows,guide-os-windows");
    // A state refresh keeps the user's choice.
    await page.locator("#guide-tab-claude").click();
    await page.locator("#guide-os-macos").click();
    await page.evaluate((value) => renderState(value), initialState);
    assert.equal(await page.locator("#guide-tab-claude").getAttribute("aria-selected"), "true");
    assert.equal(await page.locator("#guide-os-macos").getAttribute("aria-selected"), "true");
    assert.equal(await page.locator("#guide-claude-install-unix-code").isVisible(), true);
    assert.deepEqual(await page.evaluate(() => [localStorage.length, sessionStorage.length]), [0, 0]);

    // Let the copy button's transient feedback settle before documenting the UI.
    await page.evaluate(() => document.activeElement?.blur());
    await page.waitForTimeout(1600);
    fs.mkdirSync(screenshots, {recursive: true});
    // Capture the guide panel without sticky navigation overlapping a tall crop.
    const screenshotStyle = ".app > aside, .workspace-header, .skip-link { visibility: hidden !important; }";
    const select = async (provider, platform) => {
      await page.locator(`#guide-tab-${provider}`).click();
      await page.locator(`#guide-os-${platform}`).click();
      await page.evaluate(() => document.activeElement?.blur());
    };
    await page.locator("#guide-tab-opencode").click();
    await guide.screenshot({path: path.join(screenshots, "opencode-guide-desktop.png"), style: screenshotStyle});
    await select("codex", "windows");
    await guide.screenshot({path: path.join(screenshots, "usage-guide-desktop.png"), style: screenshotStyle});
    await select("claude", "linux");
    await guide.screenshot({path: path.join(screenshots, "claude-guide-desktop.png"), style: screenshotStyle});
    await select("agy", "windows");
    await page.locator("#guide-agy").screenshot({path: path.join(screenshots, "agy-guide-desktop.png"), style: screenshotStyle});
    await page.setViewportSize({width: 390, height: 844});
    for (const provider of Object.keys(cards)) {
      await select(provider, "windows");
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `mobile ${provider} guide must not overflow horizontally`);
    }
    await page.locator("#guide-tab-opencode").click();
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "mobile OpenCode guide must not overflow horizontally");
    await guide.screenshot({path: path.join(screenshots, "opencode-guide-mobile.png"), style: screenshotStyle});
    await openSetup.click();
    await planModel.locator('option[value="gpt-6.2-astra"]').waitFor({state: "attached"});
    assert.equal(await setupDialog.evaluate((dialog) => dialog.scrollWidth <= dialog.clientWidth), true, "mobile setup dialog must not overflow horizontally");
    await setupDialog.screenshot({path: path.join(screenshots, "opencode-dialog-mobile.png")});
    await page.locator("#opencode-download-cancel").click();
    await setupDialog.waitFor({state: "hidden"});
    await select("codex", "windows");
    await guide.screenshot({path: path.join(screenshots, "usage-guide-mobile.png"), style: screenshotStyle});
    await select("agy", "linux");
    await page.locator("#guide-agy").screenshot({path: path.join(screenshots, "agy-guide-mobile.png"), style: screenshotStyle});
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({browser: await browser.version(), errors, desktop: "1440x1080", mobile: "390x844", screenshots}));
  } finally {
    await browser.close();
  }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
