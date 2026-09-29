package server

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardAssetsAreNotCachedAcrossDeployments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler func(*Server, *httptest.ResponseRecorder)
	}{
		{
			name: "page",
			handler: func(server *Server, recorder *httptest.ResponseRecorder) {
				server.page(recorder, httptest.NewRequest("GET", "/", nil))
			},
		},
		{
			name: "javascript",
			handler: func(server *Server, recorder *httptest.ResponseRecorder) {
				server.javascript(recorder, httptest.NewRequest("GET", "/static/app.js", nil))
			},
		},
		{
			name: "stylesheet",
			handler: func(server *Server, recorder *httptest.ResponseRecorder) {
				server.stylesheet(recorder, httptest.NewRequest("GET", "/static/style.css", nil))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.handler(&Server{}, recorder)
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestDashboardAssetsRemainDependencyFreeAndCSPCompatible(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	stylesheet := string(styleCSS)
	for _, forbidden := range []struct {
		name  string
		value string
	}{
		{"inline style", "style="},
		{"inline click handler", "onclick="},
		{"inline submit handler", "onsubmit="},
	} {
		if strings.Contains(html, forbidden.value) {
			t.Fatalf("dashboard HTML contains %s", forbidden.name)
		}
	}
	// Official documentation links may leave the site; executable and visual
	// assets must remain same-origin under the dashboard CSP.
	externalAsset := regexp.MustCompile(`(?i)<(?:script|link|img|iframe|object|embed|source|video|audio)\b[^>]*(?:src|href|data)\s*=\s*["'](?:https?:)?//`)
	if externalAsset.MatchString(html) {
		t.Fatal("dashboard HTML loads an external asset")
	}
	for _, forbidden := range []string{
		"localStorage", "sessionStorage", "innerHTML", "insertAdjacentHTML", "eval(", "new Function",
	} {
		if strings.Contains(javascript, forbidden) {
			t.Fatalf("dashboard JavaScript contains forbidden construct %q", forbidden)
		}
	}
	if strings.Contains(stylesheet, "url(") || strings.Contains(stylesheet, "@import") {
		t.Fatal("dashboard stylesheet contains an external asset hook")
	}
	for _, required := range []string{
		`href="#overview"`, `href="#resources"`, `href="#keys"`, `href="#guide"`, `href="#billing"`, `href="#security"`, `href="#usage"`, `href="#upstream-accounts"`,
		`id="secret-dialog"`, `id="operation-status"`, `class="skip-link"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("dashboard HTML is missing %s", required)
		}
	}
}

func TestGuideIncludesPersistentClientConfiguration(t *testing.T) {
	t.Parallel()
	html := string(indexHTML)
	javascript := string(appJS)
	for _, required := range []string{
		`data-section="guide"`, `id="guide-prepare"`, `id="guide-codex"`, `id="guide-agy"`,
		`id="guide-base-url"`,
		`id="guide-codex-install-code"`, `id="guide-codex-configure-code"`, `id="guide-codex-start-code"`,
		`id="guide-agy-install-windows-code"`, `id="guide-agy-install-unix-code"`,
		`id="guide-agy-configure-code"`, `id="guide-agy-start-code"`,
		`npm install -g @openai/codex`, `gemini-3.1-pro-high`, `Win+R`, `cmd`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("client guide HTML is missing %s", required)
		}
	}
	for _, required := range []string{
		`guide: "使用指导"`, `function renderGuide()`, `location.origin`,
		`/setup/configure-client.cjs`, `all("[data-copy-target]")`,
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("client guide JavaScript is missing %s", required)
		}
	}
	for _, forbidden := range []string{
		`guide-windows-download`, `/setup/configure-codex.`, `本机 AGY`,
		`CODEX_GATEWAY_API_KEY`, `CODEX_GATEWAY_PROJECT`,
	} {
		if strings.Contains(html, forbidden) || strings.Contains(javascript, forbidden) {
			t.Fatalf("client guide still contains obsolete configuration %s", forbidden)
		}
	}
}

func TestClientSetupRoutes(t *testing.T) {
	t.Parallel()
	server := &Server{mux: http.NewServeMux()}
	server.routes()
	for _, target := range []string{"/setup/configure-client.cjs", "/setup/configure-codex.sh", "/setup/configure-codex.bat"} {
		t.Run(target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			if target != "/setup/configure-client.cjs" {
				if recorder.Code != http.StatusGone {
					t.Fatalf("retired setup returned %d, want 410", recorder.Code)
				}
				return
			}
			if recorder.Code != http.StatusOK || recorder.Body.String() != string(clientSetupScript) {
				t.Fatalf("client setup did not serve the embedded configurator: status %d", recorder.Code)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/javascript; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			if recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("client setup must disable MIME sniffing")
			}
		})
	}
}

func TestGuideEndpointDoesNotInheritDashboardSidebarLayout(t *testing.T) {
	t.Parallel()

	stylesheet := string(styleCSS)
	if strings.Contains(stylesheet, `.app aside`) {
		t.Fatal("dashboard sidebar styles also match the guide endpoint aside")
	}
	if got := strings.Count(stylesheet, `.app > aside`); got != 3 {
		t.Fatalf("direct dashboard sidebar selector count = %d, want 3", got)
	}
}

func TestDesktopSidebarKeepsProfileFixedAndScrollsNavigation(t *testing.T) {
	t.Parallel()

	stylesheet := string(styleCSS)
	for _, required := range []string{
		"nav {\n  flex: 1 1 auto;\n  min-height: 0;",
		"overflow-y: auto;",
		"scrollbar-width: thin;",
		".profile {\n  flex: 0 0 auto;",
		"nav::-webkit-scrollbar",
		"nav {\n    display: flex;",
		"overflow-x: auto;",
	} {
		if !strings.Contains(stylesheet, required) {
			t.Fatalf("sidebar stylesheet is missing %s", required)
		}
	}
}

func TestBillingDashboardIncludesReadOnlyAndOwnerWorkflows(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	for _, required := range []string{
		`data-section="billing"`, `id="billing-cash-balance"`, `id="billing-subscriptions"`,
		`id="billing-ledger-rows"`, `id="billing-user-search"`, `id="billing-rate-form"`,
		`id="billing-recharge-form"`, `id="billing-adjustment-form"`,
		`id="billing-subscription-day"`, `id="billing-subscription-week"`, `id="billing-subscription-month"`,
		`id="billing-batch-panel"`, `id="billing-batch-user-rows"`, `id="billing-batch-select-all"`,
		`id="billing-batch-recharge-form"`, `id="billing-batch-subscription-form"`,
		`id="billing-batch-result-rows"`, `id="billing-batch-start"`, `id="billing-batch-retry"`,
		`id="billing-batch-refresh-users"`,
		`name="reason" required`, `name="period_count" required type="number" min="0" max="99" step="1" value="1"`,
		`周期数（0 表示无限期）`, `class="owner-only hidden billing-owner-tools"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("billing dashboard HTML is missing %s", required)
		}
	}
	for _, required := range []string{
		`/admin/billing/me`, `/admin/billing/settings`, `/admin/billing/users`,
		`/recharges`, `/adjustments`, `/subscriptions/`, `crypto.randomUUID()`,
		`return sensitiveAction(() => {`, `beforeSend?.();`,
		`period_count: billingPeriodCount(form)`, `当前第 ${period.current}/${period.count} 个周期`,
		`第 ${period.current} 个周期 · 无限期`, `本周期结束（最终失效）`, `最终到期：`,
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("billing dashboard JavaScript is missing %s", required)
		}
	}
	if count := strings.Count(html, `name="period_count" required type="number" min="0" max="99"`); count != 4 {
		t.Fatalf("billing dashboard administrator period-count input count = %d, want 4", count)
	}
	if !strings.Contains(html, `name="period_count" type="number" min="1" max="99" step="1" required`) {
		t.Fatal("billing dashboard purchase period-count input must allow only 1–99 periods")
	}
	for _, forbidden := range []string{
		`Number(data.get("cny_amount"))`, `Number(data.get("usd_amount"))`, `Number(data.get("quota_usd"))`,
	} {
		if strings.Contains(javascript, forbidden) {
			t.Fatalf("billing dashboard converts an exact money string with %s", forbidden)
		}
	}
}

func TestUpstreamAccountsDashboardIsOwnerOnlyAndHandlesLiveQuota(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	stylesheet := string(styleCSS)
	for _, required := range []string{
		`href="#upstream-accounts" data-view="upstream-accounts" class="owner-only hidden"`,
		`class="view owner-only hidden" data-section="upstream-accounts"`,
		`id="upstream-account-filter"`, `id="upstream-account-list"`,
		`id="upstream-account-action-message"`, `id="upstream-account-refresh-message"`,
		`id="upstream-account-control-help"`, `直到 Owner 重新启用`,
		`<option value="month">本月</option>`, `未公开的 ChatGPT 上游接口`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("upstream accounts dashboard HTML is missing %s", required)
		}
	}
	for _, required := range []string{
		`const ownerOnlySections = new Set(["upstream-accounts"])`,
		`state.user.role === "owner"`,
		`/admin/upstream-accounts${querySuffix(query)}`,
		`/admin/upstream-accounts/${encodeURIComponent(account.id)}/quota`,
		`const upstreamQuotaRequestBody = '{"method":"account/rateLimits/read","id":6}'`,
		`{method: "POST", body: upstreamQuotaRequestBody}`,
		`response?.id !== 6`, `response.result`, `const receivedAt = new Date()`,
		`account.email_masked`, `account?.request_count`, `account?.error_count`,
		`account?.input_tokens`, `account?.cached_input_tokens`, `account?.output_tokens`,
		`account?.reasoning_tokens`, `account?.equivalent_cost_usd`,
		`result?.rateLimitsByLimitId`, `result?.rateLimits`, `bucket.primary`, `bucket.secondary`,
		`bucket.rateLimitReachedType`, `value.usedPercent`, `100 - usedPercent`,
		`value.windowDurationMins`, `value.resetsAt`, `上游未返回`, `已达上游限额`,
		`container.dataset.state = "loading"`, `container.dataset.state = "error"`,
		`container.dataset.state = "stale"`, `clearUpstreamQuotaTimers()`,
		`/admin/upstream-accounts/${encodeURIComponent(account.id)}/status`,
		`method: "PUT", body: JSON.stringify({enabled: operation.enabled})`,
		`account.can_manage === true`, `upstreamAccountOperation`,
		`操作已成功，但列表与统计刷新失败`, `重新启用`,
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("upstream accounts dashboard JavaScript is missing %s", required)
		}
	}
	for _, forbidden := range []string{
		"`/admin/upstream-accounts/${encodeURIComponent(account.id)}/quota`, {method: \"POST\", body: \"{}\"}",
		`result?.five_hour`, `result?.seven_day`, `result?.additional_windows`,
		`used_ratio`, `remaining_ratio`, `resets_at`, `limitName`,
	} {
		if strings.Contains(javascript, forbidden) {
			t.Fatalf("upstream accounts dashboard JavaScript still contains legacy or unsafe quota field %s", forbidden)
		}
	}
	for _, required := range []string{
		`.upstream-account-grid {`, `.upstream-quota-result[data-state="loading"]`,
		`.upstream-quota-result[data-state="error"]`, `.upstream-quota-result[data-state="stale"]`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Fatalf("upstream accounts dashboard stylesheet is missing %s", required)
		}
	}
}

