"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const application = source.slice(0, source.lastIndexOf("\nstart().catch("));
const html = fs.readFileSync(path.join(__dirname, "../assets/index.html"), "utf8");
const guideHTML = html.slice(html.indexOf('data-section="guide"'), html.indexOf("</section>", html.indexOf('data-section="guide"')));

const datasetName = (attribute) => attribute.replace(/^data-/, "").replace(/-([a-z])/g, (_, letter) => letter.toUpperCase());

class Element {
  constructor(attributes = {}) {
    this.attributes = {}; this.dataset = {}; this.listeners = {}; this.textContent = ""; this.focused = 0;
    const classes = new Set(String(attributes.class || "").split(/\s+/).filter(Boolean));
    this.classList = {
      add: (name) => classes.add(name), remove: (name) => classes.delete(name), contains: (name) => classes.has(name),
      toggle: (name, force) => { const next = force === undefined ? !classes.has(name) : Boolean(force); if (next) classes.add(name); else classes.delete(name); return next; },
    };
    for (const [name, value] of Object.entries(attributes)) {
      if (name === "class") continue;
      if (name.startsWith("data-")) this.dataset[datasetName(name)] = value;
      this.attributes[name] = value;
    }
    this.tabIndex = attributes.tabindex === undefined ? 0 : Number(attributes.tabindex);
  }
  get hidden() { return this.classList.contains("hidden"); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name]; }
  addEventListener(name, handler) { this.listeners[name] = handler; }
  replaceChildren() {}
  focus() { this.focused++; }
}

// Build stub elements from the real guide markup so the test also checks the
// initial HTML state.
function guideElements() {
  const byAttribute = {"data-guide-provider-tab": [], "data-guide-provider-panel": [], "data-guide-os-tab": [], "data-guide-os": []};
  for (const [tag] of guideHTML.matchAll(/<[a-z][^>]*\sdata-guide-[a-z-]+="[^"]*"[^>]*>/g)) {
    const attributes = {};
    for (const [, name, value] of tag.matchAll(/\s([a-z-]+)="([^"]*)"/g)) attributes[name] = value;
    const element = new Element(attributes);
    for (const name of Object.keys(byAttribute)) if (Object.hasOwn(attributes, name)) byAttribute[name].push(element);
  }
  return byAttribute;
}

function dashboard(navigator) {
  const elements = guideElements();
  const nodes = new Map();
  for (const list of Object.values(elements)) for (const element of list) if (element.attributes.id) nodes.set(element.attributes.id, element);
  const node = (id) => { if (!nodes.has(id)) nodes.set(id, new Element({id})); return nodes.get(id); };
  const context = vm.createContext({
    URL, URLSearchParams, btoa,
    document: {
      getElementById: node, createElement: () => new Element(), addEventListener() {},
      querySelectorAll: (selector) => elements[selector.replace(/^\[|\]$/g, "")] || [],
    },
    window: {}, location: {hash: "#guide", origin: "https://gateway.example.test"},
    ...(navigator ? {navigator} : {}),
  });
  const run = (code) => vm.runInContext(code, context);
  run(application);
  return {run, node, elements};
}

const providerState = (ui) => ui.elements["data-guide-provider-tab"].map((tab) => [tab.dataset.guideProviderTab, tab.getAttribute("aria-selected"), tab.tabIndex, tab.classList.contains("active")]);
const visiblePanels = (ui) => ui.elements["data-guide-provider-panel"].filter((panel) => !panel.hidden).map((panel) => panel.dataset.guideProviderPanel);
const visibleOSBlocks = (ui, platform) => ui.elements["data-guide-os"].filter((block) => block.dataset.guideOs.split(" ").includes(platform));

