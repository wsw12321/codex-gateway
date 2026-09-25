"use strict";

const byId = (id) => document.getElementById(id);
const all = (selector, root = document) => Array.from(root.querySelectorAll(selector));
const sectionTitles = {
  overview: "概览",
  resources: "资源",
  keys: "API Keys",
  guide: "使用指导",
  billing: "额度与订阅",
  groups: "群组额度",
  security: "账号安全",
  usage: "使用统计",
  monitoring: "请求监控",
  "model-access": "模型权限",
  "model-identification": "模型鉴别",
  "upstream-accounts": "上游账号",
  information: "信息管理",
};
const ownerOnlySections = new Set(["upstream-accounts"]);
ownerOnlySections.add("model-access");
ownerOnlySections.add("model-identification");
ownerOnlySections.add("groups");
ownerOnlySections.add("information");
ownerOnlySections.add("monitoring");
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
let secretDismissible = false;
let personalRequestSequence = 0;
let globalRequestSequence = 0;
let checkingSession = false;
let loggingOut = false;
let identityGeneration = 0;
let billingDetail = null;
let billingUsers = [];
let billingSettings = null;
let billingLedgerOffset = 0;
let billingLedgerNextOffset = 0;
let billingRequestSequence = 0;
let billingUsersRequestSequence = 0;
let billingUserID = "";
let billingDetailLoading = false;
let billingSourceOperation = null;
let billingSourceGeneration = 0;
let billingUserSearch = null;
let billingBatch = null;
let billingBatchSelectedIDs = new Set();
let billingBatchUsersReady = false;
let billingBatchGeneration = 0;
let billingBatchRefreshing = false;
let globalUserSearch = null;
let recoveryUserSearch = null;
let modelAccessModels = [];
let modelAccessUsers = [];
let modelAccessUserEntries = [];
let modelAccessSelectedModels = new Set();
let modelAccessSelectedUsers = new Set();
let modelAccessSelectionInitialized = false;
let modelAccessModelsRequestSequence = 0;
let modelAccessUsersRequestSequence = 0;
let modelIdentificationOptionsSequence = 0;
let modelIdentificationRecordsSequence = 0;
let modelIdentificationTimer = 0;
let modelIdentificationPolling = false;
let modelIdentificationOptionsReady = false;
let modelIdentificationRunning = false;
let upstreamAccountRequestSequence = 0;
let upstreamAccounts = [];
let upstreamAccountSyncHealthy = false;
let upstreamAccountListLoading = false;
let upstreamAccountOperation = null;
const upstreamQuotaStaleTimers = new Map();
let upstreamConcurrencyTimer = 0;
let upstreamConcurrencyStaleTimer = 0;
let upstreamConcurrencyController = null;
let upstreamConcurrencyGeneration = 0;
let upstreamConcurrencyPolling = false;
let upstreamConcurrencySnapshot = null;
let informationSequence = 0;
let informationOverviewSequence = 0;
let informationUsersSequence = 0;
let informationPreview = null;
let informationJob = null;
let informationOperation = false;
let informationJobTimer = 0;
let informationUsers = [];
let informationSelectedUsers = new Set();
let informationSelectedDetails = new Map();
let informationUsersOffset = 0;
let informationUsersReady = false;
let informationLoaded = false;
let monitoringTimer = 0;
let monitoringRequestSequence = 0;
let monitoringPolling = false;
let monitoringController = null;
let monitoringSnapshot = null;
let reauthResolve = null;
let reauthReject = null;
let reauthPromise = null;
let reauthRequestCurrent = null;

const billingLedgerPageSize = 50;
const upstreamQuotaStaleAfterMS = 5 * 60 * 1000;
const upstreamQuotaRequestBody = '{"method":"account/rateLimits/read","id":6}';
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
    active: "活跃", available: "可用", disabled: "已停用", unavailable: "不可用", archived: "已归档", revoked: "已撤销",
    completed: "已完成", degraded: "被降智", failed: "失败", cancelled: "已取消", in_progress: "进行中",
    open: "开放", acknowledged: "已确认", resolved: "已解决", stale: "可能已过时",
  })[status] || status || "未知";
}

function statusBadge(status) {
  return element("span", {className: "status-badge", text: statusLabel(status), dataset: {status: status || "unknown"}});
}

const upstreamCliproxyStatusLabels = {
  active: "可用", unavailable: "暂不可用", disabled: "凭据已停用", error: "状态错误", unknown: "状态未知",
};
const upstreamGatewayManualStatusLabels = {
  enabled: "已启用", manual_disabled: "手动禁用", unknown: "状态未知",
};
const upstreamGatewayQuotaStatusLabels = {
  available: "正常", quota_exhausted: "已锁定", unknown: "状态未知",
};
const upstreamFinalStatusLabels = {
  available: "可用", unavailable: "不可用", unknown: "状态未知",
};

function upstreamStatusKnown(account) {
  return ["active", "unavailable", "disabled", "error"].includes(account?.cliproxy_status) &&
    ["enabled", "manual_disabled"].includes(account?.gateway_manual_status) &&
    ["available", "quota_exhausted"].includes(account?.gateway_quota_status);
}

function upstreamFinalStatus(account) {
  if (!upstreamStatusKnown(account)) return "unknown";
  return account.status === "available" || account.status === "unavailable" ? account.status : "unknown";
}

function upstreamStatusBadge(label, status, labels, className = "") {
  const value = Object.prototype.hasOwnProperty.call(labels, status) ? status : "unknown";
  const badge = statusBadge(value);
  badge.textContent = `${label}：${labels[value]}`;
  if (className) badge.classList.add(className);
  return badge;
}

function upstreamAccountStatusBadges(account) {
  const final = upstreamFinalStatus(account);
  const finalBadge = upstreamStatusBadge("最终分流", final, upstreamFinalStatusLabels, "upstream-account-status");
  return [
    upstreamStatusBadge("CLIProxyAPI", account?.cliproxy_status, upstreamCliproxyStatusLabels, "upstream-account-cliproxy-status"),
    upstreamStatusBadge("Gateway手动", account?.gateway_manual_status, upstreamGatewayManualStatusLabels, "upstream-account-manual-status"),
    upstreamStatusBadge("Gateway额度", account?.gateway_quota_status, upstreamGatewayQuotaStatusLabels, "upstream-account-quota-status"),
    finalBadge,
  ];
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
  if (host.matches?.("#billing-recharge-form, #billing-adjustment-form, .billing-subscription-form") ||
      host.closest?.(".billing-subscription-form, .billing-pagination")) syncBillingUserControls();
  if (host.closest?.("#billing-batch-panel")) syncBillingBatchControls();
  if (host.closest?.("#groups, #group-detail") || host.id === "groups-refresh") syncGroupControls();
  if (host.closest?.('[data-section="information"]')) syncInformationControls();
  if (host.id === "model-identification-form") syncModelIdentificationControls();
}

