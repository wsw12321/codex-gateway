"use strict";
// Consume URL data before any API request. Never retain authorization codes in
// history, storage, markup, logs, links or error messages.
const oidcParameters = new URLSearchParams(location.search);
history.replaceState(null, "", "/auth/oidc/callback");
let oidcFlowID = "";
let oidcBusy = false;
let oidcPageActive = true;
let oidcFlowKind = "";
let oidcRegistrationExpiresAt = "";
let oidcRegistrationEditing = false;
let oidcCancellationID = oidcParameters.get("state") || "";
let oidcFinished = false;
const oidcMessage = document.getElementById("oidc-message");
const oidcPreview = document.getElementById("oidc-preview");
const oidcConfirm = document.getElementById("oidc-confirm");
const oidcCancel = document.getElementById("oidc-cancel");
const oidcRegistration = document.getElementById("oidc-registration");
const oidcRegisterForm = document.getElementById("oidc-register-form");
const oidcRegisterMessage = document.getElementById("oidc-register-message");
const oidcRegisterButtons = ["oidc-register-link", "oidc-register-create", "oidc-register-cancel", "oidc-register-submit"].map((id) => document.getElementById(id));
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
    oidcMessage.textContent = "授权页面已失效，请返回水源喵重新开始。";
  }
});
async function oidcPost(path, body) {
  const response = await fetch(path, {
    method: "POST", credentials: "same-origin", cache: "no-store",
    headers: {"Content-Type": "application/json"}, body: JSON.stringify(body),
  });
  const result = await response.json();
  if (!oidcPageActive) throw new Error("授权页面已关闭，请重新开始。");
  if (!response.ok) throw Object.assign(new Error(result?.error?.message || "授权未完成，请返回水源喵重新开始。"), {
    status: response.status, code: result?.error?.code, flowID: result.flow_id, expiresAt: result.expires_at,
  });
  return result;
}
function oidcFail(error) {
  oidcFlowID = "";
  oidcPreview.classList.add("hidden");
  oidcRegistration.classList.add("hidden");
  try { sessionStorage.removeItem("cg_oidc_reauth_return"); } catch (_) { /* Navigation metadata is optional. */ }
  if (oidcPageActive) oidcMessage.textContent = error.message || "授权未完成，请返回水源喵重新开始。";
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
    oidcMessage.textContent = action === "confirm" ? "绑定成功。原水源喵账号及业务数据保持不变。" : "已取消绑定。";
  } catch (error) { oidcFail(error); }
}
async function oidcRegister(action) {
  if (oidcBusy || !oidcFlowID || !oidcPageActive || oidcFlowKind !== "registration") return;
  if (action === "create" && !oidcRegistrationEditing) return;
  if (action === "create" && !oidcRegisterForm.reportValidity()) return;
  oidcBusy = true;
  oidcRegisterButtons.forEach((button) => { button.disabled = true; });
  oidcRegisterMessage.classList.add("hidden");
  const flowID = oidcFlowID;
  oidcFlowID = "";
  try {
    const result = action === "create"
      ? await oidcPost("/auth/oidc/register", {flow_id: flowID, username: oidcRegisterForm.elements.username.value, display_name: oidcRegisterForm.elements.display_name.value})
      : await oidcPost("/auth/oidc/cancel", {flow_id: oidcCancellationID});
    if (action === "create" && !["login", "logged_in"].includes(result.status || result.result)) throw new Error("开通结果无效，请重新登录。");
    oidcFinished = true;
    oidcRegistration.classList.add("hidden");
    if (action === "link") { location.replace("/?link=water5"); return; }
    if (action === "create") {
      location.replace("/#overview");
      return;
    }
    oidcMessage.textContent = "已取消开通，未创建本站账号。";
    document.getElementById("oidc-return").href = "/";
  } catch (error) {
    const retryable = action === "create" && ((error.status === 400 && error.code === "invalid_profile") ||
      (error.status === 409 && error.code === "username_taken"));
    if (oidcPageActive && retryable && typeof error.flowID === "string" && error.flowID && error.flowID !== flowID &&
        error.expiresAt === oidcRegistrationExpiresAt && new Date(error.expiresAt).getTime() > Date.now()) {
      oidcFlowID = error.flowID;
      oidcBusy = false;
      oidcRegisterButtons.forEach((button) => { button.disabled = false; });
      oidcRegisterMessage.textContent = error.message;
      oidcRegisterMessage.classList.remove("hidden");
    } else oidcFail(error);
  }
}
function oidcOpenRegistration() {
  if (oidcBusy || !oidcFlowID || !oidcPageActive || oidcFlowKind !== "registration") return;
  oidcRegistrationEditing = true;
  oidcRegisterForm.classList.remove("hidden");
  oidcRegisterButtons[1].classList.add("hidden");
  oidcRegisterForm.elements.username.focus();
}
oidcConfirm.addEventListener("click", () => oidcFinish("confirm"));
oidcCancel.addEventListener("click", () => oidcFinish("cancel"));
oidcRegisterButtons[0].addEventListener("click", () => oidcRegister("link"));
oidcRegisterButtons[1].addEventListener("click", oidcOpenRegistration);
oidcRegisterButtons[2].addEventListener("click", () => oidcRegister("cancel"));
oidcRegisterForm.addEventListener("submit", (event) => { event.preventDefault(); oidcRegister("create"); });
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
    oidcRegistrationExpiresAt = result.expires_at;
    oidcFlowKind = "registration";
    oidcMessage.textContent = "首次使用吾水阁账号登录";
    document.getElementById("oidc-registration-account").textContent = `已验证吾水阁账号 ${result.masked_email || "（邮箱未提供）"}。请选择如何开通本站账号。`;
    document.getElementById("oidc-return").href = "/";
    document.getElementById("oidc-registration-expiry").textContent = `请在 ${new Date(result.expires_at).toLocaleTimeString("zh-CN")} 前完成设置，重试不会延长有效期。`;
    oidcRegistration.classList.remove("hidden");
    return;
  }
  if (result.result !== "confirm" || !result.flow_id || !result.user?.username) throw new Error("授权结果无效，请重新开始。");
  oidcFlowID = result.flow_id;
  oidcMessage.textContent = "请确认账号绑定";
  document.getElementById("oidc-accounts").textContent = `水源喵账号 ${result.user.username} 将绑定吾水阁账号 ${result.masked_email || "（邮箱未提供）"}。`;
  document.getElementById("oidc-expiry").textContent = `请在 ${new Date(result.expires_at).toLocaleTimeString("zh-CN")} 前确认，过期后需重新验证身份。`;
  oidcPreview.classList.remove("hidden");
})().catch(oidcFail);
