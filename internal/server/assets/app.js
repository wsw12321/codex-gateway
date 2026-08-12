"use strict";

const byId = (id) => document.getElementById(id);
const all = (selector, root = document) => Array.from(root.querySelectorAll(selector));
const sectionTitles = {
  overview: "概览",
  resources: "资源",
  keys: "API Keys",
  billing: "额度与订阅",
  security: "账号安全",
  usage: "使用统计",
};
const dateTimeFormatter = new Intl.DateTimeFormat("zh-CN", {
  year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit",
});
const dateFormatter = new Intl.DateTimeFormat("zh-CN", {
  year: "numeric", month: "2-digit", day: "2-digit",
});
const integerFormatter = new Intl.NumberFormat("zh-CN", {maximumFractionDigits: 0});

let invitationToken = "";
let invitationKind = "member";
let state = null;
let overviewSummary = null;
let noticeTimer = 0;
let secretAfterClose = null;
let personalRequestSequence = 0;
let globalRequestSequence = 0;
let checkingSession = false;
let billingDetail = null;
let billingUsers = [];
let billingSettings = null;
let billingLedgerOffset = 0;
let billingLedgerNextOffset = 0;
let billingRequestSequence = 0;

const billingLedgerPageSize = 50;
const billingTiers = [
  {id: "day", label: "日订阅", duration: "24 小时"},
  {id: "week", label: "周订阅", duration: "7 天"},
  {id: "month", label: "月订阅", duration: "31 天"},
];

function show(target) {
  const element = typeof target === "string" ? byId(target) : target;
  element?.classList.remove("hidden");
}

function hide(target) {
  const element = typeof target === "string" ? byId(target) : target;
  element?.classList.add("hidden");
}

function announce(message) {
  byId("operation-status").textContent = message;
}

function notice(message, kind = "info", persistent = false) {
  const element = byId("notice");
  window.clearTimeout(noticeTimer);
  element.textContent = message;
  element.dataset.kind = kind;
  element.setAttribute("role", kind === "error" ? "alert" : "status");
  show(element);
  if (!persistent) {
    noticeTimer = window.setTimeout(() => hide(element), 6500);
  }
}

function setConnection(label, status) {
  const element = byId("connection");
  element.textContent = label;
  element.dataset.state = status;
}

function element(tag, options = {}, ...children) {
  const node = document.createElement(tag);
  if (options.className) node.className = options.className;
  if (options.text != null) node.textContent = String(options.text);
  if (options.type) node.type = options.type;
  if (options.dataset) Object.assign(node.dataset, options.dataset);
  if (options.attributes) {
    for (const [name, value] of Object.entries(options.attributes)) {
      if (value != null) node.setAttribute(name, String(value));
    }
  }
  node.append(...children.filter(Boolean));
  return node;
}

function field(object, ...names) {
  for (const name of names) {
    if (object != null && Object.prototype.hasOwnProperty.call(object, name)) return object[name];
  }
  return undefined;
}

function formatInteger(value) {
  const number = Number(value || 0);
  return Number.isFinite(number) ? integerFormatter.format(number) : String(value ?? "0");
}

function formatPercent(value, empty = "0.0%") {
  const number = Number(value);
  if (!Number.isFinite(number)) return empty;
  return `${(number * 100).toFixed(Math.abs(number) < .001 && number !== 0 ? 2 : 1)}%`;
}

function formatMoney(value, currency) {
  const raw = String(value ?? "0").trim();
  const match = raw.match(/^(-?)(\d+)(?:\.(\d+))?$/);
  if (!match) return `${currency} ${raw}`;
  const sign = match[1];
  const whole = match[2].replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  let fraction = (match[3] || "").padEnd(2, "0");
  while (fraction.length > 2 && fraction.endsWith("0")) fraction = fraction.slice(0, -1);
  return `${sign}${currency === "USD" ? "US$" : "¥"}${whole}.${fraction}`;
}

function formatUSD(value, fallback = "—") {
  if (value == null || String(value).trim() === "") return fallback;
  return formatMoney(String(value), "USD");
}

function formatDateTime(value, fallback = "从未使用") {
  if (!value) return fallback;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? fallback : dateTimeFormatter.format(date);
}

function formatDate(value, fallback = "—") {
  if (!value) return fallback;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? fallback : dateFormatter.format(date);
}

function inputDate(date) {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, "0");
  const day = String(date.getDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}

function statusLabel(status) {
  return ({
    active: "活跃", disabled: "已停用", archived: "已归档", revoked: "已撤销",
    completed: "已完成", failed: "失败", cancelled: "已取消", in_progress: "进行中",
    open: "开放", acknowledged: "已确认", resolved: "已解决",
  })[status] || status || "未知";
}

function statusBadge(status) {
  return element("span", {className: "status-badge", text: statusLabel(status), dataset: {status: status || "unknown"}});
}

function friendlyError(error) {
  if (!error) return "操作失败，请重试。";
  if (error.name === "NotAllowedError") return "已取消 Passkey 操作，或验证等待超时。";
  if (error.name === "InvalidStateError") return "这枚 Passkey 已经注册，或当前设备状态不允许此操作。";
  if (error.name === "SecurityError") return "当前页面无法安全使用 Passkey，请检查站点地址与 HTTPS 配置。";
  if (error.name === "AbortError") return "操作已取消。";
  if (error.network) return "无法连接到网关，请检查网络后重试。";
  return error.message || "操作失败，请重试。";
}

function setLocalMessage(host, message = "", kind = "error") {
  if (!host) return false;
  let scope = host.matches?.("form") ? host : host.closest?.("form, .tool-block, .auth-card, dialog");
  if (!scope && host.querySelector) scope = host;
  const target = scope?.querySelector?.(".form-message");
  if (!target) return false;
  target.textContent = message;
  target.dataset.kind = kind;
  target.setAttribute("role", kind === "error" ? "alert" : "status");
  target.classList.toggle("hidden", !message);
  return true;
}

function setBusy(host, busy, label = "处理中…") {
  if (!host) return;
  const control = host.matches?.("button") ? host : host.querySelector?.("button[type=submit]");
  host.dataset.busy = busy ? "true" : "false";
  host.setAttribute?.("aria-busy", busy ? "true" : "false");
  if (!control) return;
  if (busy) {
    if (control.childElementCount === 0) {
      control.dataset.idleLabel = control.textContent;
      control.textContent = label;
    }
    control.disabled = true;
  } else {
    if (control.dataset.idleLabel) control.textContent = control.dataset.idleLabel;
    delete control.dataset.idleLabel;
    control.disabled = false;
  }
}

function bindAsync(id, eventName, handler, busyLabel = "处理中…") {
  const host = byId(id);
  if (!host) return;
  host.addEventListener(eventName, async (event) => {
    if (host.dataset.busy === "true") return;
    if (eventName === "submit") event.preventDefault();
    setLocalMessage(host);
    setBusy(host, true, busyLabel);
    try {
      await handler(event);
    } catch (error) {
      const message = friendlyError(error);
      if (!setLocalMessage(host, message)) notice(message, "error");
    } finally {
      setBusy(host, false);
    }
  });
}

async function runButton(button, handler, busyLabel = "处理中…") {
  if (button.dataset.busy === "true") return;
  setBusy(button, true, busyLabel);
  try {
    await handler();
  } catch (error) {
    const message = friendlyError(error);
    if (!setLocalMessage(button, message)) notice(message, "error");
  } finally {
    setBusy(button, false);
  }
}

function clearSensitiveDOM() {
  const dialog = byId("secret-dialog");
  secretAfterClose = null;
  byId("secret-value").textContent = "";
  byId("secret-title").textContent = "只显示一次";
  byId("secret-description").textContent = "请立即安全保存。关闭后内容会从页面清除，无法再次查看。";
  setLocalMessage(dialog);
  if (dialog.open) dialog.close();
}

function handleUnauthorized() {
  invitationToken = "";
  secretAfterClose = null;
  state = null;
  overviewSummary = null;
  billingDetail = null;
  billingUsers = [];
  billingSettings = null;
  billingLedgerOffset = 0;
  billingLedgerNextOffset = 0;
  all("dialog[open]").forEach((dialog) => dialog.close());
  clearSensitiveDOM();
  hide("dashboard");
  show("auth");
  hide("join-view");
  hide("recover-view");
  show("login-view");
  byId("whoami").textContent = "—";
  byId("role").textContent = "—";
  for (const id of [
    "metric-requests", "metric-tokens", "metric-errors", "metric-active-keys",
    "metric-global-tokens", "metric-global-cost", "usage-requests", "metric-cache",
    "metric-ttft", "metric-duration", "global-usd", "global-cny",
    "global-total-tokens", "global-request-users", "billing-cash-balance",
    "billing-day-remaining", "billing-week-remaining", "billing-month-remaining",
  ]) byId(id).textContent = "—";
  byId("resource-summary").replaceChildren();
  byId("onboarding").replaceChildren();
  byId("alert-summary").replaceChildren();
  byId("global-overview").replaceChildren();
  byId("pricing-note").replaceChildren();
  byId("billing-subscriptions").replaceChildren(element("div", {className: "empty", text: "登录后加载。"}));
  byId("billing-ledger-rows").replaceChildren(tableMessage(5, "登录后加载账务流水。"));
  byId("billing-current-rate").textContent = "—";
  byId("billing-user-select").replaceChildren(option("", "登录后加载用户"));
  byId("billing-ledger-page").textContent = "—";
  byId("billing-ledger-prev").disabled = true;
  byId("billing-ledger-next").disabled = true;
  for (const tier of billingTiers) byId(`billing-${tier.id}-ends`).textContent = "未启用";
  hide("personal-scope");
  byId("usage-rows").replaceChildren(tableMessage(8, "登录后加载使用明细。"));
  byId("global-rows").replaceChildren(tableMessage(6, "登录后加载全员汇总。"));
  for (const id of ["devices", "projects", "keys", "passkeys"]) {
    byId(id).replaceChildren(element("div", {className: "empty", text: "登录后加载。"}));
  }
  if (!checkingSession) notice("登录会话已失效，请重新使用 Passkey 登录。", "error", true);
}