function bindAsync(id, eventName, handler, busyLabel = "处理中…", requestCurrent = null) {
  const host = byId(id);
  if (!host) return;
  host.addEventListener(eventName, async (event) => {
    if (host.dataset.busy === "true") return;
    const current = requestCurrent?.();
    if (eventName === "submit") event.preventDefault();
    setLocalMessage(host);
    setBusy(host, true, busyLabel);
    try {
      await handler(event);
    } catch (error) {
      if (current && !current()) return;
      const message = friendlyError(error);
      if (!setLocalMessage(host, message)) notice(message, "error");
    } finally {
      if (!current || current()) setBusy(host, false);
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

async function copyText(value) {
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
}

function clearSensitiveDOM() {
  const dialog = byId("secret-dialog");
  secretAfterClose = null;
  secretDismissible = false;
  byId("secret-value").textContent = "";
  byId("secret-eyebrow").textContent = "ONLY ONCE";
  byId("secret-title").textContent = "只显示一次";
  byId("secret-description").textContent = "请立即安全保存。关闭后内容会从页面清除，无法再次查看。";
  byId("save-secret").textContent = "我已安全保存并关闭";
  setLocalMessage(dialog);
  if (dialog.open) dialog.close();
}

function handleUnauthorized() {
  stopUpstreamConcurrency();
  stopMonitoring();
  resetMonitoring();
  resetInformation();
  resetGroupManagement();
  identityGeneration++;
  cancelReauthentication();
  invitationToken = "";
  secretAfterClose = null;
  personalRequestSequence++;
  globalRequestSequence++;
  billingRequestSequence++;
  billingUsersRequestSequence++;
  modelAccessModelsRequestSequence++;
  modelAccessUsersRequestSequence++;
  upstreamAccountRequestSequence++;
  upstreamAccounts = [];
  upstreamAccountSyncHealthy = false;
  upstreamAccountListLoading = false;
  upstreamAccountOperation = null;
  stopModelIdentification();
  resetModelIdentification();
  setUpstreamAccountMessage("upstream-account-action-message");
  setUpstreamAccountMessage("upstream-account-refresh-message");
  clearUpstreamQuotaTimers();
  state = null;
  overviewSummary = null;
  billingDetail = null;
  billingUsers = [];
  billingSettings = null;
  billingLedgerOffset = 0;
  billingLedgerNextOffset = 0;
  modelAccessModels = [];
  modelAccessUsers = [];
  modelAccessUserEntries = [];
  modelAccessSelectedModels.clear();
  modelAccessSelectedUsers.clear();
  modelAccessSelectionInitialized = false;
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
  resetBillingUserSearch();
  globalUserSearch?.reset();
  recoveryUserSearch?.reset();
  byId("billing-ledger-page").textContent = "—";
  byId("billing-ledger-prev").disabled = true;
  byId("billing-ledger-next").disabled = true;
  byId("model-access-model-select").replaceChildren(element("p", {text: "登录后加载模型"}));
  byId("model-access-model-search").value = "";
  byId("model-access-user-search").value = "";
  byId("model-access-model-count").textContent = "已选 0 个模型";
  byId("model-access-default-state").textContent = "请选择要管理的模型。";
  byId("model-access-enabled-count").textContent = "—";
  byId("model-access-disabled-count").textContent = "—";
  byId("model-access-user-rows").replaceChildren(tableMessage(5, "登录后加载用户权限。"));
  byId("model-access-select-all").checked = false;
  byId("model-access-selected-count").textContent = "已选 0";
  for (const tier of billingTiers) byId(`billing-${tier.id}-ends`).textContent = "未启用";
  hide("personal-scope");
  resetPersonalUsageSummary();
  hide("personal-loading");
  byId("usage-rows").replaceChildren(tableMessage(8, "登录后加载使用明细。"));
  byId("global-rows").replaceChildren(tableMessage(6, "登录后加载全员汇总。"));
  hide("upstream-account-loading");
  byId("upstream-account-period").textContent = "—";
  byId("upstream-allocation-period").textContent = "近 24 小时费用独立于历史统计筛选；费用占比以所有已归因账号费用为分母。";
  byId("upstream-account-list").setAttribute("aria-busy", "false");
  byId("upstream-account-list").replaceChildren(emptyState("登录后加载上游账号。"));
  for (const id of ["devices", "projects", "keys", "passkeys"]) {
    byId(id).replaceChildren(element("div", {className: "empty", text: "登录后加载。"}));
  }
  if (!checkingSession) notice("登录会话已失效，请重新登录。", "error", true);
}

function requireCurrentRequest(current) {
  if (current && !current()) throw Object.assign(new Error("页面或登录身份已变化，请求结果已忽略。"), {code: "stale_request"});
}

async function api(path, options = {}, current = null) {
  requireCurrentRequest(current);
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
  requireCurrentRequest(current);
  if (!response.ok) {
    if (response.status === 401 && ["session_required", "invalid_session"].includes(body?.error?.code)) handleUnauthorized();
    const error = new Error(body?.error?.message || `请求失败 (${response.status})`);
    error.code = body?.error?.code;
    error.status = response.status;
    error.blockers = Array.isArray(body?.blockers) ? body.blockers : [];
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
  await finishLogin();
}

async function passwordLogin(event) {
  const data = Object.fromEntries(new FormData(event.currentTarget));
  await api("/auth/password/login", {method: "POST", body: JSON.stringify(data)});
  await finishLogin();
}

async function finishLogin() {
  history.replaceState(null, "", "/#overview");
  await loadDashboard();
}

async function reauthenticate(current = null) {
	return chooseReauthentication(current);
}

async function passkeyReauthenticate(current = null) {
  const ceremony = await api("/auth/reauth/begin", {method: "POST", body: "{}"}, current);
  const credential = await getPasskey(ceremony);
  await api("/auth/reauth/finish", {method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential})}, current);
  requireCurrentRequest(current);
  if (state) {
    state.recently_verified = true;
    state.recent_verification_expires_at = null;
  }
  notice("Passkey 二次验证成功，敏感操作已临时解锁。", "ok");
}

function chooseReauthentication(current = null) {
  if (reauthPromise) return reauthPromise;
  const actorUserID = state?.user?.id;
  const generation = identityGeneration;
  const requestCurrent = () => reauthRequestCurrent === requestCurrent && Boolean(actorUserID) && !loggingOut && state?.user?.id === actorUserID &&
    identityGeneration === generation && (!current || current());
  const dialog = byId("reauth-dialog");
  const select = byId("reauth-form").elements.method;
  const methods = state?.login_methods || {};
  all("option", select).forEach((option) => {
    option.disabled = !methods[option.value] || (option.value === "passkey" && !webAuthnSupported());
  });
  const available = all("option", select).find((option) => !option.disabled);
  if (!available) return Promise.reject(new Error("账号没有当前可用的二次验证方式。"));
  reauthRequestCurrent = requestCurrent;
  select.value = available.value;
  syncReauthMethod();
  dialog.showModal();
  const pending = new Promise((resolve, reject) => { reauthResolve = resolve; reauthReject = reject; });
  const shared = pending.finally(() => {
    if (reauthPromise === shared) reauthPromise = null;
  });
  reauthPromise = shared;
  return shared;
}

function cancelReauthentication() {
  const reject = reauthReject;
  reauthResolve = reauthReject = reauthRequestCurrent = reauthPromise = null;
  const dialog = byId("reauth-dialog");
  if (dialog.open) dialog.close();
  const form = byId("reauth-form");
  form.reset();
  setLocalMessage(form);
  setBusy(form, false);
  reject?.(Object.assign(new Error("身份验证已取消。"), {code: "reauth_cancelled"}));
}

function syncReauthMethod() {
  const password = byId("reauth-form").elements.method.value === "password";
  byId("reauth-form").querySelector(".reauth-password").classList.toggle("hidden", !password);
  byId("reauth-form").elements.password.required = password;
}

async function submitReauthentication(event) {
  const form = event.currentTarget;
  const current = reauthRequestCurrent;
  if (!current) return;
  requireCurrentRequest(current);
  const method = form.elements.method.value;
  if (method === "password") {
    await api("/auth/password/reauth", {method: "POST", body: JSON.stringify({password: form.elements.password.value})}, current);
    requireCurrentRequest(current);
    notice("密码二次验证成功，敏感操作已临时解锁。", "ok");
  } else {
    await passkeyReauthenticate(current);
  }
  requireCurrentRequest(current);
  if (state) { state.recently_verified = true; state.recent_verification_expires_at = null; }
  const resolve = reauthResolve;
  reauthResolve = reauthReject = reauthRequestCurrent = null;
  form.reset();
  setBusy(form, false);
  form.closest("dialog").close();
  resolve?.();
}

function verificationIsRecent() {
  if (!state?.recently_verified) return false;
  const expires = state.recent_verification_expires_at;
  return !expires || new Date(expires).getTime() > Date.now();
}

async function sensitiveAction(operation, current = null) {
  requireCurrentRequest(current);
  if (!verificationIsRecent()) await reauthenticate(current);
  requireCurrentRequest(current);
  try {
    return await operation();
  } catch (error) {
    requireCurrentRequest(current);
    if (error.code !== "recent_identity_verification_required") throw error;
    if (state) state.recently_verified = false;
    await reauthenticate(current);
    requireCurrentRequest(current);
    return operation();
  }
}

function showSecret(title, value, description, afterClose = null, dismissible = false) {
  const dialog = byId("secret-dialog");
  if (dialog.open) {
    secretAfterClose = null;
    dialog.close();
  }
  secretDismissible = dismissible;
  byId("secret-eyebrow").textContent = dismissible ? "REVEAL" : "ONLY ONCE";
  byId("secret-title").textContent = title;
  byId("secret-description").textContent = description;
  byId("secret-value").textContent = value;
  byId("save-secret").textContent = dismissible ? "关闭" : "我已安全保存并关闭";
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
  if (data.login_method === "password") {
    if (data.password !== data.password_confirmation) throw new Error("两次输入的新密码不一致。");
    delete data.login_method;
    delete data.password_confirmation;
    if (invitationKind === "recovery") {
      delete data.username;
      delete data.display_name;
      const result = await api("/auth/password/recovery", {method: "POST", body: JSON.stringify(data)});
      finishWithRecoveryCodes(result.recovery_codes);
      return;
    }
    const result = await api("/auth/password/register", {method: "POST", body: JSON.stringify(data)});
    finishWithRecoveryCodes(result.recovery_codes);
    return;
  }
  delete data.login_method;
  delete data.password;
  delete data.password_confirmation;
  const ceremony = await api("/auth/register/begin", {method: "POST", body: JSON.stringify(data)});
  const credential = await createPasskey(ceremony);
  const result = await api("/auth/register/finish", {
    method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential}),
  });
  finishWithRecoveryCodes(result.recovery_codes);
}

async function recover(event) {
  const data = Object.fromEntries(new FormData(event.currentTarget));
  if (data.login_method === "password") {
    if (data.password !== data.password_confirmation) throw new Error("两次输入的新密码不一致。");
    delete data.login_method;
    delete data.password_confirmation;
    const result = await api("/auth/password/recovery", {method: "POST", body: JSON.stringify(data)});
    finishWithRecoveryCodes(result.recovery_codes);
    return;
  }
  delete data.login_method;
  delete data.password;
  delete data.password_confirmation;
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
    {done: Boolean(state?.api_keys?.length), title: "创建 API Key", detail: "二次验证后可再次查看", section: "keys"},
    {
      done: Number(overviewSummary?.requests || 0) > 0 || Boolean(state?.api_keys?.some((key) => key.last_used_at)),
      title: "配置 Codex 并完成首次请求", detail: "按使用指导接入 Gateway", section: "guide",
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
      hasDevice ? "还没有 API Key。新 Key 可在二次验证后查看。" : "还没有 API Key，且当前没有活跃设备。",
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
    if (key.secret_available) {
      const reveal = element("button", {type: "button", className: "secondary", text: "查看"});
      reveal.addEventListener("click", () => runButton(reveal, () => revealKey(key), "解密中…"));
      actions.append(reveal);
    }
    const nextStatus = key.status === "active" ? "disabled" : "active";
    const status = element("button", {
      type: "button", className: "secondary", text: nextStatus === "active" ? "启用" : "停用",
    });
    status.addEventListener("click", () => runButton(status, () => changeKeyStatus(key, nextStatus), nextStatus === "active" ? "启用中…" : "停用中…"));
    const remove = element("button", {type: "button", className: "danger", text: "删除"});
    remove.addEventListener("click", () => runButton(remove, () => deleteKey(key), "删除中…"));
    actions.append(status, remove);
    const allowlist = Array.isArray(key.model_allowlist) && key.model_allowlist.length ? key.model_allowlist.join(", ") : "全部模型";
    const detail = element("div", {className: "list-detail"},
      element("span", {text: `设备：${devices.get(key.device_id) || key.device_id}`}),
      element("span", {text: `默认项目：${key.default_project_id ? (projects.get(key.default_project_id) || key.default_project_id) : "未分配"}`}),
      element("span", {text: `模型：${allowlist}`}),
      element("span", {text: `到期：${formatDateTime(key.expires_at, "—")}`}),
      element("span", {text: `最后使用：${formatDateTime(key.last_used_at)}`}),
      element("span", {text: `创建：${formatDateTime(key.created_at, "—")}`}),
      ...(!key.secret_available ? [element("span", {text: "完整值：历史 Key 无法查看"})] : []),
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

function renderLoginMethods() {
  const methods = state.login_methods || {passkey: state.passkeys.length > 0, password: false};
  byId("password-status").replaceChildren(
    summaryItem("密码登录", methods.password ? "已设置" : "未设置"),
    summaryItem("Passkey 登录", methods.passkey ? "已设置" : "未设置"),
  );
  byId("set-password").textContent = methods.password ? "更改密码" : "设置密码";
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

  renderGuide();
}

function renderGuide() {
  const baseURL = `${location.origin}/v1`;

  byId("guide-base-url").textContent = baseURL;
  byId("guide-install-code").textContent = `curl -fsSL '${location.origin}/setup/configure-codex.sh' | sh`;
  byId("guide-config-code").textContent = `openai_base_url = "${baseURL}"`;

  if (!state) return;
  const activeDevices = state.devices.filter((item) => item.status === "active").length;
  const activeProjects = state.projects.filter((item) => item.status === "active").length;
  const activeKeys = state.api_keys.filter((item) => item.status === "active").length;
  const ready = activeDevices > 0 && activeKeys > 0;
  const badge = byId("guide-resource-badge");
  badge.textContent = ready ? "可以开始配置" : "需要准备资源";
  badge.dataset.status = ready ? "active" : "in_progress";
  byId("guide-resource-status").replaceChildren(
    summaryItem("活跃设备", formatInteger(activeDevices)),
    summaryItem("活跃项目（可选）", formatInteger(activeProjects)),
    summaryItem("活跃 API Keys", formatInteger(activeKeys)),
  );
}

function renderState(value) {
  const previousUser = state?.user;
  state = {
    ...value,
    devices: Array.isArray(value.devices) ? value.devices : [],
    projects: Array.isArray(value.projects) ? value.projects : [],
    api_keys: Array.isArray(value.api_keys) ? value.api_keys : [],
    passkeys: Array.isArray(value.passkeys) ? value.passkeys : [],
    login_methods: value.login_methods || {},
  };
  if (!state.user) throw new Error("管理台状态缺少当前用户信息。");
  const owner = state.user.role === "owner";
  if (previousUser && (previousUser.id !== state.user.id || previousUser.role !== state.user.role)) {
    stopUpstreamConcurrency();
    stopMonitoring();
    stopModelIdentification();
    resetModelIdentification();
    resetMonitoring();
    resetInformation();
    identityGeneration++;
    resetGroupManagement();
    billingRequestSequence++;
    billingUsersRequestSequence++;
    globalRequestSequence++;
    resetBillingUserSearch();
    globalUserSearch?.reset();
  }
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
  renderLoginMethods();
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
    `API Key ${result.prefix || ""} 已加密保存。请复制到目标设备；之后可通过二次验证再次查看。`,
    null,
    true,
  );
  result.api_key = "";
  await refreshAfterMutation();
}

async function revealKey(key) {
  const result = await sensitiveAction(() => api(
    `/admin/api-keys/${encodeURIComponent(key.id)}/reveal`, {method: "POST", body: "{}"},
  ));
  showSecret(
    `查看 ${key.name}`,
    result.api_key || "",
    `这是 API Key ${key.key_prefix}。关闭弹窗后，完整值会立即从页面清除。`,
    null,
    true,
  );
  result.api_key = "";
}

function renderAPIKeyMutation() {
  renderAPIKeys();
  renderSelects();
  renderResourceSummary();
  renderOnboarding();
}

async function changeKeyStatus(key, status) {
  if (status === "disabled" && !window.confirm("停用后，该 API Key 的新请求会立即被拒绝。确定继续？")) return;
  await sensitiveAction(() => api(`/admin/api-keys/${encodeURIComponent(key.id)}/status`, {
    method: "PUT", body: JSON.stringify({status}),
  }));
  key.status = status;
  renderAPIKeyMutation();
  notice(`API Key 已${status === "active" ? "启用" : "停用"}。`, "ok");
  await refreshAfterMutation();
}

async function deleteKey(key) {
  if (!window.confirm("删除后，API Key 会立即失效，且凭证和完整值无法恢复。既有用量与账务历史仍会保留。确定永久删除？")) return;
  await sensitiveAction(() => api(`/admin/api-keys/${encodeURIComponent(key.id)}`, {method: "DELETE"}));
  state.api_keys = state.api_keys.filter((item) => item.id !== key.id);
  renderAPIKeyMutation();
  notice("API Key 已永久删除。", "ok");
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

async function setPassword(event) {
  const form = event.currentTarget;
  const data = Object.fromEntries(new FormData(form));
  if (data.password !== data.password_confirmation) throw new Error("两次输入的新密码不一致。");
  await sensitiveAction(() => api("/admin/password", {method: "PUT", body: JSON.stringify({password: data.password})}));
  form.reset();
  form.closest("dialog")?.close();
  notice("密码已保存，其他会话已撤销。", "ok");
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
  const selected = billingUserID;
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

function billingSubscriptionPeriod(subscription) {
  const count = Number(subscription?.period_count);
  const current = Number(subscription?.current_period_number);
  return {
    count: Number.isInteger(count) && count >= 0 && count <= 99 ? count : 0,
    current: Number.isInteger(current) && current >= 1 ? current : 1,
  };
}

function billingPeriodProgress(subscription) {
  const period = billingSubscriptionPeriod(subscription);
  return period.count === 0
    ? `第 ${period.current} 个周期 · 无限期`
    : `当前第 ${period.current}/${period.count} 个周期`;
}

function billingPeriodEndLabel(subscription) {
  const period = billingSubscriptionPeriod(subscription);
  return period.count > 0 && period.current >= period.count ? "本周期结束（最终失效）" : "本周期结束";
}

function billingCashBalance(detail = billingDetail) {
  return detail?.cash_balance_usd ?? detail?.account?.cash_balance_usd ?? "0";
}

function billingSourceDisabled(source, detail = billingDetail) {
  return (detail?.source_disabled ?? detail?.account?.source_disabled)?.[source] === true;
}

function billingSourceReady() {
  return !loggingOut && Boolean(state?.user?.id) && !billingDetailLoading && Boolean(billingDetail) &&
    selectedBillingUserID() === state.user.id && billingUserForDetail().id === state.user.id;
}

function billingSourceLabel(source) {
  return source === "cash" ? "现金余额" : billingTiers.find((tier) => tier.id === source)?.label;
}

function billingSourceControl(source) {
  const button = element("button", {type: "button", className: "secondary billing-source-button", dataset: {billingSource: source}});
  button.disabled = true;
  button.addEventListener("click", () => changeBillingSource(source));
  return element("div", {className: "billing-source-control"},
    element("small", {className: "billing-source-state", dataset: {billingSourceState: source}}), button,
  );
}

function setBillingSourceMessage(message = "", error = false) {
  const node = byId("billing-source-message");
  node.textContent = message;
  node.dataset.kind = error ? "error" : "ok";
  node.setAttribute("role", error ? "alert" : "status");
  node.classList.toggle("hidden", !message);
}

function resetBillingSourceState() {
  billingSourceGeneration++;
  billingSourceOperation = null;
  if (reauthRequestCurrent && !reauthRequestCurrent()) cancelReauthentication();
  setBillingSourceMessage();
}

function syncBillingSourceControls() {
  const ready = billingSourceReady();
  const self = Boolean(state?.user?.id) && selectedBillingUserID() === state.user.id;
  all("[data-billing-source]").forEach((button) => {
    const source = button.dataset.billingSource;
    const disabled = billingSourceDisabled(source);
    const saving = billingSourceOperation?.source === source;
    button.disabled = !ready || Boolean(billingSourceOperation);
    button.classList.toggle("hidden", !self);
    button.textContent = saving ? "保存中…" : disabled ? "恢复扣费" : "禁用扣费";
    button.setAttribute("aria-label", `${billingSourceLabel(source)}：${button.textContent}`);
  });
  all("[data-billing-source-state]").forEach((node) => {
    const disabled = billingSourceDisabled(node.dataset.billingSourceState);
    node.textContent = !billingDetail || billingDetailLoading ? "等待额度加载" : disabled ? "已禁用扣费" : "允许扣费";
    node.dataset.disabled = String(Boolean(billingDetail) && !billingDetailLoading && disabled);
  });
  byId("billing-source-readonly").classList.toggle("hidden", self || !state);
}

function billingSourceOperationCurrent(operation) {
  return billingSourceOperation === operation && billingSourceGeneration === operation.generation &&
    !loggingOut && state?.user?.id === operation.userID && selectedBillingUserID() === operation.userID;
}

async function changeBillingSource(source) {
  if (!billingSourceLabel(source) || !billingSourceReady() || billingSourceOperation) return;
  const operation = {source, userID: state.user.id, generation: billingSourceGeneration, disabled: !billingSourceDisabled(source)};
  const current = () => billingSourceOperationCurrent(operation);
  billingSourceOperation = operation;
  setBillingSourceMessage();
  syncBillingSourceControls();
  let saved = false;
  let saveError = null;
  try {
    try {
      const result = await sensitiveAction(() => {
        if (!billingSourceOperationCurrent(operation)) throw new Error("登录身份或查看的用户已变化，操作已停止。");
        return api(`/admin/billing/me/sources/${source}/status`, {
          method: "PUT", body: JSON.stringify({disabled: operation.disabled}),
        }, current);
      }, current);
      if (!billingSourceOperationCurrent(operation)) return;
      if (result?.source !== source || result?.disabled !== operation.disabled) throw new Error("服务器返回的扣费设置不一致。");
      saved = true;
    } catch (error) {
      if (!billingSourceOperationCurrent(operation)) return;
      saveError = error;
    }
    let refreshError = null;
    try {
      await loadBillingDetail(operation.userID, 0);
    } catch (error) {
      refreshError = error;
    }
    if (!billingSourceOperationCurrent(operation)) return;
    const message = saved
      ? `${billingSourceLabel(source)}已${operation.disabled ? "禁用" : "恢复"}扣费；仅影响新请求。`
      : `保存未确认：${friendlyError(saveError)}。${refreshError ? "" : "已重新加载服务器设置。"}`;
    setBillingSourceMessage(`${message}${refreshError ? ` 额度刷新失败：${friendlyError(refreshError)}，请刷新页面后重试。` : ""}`, !saved || Boolean(refreshError));
  } finally {
    if (billingSourceOperation === operation) {
      billingSourceOperation = null;
      syncBillingSourceControls();
    }
  }
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
      element("small", {text: enabled ? billingPeriodProgress(subscription) : "剩余额度不会结转"}),
      element("small", {text: enabled ? `开始：${formatDateTime(subscription?.period_started_at, "—")}` : "Owner 可随时启用"}),
      element("small", {text: enabled ? `${billingPeriodEndLabel(subscription)}：${formatDateTime(subscription?.period_ends_at, "—")}` : "周期数可设为 1–99 或 0（无限期）"}),
      element("small", {text: enabled ? `最终失效：${subscription?.expires_at ? formatDateTime(subscription.expires_at, "—") : "无限期"}` : ""}),
      billingSourceControl(tier.id),
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
      for (const [name, label] of [["input_tokens", "输入"], ["cached_input_tokens", "缓存读取"], ["cache_write_tokens", "缓存写入"], ["output_tokens", "输出"]]) {
        const value = field(entry, name);
        if (value != null) tokenParts.push(`${label} ${String(value)}`);
      }
      if (tokenParts.length) request.append(element("small", {text: tokenParts.join(" · ")}));
      const pricingParts = [];
      for (const [name, label] of [["pricing_service_tier", "计价层"], ["context_class", "上下文"], ["cache_write_mode", "写入模式"]]) {
        const value = field(entry, name);
        if (value) pricingParts.push(`${label} ${String(value)}`);
      }
      const fallback = field(entry, "pricing_fallback_reason");
      if (fallback) pricingParts.push(`兜底 ${String(fallback)}`);
      if (pricingParts.length) request.append(element("small", {text: pricingParts.join(" · ")}));

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
    if (document.activeElement !== form.elements.period_count) {
      form.elements.period_count.value = subscription?.id ? String(subscription.period_count ?? 1) : "1";
    }
    form.querySelector("[data-disable-subscription]").disabled = !billingSubscriptionEnabled(subscription);
  }
  syncBillingUserControls();
}

function renderBillingDetail(detail) {
  billingDetail = detail || {};
  renderBillingGroup(detail?.group);
  const user = billingUserForDetail(detail);
  const cash = billingCashBalance(detail);
  byId("billing-cash-balance").textContent = formatUSD(cash, formatUSD("0"));
  byId("billing-cash-source").replaceChildren(billingSourceControl("cash"));
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
      ? `${billingPeriodProgress(subscription)} · ${billingPeriodEndLabel(subscription)}：${formatDateTime(subscription?.period_ends_at, "—")} · 最终失效：${subscription?.expires_at ? formatDateTime(subscription.expires_at, "—") : "无限期"}`
      : `固定 ${tier.duration}`;
  }
  renderBillingSubscriptions(detail);
  renderBillingLedger(detail);
  renderBillingAdminValues(detail);
}

/*
 * User search is deliberately kept in one place.  The API returns the same
 * user shape to all owner tools, but older deployments only include username
 * and display_name.  Newer responses may additionally carry a pre-computed
 * pinyin index (search_index/pinyin_full/pinyin_initials); using all of the
 * fields here keeps the picker backwards compatible while making Chinese
 * names searchable by full pinyin and initials.
 */
function userSearchIndex(user) {
  if (!user || typeof user !== "object") return "";
  const values = [];
  const add = (value) => {
    if (Array.isArray(value)) value.forEach(add);
    else if (value != null) values.push(String(value));
  };
  for (const key of [
    "username", "display_name", "name", "id", "search_index", "search_text",
    "pinyin", "pinyin_full", "pinyin_initials", "display_name_pinyin",
    "display_name_pinyin_full", "display_name_pinyin_initials", "initials",
  ]) add(user[key]);
  return values.join(" ").toLocaleLowerCase();
}

function userMatchesSearch(user, query) {
  const terms = String(query || "").trim().toLocaleLowerCase().split(/\s+/).filter(Boolean);
  if (!terms.length) return true;
  const index = userSearchIndex(user);
  return terms.every((term) => index.includes(term));
}

function matchingUsers(query, users = []) {
  return (Array.isArray(users) ? users : []).filter((user) => userMatchesSearch(user, query));
}

/**
 * Shared single/multi user-picker primitive.
 *
 * Existing pages use createUserSearch for a single owner target.  Keeping
 * that function as a thin wrapper around UserPicker lets the batch and
 * checkbox pages share the exact same search index and interaction helpers
 * without changing their business-specific selection rules.
 */
class UserPicker {
  constructor(id, onSelect = null, describe = () => "") {
    this.id = id;
    this.input = byId(id);
    this.results = byId(`${id}-results`);
    this.status = byId(`${id}-status`);
    this.host = this.input?.closest?.(".user-search") || this.input?.parentElement;
    this.onSelect = onSelect;
    this.describe = describe;
    this.users = [];
    this.matches = [];
    this.activeIndex = -1;
    this.composing = false;
    this.bound = false;
    this.bind();
  }

  bind() {
    const input = this.input;
    const results = this.results;
    if (!input || !results || this.bound) return;
    this.bound = true;
    input.addEventListener("focus", () => this.render());
    input.addEventListener("click", () => {
      if (input.getAttribute("aria-expanded") !== "true") this.render();
    });
    input.addEventListener("input", () => { if (!this.composing) this.render(); });
    input.addEventListener("compositionstart", () => { this.composing = true; this.close(); });
    input.addEventListener("compositionend", () => { this.composing = false; this.render(); });
    input.addEventListener("keydown", (event) => this.keydown(event));
    results.addEventListener("mousedown", (event) => {
      if (event.target.closest?.('[role="option"]')) event.preventDefault();
    });
    this.host?.addEventListener?.("focusout", (event) => {
      if (!this.host.contains(event.relatedTarget)) this.close();
    });
    document.addEventListener("pointerdown", (event) => {
      if (!this.host?.contains?.(event.target)) this.close();
    });
  }

  close() {
    if (!this.input || !this.results) return;
    hide(this.results);
    this.input.setAttribute("aria-expanded", "false");
    this.input.removeAttribute("aria-activedescendant");
    this.activeIndex = -1;
  }

  activate(index) {
    this.activeIndex = index;
    Array.from(this.results?.children || []).forEach((node, position) => {
      if (node.getAttribute("role") === "option") node.setAttribute("aria-selected", String(position === index));
    });
    const active = index >= 0 ? this.results?.children?.[index] : null;
    if (active) {
      this.input.setAttribute("aria-activedescendant", active.id);
      active.scrollIntoView?.({block: "nearest"});
    } else this.input.removeAttribute("aria-activedescendant");
  }

  async choose(user) {
    if (!this.input || this.input.disabled || this.composing || state?.user?.role !== "owner") return;
    this.input.value = user.username || user.display_name || user.id || "";
    this.input.focus({preventScroll: true});
    this.close();
    if (this.status) this.status.textContent = `已选择 ${user.display_name || user.username || user.id} (${user.username || user.id})`;
    if (!this.onSelect) return;
    try { await this.onSelect(user); }
    catch (error) { if (state?.user?.role === "owner") notice(friendlyError(error), "error"); }
  }

  keydown(event) {
    const input = this.input;
    if (input.disabled || this.composing || event.isComposing || event.keyCode === 229) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      if (input.getAttribute("aria-expanded") !== "true") this.render();
      if (!this.matches.length) return;
      const next = this.activeIndex < 0
        ? (event.key === "ArrowDown" ? 0 : this.matches.length - 1)
        : (this.activeIndex + (event.key === "ArrowDown" ? 1 : -1) + this.matches.length) % this.matches.length;
      this.activate(next);
    } else if (event.key === "Enter" && this.activeIndex >= 0 && input.getAttribute("aria-expanded") === "true") {
      event.preventDefault();
      this.choose(this.matches[this.activeIndex]);
    } else if (event.key === "Escape") {
      event.preventDefault();
      this.close();
    } else if (event.key === "Tab") this.close();
  }

  render(open = true) {
    if (!this.input || this.input.disabled || !this.results) return;
    const query = this.input.value.trim();
    this.matches = matchingUsers(query, this.users);
    this.activeIndex = -1;
    this.input.removeAttribute("aria-activedescendant");
    if (this.status) this.status.textContent = query
      ? (this.matches.length ? `找到 ${this.matches.length} 位用户` : "未找到匹配的用户")
      : `共 ${this.users.length} 位用户`;
    this.results.replaceChildren(...this.matches.map((user, index) => {
      const role = user.role ? (user.role === "owner" ? "Owner" : "Member") : "";
      const status = user.status ? statusLabel(user.status) : "";
      const context = this.describe(user);
      const detail = [role, status, context].filter(Boolean).join(" · ");
      const item = element("div", {className: "user-search-option", attributes: {
        id: `${this.id}-option-${index}`, role: "option", "aria-selected": "false", tabindex: "-1",
      }},
      element("span", {className: "user-search-name", text: user.display_name || user.username || user.id}),
      element("small", {className: "user-search-username", text: user.username || user.id}),
      detail ? element("small", {className: "user-search-detail", text: detail}) : null);
      item.addEventListener("click", () => this.choose(user));
      return item;
    }));
    if (!this.matches.length) this.results.append(element("div", {className: "user-search-empty", text: this.status?.textContent || "没有匹配的用户"}));
    if (open) {
      show(this.results);
      this.input.setAttribute("aria-expanded", "true");
    } else this.close();
  }

  setUsers(values) {
    this.users = Array.isArray(values) ? values : [];
    if (!this.input) return;
    this.input.disabled = false;
    this.render(document.activeElement === this.input);
  }

  unavailable(message) {
    this.users = [];
    this.matches = [];
    if (!this.input) return;
    this.input.disabled = true;
    this.close();
    this.results?.replaceChildren();
    if (this.status) this.status.textContent = message;
  }

  reset() {
    if (this.input) this.input.value = "";
    this.composing = false;
    this.unavailable("登录后加载用户");
  }
}

function createUserSearch(id, onSelect, describe = () => "") {
  return new UserPicker(id, onSelect, describe);
}

/* Kept as a named factory for pages that need a multi-select picker. */
function createUserPicker(id, onSelect, describe = () => "") {
  return new UserPicker(id, onSelect, describe);
}

/* Legacy implementation intentionally removed; see UserPicker above. */
/* istanbul ignore next */
function createLegacyUserSearch(id, onSelect, describe = () => "") {
  const input = byId(id);
  const results = byId(`${id}-results`);
  const status = byId(`${id}-status`);
  const host = input.closest(".user-search");
  let users = [];
  let matches = [];
  let activeIndex = -1;
  let composing = false;

  function close() {
    hide(results);
    input.setAttribute("aria-expanded", "false");
    input.removeAttribute("aria-activedescendant");
    activeIndex = -1;
  }

  function activate(index) {
    activeIndex = index;
    Array.from(results.children).forEach((node, position) => {
      if (node.getAttribute("role") === "option") node.setAttribute("aria-selected", String(position === index));
    });
    const active = index >= 0 ? results.children[index] : null;
    if (active) {
      input.setAttribute("aria-activedescendant", active.id);
      active.scrollIntoView({block: "nearest"});
    } else {
      input.removeAttribute("aria-activedescendant");
    }
  }

  async function choose(user) {
    if (input.disabled || composing || state?.user?.role !== "owner") return;
    input.value = user.username;
    input.focus({preventScroll: true});
    close();
    status.textContent = `已选择 ${user.display_name || user.username} (${user.username})`;
    try {
      await onSelect(user);
    } catch (error) {
      if (state?.user?.role === "owner") notice(friendlyError(error), "error");
    }
  }

  function render(open = true) {
    if (input.disabled) return;
    const query = input.value.trim().toLowerCase();
    matches = users.filter((user) => String(user.username).toLowerCase().includes(query));
    activeIndex = -1;
    input.removeAttribute("aria-activedescendant");
    status.textContent = query
      ? (matches.length ? `找到 ${matches.length} 位用户` : "未找到匹配的用户名")
      : `共 ${users.length} 位用户`;
    results.replaceChildren(...matches.map((user, index) => {
      const detail = describe(user);
      const item = element("div", {className: "user-search-option", attributes: {
        id: `${id}-option-${index}`, role: "option", "aria-selected": "false", tabindex: "-1",
      }},
      element("span", {className: "user-search-name", text: user.display_name || user.username}),
      element("small", {className: "user-search-username", text: user.username}),
      detail ? element("small", {className: "user-search-detail", text: detail}) : null);
      item.addEventListener("click", () => choose(user));
      return item;
    }));
    if (!matches.length) results.append(element("div", {className: "user-search-empty", text: status.textContent}));
    if (open) {
      show(results);
      input.setAttribute("aria-expanded", "true");
    } else close();
  }

  input.addEventListener("focus", () => render());
  input.addEventListener("click", () => {
    if (input.getAttribute("aria-expanded") !== "true") render();
  });
  input.addEventListener("input", () => { if (!composing) render(); });
  input.addEventListener("compositionstart", () => { composing = true; close(); });
  input.addEventListener("compositionend", () => { composing = false; render(); });
  input.addEventListener("keydown", (event) => {
    if (input.disabled || composing || event.isComposing || event.keyCode === 229) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      if (input.getAttribute("aria-expanded") !== "true") render();
      if (matches.length) {
        const next = activeIndex < 0
          ? (event.key === "ArrowDown" ? 0 : matches.length - 1)
          : (activeIndex + (event.key === "ArrowDown" ? 1 : -1) + matches.length) % matches.length;
        activate(next);
      }
    } else if (event.key === "Enter" && activeIndex >= 0 && input.getAttribute("aria-expanded") === "true") {
      event.preventDefault();
      choose(matches[activeIndex]);
    } else if (event.key === "Escape") {
      event.preventDefault();
      close();
    } else if (event.key === "Tab") close();
  });
  results.addEventListener("mousedown", (event) => {
    if (event.target.closest('[role="option"]')) event.preventDefault();
  });
  host.addEventListener("focusout", (event) => {
    if (!host.contains(event.relatedTarget)) close();
  });
  document.addEventListener("pointerdown", (event) => {
    if (!host.contains(event.target)) close();
  });

  return {
    close,
    setUsers(values) {
      users = values;
      input.disabled = false;
      render(document.activeElement === input);
    },
    unavailable(message) {
      users = [];
      matches = [];
      input.disabled = true;
      close();
      results.replaceChildren();
      status.textContent = message;
    },
    reset() {
      input.value = "";
      composing = false;
      this.unavailable("登录后加载用户");
    },
  };
}

function renderBillingUsers(result) {
  const values = Array.isArray(result) ? result : (result?.users || result?.items);
  if (!Array.isArray(values)) throw new Error("账务用户列表响应格式无效。");
  const unique = new Map();
  for (const value of Array.isArray(values) ? values : []) {
    const user = normalizeBillingUser(value);
    if (user.id) unique.set(user.id, user);
  }
  const current = normalizeBillingUser(state?.user);
  if (current.id && !unique.has(current.id)) unique.set(current.id, current);
  billingUsers = Array.from(unique.values()).sort((left, right) =>
    String(left.username || left.display_name).localeCompare(String(right.username || right.display_name), "zh-CN"));
  if (!billingUserID) billingUserID = current.id;
  billingUserSearch.setUsers(billingUsers);
  recoveryUserSearch?.setUsers(billingUsers);
  billingBatchUsersReady = true;
  billingBatchSelectedIDs = new Set(Array.from(billingBatchSelectedIDs).filter((id) => unique.has(id)));
  renderBillingBatchUsers();
}

function resetBillingUserSearch() {
  resetBillingSourceState();
  billingUserID = "";
  billingDetail = null;
  billingDetailLoading = false;
  billingUsers = [];
  billingUserSearch?.reset();
  recoveryUserSearch?.reset();
  resetBillingBatchState();
  byId("billing-scope-name").textContent = "—";
  syncBillingUserControls();
}

function billingUserReady() {
  return state?.user?.role === "owner" && !billingDetailLoading && Boolean(billingDetail) &&
    billingUserForDetail().id === selectedBillingUserID();
}

function syncBillingUserControls() {
  syncBillingSourceControls();
  const ready = billingUserReady();
  all("#billing-recharge-form, #billing-adjustment-form, .billing-subscription-form").forEach((form) => {
    const busy = form.dataset.busy === "true";
    all("input, button", form).forEach((control) => {
      const tier = control.dataset.disableSubscription;
      control.disabled = !ready || (busy && control.matches("button")) || control.dataset.busy === "true" ||
        Boolean(tier && !billingSubscriptionEnabled(billingSubscription(billingDetail, tier)));
    });
  });
  if (!billingDetail || billingDetailLoading) {
    byId("billing-ledger-prev").disabled = true;
    byId("billing-ledger-next").disabled = true;
  }
}

function billingWriteUserID() {
  if (!billingUserReady()) throw new Error("请等待所选用户的额度加载成功后再操作。");
  return selectedBillingUserID();
}

async function selectBillingUser(user) {
  if (user.id !== selectedBillingUserID()) resetBillingSourceState();
  billingUserID = user.id;
  all(".billing-form, .billing-subscription-form").forEach((form) => setLocalMessage(form));
  await loadBillingDetail(user.id, 0);
  if (billingUserReady() && billingUserID === user.id) announce("所选用户的额度与账务流水已更新。");
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
  return billingUserID || state?.user?.id || "";
}

function billingDetailPath(userID) {
  if (!userID || userID === state?.user?.id) return "/admin/billing/me";
  return `/admin/billing/users/${encodeURIComponent(userID)}`;
}

async function loadBillingDetail(userID = selectedBillingUserID(), offset = 0) {
  if (loggingOut || !state) return;
  if (!userID) throw new Error("无法确定要查看的账务用户。");
  const sequence = ++billingRequestSequence;
  const actorUserID = state.user.id;
  const current = () => sequence === billingRequestSequence && !loggingOut && state?.user?.id === actorUserID && selectedBillingUserID() === userID;
  billingDetailLoading = true;
  billingDetail = null;
  const user = billingUsers.find((item) => item.id === userID) || (state?.user?.id === userID ? state.user : null);
  byId("billing-scope-name").textContent = `${user?.display_name || user?.username || userID} (${user?.username || userID}) · 正在加载`;
  byId("billing-cash-balance").textContent = "—";
  for (const tier of billingTiers) {
    byId(`billing-${tier.id}-remaining`).textContent = "—";
    byId(`billing-${tier.id}-ends`).textContent = "正在加载…";
  }
  byId("billing-subscriptions").setAttribute("aria-busy", "true");
  byId("billing-subscriptions").textContent = "正在加载周期额度…";
  syncBillingUserControls();
  billingLedgerOffset = Math.max(0, Number(offset) || 0);
  show("billing-loading");
  setTableBusy(byId("billing-ledger-rows"), 5, "正在加载账务流水…");
  const query = new URLSearchParams({limit: String(billingLedgerPageSize), offset: String(billingLedgerOffset)});
  try {
    const result = await api(`${billingDetailPath(userID)}?${query}`, undefined, current);
    if (!current()) return;
    if (normalizeBillingUser(result?.user || result).id !== userID) throw new Error("返回的账务用户与所选用户不一致，请重新选择重试。");
    billingDetailLoading = false;
    renderBillingDetail(result);
  } catch (error) {
    if (!current()) return;
    billingDetail = null;
    byId("billing-scope-name").textContent = `${user?.display_name || user?.username || userID} (${user?.username || userID}) · 加载失败，请重新选择重试`;
    for (const tier of billingTiers) byId(`billing-${tier.id}-ends`).textContent = "加载失败";
    byId("billing-subscriptions").classList.remove("loading");
    byId("billing-subscriptions").setAttribute("aria-busy", "false");
    byId("billing-subscriptions").replaceChildren(emptyState(`额度加载失败：${friendlyError(error)}`));
    byId("billing-ledger-rows").closest("table")?.setAttribute("aria-busy", "false");
    byId("billing-ledger-rows").replaceChildren(tableMessage(5, friendlyError(error)));
    throw error;
  } finally {
    if (current()) {
      billingDetailLoading = false;
      syncBillingUserControls();
      hide("billing-loading");
    }
  }
}

async function loadBillingUsers() {
  if (loggingOut || state?.user?.role !== "owner") return;
  const sequence = ++billingUsersRequestSequence;
  const actorUserID = state.user.id;
  const generation = identityGeneration;
  const current = () => sequence === billingUsersRequestSequence && generation === identityGeneration &&
    !loggingOut && state?.user?.id === actorUserID && state.user.role === "owner";
  billingUserSearch.unavailable("正在加载用户…");
  billingBatchUsersReady = false;
  renderBillingBatchUsers("正在加载用户…");
  try {
    const result = await api("/admin/billing/users", undefined, current);
    if (!current()) return;
    renderBillingUsers(result);
  } catch (error) {
    if (!current()) return;
    billingUserSearch.unavailable("用户列表加载失败，请刷新重试");
    billingBatchUsersReady = false;
    renderBillingBatchUsers("用户列表加载失败，请点击“刷新账务数据”；已有批次仍可重试未成功项。");
    throw error;
  }
}

async function loadBillingSettings() {
  if (loggingOut || state?.user?.role !== "owner") return;
  const actorUserID = state.user.id;
  const generation = identityGeneration;
  const current = () => generation === identityGeneration && !loggingOut &&
    state?.user?.id === actorUserID && state.user.role === "owner";
  try {
    const result = await api("/admin/billing/settings", undefined, current);
    if (current()) renderBillingSettings(result);
  } catch (error) {
    if (current()) throw error;
  }
}

async function loadBillingDashboard() {
  const tasks = [loadBillingDetail(selectedBillingUserID(), 0)];
  if (state.user.role === "owner") {
    tasks.push(
      loadBillingSettings().catch((error) => {
        setLocalMessage(byId("billing-rate-form"), `充值汇率加载失败：${friendlyError(error)}`);
      }),
      loadBillingUsers().catch((error) => {
        notice(`账务用户列表加载失败：${friendlyError(error)}`, "error");
      }),
    );
  }
  await Promise.all(tasks);
}

function billingBatchMatches() {
  return matchingUsers(byId("billing-batch-search").value, billingUsers);
}

function syncBillingBatchControls() {
  const owner = !loggingOut && state?.user?.role === "owner";
  const locked = !owner || !billingBatchUsersReady || Boolean(billingBatch);
  const matches = billingBatchUsersReady ? billingBatchMatches() : [];
  const selectedMatches = matches.filter((user) => billingBatchSelectedIDs.has(user.id)).length;
  const selectAll = byId("billing-batch-select-all");
  const panel = byId("billing-batch-panel");
  all("input, select", panel).forEach((control) => { control.disabled = locked; });
  all("button[type=submit]", panel).forEach((control) => {
    control.disabled = locked || billingBatchSelectedIDs.size === 0;
  });
  selectAll.disabled = locked || matches.length === 0;
  selectAll.checked = matches.length > 0 && selectedMatches === matches.length;
  selectAll.indeterminate = selectedMatches > 0 && selectedMatches < matches.length;
  byId("billing-batch-clear-selection").disabled = locked || billingBatchSelectedIDs.size === 0;
  byId("billing-batch-refresh-users").disabled = !owner || billingBatchRefreshing || Boolean(billingBatch?.running);
  byId("billing-batch-refresh-users").textContent = billingBatchRefreshing ? "刷新中…" : "刷新账务数据";
  byId("billing-batch-selected-count").textContent = `已选 ${billingBatchSelectedIDs.size} 位用户`;
  const preview = billingBatch?.phase === "preview";
  const running = Boolean(billingBatch?.running);
  byId("billing-batch-start").disabled = !owner || !billingBatchUsersReady || !preview || running;
  byId("billing-batch-retry").disabled = !owner || !billingBatch || preview || running ||
    !billingBatch.items.some((item) => item.status !== "success");
  byId("billing-batch-reset").disabled = !owner || !billingBatch || running;
  byId("billing-batch-reset").textContent = preview ? "取消本批次" : "结束本批次";
  byId("billing-batch-start").classList.toggle("hidden", !preview);
  byId("billing-batch-retry").classList.toggle("hidden", !billingBatch || preview ||
    !billingBatch.items.some((item) => item.status !== "success"));
}

function setBillingBatchUserSelected(userID, selected) {
  if (loggingOut || state?.user?.role !== "owner" || !billingBatchUsersReady || billingBatch) return;
  if (!billingUsers.some((user) => user.id === userID)) return;
  if (selected) billingBatchSelectedIDs.add(userID);
  else billingBatchSelectedIDs.delete(userID);
  syncBillingBatchControls();
}

function selectBillingBatchMatches(checked) {
  if (loggingOut || state?.user?.role !== "owner" || !billingBatchUsersReady || billingBatch) return;
  for (const user of billingBatchMatches()) {
    if (checked) billingBatchSelectedIDs.add(user.id);
    else billingBatchSelectedIDs.delete(user.id);
  }
  renderBillingBatchUsers();
}

function clearBillingBatchSelection() {
  if (loggingOut || state?.user?.role !== "owner" || !billingBatchUsersReady || billingBatch) return;
  billingBatchSelectedIDs.clear();
  renderBillingBatchUsers();
}

function billingBatchIdentity(user) {
  return element("div", {className: "billing-batch-user-identity"},
    element("strong", {text: user.display_name || user.username || user.id}),
    element("small", {text: user.username || user.id}));
}

function renderBillingBatchUsers(message = "") {
  const ready = billingBatchUsersReady && !loggingOut && state?.user?.role === "owner";
  const matches = ready ? billingBatchMatches() : [];
  const rows = matches.map((user) => {
    const checkbox = element("input", {
      type: "checkbox", className: "billing-batch-checkbox billing-batch-user-select",
      attributes: {value: user.id, "aria-label": `选择用户 ${user.username || user.id}`},
    });
    checkbox.checked = billingBatchSelectedIDs.has(user.id);
    checkbox.addEventListener("change", () => setBillingBatchUserSelected(user.id, checkbox.checked));
    return element("tr", {},
      element("td", {}, checkbox),
      element("td", {}, billingBatchIdentity(user)),
      element("td", {}, element("span", {text: user.role === "owner" ? "Owner" : "Member"}), statusBadge(user.status)),
      element("td", {text: formatUSD(user.cash_balance_usd)}));
  });
  const empty = ready ? "没有匹配的用户。" : (message || "正在加载用户…");
  byId("billing-batch-user-rows").replaceChildren(...(rows.length ? rows : [tableMessage(4, empty)]));
  byId("billing-batch-users-status").textContent = ready
    ? `显示 ${matches.length} / ${billingUsers.length} 位用户；搜索不会清除已选用户。` : empty;
  syncBillingBatchControls();
}

function resetBillingBatchState() {
  billingBatchGeneration++;
  billingBatch = null;
  billingBatchSelectedIDs.clear();
  billingBatchUsersReady = false;
  billingBatchRefreshing = false;
  byId("billing-batch-search").value = "";
  for (const id of ["billing-batch-recharge-form", "billing-batch-subscription-form"]) {
    byId(id).reset();
    setLocalMessage(byId(id));
  }
  renderBillingBatchUsers("登录后加载用户。");
  renderBillingBatchResult();
}

function billingBatchAmount(value) {
  const amount = String(value || "").trim();
  if (!/^(0|[1-9][0-9]{0,17})(\.[0-9]{1,6})?$/.test(amount) || !/[1-9]/.test(amount)) {
    throw new Error("金额必须大于 0，最多 18 位整数和 6 位小数。");
  }
  return amount;
}

function billingBatchTotal(amount, count) {
  const [whole, fraction = ""] = billingBatchAmount(amount).split(".");
  const units = BigInt(whole) * 1000000n + BigInt(fraction.padEnd(6, "0"));
  const total = units * BigInt(count);
  const decimal = String(total % 1000000n).padStart(6, "0").replace(/0+$/, "");
  return `${total / 1000000n}${decimal ? `.${decimal}` : ""}`;
}

function prepareBillingBatch(event, kind) {
  if (loggingOut || state?.user?.role !== "owner") throw new Error("仅 Owner 可执行批量账务操作。");
  if (billingBatch) throw new Error("请先结束或取消当前批次。");
  if (!billingBatchUsersReady) throw new Error("请等待用户列表加载成功后再操作。");
  const selected = billingUsers.filter((user) => billingBatchSelectedIDs.has(user.id));
  if (!selected.length) throw new Error("请至少选择一位用户。");
  if (selected.length !== billingBatchSelectedIDs.size) throw new Error("用户列表已变化，请重新选择用户。");
  if (!crypto?.randomUUID) throw new Error("当前浏览器无法生成安全的操作 ID，请升级浏览器后重试。");
  const form = event.currentTarget;
  const data = new FormData(form);
  const reason = billingReason(form);
  if (Array.from(reason).length > 500) throw new Error("操作原因最多 500 字。");
  let payload;
  let tier = "";
  if (kind === "recharge") {
    payload = {cny_amount: billingBatchAmount(data.get("cny_amount")), reason};
  } else if (kind === "subscription") {
    tier = String(data.get("tier") || "");
    if (!billingTiers.some((item) => item.id === tier)) throw new Error("订阅档位无效。");
    payload = {quota_usd: billingBatchAmount(data.get("quota_usd")), period_count: billingPeriodCount(form), reason};
  } else throw new Error("批量操作类型无效。");
  billingBatch = {
    kind, tier, payload: Object.freeze(payload), actorUserID: state.user.id,
    generation: billingBatchGeneration, phase: "preview", running: false, message: "",
    items: selected.map((user) => ({
      user: Object.freeze({...user}), operationID: crypto.randomUUID(), status: "pending", message: "",
    })),
  };
  renderBillingBatchResult();
  byId("billing-batch-result").focus();
}

function billingBatchIsCurrent(batch) {
  return Boolean(batch && batch === billingBatch && batch.generation === billingBatchGeneration &&
    !loggingOut && state?.user?.role === "owner" && state.user.id === batch.actorUserID);
}

function renderBillingBatchResult() {
  const batch = billingBatch;
  const result = byId("billing-batch-result");
  if (!batch) {
    hide(result);
    byId("billing-batch-summary").textContent = "";
    byId("billing-batch-progress").textContent = "";
    byId("billing-batch-result-rows").replaceChildren();
    setLocalMessage(result);
    syncBillingBatchControls();
    return;
  }
  show(result);
  const count = batch.items.length;
  const action = batch.kind === "recharge" ? "批量充值" : "批量开通订阅";
  const specification = batch.kind === "recharge"
    ? `每人 ${batch.payload.cny_amount} CNY，合计 ${billingBatchTotal(batch.payload.cny_amount, count)} CNY。各笔充值按入账时的汇率换算。`
    : `${billingTiers.find((tier) => tier.id === batch.tier).label}：每人每周期 ${batch.payload.quota_usd} USD，${batch.payload.period_count === 0 ? "无限期" : `共 ${batch.payload.period_count} 个周期`}。\n已有同档位订阅将立即关闭旧周期，按新配置从第 1 期重开，旧周期剩余额度不会结转。`;
  byId("billing-batch-summary").textContent = `${action} · ${count} 位用户\n${specification}\n操作原因：${batch.payload.reason}`;
  const counts = {pending: 0, running: 0, success: 0, failed: 0, unknown: 0};
  const labels = {pending: "待处理", running: "处理中", success: "成功", failed: "失败", unknown: "结果待确认"};
  const rows = batch.items.map((item) => {
    counts[item.status]++;
    return element("tr", {},
      element("td", {}, billingBatchIdentity(item.user)),
      element("td", {}, element("span", {className: "billing-batch-status", text: labels[item.status], dataset: {status: item.status}})),
      element("td", {text: item.message || (batch.phase === "preview" ? "确认后执行" : "等待处理")}));
  });
  byId("billing-batch-result-rows").replaceChildren(...rows);
  const progress = batch.phase === "preview" ? "请核对以下用户名单和参数，确认后执行。" :
    `${batch.running ? "处理中" : batch.phase === "paused" ? "已暂停" : "本轮处理完成"}：成功 ${counts.success}，失败 ${counts.failed}，结果待确认 ${counts.unknown}，待处理 ${counts.pending + counts.running} / 共 ${count} 位。`;
  byId("billing-batch-progress").textContent = progress;
  const uncertainty = counts.unknown ? "结果待确认的操作可能已生效，请使用“重试未成功项”核对，避免重新创建相同操作。" : "";
  setLocalMessage(result, [batch.message, uncertainty].filter(Boolean).join(" "));
  syncBillingBatchControls();
}

async function startBillingBatch() {
  const batch = billingBatch;
  if (!billingBatchIsCurrent(batch) || batch.running || batch.phase !== "preview") return;
  if (!billingBatchUsersReady) throw new Error("请等待用户列表加载成功后再操作。");
  await executeBillingBatch(batch);
}

async function retryBillingBatch() {
  const batch = billingBatch;
  if (!billingBatchIsCurrent(batch) || batch.running || batch.phase === "preview") return;
  await executeBillingBatch(batch);
}

async function executeBillingBatch(batch) {
  if (!billingBatchIsCurrent(batch) || batch.running || !batch.items.some((item) => item.status !== "success")) return;
  batch.running = true;
  batch.phase = "running";
  batch.message = "";
  renderBillingBatchResult();
  try {
    for (const item of batch.items) {
      if (!billingBatchIsCurrent(batch)) return;
      if (item.status === "success") continue;
      const previousStatus = item.status;
      let attempted = false;
      item.status = "running";
      item.message = "";
      renderBillingBatchResult();
      const path = `/admin/billing/users/${encodeURIComponent(item.user.id)}/${batch.kind === "recharge" ? "recharges" : `subscriptions/${batch.tier}`}`;
      try {
        await billingMutation(path, batch.kind === "recharge" ? "POST" : "PUT", batch.payload, item.operationID, () => {
          // Recheck after every authentication await, including verification-expiry retries.
          if (!billingBatchIsCurrent(batch)) throw new Error("当前批次已停止。");
          attempted = true;
        });
        if (!billingBatchIsCurrent(batch)) return;
        item.status = "success";
        item.message = batch.kind === "recharge" ? "充值已入账。" : "订阅已按新配置从第 1 期重开。";
      } catch (error) {
        if (!billingBatchIsCurrent(batch)) return;
        if (error.code === "owner_required" || error.code === "user_disabled") {
          resetBillingBatchState();
          notice("账号权限已变化，批量操作已停止。请重新登录以更新权限。", "error");
          return;
        }
        item.message = friendlyError(error);
        if (!attempted || error.code === "reauth_cancelled") {
          item.status = previousStatus;
          batch.phase = "paused";
          batch.message = `批次已暂停：${friendlyError(error)} 可使用“重试未成功项”继续。`;
          break;
        }
        const uncertain = previousStatus === "unknown" || error.network || !error.status ||
          error.status >= 500 || error.status === 408;
        item.status = uncertain ? "unknown" : "failed";
        if (error.status === 401 || error.status === 403 || error.code === "recent_identity_verification_required") {
          batch.phase = "paused";
          batch.message = "身份验证或权限检查未通过，已停止后续操作。恢复权限后可重试未成功项。";
          break;
        }
      }
      renderBillingBatchResult();
    }
    if (!billingBatchIsCurrent(batch)) return;
    if (batch.phase !== "paused") batch.phase = "finished";
    renderBillingBatchResult();
    // Refresh once per attempt. A failed read must never turn a committed write into a failed item.
    const refreshed = await Promise.allSettled([
      loadBillingUsers(), loadBillingDetail(selectedBillingUserID(), 0),
    ]);
    if (!billingBatchIsCurrent(batch)) return;
    if (refreshed.some((entry) => entry.status === "rejected")) {
      batch.message += " 用户摘要或账务详情刷新失败，操作结果已保留；请点击“刷新账务数据”重试读取。";
    }
  } finally {
    if (billingBatchIsCurrent(batch)) {
      batch.running = false;
      renderBillingBatchResult();
    } else if (batch === billingBatch) {
      // A lifecycle reset usually clears this first; also fail closed if identity
      // changes while an awaited verification or write is still outstanding.
      resetBillingBatchState();
    }
  }
}

function finishBillingBatch() {
  if (!billingBatchIsCurrent(billingBatch) || billingBatch.running) return;
  if (billingBatch.items.some((item) => item.status === "unknown") &&
      !window.confirm("仍有结果待确认的操作，可能已经生效。结束本批次会丢失安全重试记录，请先核对账务流水。确定结束？")) return;
  billingBatch = null;
  renderBillingBatchResult();
  renderBillingBatchUsers();
}

async function refreshBillingBatchData() {
  if (loggingOut || state?.user?.role !== "owner" || billingBatch?.running || billingBatchRefreshing) return;
  const generation = billingBatchGeneration;
  billingBatchRefreshing = true;
  syncBillingBatchControls();
  try {
    const refreshed = await Promise.allSettled([loadBillingUsers(), loadBillingDetail(selectedBillingUserID(), 0)]);
    if (generation !== billingBatchGeneration || loggingOut || state?.user?.role !== "owner") return;
    if (refreshed.some((entry) => entry.status === "rejected")) {
      notice("部分账务数据刷新失败，请重试；当前批次及操作结果已保留。", "error");
    } else announce("账务数据已刷新，当前批次及操作结果已保留。");
  } finally {
    if (generation === billingBatchGeneration) {
      billingBatchRefreshing = false;
      syncBillingBatchControls();
    }
  }
}

function bindBillingBatchAction(id, eventName, handler) {
  const host = byId(id);
  host.addEventListener(eventName, async (event) => {
    if (eventName === "submit") event.preventDefault();
    const generation = billingBatchGeneration;
    // The batch's synchronous running/preview guards own concurrency. Keeping
    // busy state on a button would outlive logout while an old request hangs.
    setLocalMessage(host);
    try {
      await handler(event);
    } catch (error) {
      if (generation === billingBatchGeneration && !loggingOut && state?.user?.role === "owner") {
        if (!setLocalMessage(host, friendlyError(error))) notice(friendlyError(error), "error");
      }
    } finally {
      if (generation === billingBatchGeneration) syncBillingBatchControls();
    }
  });
}

function billingReason(form) {
  const reason = String(new FormData(form).get("reason") || "").trim();
  if (!reason) throw new Error("必须填写操作原因。");
  return reason;
}

function billingPeriodCount(form) {
  const value = String(new FormData(form).get("period_count") || "").trim();
  if (!/^(0|[1-9][0-9]?)$/.test(value)) throw new Error("周期数必须是 0–99 的整数；0 表示无限期。");
  return Number(value);
}

async function billingMutation(path, method, payload, operationID = "", beforeSend = null) {
  if (state?.user?.role !== "owner") throw new Error("仅 Owner 可执行此账务操作。");
  if (!crypto?.randomUUID) throw new Error("当前浏览器无法生成安全的操作 ID，请升级浏览器后重试。");
  const actorUserID = state.user.id;
  const input = {...payload, operation_id: operationID || crypto.randomUUID()};
  const body = JSON.stringify(input);
  return sensitiveAction(() => {
    if (loggingOut || state?.user?.role !== "owner" || state.user.id !== actorUserID) {
      throw new Error("登录身份已变化，账务操作已停止。");
    }
    beforeSend?.();
    return api(path, {method, body});
  });
}

async function refreshManagedBilling(userID = selectedBillingUserID()) {
  if (loggingOut || state?.user?.role !== "owner") return;
  await Promise.all([
    userID === selectedBillingUserID() ? loadBillingDetail(userID, 0) : Promise.resolve(),
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
  const userID = billingWriteUserID();
  const data = new FormData(form);
  await billingMutation(`/admin/billing/users/${encodeURIComponent(userID)}/recharges`, "POST", {
    cny_amount: String(data.get("cny_amount") || "").trim(),
    reason: billingReason(form),
  });
  if (userID === selectedBillingUserID()) form.reset();
  await refreshManagedBilling(userID);
  notice("充值已入账并记录汇率快照。", "ok");
}

async function adjustBillingUser(event) {
  const form = event.currentTarget;
  const userID = billingWriteUserID();
  const data = new FormData(form);
  await billingMutation(`/admin/billing/users/${encodeURIComponent(userID)}/adjustments`, "POST", {
    usd_amount: String(data.get("usd_amount") || "").trim(),
    reason: billingReason(form),
  });
  if (userID === selectedBillingUserID()) form.reset();
  await refreshManagedBilling(userID);
  notice("余额调整已记入不可变账务流水。", "ok");
}

async function updateBillingSubscription(event) {
  const form = event.currentTarget;
  const tier = form.dataset.tier;
  if (!billingTiers.some((item) => item.id === tier)) throw new Error("订阅档位无效。");
  const userID = billingWriteUserID();
  const data = new FormData(form);
  await billingMutation(`/admin/billing/users/${encodeURIComponent(userID)}/subscriptions/${tier}`, "PUT", {
    quota_usd: String(data.get("quota_usd") || "").trim(),
    period_count: billingPeriodCount(form),
    reason: billingReason(form),
  });
  if (userID === selectedBillingUserID()) form.elements.reason.value = "";
  await refreshManagedBilling(userID);
  notice(`${billingTiers.find((item) => item.id === tier).label}已从当前时刻重开。`, "ok");
}

async function disableBillingSubscription(button) {
  const tier = button.dataset.disableSubscription;
  const form = button.closest("form");
  const reason = billingReason(form);
  if (!window.confirm(`停用${billingTiers.find((item) => item.id === tier)?.label || "订阅"}后，新请求将立即无法使用当前周期。确定继续？`)) return;
  const userID = billingWriteUserID();
  await billingMutation(`/admin/billing/users/${encodeURIComponent(userID)}/subscriptions/${tier}`, "DELETE", {reason});
  if (userID === selectedBillingUserID()) form.elements.reason.value = "";
  await refreshManagedBilling(userID);
  notice("订阅已立即停用。", "ok");
}

async function changeBillingLedgerPage(offset) {
  await loadBillingDetail(selectedBillingUserID(), Math.max(0, Number(offset) || 0));
  announce("账务流水已更新。");
}

function selectedModelAccessModels() {
  return modelAccessModels.filter((item) => modelAccessSelectedModels.has(item.model)).map((item) => item.model);
}

function modelAccessReason() {
  const form = byId("model-access-users-form");
  const reason = String(form.elements.reason.value || "").trim();
  if (Array.from(reason).length < 1 || Array.from(reason).length > 500) {
    setLocalMessage(form, "请填写 1–500 字的操作原因。");
    throw new Error("请填写 1–500 字的操作原因。");
  }
  return reason;
}

function selectedModelAccessUserIDs() {
  return [...modelAccessSelectedUsers];
}

function syncModelAccessSelection() {
  const checkboxes = all(".model-access-user-select", byId("model-access-user-rows"));
  const selected = checkboxes.filter((input) => input.checked).length;
  const models = selectedModelAccessModels().length;
  const users = modelAccessSelectedUsers.size;
  const selectAll = byId("model-access-select-all");
  selectAll.disabled = checkboxes.length === 0;
  selectAll.checked = checkboxes.length > 0 && selected === checkboxes.length;
  selectAll.indeterminate = selected > 0 && selected < checkboxes.length;
  byId("model-access-selected-count").textContent = `已选 ${formatInteger(models)} 个模型 × ${formatInteger(users)} 位用户 · ${formatInteger(models * users)} 项权限`;
  byId("model-access-enable-selected").disabled = users === 0 || models === 0;
  byId("model-access-disable-selected").disabled = users === 0 || models === 0;
}

function renderModelAccessSummary() {
  const selected = modelAccessModels.filter((item) => modelAccessSelectedModels.has(item.model));
  const hasModel = selected.length > 0;
  const enabledDefaults = selected.filter((item) => item.default_enabled).length;
  const checkbox = byId("model-access-default-enabled");
  checkbox.disabled = !hasModel;
  checkbox.checked = hasModel && enabledDefaults === selected.length;
  checkbox.indeterminate = enabledDefaults > 0 && enabledDefaults < selected.length;
  byId("model-access-default-form").querySelector("button[type=submit]").disabled = !hasModel;
  byId("model-access-enable-all").disabled = !hasModel;
  byId("model-access-disable-all").disabled = !hasModel;
  byId("model-access-default-state").textContent = hasModel
    ? `所选模型当前默认值：${checkbox.indeterminate ? "部分启用" : checkbox.checked ? "全部启用" : "全部禁用"}。保存将统一应用勾选状态。`
    : "请选择要管理的模型。";
  byId("model-access-model-count").textContent = `已选 ${formatInteger(selected.length)} / ${formatInteger(modelAccessModels.length)} 个模型`;
  byId("model-access-enabled-count").textContent = hasModel ? formatInteger(selected.reduce((total, item) => total + item.enabled_user_count, 0)) : "—";
  byId("model-access-disabled-count").textContent = hasModel ? formatInteger(selected.reduce((total, item) => total + item.disabled_user_count, 0)) : "—";
  syncModelAccessSelection();
}

function renderModelAccessUsers(result) {
  modelAccessUserEntries = Array.isArray(result?.users) ? result.users : [];
  const grouped = new Map();
  for (const entry of modelAccessUserEntries) {
    const user = grouped.get(entry.user_id) || {...entry, enabled_count: 0, model_count: 0};
    user.enabled_count += entry.enabled ? 1 : 0;
    user.model_count++;
    if (entry.updated_at > user.updated_at) user.updated_at = entry.updated_at;
    grouped.set(entry.user_id, user);
  }
  modelAccessUsers = [...grouped.values()];
  modelAccessSelectedUsers = new Set([...modelAccessSelectedUsers].filter((id) => grouped.has(id)));
  const query = byId("model-access-user-search")?.value || "";
  const visibleUsers = matchingUsers(query, modelAccessUsers);
  const rows = visibleUsers.map((user) => {
    const checkbox = element("input", {
      type: "checkbox", className: "model-access-checkbox model-access-user-select",
      attributes: {value: user.user_id, "aria-label": `选择用户 ${user.username || user.user_id}`},
    });
    checkbox.checked = modelAccessSelectedUsers.has(user.user_id);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) modelAccessSelectedUsers.add(user.user_id);
      else modelAccessSelectedUsers.delete(user.user_id);
      syncModelAccessSelection();
    });
    const fullyEnabled = user.enabled_count === user.model_count;
    const permission = element("span", {
      className: "status-badge", text: fullyEnabled ? "全部启用" : user.enabled_count ? "部分启用" : "全部禁用",
      dataset: {status: fullyEnabled ? "active" : user.enabled_count ? "pending" : "disabled"},
    });
    const buttons = [true, false].map((enabled) => {
      const button = element("button", {
        type: "button", className: enabled ? "model-access-toggle" : "secondary model-access-toggle",
        text: enabled ? "启用" : "禁用",
      });
      button.disabled = enabled ? fullyEnabled : user.enabled_count === 0;
      button.addEventListener("click", () => runButton(button, async () => {
        await mutateUserModelAccess(enabled, "selected", [user.user_id]);
      }, enabled ? "启用中…" : "禁用中…"));
      return button;
    });
    const identity = element("div", {className: "model-access-user-identity"},
      element("strong", {text: user.display_name || user.username || user.user_id}),
      element("small", {text: `${user.username || "—"} · ${user.user_id}`}),
    );
    if (user.updated_at) identity.append(element("small", {text: `权限更新：${formatDateTime(user.updated_at, "—")}`}));
    return element("tr", {},
      element("td", {className: "model-access-check-cell"}, checkbox),
      element("td", {}, identity),
      element("td", {}, element("span", {text: user.role === "owner" ? "Owner" : "Member"}), statusBadge(user.status)),
      element("td", {}, permission, element("small", {text: `${user.enabled_count} / ${user.model_count} 个模型`})),
      element("td", {}, element("div", {className: "button-group"}, ...buttons)),
    );
  });
  byId("model-access-user-rows").replaceChildren(...(rows.length ? rows : [tableMessage(5, query ? "没有匹配的用户。" : "当前没有用户。")]));
  syncModelAccessSelection();
  byId("model-access-user-rows").closest("table").parentElement.setAttribute("aria-busy", "false");
}