test("detects the initial OS tab without confusing Darwin with Windows", () => {
  const ui = dashboard();
  const detect = (nav) => ui.run(`detectGuideOS(${JSON.stringify(nav)})`);
  assert.equal(ui.run("detectGuideOS()"), "windows", "no navigator falls back to Windows");
  assert.equal(detect({userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64)", platform: "Win32"}), "windows");
  assert.equal(detect({userAgentData: {platform: "Windows"}}), "windows");
  assert.equal(detect({userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5)", platform: "MacIntel"}), "macos");
  assert.equal(detect({userAgent: "CFNetwork Darwin/24.0", platform: "MacIntel"}), "macos");
  assert.equal(detect({userAgentData: {platform: "macOS"}}), "macos");
  assert.equal(detect({userAgent: "Mozilla/5.0 (X11; Linux x86_64)", platform: "Linux x86_64"}), "linux");
  assert.equal(detect({userAgent: "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0)"}), "linux");
  assert.equal(detect({userAgent: "UnknownAgent/1.0"}), "windows");
  assert.equal(dashboard({userAgent: "Mozilla/5.0 (X11; Linux x86_64)", platform: "Linux x86_64"}).run("guideOS"), "linux");
});

test("initial markup already shows the default Codex and Windows content", () => {
  const ui = dashboard();
  const before = {
    providers: providerState(ui), panels: visiblePanels(ui),
    osTabs: ui.elements["data-guide-os-tab"].map((tab) => [tab.dataset.guideOsTab, tab.getAttribute("aria-selected"), tab.tabIndex]),
    blocks: ui.elements["data-guide-os"].map((block) => block.hidden),
  };
  ui.run('showGuideProvider("codex"); showGuideOS("windows");');
  assert.deepEqual(providerState(ui), before.providers);
  assert.deepEqual(visiblePanels(ui), before.panels);
  assert.deepEqual(ui.elements["data-guide-os-tab"].map((tab) => [tab.dataset.guideOsTab, tab.getAttribute("aria-selected"), tab.tabIndex]), before.osTabs);
  assert.deepEqual(ui.elements["data-guide-os"].map((block) => block.hidden), before.blocks);
  assert.deepEqual(visiblePanels(ui), ["codex"]);
});

test("provider tabs switch panels and ignore unknown names", () => {
  const ui = dashboard();
  ui.run('showGuideProvider("claude")');
  assert.deepEqual(providerState(ui), [["codex", "false", -1, false], ["claude", "true", 0, true], ["agy", "false", -1, false]]);
  assert.deepEqual(visiblePanels(ui), ["claude"]);
  ui.run('showGuideProvider("antigravity-desktop")');
  assert.equal(ui.run("guideProvider"), "claude");
  assert.deepEqual(visiblePanels(ui), ["claude"]);
});

test("OS tabs show only the blocks for the selected platform", () => {
  const ui = dashboard();
  for (const platform of ["linux", "macos", "windows"]) {
    ui.run(`showGuideOS(${JSON.stringify(platform)})`);
    for (const block of ui.elements["data-guide-os"]) {
      assert.equal(block.hidden, !block.dataset.guideOs.split(" ").includes(platform), `${block.dataset.guideOs} on ${platform}`);
    }
    assert.ok(visibleOSBlocks(ui, platform).length > 0);
    assert.deepEqual(ui.elements["data-guide-os-tab"].filter((tab) => tab.getAttribute("aria-selected") === "true").map((tab) => tab.dataset.guideOsTab), [platform]);
  }
  ui.run('showGuideOS("android")');
  assert.equal(ui.run("guideOS"), "windows");
});

test("arrow, Home and End keys move and activate guide tabs", () => {
  const ui = dashboard();
  const tabs = ui.elements["data-guide-provider-tab"];
  const press = (target, key) => {
    const event = {key, currentTarget: target, prevented: false, preventDefault() { this.prevented = true; }};
    ui.run("moveGuideTab")(event, "[data-guide-provider-tab]", "guideProviderTab", ui.run("showGuideProvider"));
    return event;
  };
  assert.equal(press(tabs[0], "ArrowLeft").prevented, true);
  assert.equal(ui.run("guideProvider"), "agy");
  assert.equal(tabs[2].focused, 1);
  press(tabs[2], "ArrowRight");
  assert.equal(ui.run("guideProvider"), "codex");
  press(tabs[0], "End");
  assert.equal(ui.run("guideProvider"), "agy");
  press(tabs[2], "Home");
  assert.equal(ui.run("guideProvider"), "codex");
  const ignored = press(tabs[0], "Enter");
  assert.equal(ignored.prevented, false);
  assert.equal(ui.run("guideProvider"), "codex");
});

test("renderGuide keeps the selection and fills every configure command", () => {
  const ui = dashboard();
  ui.run('showGuideProvider("agy"); showGuideOS("linux"); state = null; renderGuide();');
  assert.equal(ui.run("guideProvider"), "agy");
  assert.equal(ui.run("guideOS"), "linux");
  assert.deepEqual(visiblePanels(ui), ["agy"]);
  assert.equal(ui.node("guide-base-url").textContent, "https://gateway.example.test");
  for (const client of ["codex", "claude", "agy"]) {
    const command = ui.node(`guide-${client}-configure-code`).textContent;
    assert.ok(command.startsWith('node -e "'), client);
    assert.ok(command.includes(`'${client}'`), client);
  }
});