func TestModelIdentificationDashboardIsOwnerOnlyAndShowsEvidenceLimits(t *testing.T) {
	t.Parallel()
	html := string(indexHTML)
	js := string(appJS)
	for _, required := range []string{
		`href="#model-identification" data-view="model-identification" class="owner-only hidden"`,
		`class="view owner-only hidden" data-section="model-identification"`,
		`id="model-identification-account"`, `id="model-identification-model"`,
		`id="model-identification-progress"`, `id="model-identification-results"`,
		`统计匹配`, `不能证明实际模型身份`, `消耗所选上游账号的额度`,
		`探针回答不会保存`, `MIT 许可的 ModelTrace`, `可尝试模型`,
		`Owner 可直连指定账号`, `跳过日常分流限制`, `原有账号状态与配置保持不变`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("model identification HTML missing %q", required)
		}
	}
	for _, required := range []string{
		`ownerOnlySections.add("model-identification")`,
		`/admin/model-identifications/options`, `/admin/model-identifications/runs`,
		`function startModelIdentification()`, `function stopModelIdentification()`,
		`modelIdentificationTimer = window.setTimeout(tick, 3000)`,
	} {
		if !strings.Contains(js, required) {
			t.Fatalf("model identification script missing %q", required)
		}
	}
}

func TestModelIdentificationDashboardPolling(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for executable dashboard behavior tests")
	}
	command := exec.Command(node, "--test", "testdata/model_identification_browser.cjs")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("model identification dashboard behavior failed: %v\n%s", err, output)
	}
}