function filterModelAccessUsers() {
  /* Re-render from the raw latest response so filtering never fabricates
     permission rows or drops the server-provided pinyin search fields. */
  renderModelAccessUsers({users: modelAccessUserEntries});
}

function filterModelAccessModels() {
  const query = byId("model-access-model-search").value.trim().toLowerCase();
  all(".model-access-model-choice", byId("model-access-model-select")).forEach((label) => {
    label.hidden = !label.dataset.model.toLowerCase().includes(query);
  });
}

function renderModelAccessModels(result) {
  if (!Array.isArray(result?.models)) throw new Error("模型权限目录响应格式无效。");
  modelAccessModels = result.models.filter((item) => item && typeof item.model === "string" && item.model);
  const available = new Set(modelAccessModels.map((item) => item.model));
  modelAccessSelectedModels = new Set([...modelAccessSelectedModels].filter((model) => available.has(model)));
  if (!modelAccessSelectionInitialized && modelAccessModels.length) {
    modelAccessSelectedModels.add(modelAccessModels[0].model);
    modelAccessSelectionInitialized = true;
  }
  const choices = modelAccessModels.map((item) => {
    const input = element("input", {type: "checkbox", className: "model-access-checkbox model-access-model-checkbox", attributes: {value: item.model}});
    input.checked = modelAccessSelectedModels.has(item.model);
    return element("label", {className: "model-access-model-choice", dataset: {model: item.model}}, input, element("span", {text: item.model}));
  });
  byId("model-access-model-select").replaceChildren(...(choices.length ? choices : [element("p", {text: "没有可管理的模型"})]));
  filterModelAccessModels();
  renderModelAccessSummary();
}

