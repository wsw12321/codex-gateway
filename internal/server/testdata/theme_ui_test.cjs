"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "../assets/theme.js"), "utf8");

function browser({saved = null, dark = false, storageBlocked = false, writeBlocked = false, mediaUnavailable = false} = {}) {
  const documentListeners = {}, windowListeners = {}, mediaListeners = {}, writes = [], reads = [];
  const root = {dataset: {}};
  const themeMeta = {attributes: {}, setAttribute(name, value) { this.attributes[name] = value; }};
  const groups = Array.from({length: 2}, () => ["light", "dark", "system"].map((choice) => ({
    dataset: {themeChoice: choice}, attributes: {},
    setAttribute(name, value) { this.attributes[name] = value; },
  })));
  let parsed = false;
  const media = {matches: dark, addEventListener(name, listener) { mediaListeners[name] = listener; }};
  const storage = {
    getItem(key) { reads.push(key); return saved; },
    setItem(key, value) {
      if (writeBlocked) throw new Error("QuotaExceededError");
      writes.push([key, value]);
    },
  };
  const window = {
    get localStorage() {
      if (storageBlocked) throw new Error("SecurityError");
      return storage;
    },
    addEventListener(name, listener) { windowListeners[name] = listener; },
  };
  if (!mediaUnavailable) window.matchMedia = () => media;
  vm.runInNewContext(source, {
    window,
    document: {
      documentElement: root, readyState: "loading",
      querySelector: () => themeMeta,
      querySelectorAll: () => parsed ? groups.flat() : [],
      addEventListener(name, listener) { documentListeners[name] = listener; },
    },
  });
  return {
    root, groups, reads, writes, themeMeta,
    ready() { parsed = true; documentListeners.DOMContentLoaded(); },
    click(choice, group = 0) {
      const button = groups[group].find((entry) => entry.dataset.themeChoice === choice);
      documentListeners.click({target: {closest: () => button}});
    },
    system(dark) { media.matches = dark; mediaListeners.change(); },
    storage(key, value, area = storage) { windowListeners.storage({key, newValue: value, storageArea: area}); },
  };
}

function pressed(ui, choice) {
  for (const button of ui.groups.flat()) {
    assert.equal(button.attributes["aria-pressed"], String(button.dataset.themeChoice === choice));
  }
}

test("saved appearance applies before body parsing and synchronizes all controls afterward", () => {
  const ui = browser({saved: "dark", dark: false});
  assert.equal(ui.root.dataset.theme, "dark");
  assert.equal(ui.themeMeta.attributes.content, "#0d191c");
  ui.ready();
  pressed(ui, "dark");
  assert.deepEqual(ui.reads, ["shuiyuan-theme"]);
  assert.deepEqual(ui.writes, [], "initializing appearance must not write browser data");
});

test("choices persist only the appearance enum and survive reload", () => {
  const ui = browser({dark: true});
  ui.ready();
  ui.click("light", 1);
  assert.equal(ui.root.dataset.theme, "light");
  assert.equal(ui.themeMeta.attributes.content, "#f4f8f7");
  pressed(ui, "light");
  ui.system(true);
  assert.equal(ui.root.dataset.theme, "light", "explicit choice must override operating system");
  assert.deepEqual(ui.writes, [["shuiyuan-theme", "light"]]);
  const reloaded = browser({saved: ui.writes[0][1], dark: true});
  assert.equal(reloaded.root.dataset.theme, "light");
});

test("system mode tracks operating system changes and can be restored after an explicit choice", () => {
  const ui = browser({dark: false});
  ui.ready();
  pressed(ui, "system");
  ui.system(true);
  assert.equal(ui.root.dataset.theme, "dark");
  assert.equal(ui.themeMeta.attributes.content, "#0d191c");
  ui.click("light");
  ui.click("system");
  assert.equal(ui.root.dataset.theme, "dark");
  pressed(ui, "system");
  ui.system(false);
  assert.equal(ui.root.dataset.theme, "light");
});

test("cross-tab choices, removal, and clear synchronize without echoing storage writes", () => {
  const ui = browser({saved: "light", dark: true});
  ui.ready();
  ui.storage("shuiyuan-theme", "dark");
  assert.equal(ui.root.dataset.theme, "dark");
  pressed(ui, "dark");
  ui.storage("shuiyuan-theme", "light", {});
  ui.storage("unrelated-key", "light");
  assert.equal(ui.root.dataset.theme, "dark", "ignore unrelated storage and keys");
  ui.storage("shuiyuan-theme", null);
  pressed(ui, "system");
  ui.storage("shuiyuan-theme", "light");
  ui.storage(null, null);
  assert.equal(ui.root.dataset.theme, "dark");
  pressed(ui, "system");
  assert.deepEqual(ui.writes, []);
});

test("invalid stored values never become document attributes or persistent writes", () => {
  const ui = browser({saved: '<img src=x onerror="alert(1)">', dark: true});
  ui.ready();
  assert.equal(ui.root.dataset.theme, "dark");
  assert.equal(ui.root.dataset.themePreference, "system");
  pressed(ui, "system");
  ui.storage("shuiyuan-theme", "unexpected");
  assert.equal(ui.root.dataset.themePreference, "system");
  assert.deepEqual(ui.writes, []);
});

test("appearance remains interactive when storage is inaccessible or full", () => {
  for (const options of [{storageBlocked: true}, {writeBlocked: true}]) {
    const ui = browser({...options, dark: false});
    ui.ready();
    ui.click("dark");
    assert.equal(ui.root.dataset.theme, "dark");
    pressed(ui, "dark");
    ui.click("system");
    ui.system(true);
    assert.equal(ui.root.dataset.theme, "dark");
    pressed(ui, "system");
  }
});

test("browsers without a system preference API still allow explicit appearance choices", () => {
  const ui = browser({mediaUnavailable: true});
  assert.equal(ui.root.dataset.theme, "light");
  ui.ready();
  ui.click("dark");
  assert.equal(ui.root.dataset.theme, "dark");
});
