"use strict";
// Consume URL data before any API request. Never retain authorization codes in
// history, storage, markup, logs, links or error messages.
const oidcParameters = new URLSearchParams(location.search);
history.replaceState(null, "", "/auth/oidc/callback");
let oidcFlowID = "";
let oidcBusy = false;
let oidcPageActive = true;
const oidcMessage = document.getElementById("oidc-message");
const oidcPreview = document.getElementById("oidc-preview");
const oidcConfirm = document.getElementById("oidc-confirm");
const oidcCancel = document.getElementById("oidc-cancel");
window.addEventListener("pagehide", () => { oidcPageActive = false; oidcFlowID = ""; });
window.addEventListener("pageshow", (event) => {
  if (event.persisted) {
    oidcPreview.classList.add("hidden");
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
    oidcPreview.classList.add("hidden");
    oidcMessage.textContent = action === "confirm" ? "绑定成功。原网关账号及业务数据保持不变。" : "已取消绑定。";
  } catch (error) { oidcFail(error); }
}
oidcConfirm.addEventListener("click", () => oidcFinish("confirm"));
oidcCancel.addEventListener("click", () => oidcFinish("cancel"));
(async () => {
  if (["state", "code", "error"].some((key) => oidcParameters.getAll(key).length > 1)) throw new Error("授权参数无效，请重新开始。");
  const input = {state: oidcParameters.get("state") || "", code: oidcParameters.get("code") || "", error: oidcParameters.get("error") || ""};
  for (const key of [...oidcParameters.keys()]) oidcParameters.delete(key);
  const result = await oidcPost("/auth/oidc/complete", input);
  input.code = input.state = input.error = "";
  if (result.result === "login") { location.replace("/#overview"); return; }
  if (result.result !== "confirm" || !result.flow_id || !result.user?.username) throw new Error("授权结果无效，请重新开始。");
  oidcFlowID = result.flow_id;
  oidcMessage.textContent = "请确认账号绑定";
  document.getElementById("oidc-accounts").textContent = `网关账号 ${result.user.username} 将绑定吾水阁账号 ${result.masked_email || "（邮箱未提供）"}。`;
  document.getElementById("oidc-expiry").textContent = `请在 ${new Date(result.expires_at).toLocaleTimeString("zh-CN")} 前确认，过期后需重新验证身份。`;
  oidcPreview.classList.remove("hidden");
})().catch(oidcFail);