async function loadModelAccessUsers() {
  const sequence = ++modelAccessUsersRequestSequence;
  const models = selectedModelAccessModels();
  const tableWrap = byId("model-access-user-rows").closest("table").parentElement;
  if (!models.length) {
    tableWrap.setAttribute("aria-busy", "false");
    byId("model-access-user-rows").replaceChildren(tableMessage(5, "选择模型后加载用户。已选用户会保留。"));
    syncModelAccessSelection();
    return;
  }
  tableWrap.setAttribute("aria-busy", "true");
  byId("model-access-user-rows").replaceChildren(tableMessage(5, "正在加载用户权限…"));
  syncModelAccessSelection();
  try {
    const query = new URLSearchParams();
    models.forEach((model) => query.append("models", model));
    const result = await api(`/admin/model-access/users?${query}`);
    if (sequence !== modelAccessUsersRequestSequence || JSON.stringify(models) !== JSON.stringify(selectedModelAccessModels())) return;
    renderModelAccessUsers(result);
  } catch (error) {
    if (sequence === modelAccessUsersRequestSequence) {
      tableWrap.setAttribute("aria-busy", "false");
      byId("model-access-user-rows").replaceChildren(tableMessage(5, friendlyError(error)));
      syncModelAccessSelection();
    }
    throw error;
  }
}

async function loadModelAccess() {
  const sequence = ++modelAccessModelsRequestSequence;
  show("model-access-loading");
  try {
    const result = await api("/admin/model-access/models");
    if (sequence !== modelAccessModelsRequestSequence) return;
    renderModelAccessModels(result);
    await loadModelAccessUsers();
  } finally {
    if (sequence === modelAccessModelsRequestSequence) hide("model-access-loading");
  }
}

async function updateModelAccessDefault(event) {
  const form = event.currentTarget;
  const models = selectedModelAccessModels();
  const reason = String(form.elements.reason.value || "").trim();
  if (!models.length) throw new Error("请先选择模型。");
  if (Array.from(reason).length < 1 || Array.from(reason).length > 500) throw new Error("请填写 1–500 字的操作原因。");
  const enabled = form.elements.enabled.checked;
  try {
    const result = await sensitiveAction(() => api("/admin/model-access/defaults", {
      method: "PUT", body: JSON.stringify({models, enabled, reason}),
    }));
    form.elements.reason.value = "";
    notice(`已更新 ${formatInteger(models.length)} 个模型的新用户默认权限；实际变更 ${formatInteger(result.changed_count)} 项。`, "ok");
    await loadModelAccess();
  } catch (error) {
    renderModelAccessSummary();
    throw error;
  }
}

async function mutateUserModelAccess(enabled, scope, userIDs = []) {
  const models = selectedModelAccessModels();
  if (!models.length) throw new Error("请先选择模型。");
  if (scope === "selected" && (userIDs.length < 1 || userIDs.length > 5000)) throw new Error("请选择 1–5000 个用户。");
  const payload = {models, enabled, scope, reason: modelAccessReason()};
  if (scope === "selected") payload.user_ids = [...new Set(userIDs)];
  const result = await sensitiveAction(() => api("/admin/model-access/users", {
    method: "PUT", body: JSON.stringify(payload),
  }));
  byId("model-access-users-form").elements.reason.value = "";
  notice(`${enabled ? "已启用" : "已禁用"} ${formatInteger(models.length)} 个模型的 ${formatInteger(result.target_count)} 项用户权限；实际变更 ${formatInteger(result.changed_count)} 项。`, "ok");
  await loadModelAccess();
}

async function mutateSelectedModelAccess(enabled) {
  try {
    await mutateUserModelAccess(enabled, "selected", selectedModelAccessUserIDs());
  } finally {
    window.setTimeout(syncModelAccessSelection, 0);
  }
}

async function mutateAllModelAccess(enabled) {
  if (!enabled && !window.confirm(`确认禁用全部现有用户的 ${selectedModelAccessModels().length} 个所选模型权限？下一次请求将立即被拒绝。`)) return;
  await mutateUserModelAccess(enabled, "all");
}

function changedModelAccessSelection() {
  modelAccessSelectionInitialized = true;
  renderModelAccessSummary();
  setLocalMessage(byId("model-access-default-form"));
  setLocalMessage(byId("model-access-users-form"));
  loadModelAccessUsers().catch((error) => notice(`用户模型权限加载失败：${friendlyError(error)}`, "error"));
}

function usageNameMaps() {
  return {
    devices: new Map((state?.devices || []).map((item) => [item.id, item.name])),
    keys: new Map((state?.api_keys || []).map((item) => [item.id, `${item.name} · ${item.key_prefix}`])),
    projects: new Map((state?.projects || []).map((item) => [item.id, item.slug])),
  };
}

function usageColumnCount() {
  return state?.user?.role === "owner" ? 9 : 8;
}

function requestUpstreamCell(request) {
  const id = request.upstream_account_id;
  if (!id) return element("td", {text: "未归因"});
  return element("td", {className: "usage-upstream"},
    element("span", {text: request.upstream_masked_email || "邮箱不可用"}),
    element("small", {}, element("code", {text: id})),
  );
}

function renderCleanedHistory(cutoff) {
  const node = byId("usage-cleaned-history");
  if (!node || !cutoff) return;
  const date = new Date(cutoff);
  if (!Number.isFinite(date.getTime())) return;
  node.textContent = `历史清理范围：${date.toISOString().replace("T", " ").replace(".000Z", " UTC")} 之前的旧账务已清理。仍被当前资金或未结算请求引用的记录会保留；统计仅包含保留记录，无法重建的 p95 显示为“—”。`;
  show(node);
}

function resetPersonalUsageSummary(value = "—", busy = false) {
  for (const id of [
    "usage-requests", "usage-tokens", "usage-charged-usd", "metric-cache",
    "metric-cache-write", "metric-ttft", "metric-duration",
  ]) byId(id).textContent = value;
  byId("personal-usage-metrics").setAttribute("aria-busy", busy ? "true" : "false");
}

function usageModelCell(request, requestState = field(request, "state", "State")) {
  const actual = field(request, "model", "Model") || "—";
  const requested = field(request, "requested_model", "RequestedModel");
  if (requestState === "degraded" && requested && requested !== actual) {
    return element("code", {className: "usage-model-degraded", text: `${requested} → ${actual}`});
  }
  return element("code", {text: actual});
}

function renderPersonalUsage(result, updateOverview) {
  const summary = result.summary || {};
  byId("usage-requests").textContent = formatInteger(summary.requests);
  byId("usage-tokens").textContent = formatInteger(summary.tokens);
  byId("usage-charged-usd").textContent = formatMoney(summary.charged_usd, "USD");
  byId("metric-cache").textContent = formatPercent(summary.cache_rate);
  byId("metric-cache-write").textContent = formatInteger(summary.cache_write_tokens);
  byId("metric-ttft").textContent = summary.p95_ttft_ms == null ? "—" : `${formatInteger(summary.p95_ttft_ms)} ms`;
  byId("metric-duration").textContent = summary.p95_duration_ms == null ? "—" : `${formatInteger(summary.p95_duration_ms)} ms`;
  renderCleanedHistory(result.cleaned_before);
  byId("personal-usage-metrics").setAttribute("aria-busy", "false");
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
    tbody.replaceChildren(tableMessage(usageColumnCount(), "当前筛选条件下没有请求记录。"));
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
      element("td", {},
        usageModelCell(request, requestState),
        element("small", {text: [
          field(request, "pricing_service_tier", "PricingServiceTier"),
          field(request, "context_class", "ContextClass"),
          field(request, "pricing_fallback_reason", "PricingFallbackReason"),
        ].filter(Boolean).join(" · ")}),
      ),
      element("td", {}, statusBadge(requestState)),
      element("td", {text: field(request, "http_status", "HTTPStatus") ?? "—"}),
      element("td", {text: formatInteger(inputTokens + outputTokens)}),
    );
    if (state?.user?.role === "owner") tr.append(requestUpstreamCell(request));
    return tr;
  });
  tbody.replaceChildren(...rows);
}

async function loadPersonalUsage(query, updateOverview = false) {
  const sequence = ++personalRequestSequence;
  const loading = byId("personal-loading");
  show(loading);
  resetPersonalUsageSummary("加载中…", true);
  setTableBusy(byId("usage-rows"), usageColumnCount(), "正在加载使用明细…");
  const suffix = querySuffix(query);
  try {
    const result = await api(`/admin/usage${suffix}`);
    if (sequence !== personalRequestSequence) return;
    renderPersonalUsage(result, updateOverview);
  } catch (error) {
    if (sequence === personalRequestSequence) {
      resetPersonalUsageSummary();
      byId("usage-rows").closest("table")?.setAttribute("aria-busy", "false");
      byId("usage-rows").replaceChildren(tableMessage(usageColumnCount(), friendlyError(error)));
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
  byId("metric-global-cost").textContent = formatMoney(usage.actual_cost_usd ?? usage.estimated_usd, "USD");
  byId("metric-global-coverage").textContent = `${formatPercent(coverage)} ledger 覆盖`;
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
  renderCleanedHistory(result.cleaned_before);
  const summary = result.summary || {};
  const usage = summary.usage || {};
  byId("global-usd").textContent = formatMoney(usage.actual_cost_usd ?? usage.estimated_usd, "USD");
  byId("global-charge-detail").textContent = `已扣 ${formatMoney(usage.charged_usd, "USD")} · 未覆盖 ${formatMoney(usage.uncovered_usd, "USD")}`;
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
    element("p", {text: pricing.disclaimer || "OpenAI API Token 等价成本，不代表 OpenAI 实际账单。"}),
    element("p", {text: rateLine}),
    element("p", {text: models.length ? `缺少 ledger 覆盖的模型：${models.join(", ")}` : "当前区间内所有 Token 均可与不可变 ledger 对账。"}),
  );

  const breakdown = result.breakdown || {};
  const breakdownNode = byId("pricing-breakdown");
  const formatDimension = (title, values) => {
    const rows = Array.isArray(values) ? values : [];
    return element("p", {text: `${title}：${rows.length ? rows.map((item) => `${item.value} ${formatInteger(item.requests)} 次 / 写入 ${formatInteger(item.cache_write_tokens)} Token / ${formatMoney(item.actual_cost_usd, "USD")}`).join("；") : "无"}`});
  };
  breakdownNode.replaceChildren(
    formatDimension("服务层", breakdown.service_tiers),
    formatDimension("上下文档位", breakdown.context_classes),
    formatDimension("保守兜底", breakdown.fallbacks),
  );

  const users = Array.isArray(result.users) ? result.users : [];
  globalUserSearch.setUsers(users);
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
      element("span", {text: formatMoney(userUsage.actual_cost_usd ?? userUsage.estimated_usd, "USD")}),
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
  if (loggingOut || state?.user?.role !== "owner") return;
  const sequence = ++globalRequestSequence;
  globalUserSearch.unavailable("正在加载用户…");
  show("global-loading");
  setTableBusy(byId("global-rows"), 6, "正在聚合全员用量…");
  try {
    const result = await api(`/admin/usage/global${querySuffix(query)}`);
    if (sequence !== globalRequestSequence) return;
    renderGlobalUsage(result);
    if (updateOverview) renderGlobalOverview(result);
  } catch (error) {
    if (sequence !== globalRequestSequence) return;
    globalUserSearch.unavailable("用户列表加载失败，请重新应用筛选重试");
    byId("global-rows").closest("table")?.setAttribute("aria-busy", "false");
    byId("global-rows").replaceChildren(tableMessage(6, friendlyError(error)));
    if (updateOverview) {
      const container = byId("global-overview");
      container.classList.remove("loading");
      container.setAttribute("aria-busy", "false");
      container.replaceChildren(emptyState("全员统计暂时无法加载。"));
    }
    throw error;
  } finally {
    if (sequence === globalRequestSequence) hide("global-loading");
  }
}

function clearUpstreamQuotaTimer(accountID) {
  const key = String(accountID || "");
  const timer = upstreamQuotaStaleTimers.get(key);
  if (timer) window.clearTimeout(timer);
  upstreamQuotaStaleTimers.delete(key);
}

function clearUpstreamQuotaTimers() {
  for (const timer of upstreamQuotaStaleTimers.values()) window.clearTimeout(timer);
  upstreamQuotaStaleTimers.clear();
}

let managedGroups = [];
let managedGroup = null;
let groupUsers = [];
let groupSelectedUsers = new Set();
let groupListSequence = 0;
let groupDetailSequence = 0;
let groupOperation = false;
let upstreamAccessAccount = null;
let upstreamAccessUsers = [];
let upstreamAccessSelected = new Set();
let upstreamAccessSequence = 0;

function resetGroupManagement() {
  groupListSequence++;
  groupDetailSequence++;
  upstreamAccessSequence++;
  managedGroups = [];
  managedGroup = null;
  groupUsers = [];
  groupSelectedUsers.clear();
  upstreamAccessSelected.clear();
  upstreamAccessAccount = null;
  upstreamAccessUsers = [];
  groupOperation = false;
  byId("group-list").replaceChildren(emptyState("登录后加载群组。"));
  byId("group-member-list").replaceChildren();
  byId("upstream-access-user-list").replaceChildren();
  byId("group-detail").classList.add("hidden");
  byId("group-detail-name").textContent = "—";
  byId("group-detail-period").textContent = "—";
  byId("group-metrics").replaceChildren();
  byId("upstream-access-account").textContent = "—";
  groupMessage();
  for (const id of ["group-dialog", "upstream-access-dialog"]) {
    if (byId(id).open) byId(id).close();
  }
  renderBillingGroup(null);
  for (const id of ["group-form", "group-members-form", "upstream-access-form"]) {
    const form = byId(id);
    form.reset();
    delete form.dataset.operationId;
    delete form.dataset.operationPayload;
    setBusy(form, false);
    setLocalMessage(form);
  }
  for (const id of ["groups-refresh", "group-members-add", "group-members-remove", "group-archive"]) setBusy(byId(id), false);
}

function groupIdentityCurrent() {
  const generation = identityGeneration;
  const actorUserID = state?.user?.id;
  return () => generation === identityGeneration && !loggingOut && state?.user?.role === "owner" && state.user.id === actorUserID;
}

function groupPeriodLabel(group) {
  return ({day: "日 · 24 小时", week: "周 · 7 天", month: "月 · 31 天"})[group?.period] || `每 ${group?.custom_days || "—"} 天`;
}

function groupMetrics(group) {
  return [
    ["周期额度", group.limit_usd], ["实际已用", group.used_usd], ["剩余额度", group.remaining_usd],
  ].map(([label, value]) => element("article", {}, element("span", {text: label}), element("strong", {text: formatUSD(value)})));
}

function renderBillingGroup(group) {
  const host = byId("billing-group");
  host.classList.toggle("hidden", !group);
  host.replaceChildren();
  if (!group) return;
  host.append(element("h3", {text: `群组额度 · ${group.name}`}),
    element("div", {className: "metrics compact"}, ...groupMetrics(group)),
    element("p", {className: "muted", text: `${groupPeriodLabel(group)} · ${formatDateTime(group.period_starts_at)} — ${formatDateTime(group.period_ends_at)}。群组额度与个人可用资金必须同时有剩余。`}),
  );
}

function groupMessage(message = "", isError = false) {
  const host = byId("groups-message");
  host.textContent = message;
  host.dataset.kind = isError ? "error" : "ok";
  host.setAttribute("role", isError ? "alert" : "status");
  host.classList.toggle("hidden", !message);
}

function renderGroupList() {
  const cards = managedGroups.map((group) => {
    const button = element("button", {type: "button", className: "group-select secondary", attributes: {"aria-pressed": String(managedGroup?.id === group.id)}},
      element("strong", {text: group.name}),
      element("span", {text: group.archived_at ? "已归档" : `${group.member_count} 人 · ${groupPeriodLabel(group)}`}),
      element("small", {text: `剩余 ${formatUSD(group.remaining_usd)} / ${formatUSD(group.limit_usd)}`}),
    );
    button.disabled = groupOperation;
    button.addEventListener("click", () => runButton(button, () => loadGroupDetail(group.id), "加载中…"));
    return button;
  });
  byId("group-list").replaceChildren(...(cards.length ? cards : [emptyState("还没有群组，创建后即可为成员设置共同额度。") ]));
}

async function loadGroups(preferredID = managedGroup?.id || "") {
  if (groupOperation || loggingOut || state?.user?.role !== "owner") return;
  const sequence = ++groupListSequence;
  const generation = identityGeneration;
  const identityCurrent = groupIdentityCurrent();
  const current = () => identityCurrent() && sequence === groupListSequence && !groupOperation;
  let result, users;
  try { [result, users] = await Promise.all([api("/admin/groups", undefined, current), api("/admin/billing/users", undefined, current)]); }
  catch (error) { if (!current()) return; throw error; }
  if (sequence !== groupListSequence || generation !== identityGeneration || groupOperation || loggingOut || state?.user?.role !== "owner") return;
  if (!Array.isArray(result?.groups) || !Array.isArray(users?.users)) throw new Error("群组列表响应格式无效。");
  managedGroups = result.groups;
  groupUsers = users.users;
  const id = managedGroups.some((g) => g.id === preferredID) ? preferredID : managedGroups.find((g) => !g.archived_at)?.id;
  renderGroupList();
  if (id) await loadGroupDetail(id);
  else { managedGroup = null; hide("group-detail"); }
}

async function loadGroupDetail(id) {
  if (groupOperation || loggingOut || state?.user?.role !== "owner") return;
  const sequence = ++groupDetailSequence;
  const generation = identityGeneration;
  const identityCurrent = groupIdentityCurrent();
  const current = () => identityCurrent() && sequence === groupDetailSequence && !groupOperation;
  let group;
  try { group = await api(`/admin/groups/${encodeURIComponent(id)}`, undefined, current); }
  catch (error) { if (!current()) return; throw error; }
  if (sequence !== groupDetailSequence || generation !== identityGeneration || groupOperation || loggingOut || state?.user?.role !== "owner") return;
  if (group?.id !== id || !Array.isArray(group.members)) throw new Error("群组详情响应格式无效。");
  if (managedGroup?.id !== id) groupSelectedUsers.clear();
  managedGroup = group;
  byId("group-detail-name").textContent = group.name;
  byId("group-detail-period").textContent = `${groupPeriodLabel(group)} · ${formatDateTime(group.period_starts_at)} — ${formatDateTime(group.period_ends_at)}${group.archived_at ? " · 已归档" : ""}`;
  byId("group-metrics").replaceChildren(...groupMetrics(group));
  show("group-detail");
  renderGroupList();
  renderGroupMembers();
}

function matchingManagedUsers(search, users = groupUsers) {
  return matchingUsers(search, users);
}

function groupUserAvailable(user) {
  return !user.group_id || user.group_id === managedGroup?.id;
}

function renderGroupMembers() {
  const members = new Map((managedGroup?.members || []).map((member) => [member.user_id, member]));
  const rows = matchingManagedUsers(byId("group-member-search").value).map((user) => {
    const member = members.get(user.id);
    const checkbox = element("input", {type: "checkbox", attributes: {"aria-label": `选择用户 ${user.username || user.id}`}});
    checkbox.checked = groupSelectedUsers.has(user.id);
    checkbox.disabled = groupOperation || !groupUserAvailable(user) || Boolean(managedGroup?.archived_at);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) groupSelectedUsers.add(user.id); else groupSelectedUsers.delete(user.id);
      syncGroupControls();
    });
    const description = member ? `当前成员 · 本期 ${formatUSD(member.used_usd)}` : user.group_id ? "已加入其他群组，需先移除" : "未加入群组";
    return element("label", {className: "user-check-row"}, checkbox,
      element("span", {}, element("strong", {text: user.display_name || user.username}), element("small", {text: `${user.username} · ${description}`})));
  });
  byId("group-member-list").replaceChildren(...(rows.length ? rows : [emptyState("没有匹配的用户。") ]));
  syncGroupControls();
}