async function api(path, options = {}) {
  const headers = {...(options.headers || {})};
  if (options.body != null && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
  let response;
  try {
    response = await fetch(path, {credentials: "same-origin", cache: "no-store", ...options, headers});
  } catch (cause) {
    const error = new Error("网络请求失败", {cause});
    error.network = true;
    throw error;
  }
  const body = response.status === 204 ? {} : await response.json().catch(() => ({}));
  if (!response.ok) {
    if (response.status === 401) handleUnauthorized();
    const error = new Error(body?.error?.message || `请求失败 (${response.status})`);
    error.code = body?.error?.code;
    error.status = response.status;
    throw error;
  }
  return body;
}

function webAuthnSupported() {
  return Boolean(window.PublicKeyCredential && navigator.credentials);
}

function requireWebAuthn() {
  if (webAuthnSupported()) return;
  const error = new Error("此浏览器不支持 WebAuthn Passkey，请改用受支持的现代浏览器。");
  error.name = "NotSupportedError";
  throw error;
}

function fromBase64URL(value) {
  const base64 = value.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((value.length + 3) % 4);
  const binary = atob(base64);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

function toBase64URL(value) {
  if (value == null) return null;
  const bytes = new Uint8Array(value);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function creationOptions(envelope) {
  const source = envelope.publicKey;
  const options = {...source, user: {...source.user}};
  options.challenge = fromBase64URL(source.challenge);
  options.user.id = fromBase64URL(source.user.id);
  options.excludeCredentials = (source.excludeCredentials || []).map((item) => ({...item, id: fromBase64URL(item.id)}));
  return {publicKey: options, mediation: envelope.mediation};
}

function assertionOptions(envelope) {
  const source = envelope.publicKey;
  const options = {...source};
  options.challenge = fromBase64URL(source.challenge);
  options.allowCredentials = (source.allowCredentials || []).map((item) => ({...item, id: fromBase64URL(item.id)}));
  return {publicKey: options, mediation: envelope.mediation};
}

function serializeCredential(credential) {
  const response = {clientDataJSON: toBase64URL(credential.response.clientDataJSON)};
  if (credential.response.attestationObject) {
    response.attestationObject = toBase64URL(credential.response.attestationObject);
    response.transports = credential.response.getTransports?.() || [];
    response.publicKeyAlgorithm = credential.response.getPublicKeyAlgorithm?.();
    const publicKey = credential.response.getPublicKey?.();
    if (publicKey) response.publicKey = toBase64URL(publicKey);
  } else {
    response.authenticatorData = toBase64URL(credential.response.authenticatorData);
    response.signature = toBase64URL(credential.response.signature);
    response.userHandle = toBase64URL(credential.response.userHandle);
  }
  return {
    id: credential.id,
    rawId: toBase64URL(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment,
    clientExtensionResults: credential.getClientExtensionResults(),
    response,
  };
}

async function createPasskey(ceremony) {
  requireWebAuthn();
  const credential = await navigator.credentials.create(creationOptions(ceremony.options));
  if (!credential) throw new DOMException("Passkey creation returned no credential", "NotAllowedError");
  return serializeCredential(credential);
}

async function getPasskey(ceremony) {
  requireWebAuthn();
  const credential = await navigator.credentials.get(assertionOptions(ceremony.options));
  if (!credential) throw new DOMException("Passkey assertion returned no credential", "NotAllowedError");
  return serializeCredential(credential);
}

async function login() {
  const ceremony = await api("/auth/login/begin", {method: "POST", body: "{}"});
  const credential = await getPasskey(ceremony);
  await api("/auth/login/finish", {method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential})});
  location.assign("/#overview");
}

async function reauthenticate() {
  const ceremony = await api("/auth/reauth/begin", {method: "POST", body: "{}"});
  const credential = await getPasskey(ceremony);
  await api("/auth/reauth/finish", {method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential})});
  if (state) {
    state.recently_verified = true;
    state.recent_verification_expires_at = null;
  }
  notice("Passkey 二次验证成功，敏感操作已临时解锁。", "ok");
}

function verificationIsRecent() {
  if (!state?.recently_verified) return false;
  const expires = state.recent_verification_expires_at;
  return !expires || new Date(expires).getTime() > Date.now();
}

async function sensitiveAction(operation) {
  if (!verificationIsRecent()) await reauthenticate();
  try {
    return await operation();
  } catch (error) {
    if (error.code !== "recent_passkey_verification_required") throw error;
    if (state) state.recently_verified = false;
    await reauthenticate();
    return operation();
  }
}

function showSecret(title, value, description, afterClose = null) {
  const dialog = byId("secret-dialog");
  if (dialog.open) {
    secretAfterClose = null;
    dialog.close();
  }
  byId("secret-title").textContent = title;
  byId("secret-description").textContent = description;
  byId("secret-value").textContent = value;
  setLocalMessage(dialog);
  secretAfterClose = afterClose;
  dialog.showModal();
  window.setTimeout(() => byId("secret-value").focus(), 0);
}

function finishWithRecoveryCodes(codes) {
  const values = Array.isArray(codes) ? codes : [];
  invitationToken = "";
  showSecret(
    "保存新的恢复码",
    values.join("\n"),
    "恢复码只显示这一次。请立即离线保存；每个恢复码只能使用一次。",
    () => location.assign("/#overview"),
  );
}

async function register(event) {
  if (!invitationToken) throw new Error("邀请链接缺少令牌，或链接已被浏览器清理。");
  const data = Object.fromEntries(new FormData(event.currentTarget));
  data.invitation_token = invitationToken;
  const ceremony = await api("/auth/register/begin", {method: "POST", body: JSON.stringify(data)});
  const credential = await createPasskey(ceremony);
  const result = await api("/auth/register/finish", {
    method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential}),
  });
  finishWithRecoveryCodes(result.recovery_codes);
}

async function recover(event) {
  const data = Object.fromEntries(new FormData(event.currentTarget));
  const ceremony = await api("/auth/recovery/begin", {method: "POST", body: JSON.stringify(data)});
  const credential = await createPasskey(ceremony);
  const result = await api("/auth/recovery/finish", {
    method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential}),
  });
  finishWithRecoveryCodes(result.recovery_codes);
}

function option(value, label) {
  return element("option", {text: label, attributes: {value}});
}

function fillSelect(select, firstLabel, values, labelFor) {
  const previous = select.value;
  select.replaceChildren(option("", firstLabel), ...values.map((item) => option(item.id, labelFor(item))));
  if (values.some((item) => item.id === previous)) select.value = previous;
}

function summaryItem(label, value) {
  return element("div", {className: "summary-item"}, element("span", {text: label}), element("strong", {text: value}));
}

function emptyState(message, actionLabel = "", action = null) {
  const container = element("div", {className: "empty"}, element("p", {text: message}));
  if (actionLabel && action) {
    const button = element("button", {type: "button", text: actionLabel, className: "secondary"});
    button.addEventListener("click", action);
    container.append(button);
  }
  return container;
}

function openDialog(id) {
  const dialog = byId(id);
  if (!dialog || dialog.open) return;
  setLocalMessage(dialog);
  dialog.showModal();
}

function renderResourceSummary() {
  const devices = state?.devices || [];
  const projects = state?.projects || [];
  const keys = state?.api_keys || [];
  const passkeys = state?.passkeys || [];
  const container = byId("resource-summary");
  container.classList.remove("loading");
  container.setAttribute("aria-busy", "false");
  container.replaceChildren(
    summaryItem("活跃设备", `${devices.filter((item) => item.status === "active").length} / ${devices.length}`),
    summaryItem("活跃项目", `${projects.filter((item) => item.status === "active").length} / ${projects.length}`),
    summaryItem("活跃 API Keys", `${keys.filter((item) => item.status === "active").length} / ${keys.length}`),
    summaryItem("Passkeys", formatInteger(passkeys.length)),
  );
  byId("metric-active-keys").textContent = formatInteger(keys.filter((item) => item.status === "active").length);
}

