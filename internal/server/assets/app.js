"use strict";

const byId = (id) => document.getElementById(id);
let invitationToken = "";
let state = null;

function show(id) { byId(id)?.classList.remove("hidden"); }
function hide(id) { byId(id)?.classList.add("hidden"); }
function notice(message, kind = "info") {
  const el = byId("notice");
  el.textContent = message;
  el.dataset.kind = kind;
  show("notice");
}

async function api(path, options = {}) {
  const response = await fetch(path, {
    credentials: "same-origin",
    headers: {"Content-Type": "application/json", ...(options.headers || {})},
    ...options,
  });
  const body = response.status === 204 ? {} : await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(body?.error?.message || `请求失败 (${response.status})`);
    error.code = body?.error?.code;
    error.status = response.status;
    throw error;
  }
  return body;
}

function fromBase64URL(value) {
  const base64 = value.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((value.length + 3) % 4);
  const binary = atob(base64);
  return Uint8Array.from(binary, (char) => char.charCodeAt(0));
}

function toBase64URL(value) {
  if (value == null) return null;
  const bytes = new Uint8Array(value);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function creationOptions(envelope) {
  const options = envelope.publicKey;
  options.challenge = fromBase64URL(options.challenge);
  options.user.id = fromBase64URL(options.user.id);
  for (const item of options.excludeCredentials || []) item.id = fromBase64URL(item.id);
  return {publicKey: options, mediation: envelope.mediation};
}

function assertionOptions(envelope) {
  const options = envelope.publicKey;
  options.challenge = fromBase64URL(options.challenge);
  for (const item of options.allowCredentials || []) item.id = fromBase64URL(item.id);
  return {publicKey: options, mediation: envelope.mediation};
}

function serializeCredential(credential) {
  const response = {
    clientDataJSON: toBase64URL(credential.response.clientDataJSON),
  };
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
  const credential = await navigator.credentials.create(creationOptions(ceremony.options));
  return serializeCredential(credential);
}

async function getPasskey(ceremony) {
  const credential = await navigator.credentials.get(assertionOptions(ceremony.options));
  return serializeCredential(credential);
}

async function login() {
  const ceremony = await api("/auth/login/begin", {method: "POST", body: "{}"});
  const credential = await getPasskey(ceremony);
  await api("/auth/login/finish", {method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential})});
  location.assign("/");
}

async function reauthenticate() {
  const ceremony = await api("/auth/reauth/begin", {method: "POST", body: "{}"});
  const credential = await getPasskey(ceremony);
  await api("/auth/reauth/finish", {method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential})});
  notice("Passkey 二次验证成功，敏感操作已解锁 5 分钟。", "ok");
  if (state) state.recently_verified = true;
}

function showRecoveryCodes(codes) {
  hide("join-view"); hide("recover-view"); hide("dashboard");
  byId("recovery-code-list").textContent = codes.join("\n");
  show("recovery-codes");
}

async function register(event) {
  event.preventDefault();
  if (!invitationToken) return notice("邀请链接缺少令牌。", "error");
  const data = Object.fromEntries(new FormData(event.currentTarget));
  data.invitation_token = invitationToken;
  const ceremony = await api("/auth/register/begin", {method: "POST", body: JSON.stringify(data)});
  const credential = await createPasskey(ceremony);
  const result = await api("/auth/register/finish", {method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential})});
  showRecoveryCodes(result.recovery_codes || []);
}

async function recover(event) {
  event.preventDefault();
  const data = Object.fromEntries(new FormData(event.currentTarget));
  const ceremony = await api("/auth/recovery/begin", {method: "POST", body: JSON.stringify(data)});
  const credential = await createPasskey(ceremony);
  const result = await api("/auth/recovery/finish", {method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential})});
  showRecoveryCodes(result.recovery_codes || []);
}

function option(value, label) {
  const el = document.createElement("option"); el.value = value; el.textContent = label; return el;
}

function renderState(value) {
  state = value;
  show("dashboard"); show("logout"); hide("login-view");
  byId("whoami").textContent = value.user.display_name;
  byId("role").textContent = `${value.user.username} · ${value.user.role}`;
  if (value.user.role === "owner") show("owner-tools");

  byId("devices").replaceChildren(...value.devices.map((d) => row(`${d.Name || d.name}`, d.status || d.Status)));
  byId("projects").replaceChildren(...value.projects.map((p) => row(`${p.Name || p.name} · ${p.Slug || p.slug}`, p.status || p.Status)));
  byId("passkeys").replaceChildren(...value.passkeys.map((p) => row(p.Nickname || p.nickname || "Passkey", "已注册")));
  byId("keys").replaceChildren(...value.api_keys.map((key) => {
    const id = key.ID || key.id;
    const item = row(`${key.Name || key.name} · ${key.KeyPrefix || key.key_prefix}`, key.Status || key.status);
    if ((key.Status || key.status) === "active") {
      const button = document.createElement("button"); button.type = "button"; button.className = "danger"; button.textContent = "撤销";
      button.addEventListener("click", () => revokeKey(id)); item.append(button);
    }
    return item;
  }));

  const deviceSelect = byId("key-device"); deviceSelect.replaceChildren();
  for (const d of value.devices.filter((item) => (item.Status || item.status) === "active")) deviceSelect.append(option(d.ID || d.id, d.Name || d.name));
  const projectSelect = byId("key-project"); projectSelect.replaceChildren(option("", "unassigned"));
  for (const p of value.projects.filter((item) => (item.Status || item.status) === "active")) projectSelect.append(option(p.ID || p.id, `${p.Name || p.name} (${p.Slug || p.slug})`));
}