function syncGroupControls() {
  const unavailable = groupOperation || !managedGroup || Boolean(managedGroup.archived_at) || loggingOut || state?.user?.role !== "owner";
  const members = new Set((managedGroup?.members || []).map((member) => member.user_id));
  const selected = [...groupSelectedUsers];
  byId("group-selected-count").textContent = `已选 ${selected.length} 人`;
  byId("group-members-add").disabled = unavailable || !selected.some((id) => !members.has(id));
  byId("group-members-remove").disabled = unavailable || !selected.some((id) => members.has(id));
  byId("group-archive").disabled = unavailable || members.size > 0;
  byId("group-edit").disabled = unavailable;
  byId("group-create").disabled = groupOperation;
  byId("groups-refresh").disabled = groupOperation;
  byId("group-select-visible").disabled = unavailable;
  byId("group-clear-selection").disabled = groupOperation;
}

function openGroupEditor(group = null) {
  if (groupOperation || state?.user?.role !== "owner") return;
  const form = byId("group-form");
  form.reset();
  delete form.dataset.operationId;
  delete form.dataset.operationPayload;
  form.elements.group_id.value = group?.id || "";
  form.elements.name.value = group?.name || "";
  form.elements.limit_usd.value = group ? String(group.limit_usd).replace(/(\.\d*?)0+$/, "$1").replace(/\.$/, "") : "";
  form.elements.period.value = group?.period || "month";
  form.elements.custom_days.value = String(group?.custom_days || 14);
  byId("group-dialog-title").textContent = group ? "编辑群组额度" : "创建群组";
  syncGroupPeriodInput();
  openDialog("group-dialog");
}

function syncGroupPeriodInput() {
  const form = byId("group-form");
  const custom = form.elements.period.value === "custom";
  byId("group-custom-days-label").classList.toggle("hidden", !custom);
  form.elements.custom_days.disabled = !custom;
  form.elements.custom_days.required = custom;
}

async function groupMutation(host, path, method, payload) {
  if (groupOperation) throw new Error("请等待当前群组操作完成。");
  const fingerprint = JSON.stringify({path, method, payload});
  if (host.dataset.operationPayload !== fingerprint) {
    host.dataset.operationPayload = fingerprint;
    host.dataset.operationId = crypto.randomUUID();
  }
  const generation = identityGeneration;
  const current = groupIdentityCurrent();
  const body = JSON.stringify({...payload, operation_id: host.dataset.operationId});
  groupOperation = true;
  groupListSequence++;
  groupDetailSequence++;
  syncGroupControls();
  try {
    const group = await sensitiveAction(() => api(path, {method, body}, current), current);
    if (generation !== identityGeneration || loggingOut) return null;
    delete host.dataset.operationId;
    delete host.dataset.operationPayload;
    return group;
  } finally {
    if (generation === identityGeneration) {
      groupOperation = false;
      syncGroupControls();
    }
  }
}

async function refreshGroupsAfterWrite(id, message) {
  groupMessage(message);
  try { await loadGroups(id); }
  catch (error) { groupMessage(`${message} 刷新失败：${friendlyError(error)}，请点击刷新。`, true); }
}

async function saveGroup(event) {
  const form = event.currentTarget;
  const id = form.elements.group_id.value;
  const amount = String(form.elements.limit_usd.value || "").trim();
  if (!/^(0|[1-9][0-9]{0,17})(\.[0-9]{1,6})?$/.test(amount)) {
    throw new Error("额度必须大于或等于 0，最多 18 位整数和 6 位小数。");
  }
  const payload = {name: form.elements.name.value.trim(), limit_usd: amount, period: form.elements.period.value,
    custom_days: form.elements.period.value === "custom" ? Number(form.elements.custom_days.value) : 0,
    reason: billingReason(form)};
  if (form.elements.starts_at.value) payload.starts_at = new Date(form.elements.starts_at.value).toISOString();
  const current = managedGroup?.id === id ? managedGroup : null;
  if (current && (payload.period !== current.period || payload.custom_days !== current.custom_days || payload.starts_at) &&
      !window.confirm("修改周期或起点会立即关闭旧期并建立新期；已接收的请求仍记入旧期。确认修改？")) return;
  const group = await groupMutation(form, id ? `/admin/groups/${encodeURIComponent(id)}` : "/admin/groups", id ? "PUT" : "POST", payload);
  if (!group) return;
  byId("group-dialog").close();
  await refreshGroupsAfterWrite(group.id, "群组额度已保存。");
}

async function changeGroupMembers(action) {
  const group = managedGroup;
  if (!group || groupOperation) return;
  const members = new Set(group.members.map((member) => member.user_id));
  const ids = [...groupSelectedUsers].filter((id) => action === "add" ? !members.has(id) : members.has(id));
  if (!ids.length) return;
  const form = byId("group-members-form");
  const result = await groupMutation(form, `/admin/groups/${encodeURIComponent(group.id)}/members`, "PUT", {action, user_ids: ids, reason: billingReason(form)});
  if (!result) return;
  groupSelectedUsers.clear();
  await refreshGroupsAfterWrite(group.id, `已${action === "add" ? "加入" : "移除"} ${ids.length} 位成员。`);
}

async function archiveSelectedGroup() {
  const group = managedGroup;
  if (!group || group.members.length || groupOperation) return;
  const form = byId("group-members-form");
  const reason = billingReason(form);
  if (!window.confirm(`确认归档空群组“${group.name}”？历史用量会保留。`)) return;
  if (await groupMutation(form, `/admin/groups/${encodeURIComponent(group.id)}`, "DELETE", {reason})) {
    await refreshGroupsAfterWrite(group.id, "群组已归档，历史记录已保留。");
  }
}

function upstreamAccessBlock(account) {
  const exclusive = account.access_mode === "exclusive";
  const button = element("button", {type: "button", className: "secondary upstream-access-button", text: "设置使用权限"});
  button.disabled = !account.id;
  button.addEventListener("click", () => runButton(button, () => openUpstreamAccess(account), "加载用户…"));
  return element("section", {className: "upstream-access"},
    element("div", {}, element("strong", {text: exclusive ? `专属 · ${(account.authorized_user_ids || []).length} 人` : "共享 · 所有用户可用"}),
      element("small", {text: exclusive ? "仅授权用户可参与此账号分配" : "所有用户均可参与此账号分配"})), button);
}

async function openUpstreamAccess(account) {
  if (loggingOut || state?.user?.role !== "owner" || upstreamAccountOperation || !upstreamAccountSyncHealthy) return;
  const sequence = ++upstreamAccessSequence;
  const generation = identityGeneration;
  const identityCurrent = groupIdentityCurrent();
  const current = () => identityCurrent() && sequence === upstreamAccessSequence;
  let result;
  try { result = await api("/admin/billing/users", undefined, current); }
  catch (error) { if (!current()) return; throw error; }
  if (sequence !== upstreamAccessSequence || generation !== identityGeneration || loggingOut) return;
  if (!Array.isArray(result?.users)) throw new Error("用户列表响应格式无效。");
  upstreamAccessUsers = result.users;
  upstreamAccessAccount = account;
  upstreamAccessSelected = new Set(account.authorized_user_ids || []);
  const form = byId("upstream-access-form");
  form.reset();
  form.elements.mode.value = account.access_mode || "shared";
  byId("upstream-access-account").textContent = account.email_masked || account.id;
  renderUpstreamAccessUsers();
  openDialog("upstream-access-dialog");
}

function renderUpstreamAccessUsers() {
  const exclusive = byId("upstream-access-form").elements.mode.value === "exclusive";
  byId("upstream-access-users").classList.toggle("hidden", !exclusive);
  const rows = matchingManagedUsers(byId("upstream-access-search").value, upstreamAccessUsers).map((user) => {
    const checkbox = element("input", {type: "checkbox", attributes: {"aria-label": `授权用户 ${user.username || user.id}`}});
    checkbox.checked = upstreamAccessSelected.has(user.id);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) upstreamAccessSelected.add(user.id); else upstreamAccessSelected.delete(user.id);
      byId("upstream-access-selected").textContent = `已选 ${upstreamAccessSelected.size} 人`;
    });
    return element("label", {className: "user-check-row"}, checkbox,
      element("span", {}, element("strong", {text: user.display_name || user.username}), element("small", {text: user.username})));
  });
  byId("upstream-access-user-list").replaceChildren(...(rows.length ? rows : [emptyState("没有匹配的用户。") ]));
  byId("upstream-access-selected").textContent = `已选 ${upstreamAccessSelected.size} 人`;
}

async function saveUpstreamAccess(event) {
  const form = event.currentTarget;
  const account = upstreamAccessAccount;
  if (!account || upstreamAccountOperation || loggingOut || state?.user?.role !== "owner") return;
  const mode = form.elements.mode.value;
  const ids = mode === "exclusive" ? [...upstreamAccessSelected].sort() : [];
  if (mode === "exclusive" && !ids.length) throw new Error("专属账号至少需要选择一位用户。");
  const payload = {mode, user_ids: ids, reason: billingReason(form)};
  const generation = identityGeneration;
  const operation = {id: account.id, kind: "access"};
  const current = () => generation === identityGeneration && !loggingOut && state?.user?.role === "owner";
  upstreamAccountOperation = operation;
  syncUpstreamAccountControls();
  let saved = false;
  try {
    await sensitiveAction(() => {
      if (!current()) throw new Error("登录身份已变化，操作已停止。");
      return api(`/admin/upstream-accounts/${encodeURIComponent(account.id)}/access`, {method: "PUT", body: JSON.stringify(payload)}, current);
    }, current);
    if (!current()) return;
    saved = true;
    account.access_mode = mode;
    account.authorized_user_ids = ids;
    byId("upstream-access-dialog").close();
    setUpstreamAccountMessage("upstream-account-action-message", "账号使用权限已保存；下一次请求使用新权限。", "ok");
    await loadUpstreamAccounts(upstreamAccountQueryFromForm(), {afterOperation: true});
  } catch (error) {
    if (!current()) return;
    if (!saved) throw error;
    upstreamAccountSyncHealthy = false;
    setUpstreamAccountMessage("upstream-account-refresh-message", `权限已保存，但列表刷新失败：${friendlyError(error)}，请重新应用筛选。`);
  } finally {
    if (upstreamAccountOperation === operation) {
      upstreamAccountOperation = null;
      syncUpstreamAccountControls();
    }
  }
}

function bindGroupUI() {
  byId("group-create").addEventListener("click", () => openGroupEditor());
  byId("group-edit").addEventListener("click", () => openGroupEditor(managedGroup));
  byId("group-form").elements.period.addEventListener("change", syncGroupPeriodInput);
  byId("group-member-search").addEventListener("input", renderGroupMembers);
  byId("group-select-visible").addEventListener("click", () => {
    matchingManagedUsers(byId("group-member-search").value).filter(groupUserAvailable).forEach((user) => groupSelectedUsers.add(user.id));
    renderGroupMembers();
  });
  byId("group-clear-selection").addEventListener("click", () => { groupSelectedUsers.clear(); renderGroupMembers(); });
  byId("group-members-form").addEventListener("submit", (event) => event.preventDefault());
  bindAsync("groups-refresh", "click", async () => { await loadGroups(); groupMessage("群组用量已刷新。"); }, "刷新中…", groupIdentityCurrent);
  bindAsync("group-form", "submit", saveGroup, "保存中…", groupIdentityCurrent);
  bindAsync("group-members-add", "click", () => changeGroupMembers("add"), "处理中…", groupIdentityCurrent);
  bindAsync("group-members-remove", "click", () => changeGroupMembers("remove"), "处理中…", groupIdentityCurrent);
  bindAsync("group-archive", "click", archiveSelectedGroup, "处理中…", groupIdentityCurrent);
  byId("upstream-access-form").elements.mode.addEventListener("change", renderUpstreamAccessUsers);
  byId("upstream-access-search").addEventListener("input", renderUpstreamAccessUsers);
  byId("upstream-access-select-visible").addEventListener("click", () => {
    matchingManagedUsers(byId("upstream-access-search").value, upstreamAccessUsers).forEach((user) => upstreamAccessSelected.add(user.id));
    renderUpstreamAccessUsers();
  });
  byId("upstream-access-clear").addEventListener("click", () => { upstreamAccessSelected.clear(); renderUpstreamAccessUsers(); });
  bindAsync("upstream-access-form", "submit", saveUpstreamAccess, "保存中…", groupIdentityCurrent);
}

function upstreamAccountStat(label, value, detail = "") {
  const children = [element("span", {text: label}), element("strong", {text: value})];
  if (detail) children.push(element("small", {text: detail}));
  return element("div", {className: "upstream-account-stat"}, ...children);
}

function upstreamAccountStats(account) {
  const inputTokens = Number(account?.input_tokens || 0);
  const outputTokens = Number(account?.output_tokens || 0);
  const tokens = Number.isFinite(inputTokens + outputTokens) ? formatInteger(inputTokens + outputTokens) : "—";
  return element("div", {className: "upstream-account-stats"},
    upstreamAccountStat("请求", formatInteger(account?.request_count)),
    upstreamAccountStat("Token", tokens, "输入 + 输出"),
    upstreamAccountStat("错误", formatInteger(account?.error_count)),
    upstreamAccountStat("输入 Token", formatInteger(account?.input_tokens)),
    upstreamAccountStat("缓存输入", formatInteger(account?.cached_input_tokens), "输入 Token 的子集"),
    upstreamAccountStat("缓存写入", formatInteger(account?.cache_write_tokens)),
    upstreamAccountStat("输出 Token", formatInteger(account?.output_tokens)),
    upstreamAccountStat("推理 Token", formatInteger(account?.reasoning_tokens), "输出 Token 的子集"),
    upstreamAccountStat("API 等价成本", formatMoney(account?.equivalent_cost_usd, "USD"), "本地不可变 Ledger"),
  );
}

function formatUpstreamQuotaDuration(value) {
  if (!Number.isSafeInteger(value) || value <= 0) return "上游未返回";
  if (value % (24 * 60) === 0) return `${value / (24 * 60)} 天`;
  if (value % 60 === 0) return `${value / 60} 小时`;
  return `${value} 分钟`;
}

function formatUpstreamQuotaReset(value) {
  if (!Number.isSafeInteger(value) || value <= 0) return "上游未返回";
  return formatDateTime(new Date(value * 1000), "上游未返回");
}

function upstreamQuotaWindow(bucketLabel, windowLabel, quotaWindow) {
  const value = quotaWindow && typeof quotaWindow === "object" ? quotaWindow : {};
  const usedPercent = Number.isInteger(value.usedPercent) && value.usedPercent >= 0 && value.usedPercent <= 100
    ? value.usedPercent
    : null;
  const used = usedPercent == null ? "上游未返回" : `${usedPercent}%`;
  const remaining = usedPercent == null ? "上游未返回" : `${100 - usedPercent}%`;
  return element("article", {className: "upstream-quota-window"},
    element("strong", {text: `${bucketLabel} · ${windowLabel} · ${formatUpstreamQuotaDuration(value.windowDurationMins)}`}),
    element("span", {text: `已用 ${used} · 剩余 ${remaining}`}),
    element("small", {text: `重置：${formatUpstreamQuotaReset(value.resetsAt)}`}),
  );
}

function upstreamQuotaReached(bucketLabel) {
  return element("article", {className: "upstream-quota-window"},
    element("strong", {text: `${bucketLabel} · 状态`}),
    element("span", {text: "已达上游限额"}),
    element("small", {text: "额度窗口重置后可重新查询；已锁定的账号仍需手动重新启用。"}),
  );
}

function upstreamQuotaWindows(result) {
  const byLimitID = result?.rateLimitsByLimitId;
  const entries = byLimitID && typeof byLimitID === "object" && !Array.isArray(byLimitID)
    ? Object.entries(byLimitID)
    : [];
  const buckets = entries.length > 0 ? entries : [[result?.rateLimits?.limitId || "默认", result?.rateLimits]];
  const windows = [];
  for (const [limitID, quota] of buckets) {
    const bucket = quota && typeof quota === "object" ? quota : {};
    const bucketLabel = `额度桶 ${String(limitID)}`;
    windows.push(upstreamQuotaWindow(bucketLabel, "主窗口", bucket.primary));
    windows.push(upstreamQuotaWindow(bucketLabel, "次窗口", bucket.secondary));
    if (bucket.rateLimitReachedType != null && String(bucket.rateLimitReachedType).trim() !== "") {
      windows.push(upstreamQuotaReached(bucketLabel));
    }
  }
  return windows;
}

function markUpstreamQuotaStale(container) {
  if (!container?.isConnected || container.dataset.state !== "fresh") return;
  container.dataset.state = "stale";
  const freshness = container.querySelector(".upstream-quota-freshness");
  if (freshness) {
    freshness.dataset.status = "stale";
    freshness.textContent = "可能已过时";
  }
  show(container.querySelector(".upstream-quota-stale-note"));
}

function scheduleUpstreamQuotaStale(accountID, container, receivedAt) {
  clearUpstreamQuotaTimer(accountID);
  const received = new Date(receivedAt || "");
  if (Number.isNaN(received.getTime())) {
    markUpstreamQuotaStale(container);
    return;
  }
  const delay = received.getTime() + upstreamQuotaStaleAfterMS - Date.now();
  if (delay <= 0) {
    markUpstreamQuotaStale(container);
    return;
  }
  const key = String(accountID || "");
  const timer = window.setTimeout(() => {
    upstreamQuotaStaleTimers.delete(key);
    markUpstreamQuotaStale(container);
  }, delay);
  upstreamQuotaStaleTimers.set(key, timer);
}

function renderUpstreamQuota(account, container, result, receivedAt) {
  const freshness = statusBadge("ok");
  freshness.classList.add("upstream-quota-freshness");
  freshness.textContent = "刚刚查询";
  const windows = upstreamQuotaWindows(result);
  container.dataset.state = "fresh";
  container.dataset.queriedAt = receivedAt.toISOString();
  container.setAttribute("aria-busy", "false");
  container.replaceChildren(
    element("div", {className: "upstream-quota-result-head"},
      element("span", {text: `查询于 ${formatDateTime(receivedAt, "未知时间")} · ${String(account.plan || "套餐未知")}`}),
      freshness,
    ),
    element("div", {className: "upstream-quota-windows"}, ...windows),
    element("p", {className: "upstream-quota-stale-note hidden", text: "此结果已超过 5 分钟，可能不再反映当前额度。请重新查询。"}),
  );
  scheduleUpstreamQuotaStale(account.id, container, receivedAt);
}

async function loadUpstreamQuota(account, quotaBlock, container) {
  setLocalMessage(quotaBlock);
  clearUpstreamQuotaTimer(account.id);
  container.dataset.state = "loading";
  container.setAttribute("aria-busy", "true");
  container.replaceChildren(element("p", {className: "upstream-quota-state", text: "正在实时查询官方额度…"}));
  try {
    const response = await api(`/admin/upstream-accounts/${encodeURIComponent(account.id)}/quota`, {method: "POST", body: upstreamQuotaRequestBody});
    const receivedAt = new Date();
    if (response?.id !== 6 || !response.result || typeof response.result !== "object" || Array.isArray(response.result)) {
      throw new Error("官方额度响应格式异常，请稍后重试。");
    }
    if (!container.isConnected) return;
    renderUpstreamQuota(account, container, response.result, receivedAt);
    announce(`已查询 ${account.email_masked || "该上游账号"} 的官方额度。`);
  } catch (error) {
    if (!container.isConnected) return;
    container.dataset.state = "error";
    container.setAttribute("aria-busy", "false");
    container.replaceChildren(element("p", {className: "upstream-quota-state", text: "官方额度查询失败；本地统计仍可正常使用。"}));
    throw error;
  }
}

function setUpstreamAccountMessage(id, message = "", kind = "error") {
  const target = byId(id);
  target.textContent = message;
  target.dataset.kind = kind;
  target.setAttribute("role", kind === "error" ? "alert" : "status");
  target.classList.toggle("hidden", !message);
}

