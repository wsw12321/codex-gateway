"use strict";
// Consume URL data before any API request. Never retain authorization codes in
// history, storage, markup, logs, links or error messages.
const oidcParameters = new URLSearchParams(location.search);
history.replaceState(null, "", "/auth/oidc/callback");
let oidcFlowID = "";
let oidcBusy = false;
let oidcPageActive = true;
let oidcFlowKind = "";
let oidcCancellationID = oidcParameters.get("state") || "";
let oidcFinished = false;
const oidcMessage = document.getElementById("oidc-message");
const oidcPreview = document.getElementById("oidc-preview");
const oidcConfirm = document.getElementById("oidc-confirm");
const oidcCancel = document.getElementById("oidc-cancel");
const oidcRegistration = document.getElementById("oidc-registration");
const oidcRegisterButtons = ["oidc-register-link", "oidc-register-create", "oidc-register-cancel"].map((id) => document.getElementById(id));
window.addEventListener("pagehide", () => {
  oidcPageActive = false;
  oidcFlowID = "";
  if (!oidcFinished && oidcCancellationID) {
    // Cancel this exact lifecycle, including an exchange still completing on the server.
    fetch("/auth/oidc/cancel", {method: "POST", credentials: "same-origin", cache: "no-store", keepalive: true,
      headers: {"Content-Type": "application/json"}, body: JSON.stringify({flow_id: oidcCancellationID})}).catch(() => {});
  }
  if (!oidcFinished) {
    try { sessionStorage.removeItem("cg_oidc_reauth_return"); } catch (_) { /* Navigation metadata is optional. */ }
  }
  oidcCancellationID = "";
});
window.addEventListener("pageshow", (event) => {
  if (event.persisted) {
    oidcPreview.classList.add("hidden");
    oidcRegistration.classList.add("hidden");
    oidcMessage.textContent = "授权页面已失效，请返回网关重新开始。";
  }
});
async function oidcPost(path, body) {
  const response = await fetch(path, {
    method: "POST", credentials: "same-origin", cache: "no-store",
    headers: {"Content-Type": "application/json"}, body: JSON.stringify(body),
  });
  const result = await response.json();
  if (!oidcPageActive) throw new Error("授权页面已关闭，请重新开始。");
  if (!response.ok) throw new Error(result?.error?.message || "授权未完成，请返回网关重新开始。");
  return result;
}
function oidcFail(error) {
  oidcFlowID = "";
  oidcPreview.classList.add("hidden");
  oidcRegistration.classList.add("hidden");
  try { sessionStorage.removeItem("cg_oidc_reauth_return"); } catch (_) { /* Navigation metadata is optional. */ }
  if (oidcPageActive) oidcMessage.textContent = error.message || "授权未完成，请返回网关重新开始。";
}
async function oidcFinish(action) {
  if (oidcBusy || !oidcFlowID || !oidcPageActive) return;
  oidcBusy = true;
  oidcConfirm.disabled = oidcCancel.disabled = true;
  // Confirmation is single-use even on network errors. Do not retry it.
  const flowID = oidcFlowID;
  oidcFlowID = "";
  try {
    await oidcPost(`/admin/identity-link/${action}`, {flow_id: flowID});
    oidcFinished = true;
    oidcPreview.classList.add("hidden");
    oidcMessage.textContent = action === "confirm" ? "绑定成功。原网关账号及业务数据保持不变。" : "已取消绑定。";
  } catch (error) { oidcFail(error); }
}
async function oidcRegister(action) {
  if (oidcBusy || !oidcFlowID || !oidcPageActive || oidcFlowKind !== "registration") return;
  oidcBusy = true;
  oidcRegisterButtons.forEach((button) => { button.disabled = true; });
  const flowID = oidcFlowID;
  oidcFlowID = "";
  try {
    const result = await oidcPost(action === "create" ? "/auth/oidc/register" : "/auth/oidc/register/cancel", {flow_id: flowID});
    oidcFinished = true;
    oidcRegistration.classList.add("hidden");
    if (action === "link") { location.replace("/?link=water5"); return; }
    if (action === "create") {
      if (!["login", "logged_in"].includes(result.status || result.result)) throw new Error("开通结果无效，请重新登录。");
      location.replace("/#overview");
      return;
    }
    oidcMessage.textContent = "已取消开通，未创建本站账号。";
    document.getElementById("oidc-return").href = "/";
  } catch (error) { oidcFail(error); }
}
oidcConfirm.addEventListener("click", () => oidcFinish("confirm"));
oidcCancel.addEventListener("click", () => oidcFinish("cancel"));
oidcRegisterButtons[0].addEventListener("click", () => oidcRegister("link"));
oidcRegisterButtons[1].addEventListener("click", () => oidcRegister("create"));
oidcRegisterButtons[2].addEventListener("click", () => oidcRegister("cancel"));
(async () => {
  if (["state", "code", "error"].some((key) => oidcParameters.getAll(key).length > 1)) throw new Error("授权参数无效，请重新开始。");
  const input = {state: oidcParameters.get("state") || "", code: oidcParameters.get("code") || "", error: oidcParameters.get("error") || ""};
  for (const key of [...oidcParameters.keys()]) oidcParameters.delete(key);
  const result = await oidcPost("/auth/oidc/complete", input);
  input.code = input.state = input.error = "";
  const status = result.status || result.result;
  if (status === "reauthenticated") {
    oidcFinished = true;
    let section = "security";
    try {
      const saved = JSON.parse(sessionStorage.getItem("cg_oidc_reauth_return") || "null");
      sessionStorage.removeItem("cg_oidc_reauth_return");
      if (/^[a-z][a-z-]{0,40}$/.test(saved?.section || "")) section = saved.section;
    } catch (_) { /* Return to account security if storage is unavailable. */ }
    location.replace(`/?verified=water5#${section}`);
    return;
  }
  try { sessionStorage.removeItem("cg_oidc_reauth_return"); } catch (_) { /* No authorization data is stored. */ }
  if (["login", "logged_in"].includes(status)) { oidcFinished = true; location.replace("/#overview"); return; }
  if (status === "registration_required" && result.flow_id) {
    oidcFlowID = result.flow_id;
    oidcCancellationID = result.flow_id;
    oidcFlowKind = "registration";
    oidcMessage.textContent = "首次使用吾水阁账号登录";
    document.getElementById("oidc-registration-account").textContent = `已验证吾水阁账号 ${result.masked_email || "（邮箱未提供）"}。请选择如何开通本站账号。`;
    document.getElementById("oidc-return").href = "/";
    oidcRegistration.classList.remove("hidden");
    return;
  }
  if (result.result !== "confirm" || !result.flow_id || !result.user?.username) throw new Error("授权结果无效，请重新开始。");
  oidcFlowID = result.flow_id;
  oidcCancellationID = result.flow_id;
  oidcMessage.textContent = "请确认账号绑定";
  document.getElementById("oidc-accounts").textContent = `网关账号 ${result.user.username} 将绑定吾水阁账号 ${result.masked_email || "（邮箱未提供）"}。`;
  document.getElementById("oidc-expiry").textContent = `请在 ${new Date(result.expires_at).toLocaleTimeString("zh-CN")} 前确认，过期后需重新验证身份。`;
  oidcPreview.classList.remove("hidden");
})().catch(oidcFail);