function row(primary, secondary) {
  const item = document.createElement("div"); item.className = "list-row";
  const strong = document.createElement("strong"); strong.textContent = primary;
  const small = document.createElement("small"); small.textContent = secondary || "";
  item.append(strong, small); return item;
}

async function refreshState() {
  try { renderState(await api("/admin/state")); await refreshUsage(); }
  catch (error) {
    if (error.status === 401) { show("login-view"); return; }
    throw error;
  }
}

async function refreshUsage() {
  const result = await api("/admin/usage");
  const s = result.summary;
  byId("metric-requests").textContent = s.requests.toLocaleString();
  byId("metric-tokens").textContent = s.tokens.toLocaleString();
  byId("metric-errors").textContent = `${(s.error_rate * 100).toFixed(1)}%`;
  byId("metric-cache").textContent = `${(s.cache_rate * 100).toFixed(1)}%`;
  byId("metric-ttft").textContent = `${s.p95_ttft_ms} ms`;
  byId("metric-duration").textContent = `${s.p95_duration_ms} ms`;
  const rows = result.requests.map((item) => {
    const tr = document.createElement("tr");
    const values = [new Date(item.RequestedAt || item.requested_at).toLocaleString(), item.KeyPrefix || item.key_prefix,
      item.ProjectID || item.project_id || "unassigned", item.Model || item.model, item.State || item.state,
      Number(item.InputTokens || item.input_tokens) + Number(item.OutputTokens || item.output_tokens), item.TTFTMillis || item.ttft_ms || "—"];
    for (const value of values) { const td = document.createElement("td"); td.textContent = String(value); tr.append(td); }
    return tr;
  });
  byId("usage-rows").replaceChildren(...rows);
}

async function submitResource(event, path) {
  event.preventDefault(); const data = Object.fromEntries(new FormData(event.currentTarget));
  await api(path, {method: "POST", body: JSON.stringify(data)}); event.currentTarget.reset(); await refreshState();
}

async function createKey(event) {
  event.preventDefault();
  if (!state.recently_verified) await reauthenticate();
  const data = Object.fromEntries(new FormData(event.currentTarget));
  data.expires_days = Number(data.expires_days);
  data.models = data.models.split(",").map((v) => v.trim()).filter(Boolean);
  const result = await api("/admin/api-keys", {method: "POST", body: JSON.stringify(data)});
  const box = byId("one-time-key"); box.textContent = `只显示一次：${result.api_key}`; show("one-time-key");
  await refreshState();
}

async function revokeKey(id) {
  if (!confirm("撤销后立即失效，确定继续？")) return;
  if (!state.recently_verified) await reauthenticate();
  await api(`/admin/api-keys/${encodeURIComponent(id)}`, {method: "DELETE", body: "{}"}); await refreshState();
}

async function addPasskey() {
  if (!state.recently_verified) await reauthenticate();
  const ceremony = await api("/admin/passkeys/begin", {method: "POST", body: "{}"});
  const credential = await createPasskey(ceremony);
  await api("/admin/passkeys/finish", {method: "POST", body: JSON.stringify({flow_id: ceremony.flow_id, credential, nickname: "新增 Passkey"})});
  await refreshState();
}

async function invite(kind, targetUsername = "") {
  if (!state.recently_verified) await reauthenticate();
  const result = await api("/admin/invitations", {method: "POST", body: JSON.stringify({kind, target_username: targetUsername})});
  const box = byId("invite-result"); box.textContent = result.link; show("invite-result");
}

function bind(id, event, handler) {
  byId(id)?.addEventListener(event, async (e) => { try { await handler(e); } catch (error) { notice(error.message, "error"); } });
}

async function start() {
  if (!window.PublicKeyCredential) return notice("此浏览器不支持 WebAuthn Passkey。", "error");
  const path = location.pathname;
  if (path === "/join") {
    const fragment = location.hash.slice(1);
    invitationToken = new URLSearchParams(fragment).get("token") || fragment;
    history.replaceState(null, "", "/join");
    show("join-view"); return;
  }
  if (path === "/recover") { show("recover-view"); return; }
  await refreshState();
}

bind("login", "click", login);
bind("join-form", "submit", register);
bind("recover-form", "submit", recover);
bind("codes-saved", "click", async () => location.assign("/"));
bind("logout", "click", async () => { await api("/auth/logout", {method: "POST", body: "{}"}); location.reload(); });
bind("reauth", "click", reauthenticate);
bind("device-form", "submit", (event) => submitResource(event, "/admin/devices"));
bind("project-form", "submit", (event) => submitResource(event, "/admin/projects"));
bind("key-form", "submit", createKey);
bind("add-passkey", "click", addPasskey);
bind("member-invite", "click", () => invite("member"));
bind("recovery-invite-form", "submit", async (event) => { event.preventDefault(); await invite("recovery", new FormData(event.currentTarget).get("target_username")); });
start().catch((error) => notice(error.message, "error"));