function syncUpstreamAccountControls() {
  for (const card of all(".upstream-account-card[data-account-id]")) {
    const account = upstreamAccounts.find((item) => item.id === card.dataset.accountId);
    const accessButton = card.querySelector(".upstream-access-button");
    if (accessButton) accessButton.disabled = !account?.id || !upstreamAccountSyncHealthy || upstreamAccountListLoading || Boolean(upstreamAccountOperation) || loggingOut || state?.user?.role !== "owner";
    const button = card.querySelector(".upstream-account-status-button");
    const badge = card.querySelector(".upstream-account-status");
    const cliproxyBadge = card.querySelector(".upstream-account-cliproxy-status");
    const manualBadge = card.querySelector(".upstream-account-manual-status");
    const quotaBadge = card.querySelector(".upstream-account-quota-status");
    const note = card.querySelector(".upstream-account-manage-note");
    if (!account || !button) continue;
    const knownStatus = upstreamStatusKnown(account) && ["available", "unavailable"].includes(account.status);
    const canManage = Boolean(account.id) && account.can_manage === true && knownStatus && upstreamAccountSyncHealthy;
    button.disabled = !canManage || upstreamAccountListLoading || Boolean(upstreamAccountOperation) ||
      loggingOut || state?.user?.role !== "owner";
    const pending = upstreamAccountOperation?.id === account.id && upstreamAccountOperation.kind === "status";
    const gatewayBlocked = account.gateway_manual_status === "manual_disabled" || account.gateway_quota_status === "quota_exhausted";
    button.textContent = pending ? (upstreamAccountOperation.enabled ? "启用中…" : "禁用中…") :
      (gatewayBlocked ? "重新启用" : "禁用");
    button.setAttribute("aria-busy", String(pending));
    button.title = canManage ? "" : "账号未在最近一次同步中确认，刷新列表后再试。";
    const finalStatus = upstreamFinalStatus(account);
    badge.dataset.status = finalStatus;
    badge.textContent = `最终分流：${upstreamFinalStatusLabels[finalStatus] || upstreamFinalStatusLabels.unknown}`;
    if (cliproxyBadge) {
      const value = upstreamCliproxyStatusLabels[account.cliproxy_status] ? account.cliproxy_status : "unknown";
      cliproxyBadge.dataset.status = value;
      cliproxyBadge.textContent = `CLIProxyAPI：${upstreamCliproxyStatusLabels[value]}`;
    }
    if (manualBadge) {
      const value = upstreamGatewayManualStatusLabels[account.gateway_manual_status] ? account.gateway_manual_status : "unknown";
      manualBadge.dataset.status = value;
      manualBadge.textContent = `Gateway手动：${upstreamGatewayManualStatusLabels[value]}`;
    }
    if (quotaBadge) {
      const value = upstreamGatewayQuotaStatusLabels[account.gateway_quota_status] ? account.gateway_quota_status : "unknown";
      quotaBadge.dataset.status = value;
      quotaBadge.textContent = `Gateway额度：${upstreamGatewayQuotaStatusLabels[value]}`;
    }
    note.textContent = canManage ? "" : "账号未在最近一次同步中确认，暂不可操作。";
    note.classList.toggle("hidden", canManage);
    const weightInput = card.querySelector(".upstream-allocation-input");
    const weightButton = card.querySelector(".upstream-allocation-save");
    const allocationState = card.querySelector(".upstream-allocation-state");
    const savingWeight = upstreamAccountOperation?.id === account.id && upstreamAccountOperation.kind === "weight";
    weightInput.disabled = button.disabled;
    weightButton.disabled = button.disabled;
    weightButton.textContent = savingWeight ? "保存中…" : "保存系数";
    weightButton.setAttribute("aria-busy", String(savingWeight));
    allocationState.dataset.draining = String(account.allocation_weight === 0);
    allocationState.textContent = account.allocation_weight === 0 ? "停止接收新对话 · 已有有效绑定继续使用" :
      (finalStatus === "available" ? "参与新对话分配" : finalStatus === "unknown" ? "状态未知 · 暂停新对话分配" : "账号不可用 · 暂不参与新对话分配");
    const limitInput = card.querySelector(".upstream-concurrency-limit-input");
    const limitButton = card.querySelector(".upstream-concurrency-limit-save");
    const savingLimit = upstreamAccountOperation?.id === account.id && upstreamAccountOperation.kind === "limit";
    if (limitInput && limitButton) {
      limitInput.disabled = weightInput.disabled;
      limitButton.disabled = weightInput.disabled;
      limitButton.textContent = savingLimit ? "保存中…" : "保存并发上限";
      limitButton.setAttribute("aria-busy", String(savingLimit));
    }
  }
  const filter = byId("upstream-account-filter");
  filter.querySelector("button[type=submit]").disabled = Boolean(upstreamAccountOperation) || filter.dataset.busy === "true";
}

async function saveUpstreamConcurrentLimit(account, form) {
  if (upstreamAccountOperation || upstreamAccountListLoading || loggingOut || state?.user?.role !== "owner" ||
      !upstreamAccountSyncHealthy || account.can_manage !== true || !account.id || !upstreamAccounts.includes(account) ||
      !upstreamStatusKnown(account) || !["available", "unavailable"].includes(account.status)) return;
  const input = form.querySelector(".upstream-concurrency-limit-input");
  const raw = String(input.value || "").trim();
  const limit = Number(raw);
  if (!/^\d+$/.test(raw) || !Number.isSafeInteger(limit) || limit < 1 || limit > 2147483647) {
    input.setAttribute("aria-invalid", "true");
    setLocalMessage(form, "请输入 1 至 2147483647 的整数。");
    return;
  }
  input.setAttribute("aria-invalid", "false");
  setLocalMessage(form);
  const operation = {id: account.id, kind: "limit", limit};
  upstreamAccountOperation = operation;
  upstreamAccountRequestSequence++;
  setUpstreamAccountMessage("upstream-account-action-message");
  setUpstreamAccountMessage("upstream-account-refresh-message");
  syncUpstreamAccountControls();
  let confirmed = false;
  try {
    const response = await sensitiveAction(() => {
      if (upstreamAccountOperation !== operation || loggingOut || state?.user?.role !== "owner") {
        throw new DOMException("操作已取消。", "AbortError");
      }
      return api(`/admin/upstream-accounts/${encodeURIComponent(account.id)}/concurrent-limit`, {
        method: "PUT", body: JSON.stringify({concurrent_limit: operation.limit}),
      });
    });
    if (upstreamAccountOperation !== operation || loggingOut || state?.user?.role !== "owner") return;
    if (response?.id !== account.id || response.concurrent_limit !== operation.limit) throw new Error("并发上限响应格式异常，请刷新列表确认结果。");
    confirmed = true;
    account.concurrent_limit = response.concurrent_limit;
    setUpstreamAccountMessage("upstream-account-action-message", `${account.email_masked || "该上游账号"} 并发对话上限已保存为 ${operation.limit}。`, "ok");
    announce(`已保存 ${account.email_masked || "该上游账号"} 的并发对话上限。`);
    await loadUpstreamAccounts(upstreamAccountQueryFromForm(), {afterOperation: true});
  } catch (error) {
    if (upstreamAccountOperation !== operation || loggingOut || state?.user?.role !== "owner") return;
    upstreamAccountSyncHealthy = false;
    setUpstreamAccountMessage(confirmed ? "upstream-account-refresh-message" : "upstream-account-action-message",
      `${confirmed ? "操作已成功，但列表刷新失败" : "并发上限保存未确认"}：${friendlyError(error)} 请刷新列表后再试。`);
  } finally {
    if (upstreamAccountOperation === operation) { upstreamAccountOperation = null; syncUpstreamAccountControls(); }
  }
}

async function changeUpstreamAccountStatus(account) {
  if (upstreamAccountOperation || upstreamAccountListLoading || loggingOut || state?.user?.role !== "owner" ||
      !upstreamAccountSyncHealthy || account.can_manage !== true || !account.id ||
      !upstreamAccounts.includes(account) || !upstreamStatusKnown(account) || !["available", "unavailable"].includes(account.status)) return;
  const operation = {id: account.id, kind: "status", enabled: account.gateway_manual_status === "manual_disabled" || account.gateway_quota_status === "quota_exhausted"};
  upstreamAccountOperation = operation;
  upstreamAccountRequestSequence++;
  setUpstreamAccountMessage("upstream-account-action-message");
  setUpstreamAccountMessage("upstream-account-refresh-message");
  syncUpstreamAccountControls();
  let confirmed = false;
  try {
    const response = await sensitiveAction(() => {
      if (upstreamAccountOperation !== operation || loggingOut || state?.user?.role !== "owner") {
        throw new DOMException("操作已取消。", "AbortError");
      }
      return api(`/admin/upstream-accounts/${encodeURIComponent(account.id)}/status`, {
        method: "PUT", body: JSON.stringify({enabled: operation.enabled}),
      });
    });
    if (upstreamAccountOperation !== operation || loggingOut || state?.user?.role !== "owner") return;
    const responseKnown = upstreamStatusKnown(response) && ["available", "unavailable"].includes(response.status);
    if (response?.id !== account.id || !responseKnown) {
      throw new Error("上游账号状态响应格式异常，请刷新列表确认结果。");
    }
    confirmed = true;
    account.status = response.status;
    account.cliproxy_status = response.cliproxy_status;
    account.gateway_manual_status = response.gateway_manual_status;
    account.gateway_quota_status = response.gateway_quota_status;
    syncUpstreamAccountControls();
    const enabledMessage = account.allocation_weight === 0 ? "系数仍为 0，停止接收新对话；已有有效绑定继续使用。" : "已恢复参与分流；若额度仍不足，将再次锁定。";
    const message = `${account.email_masked || "该上游账号"} 已${operation.enabled ? "重新启用" : "禁用"}。${operation.enabled ? enabledMessage : "后续请求将不再分配到此账号，已开始的请求继续执行。"}`;
    setUpstreamAccountMessage("upstream-account-action-message", message, "ok");
    announce(message);
    await loadUpstreamAccounts(upstreamAccountQueryFromForm(), {afterOperation: true});
  } catch (error) {
    if (upstreamAccountOperation !== operation || loggingOut || state?.user?.role !== "owner") return;
    upstreamAccountSyncHealthy = false;
    if (confirmed) {
      setUpstreamAccountMessage("upstream-account-refresh-message", `操作已成功，但列表与统计刷新失败：${friendlyError(error)} 请重新应用筛选刷新。`);
    } else {
      setUpstreamAccountMessage("upstream-account-action-message", `账号状态操作未确认：${friendlyError(error)} 请刷新列表后再试。`);
    }
  } finally {
    if (upstreamAccountOperation === operation) {
      upstreamAccountOperation = null;
      syncUpstreamAccountControls();
    }
  }
}

async function saveUpstreamAllocationWeight(account, form) {
  if (upstreamAccountOperation || upstreamAccountListLoading || loggingOut || state?.user?.role !== "owner" ||
      !upstreamAccountSyncHealthy || account.can_manage !== true || !account.id ||
      !upstreamAccounts.includes(account) || !upstreamStatusKnown(account) || !["available", "unavailable"].includes(account.status)) return;
  const input = form.querySelector(".upstream-allocation-input");
  const raw = String(input.value || "").trim();
  const weight = Number(raw);
  if (!/^\d+$/.test(raw) || !Number.isInteger(weight) || weight > 2147483647) {
    input.setAttribute("aria-invalid", "true");
    setLocalMessage(form, "请输入 0 至 2147483647 的整数；0 表示停止接收新对话。");
    return;
  }
  input.setAttribute("aria-invalid", "false");
  setLocalMessage(form);
  const operation = {id: account.id, kind: "weight", weight};
  upstreamAccountOperation = operation;
  upstreamAccountRequestSequence++;
  setUpstreamAccountMessage("upstream-account-action-message");
  setUpstreamAccountMessage("upstream-account-refresh-message");
  syncUpstreamAccountControls();
  let confirmed = false;
  try {
    const response = await sensitiveAction(() => {
      if (upstreamAccountOperation !== operation || loggingOut || state?.user?.role !== "owner") {
        throw new DOMException("操作已取消。", "AbortError");
      }
      return api(`/admin/upstream-accounts/${encodeURIComponent(account.id)}/allocation-weight`, {
        method: "PUT", body: JSON.stringify({weight: operation.weight}),
      });
    });
    if (upstreamAccountOperation !== operation || loggingOut || state?.user?.role !== "owner") return;
    if (response?.id !== account.id || response.allocation_weight !== operation.weight) {
      throw new Error("分配系数响应格式异常，请刷新列表确认结果。");
    }
    confirmed = true;
    account.allocation_weight = response.allocation_weight;
    syncUpstreamAccountControls();
    const message = `${account.email_masked || "该上游账号"} 分配系数已保存为 ${operation.weight}。${operation.weight === 0 ? "停止接收新对话，已有有效绑定继续使用。" : "后续新分配将按近 24 小时费用逐步调整占比。"}`;
    setUpstreamAccountMessage("upstream-account-action-message", message, "ok");
    announce(message);
    await loadUpstreamAccounts(upstreamAccountQueryFromForm(), {afterOperation: true});
  } catch (error) {
    if (upstreamAccountOperation !== operation || loggingOut || state?.user?.role !== "owner") return;
    upstreamAccountSyncHealthy = false;
    if (confirmed) {
      setUpstreamAccountMessage("upstream-account-refresh-message", `操作已成功，但列表与统计刷新失败：${friendlyError(error)} 请重新应用筛选刷新。`);
    } else {
      setUpstreamAccountMessage("upstream-account-action-message", `分配系数保存未确认：${friendlyError(error)} 请刷新列表后再试。`);
    }
  } finally {
    if (upstreamAccountOperation === operation) {
      upstreamAccountOperation = null;
      syncUpstreamAccountControls();
    }
  }
}

function upstreamAllocationBlock(account) {
  const input = element("input", {
    type: "text", className: "upstream-allocation-input",
    attributes: {inputmode: "numeric", pattern: "[0-9]+", maxlength: "10", required: "",
      "aria-describedby": "upstream-allocation-help", autocomplete: "off"},
  });
  input.value = String(account.allocation_weight ?? 1);
  const button = element("button", {type: "submit", className: "secondary upstream-allocation-save", text: "保存系数"});
  const form = element("form", {className: "upstream-allocation-form", attributes: {novalidate: ""}},
    element("label", {}, element("span", {text: "分配系数"}), input), button,
    element("p", {className: "form-message hidden", attributes: {role: "alert"}}),
  );
  form.addEventListener("submit", (event) => { event.preventDefault(); saveUpstreamAllocationWeight(account, form); });
  const limitInput = element("input", {
    type: "text", className: "upstream-concurrency-limit-input",
    attributes: {inputmode: "numeric", pattern: "[0-9]+", maxlength: "10", required: "",
      "aria-describedby": "upstream-concurrency-limit-help", autocomplete: "off"},
  });
  limitInput.value = String(account.concurrent_limit ?? 1);
  const limitButton = element("button", {type: "submit", className: "secondary upstream-concurrency-limit-save", text: "保存并发上限"});
  const limitForm = element("form", {className: "upstream-concurrency-limit-form", attributes: {novalidate: ""}},
    element("label", {}, element("span", {text: "并发对话数量"}), limitInput), limitButton,
    element("p", {className: "form-message hidden", attributes: {role: "alert"}}),
  );
  limitForm.addEventListener("submit", (event) => { event.preventDefault(); saveUpstreamConcurrentLimit(account, limitForm); });
  return element("section", {className: "upstream-allocation"},
    form,
    limitForm,
    element("p", {className: "upstream-allocation-state", attributes: {"aria-live": "polite"}}),
    element("div", {className: "upstream-allocation-stats"},
      upstreamAccountStat("近 24 小时费用", formatUSD(account.rolling_cost_usd), "已结算 · 跨用户与模型"),
      upstreamAccountStat("近 24 小时费用占比", account.rolling_cost_share == null ? "—" : formatPercent(account.rolling_cost_share), "所有已归因账号"),
      upstreamAccountStat("参考目标占比", account.target_share == null ? "—" : formatPercent(account.target_share), "按当前启用账号系数"),
    ),
  );
}

function ownerSectionVisible(section) {
  return !loggingOut && state?.user?.role === "owner" && document.visibilityState !== "hidden" && location.hash === `#${section}`;
}

function monitoringIdentityCurrent(generation = identityGeneration, sequence = monitoringRequestSequence) {
  const actorUserID = state?.user?.id;
  return () => generation === identityGeneration && sequence === monitoringRequestSequence &&
    !loggingOut && state?.user?.role === "owner" && state?.user?.id === actorUserID &&
    ownerSectionVisible("monitoring");
}

function monitoringField(request, ...names) {
  return field(request, ...names);
}

function monitoringUser(request) {
  return {
    id: String(monitoringField(request, "user_id", "UserID") || ""),
    username: monitoringField(request, "username", "Username") || "",
    display_name: monitoringField(request, "display_name", "DisplayName") || "",
  };
}

function monitoringUserLink(request) {
  const user = monitoringUser(request);
  const label = user.display_name || user.username || user.id || "未知用户";
  if (!user.id && !user.username) return element("span", {text: label});
  const link = element("a", {className: "monitoring-user-link", text: label, attributes: {
    href: "#usage", "data-user-id": user.id, "aria-label": `查看用户 ${label} 的使用统计`,
  }});
  const details = [user.username && user.username !== label ? user.username : "", user.id && user.id !== user.username ? user.id : ""]
    .filter(Boolean).join(" · ");
  if (details) link.append(element("small", {text: details}));
  link.addEventListener("click", (event) => {
    event.preventDefault();
    runButton(link, () => drillDownUser(user), "加载中…");
  });
  return link;
}

function monitoringConversationCell(request) {
  const hash = monitoringField(request, "conversation_hash", "ConversationHash");
  return element("span", {className: hash ? "monitoring-conversation" : "monitoring-unattributed", text: hash || "未识别"});
}

function monitoringUpstreamCell(request, active = false) {
  const id = monitoringField(request, "upstream_account_id", "UpstreamAccountID");
  const masked = monitoringField(request, "upstream_masked_email", "UpstreamMaskedEmail", "email_masked");
  if (!id && !masked) return element("span", {className: active ? "monitoring-unassigned" : "monitoring-unattributed", text: active ? "分配中" : "未归因"});
  const host = element("span", {className: "monitoring-upstream"});
  if (masked) host.append(element("strong", {text: masked}));
  if (id) host.append(element("small", {text: String(id)}));
  return host;
}

function monitoringStatusCell(request) {
  const stateValue = monitoringField(request, "state", "State") || "unknown";
  const cell = element("td", {}, statusBadge(stateValue));
  const status = monitoringField(request, "http_status", "HTTPStatus");
  const code = monitoringField(request, "error_code", "ErrorCode");
  const details = [status != null && status !== "" ? `HTTP ${status}` : "", code || ""].filter(Boolean).join(" · ");
  if (details) cell.append(element("small", {text: details}));
  return cell;
}

function monitoringRow(request, windowName) {
  const active = monitoringField(request, "state", "State") === "in_progress";
  const timestamp = active || windowName === "recent"
    ? monitoringField(request, "requested_at", "RequestedAt")
    : monitoringField(request, "completed_at", "CompletedAt");
  return element("tr", {dataset: {requestId: monitoringField(request, "request_id", "RequestID") || ""}},
    element("td", {text: formatDateTime(timestamp, "—")}),
    element("td", {}, monitoringUserLink(request)),
    element("td", {}, monitoringConversationCell(request)),
    element("td", {}, usageModelCell(request, monitoringField(request, "state", "State"))),
    element("td", {}, monitoringUpstreamCell(request, active)),
    monitoringStatusCell(request),
  );
}

function renderMonitoring(result) {
  monitoringSnapshot = result || {};
  const windows = {
    recent: Array.isArray(result?.recent) ? result.recent : [],
    failures: Array.isArray(result?.failures) ? result.failures : [],
  };
  for (const [name, rows] of Object.entries(windows)) {
    const tbody = byId(`monitoring-${name}-rows`);
    if (!tbody) continue;
    tbody.closest("table")?.setAttribute("aria-busy", "false");
    tbody.replaceChildren(...(rows.length ? rows.map((request) => monitoringRow(request, name)) : [tableMessage(6, "当前窗口没有请求。" )]));
    byId(`monitoring-${name}-count`).textContent = formatInteger(rows.length);
  }
  const sampled = result?.sampled_at;
  byId("monitoring-sampled-at").textContent = sampled ? `最近刷新：${formatDateTime(sampled, "—")} · 每 5 秒刷新` : "最近刷新：—";
  hide("monitoring-loading");
  const message = byId("monitoring-message");
  message.textContent = "";
  message.classList.add("hidden");
}

function resetMonitoring(message = "登录后加载请求监控。") {
  monitoringRequestSequence++;
  monitoringPolling = false;
  if (typeof window !== "undefined" && typeof window.clearTimeout === "function") window.clearTimeout(monitoringTimer);
  monitoringTimer = 0;
  monitoringController?.abort();
  monitoringController = null;
  monitoringSnapshot = null;
  for (const name of ["recent", "failures"]) {
    const rows = byId(`monitoring-${name}-rows`);
    if (rows) rows.replaceChildren(tableMessage(6, message));
    const count = byId(`monitoring-${name}-count`);
    if (count) count.textContent = "—";
  }
  if (byId("monitoring-sampled-at")) byId("monitoring-sampled-at").textContent = "尚未采样";
  hide("monitoring-loading");
  const status = byId("monitoring-message");
  if (status) { status.textContent = ""; status.classList.add("hidden"); }
}

async function loadMonitoring({manual = false} = {}) {
  if (loggingOut || state?.user?.role !== "owner") return;
  if (!manual && !ownerSectionVisible("monitoring")) return;
  const sequence = ++monitoringRequestSequence;
  const generation = identityGeneration;
  const current = monitoringIdentityCurrent(generation, sequence);
  if (manual || !monitoringSnapshot) show("monitoring-loading");
  for (const name of ["recent", "failures"]) {
    byId(`monitoring-${name}-rows`)?.closest("table")?.setAttribute("aria-busy", "true");
  }
  try {
    const result = await api("/admin/monitoring", {signal: monitoringController?.signal}, current);
    if (!current()) return;
    if (!result || typeof result !== "object") throw new Error("请求监控响应格式异常。");
    renderMonitoring(result);
  } catch (error) {
    if (!current() || error?.name === "AbortError" || error?.code === "stale_request") return;
    const message = byId("monitoring-message");
    message.textContent = `请求监控加载失败：${friendlyError(error)}`;
    message.dataset.kind = "error";
    message.classList.remove("hidden");
    hide("monitoring-loading");
    for (const name of ["recent", "failures"]) {
      byId(`monitoring-${name}-rows`)?.replaceChildren(tableMessage(6, friendlyError(error)));
    }
    if (manual) throw error;
  } finally {
    if (sequence === monitoringRequestSequence) hide("monitoring-loading");
  }
}

function stopMonitoring() {
  monitoringPolling = false;
  monitoringRequestSequence++;
  if (typeof window !== "undefined" && typeof window.clearTimeout === "function") window.clearTimeout(monitoringTimer);
  monitoringTimer = 0;
  monitoringController?.abort();
  monitoringController = null;
}

function startMonitoring() {
  if (!ownerSectionVisible("monitoring") || monitoringPolling) return;
  monitoringPolling = true;
  const generation = identityGeneration;
  const tick = async () => {
    if (!monitoringPolling || !ownerSectionVisible("monitoring") || generation !== identityGeneration) return;
    monitoringController = new AbortController();
    try { await loadMonitoring(); }
    catch (_) { /* renderMonitoring/loadMonitoring already exposes the error */ }
    finally {
      monitoringController = null;
      if (monitoringPolling && ownerSectionVisible("monitoring") && generation === identityGeneration) {
        monitoringTimer = typeof window !== "undefined" && typeof window.setTimeout === "function"
          ? window.setTimeout(tick, 5000) : 0;
      }
    }
  };
  tick();
}

function renderUpstreamConcurrency() {
  const snapshot = upstreamConcurrencySnapshot;
  const sampled = Date.parse(snapshot?.sampled_at || "");
  const age = Date.now() - sampled;
  const valid = Number.isFinite(sampled) && age >= -5000 && age < 15000;
  const accounts = new Map();
  if (valid && Array.isArray(snapshot.accounts)) {
    for (const account of snapshot.accounts) {
      if (account?.id && Number.isSafeInteger(account.active_requests) && account.active_requests >= 0) {
        accounts.set(account.id, account.active_requests);
      }
    }
  }
  for (const card of all(".upstream-account-card[data-account-id]")) {
    const node = card.querySelector(".upstream-concurrency-count");
    if (!node) continue;
    const count = accounts.get(card.dataset.accountId);
    node.textContent = count === undefined ? "暂不可用" : formatInteger(count);
    node.dataset.available = count === undefined ? "false" : "true";
  }
  const note = byId("upstream-concurrency-sampled");
  if (note) note.textContent = valid ? `最近采样：${formatDateTime(snapshot.sampled_at)} · 每 5 秒刷新` : "实时并发暂不可用 · 每 5 秒刷新";
}