func TestUpstreamAccountDashboardBehavior(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for executable dashboard behavior tests")
	}
	command := exec.Command(node, "--test", "testdata/upstream_account_ui_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("upstream account dashboard behavior failed: %v\n%s", err, output)
	}
}

func TestModelAccessDashboardIsOwnerOnlyAndSupportsBatchChanges(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	stylesheet := string(styleCSS)
	for _, required := range []string{
		`href="#model-access" data-view="model-access" class="owner-only hidden"`,
		`class="view owner-only hidden" data-section="model-access"`,
		`id="model-access-model-select"`, `id="model-access-default-enabled"`,
		`id="model-access-user-rows"`, `id="model-access-select-all"`,
		`id="model-access-enable-selected"`, `id="model-access-disable-selected"`,
		`id="model-access-enable-all"`, `id="model-access-disable-all"`,
		`name="reason" required maxlength="500"`,
		`确认账号与 Key 已获得要使用的模型权限`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("model-access dashboard HTML is missing %s", required)
		}
	}
	for _, required := range []string{
		`ownerOnlySections.add("model-access")`,
		`/admin/model-access/models`, `/default`, `/users`,
		`method: "PUT"`, `scope === "selected"`, `payload.user_ids`,
		`sensitiveAction(() => api(`, `window.confirm(`,
		`loadModelAccess()`, `modelAccessUsersRequestSequence`,
		`modelAccessSelectedModels`, `modelAccessSelectedUsers`, `models, enabled, scope`,
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("model-access dashboard JavaScript is missing %s", required)
		}
	}
	for _, required := range []string{
		`.model-access-grid {`, `.model-access-bulk-form {`, `.model-access-checkbox {`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Fatalf("model-access dashboard stylesheet is missing %s", required)
		}
	}
}