function renderOnboarding() {
  const steps = [
    {done: Boolean(state?.devices?.length), title: "添加一台设备", detail: "为 API Key 建立清晰归属", section: "resources"},
    {done: Boolean(state?.api_keys?.length), title: "创建 API Key", detail: "秘密只会显示一次", section: "keys"},
    {
      done: Number(overviewSummary?.requests || 0) > 0 || Boolean(state?.api_keys?.some((key) => key.last_used_at)),
      title: "完成首次请求", detail: "验证代理与用量记录", section: "usage",
    },
  ];
  const list = byId("onboarding");
  const nodes = steps.map((step) => {
    const details = element("div", {}, element("strong", {text: step.title}), element("small", {text: step.detail}));
    const item = element("li", {className: step.done ? "done" : ""}, details);
    if (!step.done) {
      const button = element("button", {type: "button", className: "text-button", text: "前往"});
      button.addEventListener("click", () => { location.hash = step.section; });
      item.append(button);
    }
    return item;
  });
  list.replaceChildren(...nodes);
  const completed = steps.filter((step) => step.done).length;
  byId("onboarding-progress").textContent = `${completed} / ${steps.length}`;
}

function renderDevices() {
  const devices = state.devices;
  const container = byId("devices");
  container.classList.remove("loading");
  container.setAttribute("aria-busy", "false");
  byId("device-count").textContent = formatInteger(devices.length);
  if (!devices.length) {
    container.replaceChildren(emptyState("还没有设备。先添加设备，才能创建 API Key。", "新增设备", () => openDialog("device-dialog")));
    return;
  }
  container.replaceChildren(...devices.map((device) => {
    const title = element("div", {className: "list-title"}, element("strong", {text: device.name}), statusBadge(device.status));
    const detail = element("div", {className: "list-detail"},
      element("span", {text: `最后使用：${formatDateTime(device.last_seen_at)}`}),
      element("span", {text: `创建：${formatDateTime(device.created_at, "—")}`}),
    );
    return element("div", {className: "list-row"}, title, detail);
  }));
}

function renderProjects() {
  const projects = state.projects;
  const container = byId("projects");
  container.classList.remove("loading");
  container.setAttribute("aria-busy", "false");
  byId("project-count").textContent = formatInteger(projects.length);
  if (!projects.length) {
    container.replaceChildren(emptyState("还没有项目。项目可作为 API Key 的默认工作范围。", "新增项目", () => openDialog("project-dialog")));
    return;
  }
  container.replaceChildren(...projects.map((project) => {
    const title = element("div", {className: "list-title"}, element("strong", {text: project.name}), statusBadge(project.status));
    const detail = element("div", {className: "list-detail"},
      element("span", {}, "Slug：", element("code", {text: project.slug})),
      element("span", {text: `创建：${formatDateTime(project.created_at, "—")}`}),
    );
    return element("div", {className: "list-row"}, title, detail);
  }));
}

function renderAPIKeys() {
  const keys = state.api_keys;
  const devices = new Map(state.devices.map((item) => [item.id, item.name]));
  const projects = new Map(state.projects.map((item) => [item.id, item.slug]));
  const container = byId("keys");
  container.classList.remove("loading");
  container.setAttribute("aria-busy", "false");
  if (!keys.length) {
    const hasDevice = state.devices.some((item) => item.status === "active");
    container.replaceChildren(emptyState(
      hasDevice ? "还没有 API Key。创建后，秘密只会显示一次。" : "还没有 API Key，且当前没有活跃设备。",
      hasDevice ? "创建 API Key" : "先建设备",
      () => hasDevice ? openDialog("key-dialog") : (location.hash = "resources"),
    ));
    return;
  }
  container.replaceChildren(...keys.map((key) => {
    const title = element("div", {className: "list-title"},
      element("strong", {text: key.name}), element("code", {text: key.key_prefix}), statusBadge(key.status),
    );
    const actions = element("div", {className: "list-actions"});
    if (key.status === "active") {
      const revoke = element("button", {type: "button", className: "danger", text: "撤销"});
      revoke.addEventListener("click", () => runButton(revoke, () => revokeKey(key.id), "撤销中…"));
      actions.append(revoke);
    }
    const allowlist = Array.isArray(key.model_allowlist) && key.model_allowlist.length ? key.model_allowlist.join(", ") : "全部模型";
    const detail = element("div", {className: "list-detail"},
      element("span", {text: `设备：${devices.get(key.device_id) || key.device_id}`}),
      element("span", {text: `默认项目：${key.default_project_id ? (projects.get(key.default_project_id) || key.default_project_id) : "未分配"}`}),
      element("span", {text: `模型：${allowlist}`}),
      element("span", {text: `到期：${formatDateTime(key.expires_at, "—")}`}),
      element("span", {text: `最后使用：${formatDateTime(key.last_used_at)}`}),
      element("span", {text: `创建：${formatDateTime(key.created_at, "—")}`}),
    );
    return element("div", {className: "list-row"}, title, actions, detail);
  }));
}

function renderPasskeys() {
  const passkeys = state.passkeys;
  const container = byId("passkeys");
  container.classList.remove("loading");
  container.setAttribute("aria-busy", "false");
  if (!passkeys.length) {
    container.replaceChildren(emptyState("没有可显示的 Passkey。请尽快添加新的登录凭证。", "新增 Passkey", () => openDialog("passkey-dialog")));
    return;
  }
  container.replaceChildren(...passkeys.map((passkey) => {
    let backup = "不可备份";
    if (passkey.backup_eligible) backup = passkey.backup_state ? "已同步备份" : "支持备份 · 尚未备份";
    const title = element("div", {className: "list-title"}, element("strong", {text: passkey.nickname || "未命名 Passkey"}), statusBadge("active"));
    const detail = element("div", {className: "list-detail"},
      element("span", {text: `备份：${backup}`}),
      element("span", {text: `最后使用：${formatDateTime(passkey.last_used_at)}`}),
      element("span", {text: `创建：${formatDateTime(passkey.created_at, "—")}`}),
    );
    return element("div", {className: "list-row"}, title, detail);
  }));
}

function renderSelects() {
  const activeDevices = state.devices.filter((item) => item.status === "active");
  const activeProjects = state.projects.filter((item) => item.status === "active");
  const activeKeys = state.api_keys.filter((item) => item.status === "active");
  const keyDevice = byId("key-device");
  keyDevice.replaceChildren(...activeDevices.map((item) => option(item.id, item.name)));
  const keyProject = byId("key-project");
  fillSelect(keyProject, "未分配", activeProjects, (item) => `${item.name} (${item.slug})`);

  const usageForm = byId("usage-filter");
  fillSelect(usageForm.elements.device_id, "全部设备", state.devices, (item) => item.name);
  fillSelect(usageForm.elements.api_key_id, "全部 Keys", state.api_keys, (item) => `${item.name} · ${item.key_prefix}`);
  fillSelect(usageForm.elements.project_id, "全部项目", state.projects, (item) => `${item.name} (${item.slug})`);

  const canCreateKey = activeDevices.length > 0;
  byId("new-key").disabled = !canCreateKey;
  if (canCreateKey) {
    byId("new-key").removeAttribute("aria-describedby");
    hide("key-guidance");
  } else {
    byId("new-key").setAttribute("aria-describedby", "key-guidance");
    byId("key-guidance").textContent = "创建 API Key 前，请先在“资源”中添加一台活跃设备。";
    show("key-guidance");
  }
}

function renderState(value) {
  state = {
    ...value,
    devices: Array.isArray(value.devices) ? value.devices : [],
    projects: Array.isArray(value.projects) ? value.projects : [],
    api_keys: Array.isArray(value.api_keys) ? value.api_keys : [],
    passkeys: Array.isArray(value.passkeys) ? value.passkeys : [],
  };
  if (!state.user) throw new Error("管理台状态缺少当前用户信息。");
  const owner = state.user.role === "owner";
  all(".owner-only").forEach((node) => node.classList.toggle("hidden", !owner));
  if (!owner && byId("global-tab").getAttribute("aria-selected") === "true") showUsageTab("personal");
  byId("whoami").textContent = state.user.display_name || state.user.username;
  byId("role").textContent = `${state.user.username} · ${owner ? "Owner" : "Member"}`;
  hide("auth");
  hide("login-view");
  show("dashboard");
  renderDevices();
  renderProjects();
  renderAPIKeys();
  renderPasskeys();
  renderSelects();
  renderResourceSummary();
  renderOnboarding();
  routeFromHash(false);
}

async function refreshState() {
  setConnection("正在同步", "loading");
  const value = await api("/admin/state");
  renderState(value);
  setConnection("已连接", "ok");
}

async function refreshAfterMutation() {
  try {
    await refreshState();
  } catch (error) {
    setConnection("同步失败", "error");
    notice(`操作已完成，但界面刷新失败：${friendlyError(error)}`, "error");
  }
}

async function submitResource(event, path, successMessage) {
  const form = event.currentTarget;
  const data = Object.fromEntries(new FormData(form));
  await api(path, {method: "POST", body: JSON.stringify(data)});
  form.reset();
  form.closest("dialog")?.close();
  notice(successMessage, "ok");
  await refreshAfterMutation();
}

async function createKey(event) {
  const form = event.currentTarget;
  const data = Object.fromEntries(new FormData(form));
  data.expires_days = Number(data.expires_days);
  data.models = String(data.models || "").split(",").map((value) => value.trim()).filter(Boolean);
  const result = await sensitiveAction(() => api("/admin/api-keys", {method: "POST", body: JSON.stringify(data)}));
  form.reset();
  form.elements.expires_days.value = "90";
  form.closest("dialog")?.close();
  showSecret(
    "保存 API Key",
    result.api_key || "",
    `API Key ${result.prefix || ""} 只显示这一次。请复制到目标设备并安全保存。`,
  );
  await refreshAfterMutation();
}