function stopUpstreamConcurrency() {
  upstreamConcurrencyGeneration++;
  upstreamConcurrencyPolling = false;
  if (upstreamConcurrencyTimer) window.clearTimeout(upstreamConcurrencyTimer);
  if (upstreamConcurrencyStaleTimer) window.clearTimeout(upstreamConcurrencyStaleTimer);
  upstreamConcurrencyTimer = 0;
  upstreamConcurrencyStaleTimer = 0;
  upstreamConcurrencyController?.abort();
  upstreamConcurrencyController = null;
  upstreamConcurrencySnapshot = null;
  renderUpstreamConcurrency();
}

function startUpstreamConcurrency() {
  if (!ownerSectionVisible("upstream-accounts") || upstreamConcurrencyPolling) return;
  upstreamConcurrencyPolling = true;
  const generation = ++upstreamConcurrencyGeneration;
  const current = () => generation === upstreamConcurrencyGeneration && ownerSectionVisible("upstream-accounts");
  const sample = async () => {
    if (!current()) return;
    upstreamConcurrencyController = new AbortController();
    try {
      const result = await api("/admin/upstream-accounts/concurrency", {signal: upstreamConcurrencyController.signal}, current);
      if (!current()) return;
      upstreamConcurrencySnapshot = result;
      window.clearTimeout(upstreamConcurrencyStaleTimer);
      const remaining = 15000 - (Date.now() - Date.parse(result?.sampled_at || ""));
      if (Number.isFinite(remaining) && remaining > 0) {
        upstreamConcurrencyStaleTimer = window.setTimeout(() => { if (current()) renderUpstreamConcurrency(); }, remaining);
      }
    } catch (_) {
      if (!current()) return;
      upstreamConcurrencySnapshot = null;
    } finally {
      if (current()) {
        renderUpstreamConcurrency();
        upstreamConcurrencyTimer = window.setTimeout(sample, 5000);
      }
    }
  };
  sample();
}

function upstreamAccountCard(account) {
  const quotaResult = element("div", {
    className: "upstream-quota-result",
    dataset: {state: "idle"},
    attributes: {"aria-live": "polite", "aria-busy": "false"},
  }, element("p", {className: "upstream-quota-state", text: "尚未查询官方额度。"}));
  const button = element("button", {
    type: "button", className: "secondary", text: "查询官方额度",
    attributes: {"aria-describedby": "upstream-quota-warning"},
  });
  if (!account.id) {
    button.disabled = true;
    button.title = "账号缺少稳定索引，无法查询。";
  }
  const quotaBlock = element("section", {className: "upstream-quota tool-block"},
    element("div", {className: "upstream-quota-heading"},
      element("div", {}, element("h4", {text: "官方即时额度"}), element("small", {text: "每次点击都会实时查询上游"})),
      button,
    ),
    quotaResult,
    element("p", {className: "form-message hidden", attributes: {role: "alert"}}),
  );
  button.addEventListener("click", () => runButton(button, () => loadUpstreamQuota(account, quotaBlock, quotaResult), "查询中…"));
  const statusButton = element("button", {
    type: "button", className: "secondary upstream-account-status-button",
    attributes: {"aria-describedby": "upstream-account-control-help"},
  });
  statusButton.addEventListener("click", () => changeUpstreamAccountStatus(account));
  const badges = upstreamAccountStatusBadges(account);
  return element("article", {className: "panel upstream-account-card", dataset: {accountId: account.id || ""}},
    element("div", {className: "panel-heading upstream-account-heading"},
      element("div", {className: "upstream-account-identity"},
        element("h3", {text: account.email_masked || "邮箱不可用"}),
        element("p", {text: `${String(account.plan || "套餐未知")} · 最后同步 ${formatDateTime(account.last_synced_at, "从未同步")}`}),
      ),
      element("div", {className: "upstream-account-actions"}, ...badges, statusButton),
    ),
    element("div", {className: "upstream-concurrency", attributes: {"aria-live": "polite"}},
      element("span", {text: "活跃 root 对话数"}),
      element("strong", {className: "upstream-concurrency-count", text: "暂不可用"}),
      element("small", {text: "同一 root 的重叠请求共享名额"}),
    ),
    element("p", {className: "upstream-account-manage-note hidden muted"}),
    upstreamAccessBlock(account),
    upstreamAllocationBlock(account),
    element("h4", {className: "upstream-history-heading", text: "历史区间统计"}),
    upstreamAccountStats(account),
    quotaBlock,
  );
}

function unattributedAccountCard(usage) {
  const badge = statusBadge("unknown");
  badge.textContent = "仅本地统计";
  return element("article", {className: "panel upstream-account-card upstream-account-unattributed"},
    element("div", {className: "panel-heading upstream-account-heading"},
      element("div", {className: "upstream-account-identity"},
        element("h3", {text: "未归因"}),
        element("p", {text: "无法识别最终服务账号的请求，包括上线前历史数据。"}),
      ),
      badge,
    ),
    upstreamAccountStats(usage),
    element("p", {className: "upstream-unattributed-note", text: "未归因记录无法查询官方额度。"}),
  );
}

function upstreamAccountPeriod(result, query) {
  if (query.get("all") === "true") return "全部历史";
  const from = formatDate(result?.from || query.get("from"), "起始时间不限");
  const untilValue = result?.until || query.get("until");
  const untilDate = untilValue ? new Date(untilValue) : null;
  if (untilDate && !Number.isNaN(untilDate.getTime())) untilDate.setMilliseconds(untilDate.getMilliseconds() - 1);
  const until = untilDate && !Number.isNaN(untilDate.getTime()) ? formatDate(untilDate) : "结束时间不限";
  return `${from} 至 ${until}`;
}

function renderUpstreamAccounts(result, query) {
  clearUpstreamQuotaTimers();
  const accounts = (Array.isArray(result?.accounts) ? result.accounts : []).filter((account) => account && typeof account === "object");
  upstreamAccounts = accounts;
  upstreamAccountSyncHealthy = !result?.sync_warning;
  const cards = accounts.map(upstreamAccountCard);
  if (result?.unattributed && typeof result.unattributed === "object") cards.push(unattributedAccountCard(result.unattributed));
  const container = byId("upstream-account-list");
  container.setAttribute("aria-busy", "false");
  if (cards.length) container.replaceChildren(...cards);
  else container.replaceChildren(emptyState("尚未同步任何上游账号。请通过 SSH 设备登录脚本添加账号。"));
  const warning = result?.sync_warning ? " · 上游状态同步失败，当前展示最后已知的本地记录" : "";
  byId("upstream-account-period").textContent = `${formatInteger(accounts.length)} 个上游账号 · 本地统计区间：${upstreamAccountPeriod(result, query)}${warning}`;
  const allocationWindow = result?.allocation_from && result?.allocation_until ?
    `近 24 小时：${formatDateTime(result.allocation_from)} 至 ${formatDateTime(result.allocation_until)} · ` : "";
  byId("upstream-allocation-period").textContent = `${allocationWindow}独立于历史统计筛选；费用占比以所有已归因账号费用为分母。`;
  syncUpstreamAccountControls();
  renderUpstreamConcurrency();
}

function upstreamAccountQueryFromForm() {
  const form = byId("upstream-account-filter");
  const query = new URLSearchParams();
  if (form.elements.range.value === "all") {
    query.set("all", "true");
  } else {
    if (form.elements.from.value) query.set("from", localDateBoundary(form.elements.from.value));
    if (form.elements.until.value) query.set("until", localDateBoundary(form.elements.until.value, true));
  }
  return query;
}

async function loadUpstreamAccounts(query, {afterOperation = false} = {}) {
  if (loggingOut || state?.user?.role !== "owner" || (upstreamAccountOperation && !afterOperation)) return;
  const sequence = ++upstreamAccountRequestSequence;
  const container = byId("upstream-account-list");
  upstreamAccountListLoading = true;
  setUpstreamAccountMessage("upstream-account-refresh-message");
  syncUpstreamAccountControls();
  show("upstream-account-loading");
  container.setAttribute("aria-busy", "true");
  if (!upstreamAccounts.length) container.replaceChildren(emptyState("正在加载上游账号和本地统计…"));
  try {
    const result = await api(`/admin/upstream-accounts${querySuffix(query)}`);
    if (sequence !== upstreamAccountRequestSequence) return;
    if (!result || typeof result !== "object" || !Array.isArray(result.accounts)) throw new Error("上游账号响应格式异常，请稍后重试。");
    renderUpstreamAccounts(result || {}, query);
    if (result.sync_warning) {
      setUpstreamAccountMessage("upstream-account-refresh-message", "上游状态同步失败，当前展示最后已知记录；账号操作已暂停，请重新应用筛选刷新。");
    }
  } catch (error) {
    if (sequence !== upstreamAccountRequestSequence) return;
    upstreamAccountSyncHealthy = false;
    container.setAttribute("aria-busy", "false");
    if (!upstreamAccounts.length) container.replaceChildren(emptyState(`上游账号加载失败：${friendlyError(error)}`));
    setUpstreamAccountMessage("upstream-account-refresh-message", `列表与统计刷新失败：${friendlyError(error)} 当前展示最后已知记录，账号操作已暂停。`);
    throw error;
  } finally {
    if (sequence === upstreamAccountRequestSequence) {
      upstreamAccountListLoading = false;
      hide("upstream-account-loading");
      syncUpstreamAccountControls();
    }
  }
}

const informationCountLabels = {
  usage_requests: "请求明细", billing_reservations: "资金预留", quota_reservations: "额度预留",
  billing_charge_allocations: "消费分摊", billing_ledger_entries: "账务流水", billing_operations: "账务操作快照",
  billing_cash_credit_lots: "资金批次", billing_subscription_periods: "订阅周期", billing_subscriptions: "订阅历史",
  usage_daily: "日汇总", usage_monthly: "月汇总", audit_events: "审计明细", concurrency_leases: "请求并发记录",
  billing_subscription_operation_snapshots: "订阅操作快照",
};
const informationReasonLabels = {
  current_balance: "支撑当前余额", active_subscription: "有效订阅", active_subscriptions: "有效订阅",
  active_request: "请求仍在运行", in_progress: "请求仍在运行", unsettled_request: "请求尚未结算",
  pending_reservation: "待结算预留", referenced: "被保留记录引用", required_dependency: "必要关联记录",
  nonzero_balance: "余额不为零", owner: "管理员不可删除", not_member: "仅允许删除普通用户",
  request_history: "仍有请求历史", usage_history: "仍有用量明细或汇总", billing_history: "仍有账务历史",
  running_requests: "请求仍在运行", unsettled_billing: "资金预留尚未结算", unsettled_quota: "额度预留尚未结算",
  available_cash: "支撑当前可用余额", referenced_history: "被当前资金或其他保留记录引用",
  balance: "余额不为零", subscriptions: "仍有订阅记录", requests: "仍有请求历史", usage_summaries: "仍有用量汇总",
  ledger: "仍有账务流水", billing_operations: "仍有账务操作记录", cash_lots: "仍有资金批次",
  reservations: "仍有资金或额度预留", management_history: "仍有管理操作关联", not_found: "用户已不存在",
};

function informationIdentityCurrent() {
  const generation = identityGeneration;
  return () => generation === identityGeneration && !loggingOut && state?.user?.role === "owner";
}

function informationMessage(message = "", error = false) {
  const node = byId("information-message");
  if (!node) return;
  node.textContent = message;
  node.dataset.kind = error ? "error" : "ok";
  node.classList.toggle("hidden", !message);
}

function informationJobRunning() {
  return Boolean(informationJob && ["pending", "queued", "running"].includes(informationJob.status));
}

function informationCutoffText(value) {
  const date = new Date(value);
  return Number.isFinite(date.getTime()) ? date.toISOString().replace("T", " ").replace(".000Z", " UTC") : "—";
}

function resetInformation() {
  informationSequence++;
  informationOverviewSequence++;
  informationUsersSequence++;
  if (informationJobTimer) window.clearTimeout(informationJobTimer);
  informationJobTimer = 0;
  informationPreview = null;
  informationJob = null;
  informationUsers = [];
  informationSelectedUsers.clear();
  informationSelectedDetails.clear();
  informationUsersReady = false;
  informationOperation = false;
  informationLoaded = false;
  informationUsersOffset = 0;
  for (const id of ["information-preview-form", "information-users-form"]) {
    const form = byId(id);
    if (!form) continue;
    delete form.dataset.operationId;
    delete form.dataset.operationPayload;
    form.reset();
  }
  byId("information-user-rows")?.replaceChildren(tableMessage(4, "登录后加载候选用户。"));
  hide("information-preview");
  hide("information-job");
  hide("usage-cleaned-history");
  informationMessage();
  syncInformationControls();
}

function syncInformationControls() {
  const unavailable = informationOperation || loggingOut || state?.user?.role !== "owner";
  const preview = byId("information-preview-button");
  if (!preview) return;
  preview.disabled = unavailable || informationJobRunning();
  byId("information-create-job").disabled = unavailable || informationJobRunning() || !informationPreview;
  byId("information-refresh").disabled = unavailable;
  byId("information-delete-users").disabled = unavailable || !informationUsersReady || informationSelectedUsers.size === 0 || informationSelectedUsers.size > 100;
  byId("information-selected-count").textContent = `已选 ${informationSelectedUsers.size} / 100 人`;
  byId("information-select-all").disabled = unavailable || !informationUsersReady || !informationUsers.length;
  const visibleSelected = informationUsers.filter((user) => informationSelectedUsers.has(user.id)).length;
  byId("information-select-all").checked = informationUsers.length > 0 && visibleSelected === informationUsers.length;
  byId("information-select-all").indeterminate = visibleSelected > 0 && visibleSelected < informationUsers.length;
  byId("information-users-prev").disabled = unavailable || !informationUsersReady || informationUsersOffset === 0;
  byId("information-users-next").disabled = unavailable || !informationUsersReady || informationUsers.length < 100;
  all(".information-user-select").forEach((input) => { input.disabled = unavailable || !informationUsersReady; });
}

function renderInformationReport(report, target) {
  const deletions = report?.delete_counts || {};
  const retained = report?.retained_counts || {};
  const kinds = [...new Set([...Object.keys(deletions), ...Object.keys(retained)])];
  const tbody = element("tbody");
  for (const kind of kinds) tbody.append(element("tr", {},
    element("td", {text: informationCountLabels[kind] || "其他历史记录"}),
    element("td", {text: formatInteger(deletions[kind] || 0)}),
    element("td", {text: formatInteger(retained[kind] || 0)}),
  ));
  if (!kinds.length) tbody.append(tableMessage(3, "暂无统计结果。"));
  const table = element("table", {},
    element("thead", {}, element("tr", {}, ...["记录类型", "删除数量", "保留数量"].map((text) => element("th", {text})))), tbody);
  const reasons = Object.entries(report?.retained_reasons || {}).map(([reason, count]) =>
    element("li", {text: `${informationReasonLabels[reason] || reason}：${formatInteger(count)}`}));
  target.replaceChildren(element("div", {className: "table-wrap"}, table));
  if (reasons.length) target.append(element("p", {className: "muted", text: "保留原因（同一记录可能满足多个原因）"}), element("ul", {className: "information-reasons"}, ...reasons));
}

function renderInformationJob(job) {
  informationJob = job || null;
  if (!job) { hide("information-job"); syncInformationControls(); return; }
  show("information-job");
  byId("information-job-state").textContent = ({pending: "等待执行", queued: "等待执行", running: "正在分批清理", completed: "清理完成", failed: "清理失败"})[job.status] || "状态待确认";
  byId("information-job-state").dataset.status = job.status;
  byId("information-job-cutoff").textContent = `固定截止时间：${informationCutoffText(job.cutoff)} · 保留 ${formatInteger(job.retention_days)} 天`;
  byId("information-job-updated").textContent = `任务 ${job.id} · 更新于 ${formatDateTime(job.updated_at)}`;
  byId("information-job-error").textContent = job.error ? "上次分批清理未完成，服务端将自动重试。已完成的批次不会重复执行。" : "";
  byId("information-job-error").classList.toggle("hidden", !job.error);
  renderInformationReport(job.report, byId("information-job-report"));
  if (job.status === "completed") renderCleanedHistory(job.cutoff);
  syncInformationControls();
}

function scheduleInformationJob() {
  window.clearTimeout(informationJobTimer);
  if (!informationJobRunning() || !ownerSectionVisible("information")) return;
  const current = informationIdentityCurrent();
  const id = informationJob.id;
  informationJobTimer = window.setTimeout(async () => {
    if (!current() || !ownerSectionVisible("information") || informationJob?.id !== id) return;
    try {
      const job = await api(`/admin/information/jobs/${encodeURIComponent(id)}`, {}, current);
      if (!current() || informationJob?.id !== id) return;
      const finished = !["pending", "queued", "running"].includes(job.status);
      renderInformationJob(job);
      if (finished) await loadInformationUsers(0);
    } catch (error) {
      if (current()) informationMessage(`任务状态暂不可用：${friendlyError(error)}。任务仍会在服务端继续。`, true);
    } finally { if (current()) scheduleInformationJob(); }
  }, 5000);
}

async function loadInformationOverview() {
  const sequence = ++informationOverviewSequence;
  const identity = informationIdentityCurrent();
  const current = () => identity() && sequence === informationOverviewSequence;
  if (!current()) return;
  const result = await api("/admin/information", {}, current);
  if (!current()) return;
  renderInformationJob(result.active_job || result.latest_job);
  renderCleanedHistory(result.cleaned_before);
  scheduleInformationJob();
}

async function previewInformation(event) {
  const form = event.currentTarget;
  if (informationOperation || informationJobRunning() || state?.user?.role !== "owner") return;
  const raw = form.elements.retention_days.value.trim();
  if (!/^[1-9]\d*$/.test(raw) || !Number.isSafeInteger(Number(raw))) throw new Error("保留天数必须为正整数。");
  const days = Number(raw);
  const sequence = ++informationSequence;
  const identity = informationIdentityCurrent();
  const current = () => identity() && sequence === informationSequence;
  informationPreview = null;
  hide("information-preview");
  syncInformationControls();
  const report = await api("/admin/information/preview", {method: "POST", body: JSON.stringify({retention_days: days})}, current);
  if (!current()) return;
  if (!Number.isFinite(Date.parse(report?.cutoff)) || (report.retention_days != null && report.retention_days !== days)) throw new Error("清理预览响应格式异常，请重新预览。");
  informationPreview = {...report, retention_days: days};
  byId("information-preview-cutoff").textContent = `将清理 ${informationCutoffText(report.cutoff)} 之前符合条件的记录。`;
  renderInformationReport(report, byId("information-preview-report"));
  show("information-preview");
  informationMessage("预览已更新。执行时会重新检查依赖，实际数量可能变化。");
  syncInformationControls();
}

async function informationMutation(form, path, payload) {
  if (informationOperation) return null;
  const current = informationIdentityCurrent();
  if (!current()) return null;
  const fingerprint = JSON.stringify({path, payload});
  if (form.dataset.operationPayload !== fingerprint) {
    form.dataset.operationPayload = fingerprint;
    form.dataset.operationId = crypto.randomUUID();
  }
  const body = JSON.stringify({...payload, operation_id: form.dataset.operationId});
  informationOperation = true;
  syncInformationControls();
  try {
    const result = await sensitiveAction(() => api(path, {method: "POST", body}, current), current);
    if (!current()) return null;
    delete form.dataset.operationId;
    delete form.dataset.operationPayload;
    return result;
  } finally {
    if (current()) { informationOperation = false; syncInformationControls(); }
  }
}

async function createInformationJob() {
  if (!informationPreview || informationOperation || informationJobRunning() || loggingOut || state?.user?.role !== "owner") return;
  const preview = informationPreview;
  if (!window.confirm(`永久清理 ${informationCutoffText(preview.cutoff)} 之前符合条件的旧账务？\n保留 ${preview.retention_days} 天；当前余额、有效订阅及未结算请求所需记录会保留。此操作无法撤销。`)) return;
  const current = informationIdentityCurrent();
  informationOverviewSequence++;
  const job = await informationMutation(byId("information-preview-form"), "/admin/information/jobs", {
    retention_days: preview.retention_days, cutoff: preview.cutoff,
  });
  if (!job || !current()) return;
  informationPreview = null;
  hide("information-preview");
  renderInformationJob(job);
  informationMessage("清理任务已创建。任务会在服务端分批执行，离开页面或服务重启后仍会继续。");
  scheduleInformationJob();
}

function renderInformationUsers() {
  const rows = informationUsers.map((user) => {
    const input = element("input", {type: "checkbox", className: "information-user-select", value: user.id,
      attributes: {"aria-label": `选择 ${user.display_name || user.username}`}});
    input.checked = informationSelectedUsers.has(user.id);
    input.addEventListener("change", () => {
      if (input.checked && informationSelectedUsers.size < 100) {
        informationSelectedUsers.add(user.id);
        informationSelectedDetails.set(user.id, user);
      } else {
        informationSelectedUsers.delete(user.id);
        informationSelectedDetails.delete(user.id);
        input.checked = false;
      }
      syncInformationControls();
    });
    return element("tr", {}, element("td", {}, input),
      element("td", {}, element("strong", {text: user.display_name || user.username}), element("small", {text: user.username})),
      element("td", {}, statusBadge(user.status || "active")), element("td", {text: formatDateTime(user.created_at, "—")}));
  });
  byId("information-user-rows").replaceChildren(...(rows.length ? rows : [tableMessage(4, "当前没有符合条件的用户。") ]));
  byId("information-users-page").textContent = informationUsers.length ? `第 ${informationUsersOffset + 1}–${informationUsersOffset + informationUsers.length} 位候选用户` : "没有候选用户";
  syncInformationControls();
}

async function loadInformationUsers(offset = 0) {
  const sequence = ++informationUsersSequence;
  const identity = informationIdentityCurrent();
  const current = () => identity() && sequence === informationUsersSequence;
  if (!current() || informationOperation) return;
  informationUsersReady = false;
  syncInformationControls();
  setTableBusy(byId("information-user-rows"), 4, "正在重新核验候选用户…");
  try {
    const query = new URLSearchParams({q: byId("information-user-search").value.trim(), limit: "100", offset: String(offset)});
    const result = await api(`/admin/information/deletable-users?${query}`, {}, current);
    if (!current()) return;
    if (!Array.isArray(result?.users)) throw new Error("候选用户响应格式异常。");
    informationUsers = result.users;
    for (const user of informationUsers) {
      if (informationSelectedUsers.has(user.id)) informationSelectedDetails.set(user.id, user);
    }
    informationUsersOffset = offset;
    informationUsersReady = true;
    renderInformationUsers();
  } catch (error) {
    if (current()) byId("information-user-rows").replaceChildren(tableMessage(4, friendlyError(error)));
    throw error;
  } finally {
    if (current()) { byId("information-user-rows").closest("table")?.setAttribute("aria-busy", "false"); syncInformationControls(); }
  }
}

async function deleteInformationUsers() {
  if (!informationUsersReady || informationOperation || informationSelectedUsers.size === 0 || informationSelectedUsers.size > 100 || loggingOut || state?.user?.role !== "owner") return;
  const ids = [...informationSelectedUsers].sort();
  const names = ids.map((id) => {
    const user = informationSelectedDetails.get(id) || informationUsers.find((item) => item.id === id);
    return user?.display_name || user?.username || id;
  });
  if (!window.confirm(`永久删除 ${ids.length} 位无账务用户及其全部登录凭证？\n${names.slice(0, 8).join("、")}${names.length > 8 ? "等" : ""}\n删除后已有会话与 API Key 立即失效。任一用户不再符合条件时，整批取消。`)) return;
  const current = informationIdentityCurrent();
  try {
    const result = await informationMutation(byId("information-users-form"), "/admin/information/users/delete", {user_ids: ids});
    if (!result || !current()) return;
    informationSelectedUsers.clear();
    informationSelectedDetails.clear();
    informationMessage(`已永久删除 ${formatInteger(result.deleted_count)} 位用户。`);
    try {
      await loadInformationUsers(0);
    } catch (error) {
      if (current()) informationMessage(`已永久删除 ${formatInteger(result.deleted_count)} 位用户，但候选列表刷新失败：${friendlyError(error)}。请刷新后继续。`, true);
    }
  } catch (error) {
    if (!current()) return;
    const reasons = (error.blockers || []).map((blocker) => {
      const user = informationUsers.find((item) => item.id === blocker.user_id);
      return `${user?.display_name || user?.username || blocker.user_id}：${(blocker.reasons || []).map((reason) => informationReasonLabels[reason] || reason).join("、")}`;
    });
    informationMessage(`整批删除未完成：${friendlyError(error)}${reasons.length ? `。${reasons.join("；")}` : ""}`, true);
    if (error.status === 409) {
      informationSelectedUsers.clear();
      informationSelectedDetails.clear();
      await loadInformationUsers(0);
    }
    throw error;
  }
}

