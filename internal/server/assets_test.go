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
		`data-section="guide"`, `id="guide-base-url"`, `id="guide-project-select"`,
		`id="guide-shell-code"`, `id="guide-powershell-code"`, `id="guide-install-code"`, `id="guide-config-code"`,
		`id="guide-windows-download"`, `href="/setup/configure-codex.bat"`,
		`~/.codex/config.toml`, `不要把 API Key 写入 Git 仓库`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("Codex guide HTML is missing %s", required)
		}
	}
	for _, required := range []string{
		`guide: "使用指导"`, `function renderGuide()`, `` + "`base_url = \"${baseURL}\"`" + ``,
		`'env_key = "CODEX_GATEWAY_API_KEY"'`, `'wire_api = "responses"'`,
		`'env_http_headers = { "X-Codex-Project" = "CODEX_GATEWAY_PROJECT" }'`,
		`/setup/configure-codex.sh`,
		`all("[data-copy-target]")`,
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("Codex guide JavaScript is missing %s", required)
		}
	}
}

func TestCodexShellSetupUpdatesOnlyGatewayConfiguration(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.toml")
	original := "approval_policy = \"on-request\"\nmodel_provider = \"old\"\n\n[model_providers.other]\nname = \"Other\"\n\n[model_providers.gateway]\nbase_url = \"https://old.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", "-s", "--", "https://gateway.example/v1")
	command.Stdin = strings.NewReader(string(codexShellSetupScript))
	command.Env = []string{
		"CODEX_HOME=" + configDir,
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("configure script failed: %v\n%s", err, output)
	}

	configured, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	value := string(configured)
	for _, required := range []string{
		`approval_policy = "on-request"`, `[model_providers.other]`, `name = "Other"`,
		`model_provider = "gateway"`, `[model_providers.gateway]`, `base_url = "https://gateway.example/v1"`,
	} {
		if !strings.Contains(value, required) {
			t.Fatalf("configured TOML is missing %q:\n%s", required, value)
		}
	}
	if strings.Contains(value, "https://old.example/v1") || strings.Count(value, `[model_providers.gateway]`) != 1 {
		t.Fatalf("old or duplicate Gateway configuration remains:\n%s", value)
	}
	if _, err := os.Stat(configPath + ".bak"); err != nil {
		t.Fatalf("configuration backup was not created: %v", err)
	}
}

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
		`name="reason" required`, `class="owner-only hidden billing-owner-tools"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("billing dashboard HTML is missing %s", required)
		}
	}
	for _, required := range []string{
		`/admin/billing/me`, `/admin/billing/settings`, `/admin/billing/users`,
		`/recharges`, `/adjustments`, `/subscriptions/`, `crypto.randomUUID()`,
		`sensitiveAction(() => api(path, {method, body}))`,
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("billing dashboard JavaScript is missing %s", required)
		}
	}
	for _, forbidden := range []string{
		`Number(data.get("cny_amount"))`, `Number(data.get("usd_amount"))`, `Number(data.get("quota_usd"))`,
	} {
		if strings.Contains(javascript, forbidden) {
			t.Fatalf("billing dashboard converts an exact money string with %s", forbidden)
		}
	}
}