async function revokeKey(id) {
  if (!window.confirm("撤销后 API Key 会立即失效，且无法恢复。确定继续？")) return;
  await sensitiveAction(() => api(`/admin/api-keys/${encodeURIComponent(id)}`, {method: "DELETE", body: "{}"}));
  notice("API Key 已撤销。", "ok");
  await refreshAfterMutation();
}

async function addPasskey(event) {
  const form = event.currentTarget;
  const nickname = String(new FormData(form).get("nickname") || "").trim();
  await sensitiveAction(async () => {
    const ceremony = await api("/admin/passkeys/begin", {method: "POST", body: "{}"});
    const credential = await createPasskey(ceremony);
    return api("/admin/passkeys/finish", {
      method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential, nickname}),
    });
  });
  form.reset();
  form.closest("dialog")?.close();
  notice("新的 Passkey 已添加。", "ok");
  await refreshAfterMutation();
}

function recoveryHintedLink(link) {
  try {
    const url = new URL(link, location.href);
    const fragment = new URLSearchParams(url.hash.slice(1));
    fragment.set("kind", "recovery");
    url.hash = fragment.toString();
    return url.toString();
  } catch (_) {
    return link.includes("#") ? `${link}&kind=recovery` : `${link}#kind=recovery`;
  }
}

async function invite(kind, targetUsername = "") {
  const result = await sensitiveAction(() => api("/admin/invitations", {
    method: "POST", body: JSON.stringify({kind, target_username: targetUsername}),
  }));
  const link = kind === "recovery" ? recoveryHintedLink(result.link) : result.link;
  showSecret(
    kind === "recovery" ? "保存恢复邀请" : "分享成员邀请",
    link || "",
    `链接只可使用一次，将于 ${formatDateTime(result.expires_at, "24 小时内")} 失效。`,
  );
}

function localDateBoundary(value, nextDay = false) {
  const parts = String(value).split("-").map(Number);
  if (parts.length !== 3 || parts.some((part) => !Number.isInteger(part))) return "";
  const date = new Date(parts[0], parts[1] - 1, parts[2], 0, 0, 0, 0);
  if (Number.isNaN(date.getTime())) return "";
  if (nextDay) date.setDate(date.getDate() + 1);
  return date.toISOString();
}

function queryFromForm(form) {
  const query = new URLSearchParams();
  for (const [name, value] of new FormData(form)) {
    const normalized = String(value).trim();
    if (!normalized) continue;
    if (name === "from") query.set(name, localDateBoundary(normalized));
    else if (name === "until") query.set(name, localDateBoundary(normalized, true));
    else query.set(name, normalized);
  }
  return query;
}

function querySuffix(query) {
  const encoded = query.toString();
  return encoded ? `?${encoded}` : "";
}

function updateCSVLink() {
  const query = queryFromForm(byId("usage-filter"));
  byId("csv-link").href = `/admin/usage.csv${querySuffix(query)}`;
}

function tableMessage(columns, message) {
  return element("tr", {}, element("td", {className: "table-message", text: message, attributes: {colspan: columns}}));
}

function setTableBusy(tbody, columns, message) {
  tbody.closest("table")?.setAttribute("aria-busy", "true");
  tbody.replaceChildren(tableMessage(columns, message));
}

function normalizeBillingUser(value) {
  const source = value && typeof value === "object" ? value : {};
  const nested = source.user && typeof source.user === "object" ? source.user : {};
  return {
    ...nested,
    ...source,
    id: nested.id || source.user_id || source.id || "",
    username: nested.username || source.username || "",
    display_name: nested.display_name || source.display_name || "",
    role: nested.role || source.role || "",
    status: nested.status || source.status || "",
  };
}

function billingUserForDetail(detail = billingDetail) {
  if (detail?.user) return normalizeBillingUser(detail.user);
  const direct = normalizeBillingUser(detail);
  if (direct.id) return direct;
  const selected = byId("billing-user-select")?.value;
  return billingUsers.find((item) => item.id === selected) || normalizeBillingUser(state?.user);
}

function billingSubscriptions(detail = billingDetail) {
  const source = detail?.subscriptions || detail?.account?.subscriptions || {};
  if (Array.isArray(source)) {
    return Object.fromEntries(source.map((item) => [field(item, "tier", "kind"), item]));
  }
  return source && typeof source === "object" ? source : {};
}

function billingSubscription(detail, tier) {
  const subscription = billingSubscriptions(detail)[tier];
  return subscription && typeof subscription === "object" ? subscription : null;
}

function billingSubscriptionEnabled(subscription) {
  if (!subscription) return false;
  if (subscription.enabled != null) return subscription.enabled === true;
  if (subscription.status) return subscription.status === "active";
  return Boolean(subscription.period_ends_at);
}

function billingCashBalance(detail = billingDetail) {
  return detail?.cash_balance_usd ?? detail?.account?.cash_balance_usd ?? "0";
}

function billingEntries(detail = billingDetail) {
  const entries = detail?.ledger_entries || detail?.entries || [];
  return Array.isArray(entries) ? entries : [];
}

function billingTypeLabel(type) {
  return ({
    recharge: "充值",
    cash_recharge: "充值",
    adjustment: "余额调整",
    cash_adjustment: "余额调整",
    usage: "用量扣费",
    usage_charge: "用量扣费",
    recharge_rate: "充值汇率调整",
    subscription_set: "订阅重开",
    subscription_disable: "订阅停用",
    subscription_renewal: "订阅续期",
    subscription_created: "订阅启用",
    subscription_updated: "订阅重开",
    subscription_disabled: "订阅停用",
    subscription_period_opened: "订阅周期",
  })[type] || type || "账务记录";
}

function renderBillingSubscriptions(detail) {
  const container = byId("billing-subscriptions");
  container.classList.remove("loading");
  container.setAttribute("aria-busy", "false");
  const cards = billingTiers.map((tier) => {
    const subscription = billingSubscription(detail, tier.id);
    const enabled = billingSubscriptionEnabled(subscription);
    const quota = subscription?.quota_usd;
    const remaining = subscription?.remaining_usd;
    const header = element("header", {},
      element("strong", {text: tier.label}),
      statusBadge(enabled ? "active" : "disabled"),
    );
    return element("div", {className: "subscription-card"},
      header,
      element("strong", {text: enabled ? formatUSD(remaining, formatUSD("0")) : "未启用"}),
      element("small", {text: enabled ? `周期额度：${formatUSD(quota, "—")} · 固定 ${tier.duration}` : `固定 ${tier.duration}滚动周期`}),
      element("small", {text: enabled ? `开始：${formatDateTime(subscription?.period_started_at, "—")}` : "剩余额度不会结转"}),
      element("small", {text: enabled ? `续期：${formatDateTime(subscription?.period_ends_at, "—")}` : "Owner 可随时启用"}),
    );
  });
  container.replaceChildren(...cards);
}