function bindInformation() {
  bindAsync("information-preview-form", "submit", previewInformation, "正在预览…", informationIdentityCurrent);
  bindAsync("information-create-job", "click", createInformationJob, "正在创建…", informationIdentityCurrent);
  bindAsync("information-delete-users", "click", deleteInformationUsers, "正在删除…", informationIdentityCurrent);
  bindAsync("information-users-form", "submit", () => loadInformationUsers(0), "正在查询…", informationIdentityCurrent);
  bindAsync("information-refresh", "click", async () => {
    await Promise.all([loadInformationOverview(), loadInformationUsers(0)]);
    informationMessage("任务与候选用户已刷新。");
  }, "正在刷新…", informationIdentityCurrent);
  bindAsync("information-users-prev", "click", () => loadInformationUsers(Math.max(0, informationUsersOffset - 100)), "加载中…", informationIdentityCurrent);
  bindAsync("information-users-next", "click", () => loadInformationUsers(informationUsersOffset + 100), "加载中…", informationIdentityCurrent);
  byId("information-preview-form").elements.retention_days.addEventListener("input", () => {
    informationSequence++;
    informationPreview = null;
    hide("information-preview");
    syncInformationControls();
  });
  byId("information-select-all").addEventListener("change", (event) => {
    for (const user of informationUsers) {
      if (event.currentTarget.checked && informationSelectedUsers.size < 100) {
        informationSelectedUsers.add(user.id);
        informationSelectedDetails.set(user.id, user);
      } else if (!event.currentTarget.checked) {
        informationSelectedUsers.delete(user.id);
        informationSelectedDetails.delete(user.id);
      }
    }
    renderInformationUsers();
  });
}

function resetModelIdentification() {
  modelIdentificationOptionsReady = false;
  modelIdentificationRunning = false;
  modelIdentificationOptionsSequence++;
  modelIdentificationRecordsSequence++;
  const account = byId("model-identification-account");
  const model = byId("model-identification-model");
  if (!account || !model) return;
  account.replaceChildren(element("option", {text: "登录后加载账号", attributes: {value: ""}}));
  model.replaceChildren(element("option", {text: "先选择账号", attributes: {value: ""}}));
  model.disabled = true;
  byId("model-identification-version").textContent = "—";
  byId("model-identification-run-status").textContent = "尚无运行记录";
  byId("model-identification-progress").value = 0;
  hide("model-identification-progress");
  byId("model-identification-results").replaceChildren(emptyState("尚无鉴别记录。"));
  setLocalMessage(byId("model-identification-form"));
  syncModelIdentificationControls();
}

function modelIdentificationVisible() {
  return ownerSectionVisible("model-identification") && !loggingOut;
}

function syncModelIdentificationControls() {
  const form = byId("model-identification-form");
  if (!form) return;
  const account = byId("model-identification-account");
  const model = byId("model-identification-model");
  const disabled = !state || state.user.role !== "owner" || loggingOut;
  account.disabled = disabled || !modelIdentificationOptionsReady;
  model.disabled = disabled || !account.value || model.options.length <= 1;
  byId("model-identification-run").disabled = disabled || modelIdentificationRunning || !account.value || !model.value || form.dataset.busy === "true";
  byId("model-identification-refresh").disabled = disabled;
}

async function loadModelIdentificationOptions() {
  const request = ++modelIdentificationOptionsSequence;
  const generation = identityGeneration;
  const current = () => request === modelIdentificationOptionsSequence && generation === identityGeneration && modelIdentificationVisible();
  const selected = byId("model-identification-account").value;
  const response = await api("/admin/model-identifications/options", {}, current);
  if (!current()) return;
  const accounts = Array.isArray(response.accounts) ? response.accounts : [];
  const select = byId("model-identification-account");
  select.replaceChildren(element("option", {text: "请选择上游账号", attributes: {value: ""}}),
    ...accounts.map((account) => {
      const option = element("option", {
        text: `${account.masked_email || account.id} · ${account.status === "available" ? "可用" : "不可用"}`,
        attributes: {value: account.id},
      });
      // `disabled` is a boolean DOM property. Writing disabled="false" still
      // disables an option, so set the property after creating the element.
      option.disabled = account.status !== "available";
      return option;
    }));
  select.value = accounts.some((account) => account.id === selected && account.status === "available") ? selected : "";
  byId("model-identification-version").textContent = response.reference_version || "—";
  modelIdentificationOptionsReady = true;
  if (select.value) await loadModelIdentificationModels();
  else {
    byId("model-identification-model").replaceChildren(element("option", {text: "先选择账号", attributes: {value: ""}}));
    syncModelIdentificationControls();
  }
}

async function loadModelIdentificationModels() {
  const request = ++modelIdentificationOptionsSequence;
  const generation = identityGeneration;
  const accountID = byId("model-identification-account").value;
  const select = byId("model-identification-model");
  const selected = select.value;
  select.disabled = true;
  select.replaceChildren(element("option", {text: accountID ? "正在查询该账号模型…" : "先选择账号", attributes: {value: ""}}));
  syncModelIdentificationControls();
  if (!accountID) return;
  const current = () => request === modelIdentificationOptionsSequence && generation === identityGeneration &&
    modelIdentificationVisible() && byId("model-identification-account").value === accountID;
  try {
    const response = await api(`/admin/model-identifications/options?account_id=${encodeURIComponent(accountID)}`, {}, current);
    if (!current()) return;
    const models = Array.isArray(response.models) ? response.models : [];
    select.replaceChildren(element("option", {text: models.length ? "请选择模型" : "该账号没有可鉴别的已配置模型", attributes: {value: ""}}),
      ...models.map((model) => element("option", {text: model, attributes: {value: model}})));
    select.value = models.includes(selected) ? selected : "";
    byId("model-identification-version").textContent = response.reference_version || "—";
  } catch (error) {
    if (!current()) return;
    select.replaceChildren(element("option", {text: "模型查询失败", attributes: {value: ""}}));
    setLocalMessage(byId("model-identification-form"), friendlyError(error));
  }
  syncModelIdentificationControls();
}

const modelIdentificationFailureLabels = {
  model_identification_account_unavailable: "所选账号已不可用",
  model_identification_model_unavailable: "所选模型已不可用",
  model_identification_account_mismatch: "上游返回了不同的账号",
  model_identification_invalid_answer: "探针回答未通过官方输入校验",
  model_identification_timeout: "探针或整项运行超时",
  model_identification_interrupted: "运行已中断",
  interrupted: "进程中断，运行未完成",
  model_identification_probe_failed: "探针执行失败",
  model_identification_storage_failed: "保存运行结果失败",
  model_identification_reference_unavailable: "参考库暂不可用",
};

function renderModelIdentifications(items) {
  const records = Array.isArray(items) ? items : [];
  const latest = [...records].sort((a, b) => Date.parse(b.run_started_at || 0) - Date.parse(a.run_started_at || 0))[0];
  modelIdentificationRunning = records.some((item) => item.run_status === "running");
  const progress = byId("model-identification-progress");
  progress.value = Math.max(0, Math.min(3, Number(latest?.run_progress) || 0));
  progress.classList.toggle("hidden", latest?.run_status !== "running");
  const status = byId("model-identification-run-status");
  if (!latest) status.textContent = "尚无运行记录";
  else if (latest.run_status === "running") status.textContent = `${latest.requested_model} · 已完成 ${progress.value} / 3 题 · 开始于 ${formatDateTime(latest.run_started_at)}`;
  else if (latest.run_status === "succeeded") status.textContent = `${latest.requested_model} · 已完成 · ${formatDateTime(latest.run_finished_at)}`;
  else status.textContent = `${latest.requested_model} · 本次失败：${modelIdentificationFailureLabels[latest.run_error_code] || "运行失败"} · ${formatDateTime(latest.run_finished_at)}`;
  const container = byId("model-identification-results");
  if (!records.length) {
    container.replaceChildren(emptyState("尚无鉴别记录。"));
    syncModelIdentificationControls();
    return;
  }
  container.replaceChildren(...records.map((item) => {
    const account = all("option", byId("model-identification-account")).find((option) => option.value === item.account_id);
    const label = account?.textContent?.split(" · ")[0] || item.account_id;
    const card = element("article", {className: "panel model-identification-result"},
      element("h3", {text: `${label} · ${item.requested_model}`}));
    if (item.run_status === "failed") card.append(element("p", {className: "model-identification-failure", text: `本次失败：${modelIdentificationFailureLabels[item.run_error_code] || "运行失败"}。${item.conclusion ? "下方保留仍在有效期内的上次结论。" : "未产生新结论。"}`}));
    if (item.conclusion) {
      const level = {match: "明确匹配", family_only: "同家族接近", insufficient: "匹配较弱"}[item.match_level] || "无法可靠判定";
      const fields = [
        ["统计结论", item.conclusion], ["最接近的参考模型", item.closest_model || "—"],
        ["匹配强度", level], ["拟合度 / 领先差距", `${formatPercent(item.fit, "—")} / ${formatPercent(item.margin, "—")}`],
        ["参考库版本", item.reference_version || "—"], ["完成 / 到期", `${formatDateTime(item.completed_at, "—")} / ${formatDateTime(item.expires_at, "—")}`],
      ];
      card.append(element("dl", {}, ...fields.map(([name, value]) => element("div", {}, element("dt", {text: name}), element("dd", {text: value})))));
    } else card.append(element("p", {className: "muted", text: "暂无有效统计结论。"}));
    return card;
  }));
  syncModelIdentificationControls();
}

async function loadModelIdentifications() {
  const request = ++modelIdentificationRecordsSequence;
  const generation = identityGeneration;
  const current = () => request === modelIdentificationRecordsSequence && generation === identityGeneration && modelIdentificationVisible();
  const response = await api("/admin/model-identifications", {}, current);
  if (!current()) return;
  byId("model-identification-version").textContent = response.reference_version || "—";
  renderModelIdentifications(response.identifications);
}

function stopModelIdentification() {
  modelIdentificationPolling = false;
  modelIdentificationOptionsSequence++;
  modelIdentificationRecordsSequence++;
  if (modelIdentificationTimer) window.clearTimeout(modelIdentificationTimer);
  modelIdentificationTimer = 0;
}

function startModelIdentification() {
  if (!modelIdentificationVisible() || modelIdentificationPolling) return;
  modelIdentificationPolling = true;
  const generation = identityGeneration;
  const tick = async () => {
    if (!modelIdentificationPolling || !modelIdentificationVisible() || generation !== identityGeneration) return;
    try {
      if (!modelIdentificationOptionsReady) await loadModelIdentificationOptions();
      await loadModelIdentifications();
    } catch (error) {
      if (modelIdentificationVisible() && error.code !== "stale_request") setLocalMessage(byId("model-identification-form"), friendlyError(error));
    } finally {
      if (modelIdentificationPolling && modelIdentificationVisible() && generation === identityGeneration) {
        modelIdentificationTimer = window.setTimeout(tick, 3000);
      }
    }
  };
  tick();
}

function bindModelIdentification() {
  byId("model-identification-account").addEventListener("change", () => {
    setLocalMessage(byId("model-identification-form"));
    loadModelIdentificationModels();
  });
  byId("model-identification-model").addEventListener("change", syncModelIdentificationControls);
  bindAsync("model-identification-refresh", "click", async () => {
    modelIdentificationOptionsReady = false;
    await loadModelIdentificationOptions();
    await loadModelIdentifications();
  }, "刷新中…", modelIdentificationVisible);
  bindAsync("model-identification-form", "submit", async (event) => {
    const form = event.currentTarget;
    const accountID = form.elements.account_id.value;
    const model = form.elements.model.value;
    if (!accountID || !model || modelIdentificationRunning) return;
    const actorID = state?.user?.id;
    const generation = identityGeneration;
    const current = () => modelIdentificationVisible() && generation === identityGeneration && state?.user?.id === actorID;
    await sensitiveAction(() => api("/admin/model-identifications/runs", {method: "POST", body: JSON.stringify({account_id: accountID, model})}, current), current);
    if (!current()) return;
    setLocalMessage(form, "鉴别已开始，正在运行三道测试题。", "ok");
    await loadModelIdentifications();
  }, "启动中…", modelIdentificationVisible);
}

function syncVisiblePolling() {
  if (ownerSectionVisible("upstream-accounts")) startUpstreamConcurrency();
  else if (upstreamConcurrencyPolling) stopUpstreamConcurrency();
  if (ownerSectionVisible("monitoring")) startMonitoring();
  else if (monitoringPolling) stopMonitoring();
  if (modelIdentificationVisible()) startModelIdentification();
  else if (modelIdentificationPolling) stopModelIdentification();
  window.clearTimeout(informationJobTimer);
  if (ownerSectionVisible("information")) scheduleInformationJob();
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

  const upstreamForm = byId("upstream-account-filter");
  upstreamForm.elements.until.max = inputDate(today);
  upstreamForm.elements.from.max = inputDate(today);
  syncUpstreamAccountRange();
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

function syncUpstreamAccountRange() {
  const form = byId("upstream-account-filter");
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
  globalUserSearch?.close();
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
  billingUserSearch?.close();
  globalUserSearch?.close();
  const requested = location.hash.slice(1);
  const known = Object.prototype.hasOwnProperty.call(sectionTitles, requested);
  const ownerAllowed = !ownerOnlySections.has(requested) || state.user.role === "owner";
  const section = known && ownerAllowed ? requested : "overview";
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
  syncVisiblePolling();
  if (section === "information" && !informationLoaded) {
    informationLoaded = true;
    const current = informationIdentityCurrent();
    Promise.all([loadInformationOverview(), loadInformationUsers(0)]).catch((error) => {
      if (!current() || error.code === "stale_request") return;
      informationLoaded = false;
      informationMessage(`信息管理加载失败：${friendlyError(error)}`, true);
    });
  }
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
      loadUpstreamAccounts(upstreamAccountQueryFromForm()).catch((error) => {
        setLocalMessage(byId("upstream-account-filter"), friendlyError(error));
      }),
      loadModelAccess().catch((error) => {
        notice(`模型权限加载失败：${friendlyError(error)}`, "error");
      }),
      loadGroups().catch((error) => groupMessage(`群组加载失败：${friendlyError(error)}`, true)),
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
    form.querySelector("button[type=submit]").textContent = "恢复账号";
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
  if (visibleAuth) notice(message + "你仍可使用密码。", "info");
  byId("webauthn-warning").textContent = message;
  show("webauthn-warning");
  for (const id of ["login", "add-passkey"]) byId(id).disabled = true;
  byId("passkey-form").querySelector("button[type=submit]").disabled = true;
  all("#join-form select[name=login_method], #recover-form select[name=login_method]").forEach((select) => {
    select.querySelector('option[value="passkey"]').disabled = true;
    select.value = "password";
    select.dispatchEvent(new Event("change"));
  });
}

function bindUI() {
  bindInformation();
  bindModelIdentification();
  document.addEventListener("visibilitychange", syncVisiblePolling);
  bindAsync("monitoring-refresh", "click", async () => {
    await loadMonitoring({manual: true});
    announce("请求监控已刷新。");
  }, "刷新中…", () => ownerSectionVisible("monitoring"));
  bindGroupUI();
  billingUserSearch = createUserSearch("billing-user-search", selectBillingUser,
    (user) => `现金余额：${formatUSD(user.cash_balance_usd, formatUSD("0"))}`);
  globalUserSearch = createUserSearch("global-user-search", drillDownUser);
  recoveryUserSearch = createUserSearch("recovery-user-search", null,
    (user) => `${user.role === "owner" ? "Owner" : "Member"} · ${statusLabel(user.status)}`);
  syncBillingUserControls();
  syncBillingBatchControls();
  byId("billing-batch-search").addEventListener("input", () => renderBillingBatchUsers());
  byId("billing-batch-select-all").addEventListener("change", (event) => selectBillingBatchMatches(event.currentTarget.checked));
  byId("billing-batch-clear-selection").addEventListener("click", clearBillingBatchSelection);
  bindBillingBatchAction("billing-batch-recharge-form", "submit", (event) => prepareBillingBatch(event, "recharge"));
  bindBillingBatchAction("billing-batch-subscription-form", "submit", (event) => prepareBillingBatch(event, "subscription"));
  bindBillingBatchAction("billing-batch-start", "click", startBillingBatch);
  bindBillingBatchAction("billing-batch-retry", "click", retryBillingBatch);
  bindBillingBatchAction("billing-batch-reset", "click", finishBillingBatch);
  bindBillingBatchAction("billing-batch-refresh-users", "click", refreshBillingBatchData);
  bindAsync("login", "click", login, "等待 Passkey…");
  bindAsync("password-login-form", "submit", passwordLogin, "登录中…");
  bindAsync("join-form", "submit", register, "等待 Passkey…");
  bindAsync("recover-form", "submit", recover, "等待 Passkey…");
  bindAsync("logout", "click", async () => {
    loggingOut = true;
    stopUpstreamConcurrency();
    stopMonitoring();
    stopModelIdentification();
    resetModelIdentification();
    resetMonitoring();
    resetInformation();
    identityGeneration++;
    personalRequestSequence++;
    globalRequestSequence++;
    billingRequestSequence++;
    billingUsersRequestSequence++;
    modelAccessModelsRequestSequence++;
    modelAccessUsersRequestSequence++;
    upstreamAccountRequestSequence++;
    upstreamAccountOperation = null;
    upstreamAccountListLoading = false;
    syncUpstreamAccountControls();
    clearUpstreamQuotaTimers();
    resetPersonalUsageSummary();
    hide("personal-loading");
    clearSensitiveDOM();
    resetBillingUserSearch();
    globalUserSearch.reset();
    recoveryUserSearch?.reset();
    try {
      await api("/auth/logout", {method: "POST", body: "{}"});
    } catch (error) {
      loggingOut = false;
      syncVisiblePolling();
      throw error;
    }
    location.assign("/");
  }, "正在退出…");
  bindAsync("device-form", "submit", (event) => submitResource(event, "/admin/devices", "设备已添加。"), "添加中…");
  bindAsync("project-form", "submit", (event) => submitResource(event, "/admin/projects", "项目已添加。"), "添加中…");
  bindAsync("key-form", "submit", createKey, "创建中…");
  bindAsync("passkey-form", "submit", addPasskey, "等待 Passkey…");
  bindAsync("password-form", "submit", setPassword, "保存中…");
  bindAsync("reauth-form", "submit", submitReauthentication, "验证中…", () => reauthRequestCurrent);
  bindAsync("billing-rate-form", "submit", updateBillingRate, "更新中…");
  bindAsync("billing-recharge-form", "submit", rechargeBillingUser, "充值中…");
  bindAsync("billing-adjustment-form", "submit", adjustBillingUser, "调整中…");
  bindAsync("model-access-default-form", "submit", async (event) => {
    try { await updateModelAccessDefault(event); } finally { window.setTimeout(renderModelAccessSummary, 0); }
  }, "保存中…");
  bindAsync("model-access-enable-selected", "click", async () => {
    try { await mutateSelectedModelAccess(true); } finally { window.setTimeout(syncModelAccessSelection, 0); }
  }, "启用中…");
  bindAsync("model-access-disable-selected", "click", async () => {
    try { await mutateSelectedModelAccess(false); } finally { window.setTimeout(syncModelAccessSelection, 0); }
  }, "禁用中…");
  bindAsync("model-access-enable-all", "click", async () => {
    try { await mutateAllModelAccess(true); } finally { window.setTimeout(renderModelAccessSummary, 0); }
  }, "启用中…");
  bindAsync("model-access-disable-all", "click", async () => {
    try { await mutateAllModelAccess(false); } finally { window.setTimeout(renderModelAccessSummary, 0); }
  }, "禁用中…");
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
  bindAsync("upstream-account-filter", "submit", async () => {
    await loadUpstreamAccounts(upstreamAccountQueryFromForm());
    announce("上游账号本地统计已更新。");
  }, "应用中…");
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
  byId("set-password").addEventListener("click", () => openDialog("password-dialog"));
  byId("reauth-form").elements.method.addEventListener("change", syncReauthMethod);
  byId("reauth-dialog").addEventListener("close", () => {
    if (!reauthReject) return;
    cancelReauthentication();
  });
  all("#join-form select[name=login_method], #recover-form select[name=login_method]").forEach((select) => {
    select.addEventListener("change", () => {
      const fields = select.closest("form").querySelector(".password-fields");
      const password = select.value === "password";
      fields.classList.toggle("hidden", !password);
      all("input", fields).forEach((input) => { input.required = password; });
    });
  });
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
  byId("upstream-account-filter").elements.range.addEventListener("change", syncUpstreamAccountRange);
  byId("model-access-model-select").addEventListener("change", (event) => {
    const input = event.target;
    if (!input.matches(".model-access-model-checkbox")) return;
    if (input.checked) modelAccessSelectedModels.add(input.value);
    else modelAccessSelectedModels.delete(input.value);
    changedModelAccessSelection();
  });
  byId("model-access-model-search").addEventListener("input", filterModelAccessModels);
  byId("model-access-user-search").addEventListener("input", filterModelAccessUsers);
  byId("model-access-model-all").addEventListener("click", () => {
    modelAccessSelectedModels = new Set(modelAccessModels.map((item) => item.model));
    all(".model-access-model-checkbox").forEach((input) => { input.checked = true; });
    changedModelAccessSelection();
  });
  byId("model-access-model-clear").addEventListener("click", () => {
    modelAccessSelectedModels.clear();
    all(".model-access-model-checkbox").forEach((input) => { input.checked = false; });
    changedModelAccessSelection();
  });
  byId("model-access-select-all").addEventListener("change", (event) => {
    all(".model-access-user-select", byId("model-access-user-rows")).forEach((input) => {
      input.checked = event.currentTarget.checked;
      if (input.checked) modelAccessSelectedUsers.add(input.value);
      else modelAccessSelectedUsers.delete(input.value);
    });
    syncModelAccessSelection();
  });
  byId("model-access-users-form").addEventListener("submit", (event) => event.preventDefault());
  window.addEventListener("hashchange", () => routeFromHash(true));

  all("[data-copy-target]").forEach((button) => button.addEventListener("click", () => {
    runButton(button, async () => {
      const target = byId(button.dataset.copyTarget);
      if (!target) throw new Error("找不到要复制的内容。");
      await copyText(target.textContent);
      announce("内容已复制到剪贴板。");
    }, "复制中…");
  }));

  byId("copy-secret").addEventListener("click", () => runButton(byId("copy-secret"), async () => {
    const value = byId("secret-value").textContent;
    await copyText(value);
    setLocalMessage(byId("secret-dialog"), "已复制到剪贴板。", "ok");
  }, "复制中…"));
  byId("save-secret").addEventListener("click", () => byId("secret-dialog").close());
  byId("secret-dialog").addEventListener("close", () => {
    const afterClose = secretAfterClose;
    secretAfterClose = null;
    secretDismissible = false;
    byId("secret-value").textContent = "";
    byId("secret-eyebrow").textContent = "ONLY ONCE";
    byId("secret-title").textContent = "只显示一次";
    byId("secret-description").textContent = "";
    byId("save-secret").textContent = "我已安全保存并关闭";
    setLocalMessage(byId("secret-dialog"));
    if (afterClose) afterClose();
  });
  byId("secret-dialog").addEventListener("cancel", (event) => {
    if (secretDismissible) return;
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