func TestPasswordIdentityUIIncludesFallbackAndSafeSessionHandling(t *testing.T) {
	t.Parallel()
	html := string(indexHTML)
	javascript := string(appJS)
	for _, required := range []string{
		`id="password-login-form"`, `name="login_method"`, `id="password-dialog"`,
		`id="reauth-dialog"`, `id="password-status"`, `autocomplete="current-password"`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("identity UI is missing %q", required)
		}
	}
	for _, required := range []string{
		`/auth/password/login`, `/auth/password/register`, `/auth/password/recovery`,
		`/auth/password/reauth`, `/admin/password`, `recent_identity_verification_required`,
		`["session_required", "invalid_session"].includes`,
		`async function finishLogin()`, `await finishLogin();`, `await loadDashboard();`,
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("identity JavaScript is missing %q", required)
		}
	}
	if strings.Count(javascript, `location.assign("/#overview")`) != 1 {
		t.Error("login must update the dashboard in place; only recovery-code confirmation may navigate to /#overview")
	}
}

func TestAPIKeyLifecycleAndPersonalUsageDashboard(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	stylesheet := string(styleCSS)
	for _, required := range []string{
		`id="usage-tokens"`, `id="usage-charged-usd"`, `id="personal-usage-metrics"`,
		`id="secret-eyebrow"`, `检查 Key 是否活跃、是否过期`, `实际扣款`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("API key/usage dashboard HTML is missing %q", required)
		}
	}
	for _, required := range []string{
		`/reveal`, `/status`, `method: "DELETE"`, `async function revealKey(key)`,
		`async function changeKeyStatus(key, status)`, `async function deleteKey(key)`,
		`state.api_keys = state.api_keys.filter`, `sensitiveAction(() => api(`,
		`formatMoney(summary.charged_usd, "USD")`, `resetPersonalUsageSummary()`,
		`clearSensitiveDOM();`, `secretDismissible`,
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("API key/usage dashboard JavaScript is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`Number(summary.charged_usd)`, `parseFloat(summary.charged_usd)`, `async function revokeKey`,
	} {
		if strings.Contains(javascript, forbidden) {
			t.Errorf("API key/usage dashboard contains obsolete or lossy code %q", forbidden)
		}
	}
	if !strings.Contains(stylesheet, `.list-actions {`) || !strings.Contains(stylesheet, `flex-wrap: wrap;`) {
		t.Error("API key action layout does not wrap multiple lifecycle buttons")
	}
}

func TestUsageDashboardIncludesDegradedStatePresentation(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	stylesheet := string(styleCSS)
	for _, required := range []string{
		`<option value="degraded">degraded（被降智）</option>`,
		`degraded: "被降智"`,
		`usageModelCell(request, requestState)`,
		`requested_model`,
		`→ ${actual}`,
	} {
		if !strings.Contains(html+javascript, required) {
			t.Fatalf("usage dashboard is missing %s", required)
		}
	}
	if !strings.Contains(stylesheet, `.status-badge[data-status="degraded"]`) {
		t.Fatal("usage dashboard is missing degraded warning style")
	}
}