function renderBillingLedger(detail) {
  const entries = billingEntries(detail);
  const tbody = byId("billing-ledger-rows");
  tbody.closest("table")?.setAttribute("aria-busy", "false");
  if (!entries.length) {
    tbody.replaceChildren(tableMessage(5, "当前页没有账务流水。"));
  } else {
    tbody.replaceChildren(...entries.map((entry) => {
      const type = String(field(entry, "entry_type", "kind", "type") || "");
      const amount = field(entry, "amount_usd", "charged_usd", "actual_cost_usd");
      const money = element("td", {className: "money-cell"},
        element("span", {text: formatUSD(amount)}),
      );
      const balance = field(entry, "balance_after_usd", "cash_balance_after_usd");
      const actual = field(entry, "actual_cost_usd");
      const charged = field(entry, "charged_usd");
      const uncovered = field(entry, "uncovered_usd");
      if (balance != null) money.append(element("small", {text: `现金余额：${formatUSD(balance)}`}));
      if (actual != null && String(actual) !== String(amount)) money.append(element("small", {text: `实际成本：${formatUSD(actual)}`}));
      if (charged != null && String(charged) !== String(amount)) money.append(element("small", {text: `已扣额度：${formatUSD(charged)}`}));
      if (uncovered != null && String(uncovered) !== "0" && String(uncovered) !== "0.000000000000") {
        money.append(element("small", {text: `未覆盖：${formatUSD(uncovered)}`}));
      }

      const description = element("td", {className: "money-cell"},
        element("span", {text: field(entry, "reason", "description") || "—"}),
      );
      const cnyAmount = field(entry, "cny_amount");
      const rate = field(entry, "usd_per_cny", "usd_per_cny_snapshot", "recharge_rate");
      if (cnyAmount != null) {
        const rateText = rate == null ? "" : ` × ${String(rate)} USD/CNY`;
        description.append(element("small", {text: `${formatMoney(cnyAmount, "CNY")}${rateText}`}));
      } else if (rate != null) {
        description.append(element("small", {text: `新汇率：${String(rate)} USD/CNY`}));
      }
      const subscriptionTier = field(entry, "subscription_tier");
      if (subscriptionTier != null) {
        const tier = billingTiers.find((item) => item.id === subscriptionTier);
        description.append(element("small", {text: tier?.label || String(subscriptionTier)}));
      }

      const request = element("td", {className: "money-cell"});
      const requestID = field(entry, "request_id");
      const model = field(entry, "model");
      request.append(requestID ? element("code", {text: requestID}) : element("span", {text: "—"}));
      if (model) request.append(element("small", {text: String(model)}));
      const tokenParts = [];
      for (const [name, label] of [["input_tokens", "输入"], ["cached_input_tokens", "缓存"], ["output_tokens", "输出"]]) {
        const value = field(entry, name);
        if (value != null) tokenParts.push(`${label} ${String(value)}`);
      }
      if (tokenParts.length) request.append(element("small", {text: tokenParts.join(" · ")}));

      const label = billingTypeLabel(type);
      const typeCell = element("td", {className: "money-cell"}, element("span", {text: label}));
      if (type && label !== type) typeCell.append(element("small", {text: type}));
      return element("tr", {},
        element("td", {text: formatDateTime(field(entry, "occurred_at", "created_at"), "—")}),
        typeCell,
        money,
        description,
        request,
      );
    }));
  }

  const pagination = detail?.pagination || {};
  const offsetValue = Number(pagination.offset ?? billingLedgerOffset);
  const offset = Number.isFinite(offsetValue) && offsetValue >= 0 ? offsetValue : billingLedgerOffset;
  const limitValue = Number(pagination.limit ?? billingLedgerPageSize);
  const limit = Number.isFinite(limitValue) && limitValue > 0 ? limitValue : billingLedgerPageSize;
  const totalValue = Number(pagination.total ?? pagination.total_count);
  const explicitMore = pagination.has_more ?? pagination.has_next;
  const nextValue = Number(pagination.next_offset);
  billingLedgerOffset = offset;
  billingLedgerNextOffset = Number.isFinite(nextValue) && nextValue > offset ? nextValue : offset + limit;
  let hasMore = typeof explicitMore === "boolean" ? explicitMore : entries.length === limit;
  if (Number.isFinite(totalValue) && totalValue >= 0) hasMore = offset + entries.length < totalValue;
  const page = Math.floor(offset / limit) + 1;
  const pages = Number.isFinite(totalValue) && totalValue > 0 ? Math.ceil(totalValue / limit) : null;
  byId("billing-ledger-page").textContent = pages ? `${page} / ${pages}` : `第 ${page} 页`;
  byId("billing-ledger-prev").disabled = offset <= 0;
  byId("billing-ledger-prev").dataset.offset = String(Math.max(0, offset - limit));
  byId("billing-ledger-next").disabled = !hasMore;
  byId("billing-ledger-next").dataset.offset = String(billingLedgerNextOffset);
}

function renderBillingAdminValues(detail) {
  if (state?.user?.role !== "owner") return;
  for (const tier of billingTiers) {
    const subscription = billingSubscription(detail, tier.id);
    const form = byId(`billing-subscription-${tier.id}`);
    if (document.activeElement !== form.elements.quota_usd) {
      form.elements.quota_usd.value = subscription?.quota_usd == null ? "" : String(subscription.quota_usd);
    }
    form.querySelector("[data-disable-subscription]").disabled = !billingSubscriptionEnabled(subscription);
  }
}

function renderBillingDetail(detail) {
  billingDetail = detail || {};
  const user = billingUserForDetail(detail);
  const cash = billingCashBalance(detail);
  byId("billing-cash-balance").textContent = formatUSD(cash, formatUSD("0"));
  if (state?.user?.role === "owner") {
    byId("billing-scope-name").textContent = user.id === state.user.id
      ? `${user.display_name || user.username || "当前用户"} · 当前登录用户`
      : `${user.display_name || user.username || user.id} · ${user.username || user.id}`;
  }
  for (const tier of billingTiers) {
    const subscription = billingSubscription(detail, tier.id);
    const enabled = billingSubscriptionEnabled(subscription);
    byId(`billing-${tier.id}-remaining`).textContent = enabled
      ? formatUSD(subscription?.remaining_usd, formatUSD("0")) : "未启用";
    byId(`billing-${tier.id}-ends`).textContent = enabled
      ? `续期：${formatDateTime(subscription?.period_ends_at, "—")}` : `固定 ${tier.duration}`;
  }
  renderBillingSubscriptions(detail);
  renderBillingLedger(detail);
  renderBillingAdminValues(detail);
}

function renderBillingUsers(result) {
  const values = Array.isArray(result) ? result : (result?.users || result?.items || []);
  const unique = new Map();
  for (const value of Array.isArray(values) ? values : []) {
    const user = normalizeBillingUser(value);
    if (user.id) unique.set(user.id, user);
  }
  const current = normalizeBillingUser(state?.user);
  if (current.id && !unique.has(current.id)) unique.set(current.id, current);
  billingUsers = Array.from(unique.values()).sort((left, right) =>
    String(left.username || left.display_name).localeCompare(String(right.username || right.display_name), "zh-CN"));
  const select = byId("billing-user-select");
  const previous = select.value || billingUserForDetail()?.id || state?.user?.id || "";
  select.replaceChildren(...billingUsers.map((user) => option(
    user.id,
    `${user.display_name || user.username || user.id} (${user.username || user.id}) · ${formatUSD(user.cash_balance_usd, formatUSD("0"))}`,
  )));
  if (billingUsers.some((user) => user.id === previous)) select.value = previous;
  else if (billingUsers.length) select.value = billingUsers[0].id;
}

function renderBillingSettings(result) {
  billingSettings = result?.settings || result || {};
  const rate = billingSettings.usd_per_cny;
  byId("billing-current-rate").textContent = rate == null ? "—" : String(rate);
  const input = byId("billing-rate-form").elements.usd_per_cny;
  if (document.activeElement !== input) input.value = rate == null ? "" : String(rate);
}

function selectedBillingUserID() {
  if (state?.user?.role !== "owner") return state?.user?.id || "";
  return byId("billing-user-select").value || state?.user?.id || "";
}

function billingDetailPath(userID) {
  if (!userID || userID === state?.user?.id) return "/admin/billing/me";
  return `/admin/billing/users/${encodeURIComponent(userID)}`;
}

async function loadBillingDetail(userID = selectedBillingUserID(), offset = 0) {
  if (!userID) throw new Error("无法确定要查看的账务用户。");
  const sequence = ++billingRequestSequence;
  billingLedgerOffset = Math.max(0, Number(offset) || 0);
  show("billing-loading");
  setTableBusy(byId("billing-ledger-rows"), 5, "正在加载账务流水…");
  const query = new URLSearchParams({limit: String(billingLedgerPageSize), offset: String(billingLedgerOffset)});
  try {
    const result = await api(`${billingDetailPath(userID)}?${query}`);
    if (sequence !== billingRequestSequence) return;
    renderBillingDetail(result);
  } catch (error) {
    if (sequence === billingRequestSequence) {
      billingDetail = null;
      byId("billing-subscriptions").classList.remove("loading");
      byId("billing-subscriptions").setAttribute("aria-busy", "false");
      byId("billing-subscriptions").replaceChildren(emptyState(`额度加载失败：${friendlyError(error)}`));
      byId("billing-ledger-rows").closest("table")?.setAttribute("aria-busy", "false");
      byId("billing-ledger-rows").replaceChildren(tableMessage(5, friendlyError(error)));
    }
    throw error;
  } finally {
    if (sequence === billingRequestSequence) hide("billing-loading");
  }
}

async function loadBillingUsers() {
  const result = await api("/admin/billing/users");
  renderBillingUsers(result);
}

async function loadBillingSettings() {
  const result = await api("/admin/billing/settings");
  renderBillingSettings(result);
}

async function loadBillingDashboard() {
  const tasks = [loadBillingDetail(state.user.id, 0)];
  if (state.user.role === "owner") {
    tasks.push(
      loadBillingSettings().catch((error) => {
        setLocalMessage(byId("billing-rate-form"), `充值汇率加载失败：${friendlyError(error)}`);
      }),
      loadBillingUsers().catch((error) => {
        byId("billing-user-select").replaceChildren(option("", "用户列表加载失败"));
        notice(`账务用户列表加载失败：${friendlyError(error)}`, "error");
      }),
    );
  }
  await Promise.all(tasks);
}

function billingReason(form) {
  const reason = String(new FormData(form).get("reason") || "").trim();
  if (!reason) throw new Error("必须填写操作原因。");
  return reason;
}

async function billingMutation(path, method, payload) {
  if (state?.user?.role !== "owner") throw new Error("仅 Owner 可执行此账务操作。");
  if (!crypto?.randomUUID) throw new Error("当前浏览器无法生成安全的操作 ID，请升级浏览器后重试。");
  const input = {...payload, operation_id: crypto.randomUUID()};
  const body = JSON.stringify(input);
  return sensitiveAction(() => api(path, {method, body}));
}

async function refreshManagedBilling(userID = selectedBillingUserID()) {
  await Promise.all([
    loadBillingDetail(userID, 0),
    loadBillingUsers().catch((error) => notice(`用户余额摘要刷新失败：${friendlyError(error)}`, "error")),
  ]);
}

