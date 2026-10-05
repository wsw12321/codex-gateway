"use strict";

// Run before the stylesheet so the saved appearance is applied on first paint.
// This isolated script stores only an allowlisted appearance preference.
(() => {
  const storageKey = "shuiyuan-theme";
  const choices = new Set(["light", "dark", "system"]);
  const systemTheme = typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-color-scheme: dark)") : null;
  let storage = null;
  let preference = "system";
  const normalize = (value) => choices.has(value) ? value : "system";
  try {
    storage = window.localStorage;
    preference = normalize(storage.getItem(storageKey));
  } catch (_) { /* Appearance remains usable when browser storage is blocked. */ }

  function syncControls() {
    document.querySelectorAll("[data-theme-choice]").forEach((button) => {
      button.setAttribute("aria-pressed", String(button.dataset.themeChoice === preference));
    });
  }

  function applyTheme() {
    const theme = preference === "system"
      ? (systemTheme?.matches ? "dark" : "light") : preference;
    document.documentElement.dataset.theme = theme;
    document.documentElement.dataset.themePreference = preference;
    document.querySelector('meta[name="theme-color"]')?.setAttribute("content", theme === "dark" ? "#0d191c" : "#f4f8f7");
    syncControls();
  }

  applyTheme();
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", syncControls, {once: true});
  }
  document.addEventListener("click", (event) => {
    const button = event.target.closest?.("[data-theme-choice]");
    if (!button || !choices.has(button.dataset.themeChoice)) return;
    preference = button.dataset.themeChoice;
    applyTheme();
    try { storage?.setItem(storageKey, preference); } catch (_) { /* Saving is optional. */ }
  });
  const onSystemChange = () => {
    if (preference === "system") applyTheme();
  };
  if (systemTheme?.addEventListener) systemTheme.addEventListener("change", onSystemChange);
  else if (systemTheme?.addListener) systemTheme.addListener(onSystemChange);
  window.addEventListener("storage", (event) => {
    if (event.storageArea && event.storageArea !== storage) return;
    if (event.key !== null && event.key !== storageKey) return;
    preference = normalize(event.newValue);
    applyTheme();
  });
})();
