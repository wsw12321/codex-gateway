package server

import (
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
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
		{"external HTML URL", "https://"},
		{"external HTML URL", "http://"},
		{"inline style", "style="},
		{"inline click handler", "onclick="},
		{"inline submit handler", "onsubmit="},
	} {
		if strings.Contains(html, forbidden.value) {
			t.Fatalf("dashboard HTML contains %s", forbidden.name)
		}
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
		`href="#overview"`, `href="#resources"`, `href="#keys"`, `href="#guide"`, `href="#billing"`, `href="#security"`, `href="#usage"`,
		`id="secret-dialog"`, `id="operation-status"`, `class="skip-link"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("dashboard HTML is missing %s", required)
		}
	}
}

func TestGuideIncludesCodexGatewayConfiguration(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	for _, required := range []string{
		`data-section="guide"`, `id="guide-base-url"`, `id="guide-install-code"`, `id="guide-config-code"`,
		`id="guide-windows-download"`, `href="/setup/configure-codex.bat"`,
		`~/.codex/config.toml`, `codex login --with-api-key`, `不要把 API Key 写入 Git 仓库`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("Codex guide HTML is missing %s", required)
		}
	}
	for _, required := range []string{
		`guide: "使用指导"`, `function renderGuide()`, `` + "`openai_base_url = \"${baseURL}\"`" + ``,
		`/setup/configure-codex.sh`,
		`all("[data-copy-target]")`,
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("Codex guide JavaScript is missing %s", required)
		}
	}
	for _, forbidden := range []string{
		`id="guide-project-select"`, `id="guide-shell-code"`, `id="guide-powershell-code"`,
		`CODEX_GATEWAY_API_KEY`, `CODEX_GATEWAY_PROJECT`,
	} {
		if strings.Contains(html, forbidden) || strings.Contains(javascript, forbidden) {
			t.Fatalf("Codex guide still contains obsolete configuration %s", forbidden)
		}
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

func TestCodexShellSetupAddsOrReplacesOpenAIBaseURL(t *testing.T) {
	t.Parallel()

	const baseURL = "https://gateway.example/v1"
	tests := []struct {
		name     string
		original *string
		verify   func(*testing.T, string)
	}{
		{
			name: "new file",
			verify: func(t *testing.T, configured string) {
				if configured != `openai_base_url = "`+baseURL+"\"\n" {
					t.Fatalf("new configuration = %q", configured)
				}
			},
		},
		{
			name:     "missing setting",
			original: stringPointerValue("approval_policy = \"on-request\"\n\n[model_providers.gateway]\nbase_url = \"https://legacy.example/v1\"\n"),
			verify: func(t *testing.T, configured string) {
				if !strings.HasPrefix(configured, `openai_base_url = "`+baseURL+"\"\n") {
					t.Fatalf("openai_base_url was not inserted first:\n%s", configured)
				}
				for _, legacy := range []string{
					`approval_policy = "on-request"`, `[model_providers.gateway]`, `base_url = "https://legacy.example/v1"`,
				} {
					if !strings.Contains(configured, legacy) {
						t.Fatalf("legacy configuration %q was removed:\n%s", legacy, configured)
					}
				}
			},
		},
		{
			name:     "existing top level setting",
			original: stringPointerValue("approval_policy = \"on-request\"\nopenai_base_url = \"https://old.example/v1\" # old\n\n[features]\nresponses = true\n"),
			verify: func(t *testing.T, configured string) {
				if !strings.HasPrefix(configured, "approval_policy") {
					t.Fatalf("existing setting was not replaced in place:\n%s", configured)
				}
				if strings.Contains(configured, "https://old.example/v1") || strings.Count(configured, "openai_base_url =") != 1 {
					t.Fatalf("existing setting was not replaced exactly once:\n%s", configured)
				}
			},
		},
		{
			name:     "comment and nested setting",
			original: stringPointerValue("# openai_base_url = \"https://comment.example/v1\"\n[profile.local]\nopenai_base_url = \"https://nested.example/v1\"\n"),
			verify: func(t *testing.T, configured string) {
				if !strings.HasPrefix(configured, `openai_base_url = "`+baseURL+"\"\n") {
					t.Fatalf("top-level setting was not inserted:\n%s", configured)
				}
				for _, preserved := range []string{"https://comment.example/v1", "https://nested.example/v1"} {
					if !strings.Contains(configured, preserved) {
						t.Fatalf("non-top-level setting %q was changed:\n%s", preserved, configured)
					}
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			configDir := t.TempDir()
			configPath := filepath.Join(configDir, "config.toml")
			if test.original != nil {
				if err := os.WriteFile(configPath, []byte(*test.original), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			runCodexShellSetup(t, configDir, baseURL)
			configured, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			configuredValue := string(configured)
			test.verify(t, configuredValue)

			backup, err := os.ReadFile(configPath + ".bak")
			if test.original == nil {
				if !os.IsNotExist(err) {
					t.Fatalf("new configuration unexpectedly has a backup: %v", err)
				}
			} else if err != nil || string(backup) != *test.original {
				t.Fatalf("backup does not match original: err=%v backup=%q", err, backup)
			}

			runCodexShellSetup(t, configDir, baseURL)
			configuredAgain, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(configuredAgain) != configuredValue {
				t.Fatalf("repeated setup changed configuration:\nfirst:\n%s\nsecond:\n%s", configuredValue, configuredAgain)
			}
			backup, err = os.ReadFile(configPath + ".bak")
			if err != nil || string(backup) != configuredValue {
				t.Fatalf("repeated setup backup does not match previous configuration: err=%v backup=%q", err, backup)
			}
		})
	}
}

func runCodexShellSetup(t *testing.T, configDir, baseURL string) {
	t.Helper()
	command := exec.Command("sh", "-s", "--", baseURL)
	command.Stdin = strings.NewReader(string(codexShellSetupScript))
	command.Env = []string{"CODEX_HOME=" + configDir, "HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("configure script failed: %v\n%s", err, output)
	}
}

func stringPointerValue(value string) *string { return &value }

func TestCodexSetupDownloadsUseConfiguredPublicURL(t *testing.T) {
	t.Parallel()

	publicURL, err := url.Parse("https://codex.example.test/console")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{config: config.Config{PublicURL: publicURL}}

	shellRecorder := httptest.NewRecorder()
	server.codexShellSetup(shellRecorder, httptest.NewRequest("GET", "/setup/configure-codex.sh", nil))
	if shellRecorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("shell setup response is cacheable")
	}
	if body := shellRecorder.Body.String(); !strings.Contains(body, "https://codex.example.test/v1") || strings.Contains(body, codexGatewayBaseURLPlaceholder) {
		t.Fatalf("shell setup contains an unexpected base URL:\n%s", body)
	}
	if body := shellRecorder.Body.String(); !strings.Contains(body, `openai_base_url`) || strings.Contains(body, `[model_providers.gateway]`) {
		t.Fatal("shell setup does not use the simplified openai_base_url configuration")
	}

	windowsRecorder := httptest.NewRecorder()
	server.codexWindowsSetup(windowsRecorder, httptest.NewRequest("GET", "/setup/configure-codex.bat", nil))
	if got := windowsRecorder.Header().Get("Content-Disposition"); got != `attachment; filename="configure-codex.bat"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	windowsSetup := windowsRecorder.Body.String()
	if !strings.Contains(windowsSetup, "https://codex.example.test/v1") || strings.Contains(windowsSetup, codexGatewayBaseURLPlaceholder) {
		t.Fatalf("Windows setup contains an unexpected base URL:\n%s", windowsSetup)
	}
	if !strings.Contains(windowsSetup, `:POWERSHELL`) || !strings.Contains(windowsSetup, `config.toml.bak`) {
		t.Fatal("Windows setup is missing its embedded PowerShell configurator")
	}
	if !strings.Contains(windowsSetup, `openai_base_url`) || strings.Contains(windowsSetup, `[model_providers.gateway]`) {
		t.Fatal("Windows setup does not use the simplified openai_base_url configuration")
	}
}

func TestBillingDashboardIncludesReadOnlyAndOwnerWorkflows(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	for _, required := range []string{
		`data-section="billing"`, `id="billing-cash-balance"`, `id="billing-subscriptions"`,
		`id="billing-ledger-rows"`, `id="billing-user-select"`, `id="billing-rate-form"`,
		`id="billing-recharge-form"`, `id="billing-adjustment-form"`,
		`id="billing-subscription-day"`, `id="billing-subscription-week"`, `id="billing-subscription-month"`,
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
		`sensitiveAction(() => api(path, {method, body}))`,
		`period_count: billingPeriodCount(form)`, `当前第 ${period.current}/${period.count} 个周期`,
		`第 ${period.current} 个周期 · 无限期`, `本周期结束（最终失效）`, `最终失效：`,
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("billing dashboard JavaScript is missing %s", required)
		}
	}
	if count := strings.Count(html, `name="period_count"`); count != 3 {
		t.Fatalf("billing dashboard period-count input count = %d, want 3", count)
	}
	for _, forbidden := range []string{
		`Number(data.get("cny_amount"))`, `Number(data.get("usd_amount"))`, `Number(data.get("quota_usd"))`,
	} {
		if strings.Contains(javascript, forbidden) {
			t.Fatalf("billing dashboard converts an exact money string with %s", forbidden)
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