async function updateBillingRate(event) {
  const form = event.currentTarget;
  const data = new FormData(form);
  await billingMutation("/admin/billing/settings/recharge-rate", "PUT", {
    usd_per_cny: String(data.get("usd_per_cny") || "").trim(),
    reason: billingReason(form),
  });
  form.elements.reason.value = "";
  await loadBillingSettings();
  notice("充值汇率已更新；历史充值汇率快照保持不变。", "ok");
}

async function rechargeBillingUser(event) {
  const form = event.currentTarget;
  const userID = selectedBillingUserID();
  const data = new FormData(form);
  await billingMutation(`/admin/billing/users/${encodeURIComponent(userID)}/recharges`, "POST", {
    cny_amount: String(data.get("cny_amount") || "").trim(),
    reason: billingReason(form),
  });
  form.reset();
  await refreshManagedBilling(userID);
  notice("充值已入账并记录汇率快照。", "ok");
}

async function adjustBillingUser(event) {
  const form = event.currentTarget;
  const userID = selectedBillingUserID();
  const data = new FormData(form);
  await billingMutation(`/admin/billing/users/${encodeURIComponent(userID)}/adjustments`, "POST", {
    usd_amount: String(data.get("usd_amount") || "").trim(),
    reason: billingReason(form),
  });
  form.reset();
  await refreshManagedBilling(userID);
  notice("余额调整已记入不可变账务流水。", "ok");
}

async function updateBillingSubscription(event) {
  const form = event.currentTarget;
  const tier = form.dataset.tier;
  if (!billingTiers.some((item) => item.id === tier)) throw new Error("订阅档位无效。");
  const userID = selectedBillingUserID();
  const data = new FormData(form);
  await billingMutation(`/admin/billing/users/${encodeURIComponent(userID)}/subscriptions/${tier}`, "PUT", {
    quota_usd: String(data.get("quota_usd") || "").trim(),
    reason: billingReason(form),
  });
  form.elements.reason.value = "";
  await refreshManagedBilling(userID);
  notice(`${billingTiers.find((item) => item.id === tier).label}已从当前时刻重开。`, "ok");
}

async function disableBillingSubscription(button) {
  const tier = button.dataset.disableSubscription;
  const form = button.closest("form");
  const reason = billingReason(form);
  if (!window.confirm(`停用${billingTiers.find((item) => item.id === tier)?.label || "订阅"}后，新请求将立即无法使用当前周期。确定继续？`)) return;
  const userID = selectedBillingUserID();
  await billingMutation(`/admin/billing/users/${encodeURIComponent(userID)}/subscriptions/${tier}`, "DELETE", {reason});
  form.elements.reason.value = "";
  await refreshManagedBilling(userID);
  notice("订阅已立即停用。", "ok");
}

async function changeBillingLedgerPage(offset) {
  await loadBillingDetail(selectedBillingUserID(), Math.max(0, Number(offset) || 0));
  announce("账务流水已更新。");
}

function usageNameMaps() {
  return {
    devices: new Map((state?.devices || []).map((item) => [item.id, item.name])),
    keys: new Map((state?.api_keys || []).map((item) => [item.id, `${item.name} · ${item.key_prefix}`])),
    projects: new Map((state?.projects || []).map((item) => [item.id, item.slug])),
  };
}

function renderPersonalUsage(result, updateOverview) {
  const summary = result.summary || {};
  byId("usage-requests").textContent = formatInteger(summary.requests);
  byId("metric-cache").textContent = formatPercent(summary.cache_rate);
  byId("metric-ttft").textContent = `${formatInteger(summary.p95_ttft_ms)} ms`;
  byId("metric-duration").textContent = `${formatInteger(summary.p95_duration_ms)} ms`;
  if (updateOverview) {
    overviewSummary = summary;
    byId("metric-requests").textContent = formatInteger(summary.requests);
    byId("metric-tokens").textContent = formatInteger(summary.tokens);
    byId("metric-errors").textContent = formatPercent(summary.error_rate);
    renderOnboarding();
  }

  const requests = Array.isArray(result.requests) ? result.requests : [];
  const tbody = byId("usage-rows");
  tbody.closest("table")?.setAttribute("aria-busy", "false");
  if (!requests.length) {
    tbody.replaceChildren(tableMessage(8, "当前筛选条件下没有请求记录。"));
    return;
  }
  const names = usageNameMaps();
  const rows = requests.map((request) => {
    const deviceID = field(request, "device_id", "DeviceID") || "";
    const keyID = field(request, "api_key_id", "APIKeyID") || "";
    const keyPrefix = field(request, "key_prefix", "KeyPrefix") || "—";
    const projectID = field(request, "project_id", "ProjectID");
    const requestState = field(request, "state", "State") || "";
    const inputTokens = Number(field(request, "input_tokens", "InputTokens") || 0);
    const outputTokens = Number(field(request, "output_tokens", "OutputTokens") || 0);
    const tr = element("tr");
    tr.append(
      element("td", {text: formatDateTime(field(request, "requested_at", "RequestedAt"), "—")}),
      element("td", {text: names.devices.get(deviceID) || deviceID || "—"}),
      element("td", {text: names.keys.get(keyID) || keyPrefix}),
      element("td", {text: projectID ? (names.projects.get(projectID) || projectID) : "未分配"}),
      element("td", {}, element("code", {text: field(request, "model", "Model") || "—"})),
      element("td", {}, statusBadge(requestState)),
      element("td", {text: field(request, "http_status", "HTTPStatus") ?? "—"}),
      element("td", {text: formatInteger(inputTokens + outputTokens)}),
    );
    return tr;
  });
  tbody.replaceChildren(...rows);
}

async function loadPersonalUsage(query, updateOverview = false) {
  const sequence = ++personalRequestSequence;
  const loading = byId("personal-loading");
  show(loading);
  setTableBusy(byId("usage-rows"), 8, "正在加载使用明细…");
  const suffix = querySuffix(query);
  try {
    const result = await api(`/admin/usage${suffix}`);
    if (sequence !== personalRequestSequence) return;
    renderPersonalUsage(result, updateOverview);
  } catch (error) {
    if (sequence === personalRequestSequence) {
      byId("usage-rows").closest("table")?.setAttribute("aria-busy", "false");
      byId("usage-rows").replaceChildren(tableMessage(8, friendlyError(error)));
    }
    throw error;
  } finally {
    if (sequence === personalRequestSequence) hide(loading);
  }
}

function periodLabel(period) {
  if (period?.all) return "全部历史";
  if (!period?.from || !period?.until) return "—";
  const until = new Date(period.until);
  if (!Number.isNaN(until.getTime())) until.setMilliseconds(until.getMilliseconds() - 1);
  return `${formatDate(period.from)} – ${formatDate(until)}`;
}

function renderGlobalOverview(result) {
  const summary = result.summary || {};
  const usage = summary.usage || {};
  const coverage = summary.pricing_coverage || "0";
  byId("metric-global-tokens").textContent = formatInteger(usage.tokens);
  byId("metric-global-cost").textContent = formatMoney(usage.estimated_usd, "USD");
  byId("metric-global-coverage").textContent = `${formatPercent(coverage)} 已定价 · 仅供参考`;
  const container = byId("global-overview");
  container.classList.remove("loading");
  container.setAttribute("aria-busy", "false");
  container.replaceChildren(
    summaryItem("请求", formatInteger(usage.requests)),
    summaryItem("活跃 / 全部用户", `${formatInteger(summary.active_users)} / ${formatInteger(summary.total_users)}`),
    summaryItem("计价覆盖", formatPercent(coverage)),
    summaryItem("未定价 Token", formatInteger(usage.unpriced_tokens)),
  );
}

function setPersonalScope(user = null) {
  const form = byId("usage-filter");
  const isOtherUser = Boolean(user && user.id && user.id !== state?.user?.id);
  form.elements.user_id.value = isOtherUser ? user.id : "";
  for (const name of ["device_id", "api_key_id", "project_id"]) {
    form.elements[name].value = "";
    form.elements[name].disabled = isOtherUser;
  }
  if (isOtherUser) {
    byId("personal-scope-name").textContent = `${user.display_name || user.username} (${user.username})`;
    show("personal-scope");
  } else {
    byId("personal-scope-name").textContent = "";
    hide("personal-scope");
  }
  updateCSVLink();
}

async function drillDownUser(user) {
  location.hash = "usage";
  showUsageTab("personal");
  setPersonalScope(user);
  const model = byId("global-filter").elements.model.value.trim();
  byId("usage-filter").elements.model.value = model;
  byId("usage-filter").elements.state.value = "";
  byId("usage-filter").elements.status.value = "";
  const range = byId("global-filter").elements.range.value;
  if (range !== "all") {
    byId("usage-filter").elements.from.value = byId("global-filter").elements.from.value;
    byId("usage-filter").elements.until.value = byId("global-filter").elements.until.value;
  }
  const query = queryFromForm(byId("usage-filter"));
  updateCSVLink();
  await loadPersonalUsage(query, false);
  byId("content").focus({preventScroll: true});
}

function renderGlobalUsage(result) {
  const summary = result.summary || {};
  const usage = summary.usage || {};
  byId("global-usd").textContent = formatMoney(usage.estimated_usd, "USD");
  byId("global-cny").textContent = formatMoney(usage.estimated_cny, "CNY");
  byId("global-total-tokens").textContent = formatInteger(usage.tokens);
  byId("global-coverage").textContent = `${formatPercent(summary.pricing_coverage)} 已定价 · ${formatInteger(usage.unpriced_tokens)} 未定价 Token`;
  byId("global-request-users").textContent = `${formatInteger(usage.requests)} / ${formatInteger(summary.active_users)} · ${formatInteger(summary.total_users)}`;
  byId("global-period").textContent = `${periodLabel(result.period)} · 请求 / 活跃用户 / 全部用户`;

  const pricing = result.pricing || {};
  const note = byId("pricing-note");
  const rateLine = `价格目录：${pricing.catalog_as_of || "未标注"} · USD/CNY 固定汇率 ${pricing.usd_cny_rate || "—"}（${pricing.fx_as_of || "未标注"}）`;
  const models = Array.isArray(pricing.unpriced_models) ? pricing.unpriced_models : [];
  note.replaceChildren(
    element("p", {text: pricing.disclaimer || "API 等价费用估算，不代表实际费用或账单。"}),
    element("p", {text: rateLine}),
    element("p", {text: models.length ? `未配置价格的模型：${models.join(", ")}` : "当前区间内所有有用量模型均有精确价格匹配。"}),
  );

  const users = Array.isArray(result.users) ? result.users : [];
  const tbody = byId("global-rows");
  tbody.closest("table")?.setAttribute("aria-busy", "false");
  if (!users.length) {
    tbody.replaceChildren(tableMessage(6, "没有可显示的用户。"));
    return;
  }
  tbody.replaceChildren(...users.map((user) => {
    const userUsage = user.usage || {};
    const tokens = Number(userUsage.tokens || 0);
    const pricedTokens = Number(userUsage.priced_tokens || 0);
    const calculatedCoverage = tokens > 0 ? pricedTokens / tokens : null;
    let coverage = user.pricing_coverage == null ? calculatedCoverage : Number(user.pricing_coverage);
    if (user.pricing_status === "no_usage") coverage = null;
    const coverageLabel = ({
      complete: "完整计价", partial: "部分计价", unpriced: "未计价", no_usage: "无用量",
    })[user.pricing_status] || (coverage == null ? "无用量" : formatPercent(coverage));
    const drill = element("button", {type: "button", className: "user-drilldown"},
      element("span", {text: user.display_name || user.username}),
      element("small", {text: user.username}),
    );
    drill.addEventListener("click", () => runButton(drill, () => drillDownUser(user), "加载中…"));
    const money = element("td", {className: "money-cell"},
      element("span", {text: formatMoney(userUsage.estimated_usd, "USD")}),
      element("small", {text: formatMoney(userUsage.estimated_cny, "CNY")}),
    );
    const coverageCell = element("td", {className: "coverage-cell"},
      element("span", {text: coverage == null ? coverageLabel : `${formatPercent(coverage)} · ${coverageLabel}`}),
      element("small", {text: `${formatInteger(userUsage.unpriced_tokens)} 未定价 Token`}),
    );
    return element("tr", {},
      element("td", {}, drill),
      element("td", {text: formatInteger(userUsage.requests)}),
      element("td", {text: formatInteger(userUsage.tokens)}),
      money,
      element("td", {text: formatPercent(user.share)}),
      coverageCell,
    );
  }));
}

function globalQueryFromForm() {
  const form = byId("global-filter");
  const query = new URLSearchParams();
  if (form.elements.range.value === "all") {
    query.set("all", "true");
  } else {
    if (form.elements.from.value) query.set("from", localDateBoundary(form.elements.from.value));
    if (form.elements.until.value) query.set("until", localDateBoundary(form.elements.until.value, true));
  }
  const model = form.elements.model.value.trim();
  if (model) query.set("model", model);
  return query;
}

async function loadGlobalUsage(query, updateOverview = false) {
  const sequence = ++globalRequestSequence;
  show("global-loading");
  setTableBusy(byId("global-rows"), 6, "正在聚合全员用量…");
  try {
    const result = await api(`/admin/usage/global${querySuffix(query)}`);
    if (sequence !== globalRequestSequence) return;
    renderGlobalUsage(result);
    if (updateOverview) renderGlobalOverview(result);
  } catch (error) {
    if (sequence === globalRequestSequence) {
      byId("global-rows").closest("table")?.setAttribute("aria-busy", "false");
      byId("global-rows").replaceChildren(tableMessage(6, friendlyError(error)));
      if (updateOverview) {
        const container = byId("global-overview");
        container.classList.remove("loading");
        container.setAttribute("aria-busy", "false");
        container.replaceChildren(emptyState("全员统计暂时无法加载。"));
      }
    }
    throw error;
  } finally {
    if (sequence === globalRequestSequence) hide("global-loading");
  }
}

async function loadAlerts() {
  const container = byId("alert-summary");
  try {
    const result = await api("/admin/alerts?status=open");
    const alerts = Array.isArray(result.alerts) ? result.alerts : [];
    const severity = (name) => alerts.filter((item) => field(item, "severity", "Severity") === name).length;
    container.classList.remove("loading");
    container.setAttribute("aria-busy", "false");
    if (!alerts.length) {
      container.replaceChildren(summaryItem("状态", "目前没有开放告警"));
      return;
    }
    container.replaceChildren(
      summaryItem("开放总数", formatInteger(alerts.length)),
      summaryItem("严重", formatInteger(severity("critical"))),
      summaryItem("警告", formatInteger(severity("warning"))),
      summaryItem("信息", formatInteger(severity("info"))),
    );
  } catch (error) {
    container.classList.remove("loading");
    container.setAttribute("aria-busy", "false");
    container.replaceChildren(emptyState(`告警加载失败：${friendlyError(error)}`));
  }
}

function initializeDateFilters() {
  const today = new Date();
  const weekStart = new Date(today);
  weekStart.setDate(weekStart.getDate() - 6);
  const usageForm = byId("usage-filter");
  usageForm.elements.from.value = inputDate(weekStart);
  usageForm.elements.until.value = inputDate(today);
  usageForm.elements.from.max = inputDate(today);
  usageForm.elements.until.max = inputDate(today);

  const globalForm = byId("global-filter");
  globalForm.elements.until.max = inputDate(today);
  globalForm.elements.from.max = inputDate(today);
  syncGlobalRange();
  updateCSVLink();
}

function syncGlobalRange() {
  const form = byId("global-filter");
  const range = form.elements.range.value;
  const from = form.elements.from;
  const until = form.elements.until;
  const today = new Date();
  const start = new Date(today);
  if (range === "month") start.setDate(1);
  if (range === "7") start.setDate(start.getDate() - 6);
  if (range === "30") start.setDate(start.getDate() - 29);
  if (["month", "7", "30"].includes(range)) {
    from.value = inputDate(start);
    until.value = inputDate(today);
  }
  const custom = range === "custom";
  const allHistory = range === "all";
  from.disabled = !custom;
  until.disabled = !custom;
  from.required = custom;
  until.required = custom;
  if (allHistory) {
    from.value = "";
    until.value = "";
  } else if (custom && (!from.value || !until.value)) {
    start.setDate(today.getDate() - 29);
    from.value = inputDate(start);
    until.value = inputDate(today);
  }
}

function showUsageTab(tab) {
  const isGlobal = tab === "global" && state?.user?.role === "owner";
  byId("personal-tab").classList.toggle("active", !isGlobal);
  byId("global-tab").classList.toggle("active", isGlobal);
  byId("personal-tab").setAttribute("aria-selected", isGlobal ? "false" : "true");
  byId("global-tab").setAttribute("aria-selected", isGlobal ? "true" : "false");
  byId("personal-tab").tabIndex = isGlobal ? -1 : 0;
  byId("global-tab").tabIndex = isGlobal ? 0 : -1;
  byId("personal-usage").classList.toggle("hidden", isGlobal);
  byId("global-usage").classList.toggle("hidden", !isGlobal);
}

function routeFromHash(focusContent = true) {
  if (!state) return;
  const requested = location.hash.slice(1);
  const section = Object.prototype.hasOwnProperty.call(sectionTitles, requested) ? requested : "overview";
  if (requested !== section) history.replaceState(null, "", `#${section}`);
  all(".view").forEach((view) => view.classList.toggle("hidden", view.dataset.section !== section));
  all("nav [data-view]").forEach((link) => {
    const active = link.dataset.view === section;
    link.classList.toggle("active", active);
    if (active) link.setAttribute("aria-current", "page"); else link.removeAttribute("aria-current");
  });
  byId("page-title").textContent = sectionTitles[section];
  document.title = `${sectionTitles[section]} · Codex Gateway`;
  if (focusContent) byId("content").focus({preventScroll: true});
}

async function loadDashboard() {
  await refreshState();
  const personalQuery = queryFromForm(byId("usage-filter"));
  const tasks = [
    loadPersonalUsage(personalQuery, true).catch((error) => {
      setLocalMessage(byId("usage-filter"), friendlyError(error));
      byId("metric-requests").textContent = "加载失败";
      byId("metric-tokens").textContent = "—";
      byId("metric-errors").textContent = "—";
      notice(`个人用量加载失败：${friendlyError(error)}`, "error");
    }),
    loadBillingDashboard().catch((error) => {
      notice(`额度与订阅加载失败：${friendlyError(error)}`, "error");
    }),
  ];
  if (state.user.role === "owner") {
    tasks.push(
      loadGlobalUsage(globalQueryFromForm(), true).catch((error) => {
        setLocalMessage(byId("global-filter"), friendlyError(error));
      }),
      loadAlerts(),
    );
  }
  await Promise.allSettled(tasks);
}

function configureInvitationView() {
  const rawFragment = location.hash.slice(1);
  const parameters = new URLSearchParams(rawFragment);
  invitationToken = parameters.get("token") || (rawFragment.includes("=") ? "" : rawFragment);
  invitationKind = parameters.get("kind") === "recovery" ? "recovery" : "member";
  history.replaceState(null, "", "/join");
  hide("login-view");
  hide("recover-view");
  show("join-view");
  const form = byId("join-form");
  if (invitationKind === "recovery") {
    byId("join-eyebrow").textContent = "账号恢复邀请";
    byId("join-title").textContent = "为账号创建新的 Passkey";
    byId("join-help").textContent = "此一次性链接已绑定到待恢复账号；无需填写用户名或显示名称。成功后旧会话和旧恢复码将失效。";
    for (const id of ["join-username", "join-display"]) {
      hide(id);
      const input = byId(id).querySelector("input");
      input.required = false;
      input.disabled = true;
    }
    form.querySelector("button[type=submit]").textContent = "创建新的 Passkey";
  }
  if (!invitationToken) {
    setLocalMessage(form, "邀请链接缺少令牌，无法继续。请向 Owner 重新索取链接。");
    form.querySelector("button[type=submit]").disabled = true;
  }
}

function applyWebAuthnSupport() {
  if (webAuthnSupported()) return;
  const message = "此浏览器不支持 WebAuthn Passkey。请使用支持 Passkey 的现代浏览器，并确认站点通过 HTTPS 访问。";
  const visibleAuth = all("#login-view, #join-view, #recover-view").find((view) => !view.classList.contains("hidden"));
  if (visibleAuth) setLocalMessage(visibleAuth, message);
  byId("webauthn-warning").textContent = message;
  show("webauthn-warning");
  for (const id of ["login", "add-passkey"]) byId(id).disabled = true;
  all("#join-form button[type=submit], #recover-form button[type=submit], #passkey-form button[type=submit]").forEach((button) => {
    button.disabled = true;
  });
}

function bindUI() {
  bindAsync("login", "click", login, "等待 Passkey…");
  bindAsync("join-form", "submit", register, "等待 Passkey…");
  bindAsync("recover-form", "submit", recover, "等待 Passkey…");
  bindAsync("logout", "click", async () => {
    await api("/auth/logout", {method: "POST", body: "{}"});
    location.assign("/");
  }, "正在退出…");
  bindAsync("device-form", "submit", (event) => submitResource(event, "/admin/devices", "设备已添加。"), "添加中…");
  bindAsync("project-form", "submit", (event) => submitResource(event, "/admin/projects", "项目已添加。"), "添加中…");
  bindAsync("key-form", "submit", createKey, "创建中…");
  bindAsync("passkey-form", "submit", addPasskey, "等待 Passkey…");
  bindAsync("billing-rate-form", "submit", updateBillingRate, "更新中…");
  bindAsync("billing-recharge-form", "submit", rechargeBillingUser, "充值中…");
  bindAsync("billing-adjustment-form", "submit", adjustBillingUser, "调整中…");
  for (const tier of billingTiers) {
    bindAsync(`billing-subscription-${tier.id}`, "submit", updateBillingSubscription, "保存中…");
  }
  bindAsync("member-invite", "click", () => invite("member"), "生成中…");
  bindAsync("recovery-invite-form", "submit", (event) => {
    const username = String(new FormData(event.currentTarget).get("target_username") || "").trim();
    return invite("recovery", username);
  }, "签发中…");
  bindAsync("usage-filter", "submit", async (event) => {
    updateCSVLink();
    await loadPersonalUsage(queryFromForm(event.currentTarget), false);
    announce("个人使用统计已更新。");
  }, "应用中…");
  bindAsync("global-filter", "submit", async () => {
    await loadGlobalUsage(globalQueryFromForm(), false);
    announce("全员使用统计已更新。");
  }, "聚合中…");
  bindAsync("billing-user-select", "change", async (event) => {
    const userID = event.currentTarget.value;
    if (!userID) return;
    billingLedgerOffset = 0;
    all(".billing-form, .billing-subscription-form").forEach((form) => setLocalMessage(form));
    await loadBillingDetail(userID, 0);
    announce("所选用户的额度与账务流水已更新。");
  });

  all("[data-disable-subscription]").forEach((button) => button.addEventListener("click", () => {
    runButton(button, () => disableBillingSubscription(button), "停用中…").then(() => {
      if (billingDetail) renderBillingAdminValues(billingDetail);
    });
  }));
  for (const [id, fallback] of [["billing-ledger-prev", 0], ["billing-ledger-next", billingLedgerNextOffset]]) {
    const button = byId(id);
    button.addEventListener("click", () => {
      const offset = Number(button.dataset.offset ?? fallback);
      runButton(button, () => changeBillingLedgerPage(offset), "加载中…").then(() => {
        if (billingDetail) renderBillingLedger(billingDetail);
      });
    });
  }

  all("[data-open]").forEach((button) => button.addEventListener("click", () => {
    if (!button.disabled) openDialog(button.dataset.open);
  }));
  all("[data-close]").forEach((button) => button.addEventListener("click", () => button.closest("dialog")?.close()));
  all("dialog").forEach((dialog) => dialog.addEventListener("close", () => setLocalMessage(dialog)));

  byId("add-passkey").addEventListener("click", () => openDialog("passkey-dialog"));
  byId("personal-tab").addEventListener("click", () => showUsageTab("personal"));
  byId("global-tab").addEventListener("click", () => showUsageTab("global"));
  for (const tab of [byId("personal-tab"), byId("global-tab")]) {
    tab.addEventListener("keydown", (event) => {
      if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
      const target = tab === byId("personal-tab") && !byId("global-tab").classList.contains("hidden")
        ? byId("global-tab") : byId("personal-tab");
      event.preventDefault();
      showUsageTab(target === byId("global-tab") ? "global" : "personal");
      target.focus();
    });
  }
  all("[data-usage-tab]").forEach((button) => button.addEventListener("click", () => {
    location.hash = "usage";
    showUsageTab(button.dataset.usageTab);
  }));
  byId("clear-user-filter").addEventListener("click", () => runButton(byId("clear-user-filter"), async () => {
    setPersonalScope();
    await loadPersonalUsage(queryFromForm(byId("usage-filter")), false);
  }, "加载中…"));

  byId("usage-filter").addEventListener("input", updateCSVLink);
  byId("usage-filter").addEventListener("change", updateCSVLink);
  byId("global-filter").elements.range.addEventListener("change", syncGlobalRange);
  window.addEventListener("hashchange", () => routeFromHash(true));

  byId("copy-secret").addEventListener("click", () => runButton(byId("copy-secret"), async () => {
    const value = byId("secret-value").textContent;
    try {
      await navigator.clipboard.writeText(value);
    } catch (_) {
      const temporary = element("textarea", {className: "sr-only"});
      temporary.value = value;
      temporary.readOnly = true;
      document.body.append(temporary);
      try {
        temporary.select();
        if (!document.execCommand("copy")) throw new Error("浏览器拒绝复制，请手动选择内容。");
      } finally {
        temporary.remove();
      }
    }
    setLocalMessage(byId("secret-dialog"), "已复制到剪贴板。", "ok");
  }, "复制中…"));
  byId("save-secret").addEventListener("click", () => byId("secret-dialog").close());
  byId("secret-dialog").addEventListener("close", () => {
    const afterClose = secretAfterClose;
    secretAfterClose = null;
    byId("secret-value").textContent = "";
    byId("secret-title").textContent = "只显示一次";
    byId("secret-description").textContent = "";
    setLocalMessage(byId("secret-dialog"));
    if (afterClose) afterClose();
  });
  byId("secret-dialog").addEventListener("cancel", (event) => {
    event.preventDefault();
    setLocalMessage(byId("secret-dialog"), "请先安全保存内容，再使用“我已安全保存并关闭”确认。");
  });
}

async function start() {
  bindUI();
  initializeDateFilters();
  const path = location.pathname;
  if (path === "/join") {
    configureInvitationView();
    applyWebAuthnSupport();
    return;
  }
  if (path === "/recover") {
    hide("login-view");
    hide("join-view");
    show("recover-view");
    applyWebAuthnSupport();
    return;
  }

  hide("join-view");
  hide("recover-view");
  show("login-view");
  checkingSession = true;
  setLocalMessage(byId("login-view"), "正在检查现有会话…", "ok");
  try {
    await loadDashboard();
    setLocalMessage(byId("login-view"));
  } catch (error) {
    if (error.status === 401) {
      setLocalMessage(byId("login-view"));
    } else {
      setConnection("连接失败", "error");
      setLocalMessage(byId("login-view"), `无法加载控制台：${friendlyError(error)}`);
    }
  } finally {
    checkingSession = false;
  }
  applyWebAuthnSupport();
}

start().catch((error) => {
  show("login-view");
  setLocalMessage(byId("login-view"), friendlyError(error));
  notice(friendlyError(error), "error", true);
});
