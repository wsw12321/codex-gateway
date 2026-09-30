package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

func TestCPAAGYAliasesPreserveAdmissionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, alias, model, body, allowlist string
		noRoute, noPrice                    bool
		status                              int
		state                               string
		begins                              int64
	}{
		{name: "Flash high user permission", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: nativeFlashHigh, allowlist: `["gemini-3.8-flash-high"]`, status: 403, state: "PERMISSION_DENIED", begins: 1},
		{name: "Flash default uses high permission", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", allowlist: `["gemini-3.8-flash-high"]`, status: 403, state: "PERMISSION_DENIED", begins: 1},
		{name: "Flash high level uses high permission", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: `{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"}}}`, allowlist: `["gemini-3.8-flash-high"]`, status: 403, state: "PERMISSION_DENIED", begins: 1},
		{name: "exact native ID keeps custom budget", alias: "gemini-3.8-flash-high", model: "gemini-3.8-flash-high", body: strings.Replace(nativeFlashHigh, `"thinkingBudget":-1`, `"thinkingBudget":4001`, 1), allowlist: `["gemini-3.8-flash-high"]`, status: 403, state: "PERMISSION_DENIED", begins: 1},
		{name: "Flash alias cannot grant permission", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: nativeFlashHigh, allowlist: `["gemini-3.8-flash"]`, status: 403, state: "PERMISSION_DENIED"},
		{name: "title uses lite permission", alias: "gemini-3.1-flash-lite-preview", model: "gemini-3.1-flash-lite", allowlist: `["gemini-3.1-flash-lite"]`, status: 403, state: "PERMISSION_DENIED", begins: 1},
		{name: "title alias cannot grant permission", alias: "gemini-3.1-flash-lite-preview", model: "gemini-3.1-flash-lite", allowlist: `["gemini-3.1-flash-lite-preview"]`, status: 403, state: "PERMISSION_DENIED"},
		{name: "title cannot use main model permission", alias: "gemini-3.1-flash-lite-preview", model: "gemini-3.1-flash-lite", allowlist: `["gemini-3.8-flash-high"]`, status: 403, state: "PERMISSION_DENIED"},
		{name: "canonical route required", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: nativeFlashHigh, noRoute: true, status: 404, state: "NOT_FOUND"},
		{name: "canonical price required", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: nativeFlashHigh, noPrice: true, status: 404, state: "NOT_FOUND"},
		{name: "medium remains retired", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: nativeFlashMedium, status: 404, state: "NOT_FOUND"},
		{name: "custom budget is not high", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: strings.Replace(nativeFlashHigh, `"thinkingBudget":-1`, `"thinkingBudget":4001`, 1), status: 400, state: "INVALID_ARGUMENT"},
		{name: "conflicting level rejected", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: strings.Replace(nativeFlashHigh, `"thinkingBudget":-1`, `"thinkingBudget":-1,"thinkingLevel":"LOW"`, 1), status: 400, state: "INVALID_ARGUMENT"},
		{name: "duplicate budget rejected", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: strings.Replace(nativeFlashHigh, `"thinkingBudget":-1`, `"thinkingBudget":4000,"thinkingBudget":-1`, 1), status: 400, state: "INVALID_ARGUMENT"},
		{name: "malformed JSON rejected", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: `{"contents":`, status: 400, state: "INVALID_ARGUMENT"},
		{name: "body bound retained", alias: "gemini-3.8-flash", model: "gemini-3.8-flash-high", body: strings.Repeat("x", (1<<20)+1), status: 413, state: "INVALID_ARGUMENT"},
		{name: "old Pro alias remains retired", alias: "gemini-3.1-pro-preview", model: "gemini-pro-agent", status: 404, state: "NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponsesWebSocketTestHarness(t)
			h.server.config.AntigravityTransport = "cpa"
			h.server.config.UsagePricing = nativeGeminiPricing(t)
			if tc.noPrice {
				delete(h.server.config.UsagePricing.Models, tc.model)
			}
			if !tc.noRoute {
				h.server.config.AntigravityModelRoutes = map[string]string{tc.model: tc.model}
			}
			database := &geminiAdmissionTestConnector{auth: h.database, keyAllowlist: tc.allowlist, model: tc.model}
			db := sql.OpenDB(database)
			t.Cleanup(func() { _ = db.Close() })
			h.server.store = store.New(db)
			if tc.body == "" {
				tc.body = nativeGeminiText
			}
			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+tc.alias+":streamGenerateContent?alt=sse", strings.NewReader(tc.body))
			req.Header.Set("X-Goog-Api-Key", h.apiKey)
			response := httptest.NewRecorder()
			h.handler.ServeHTTP(response, req)
			checkNativeGeminiError(t, response, tc.status, tc.state)
			if h.upstreamCalls.Load() != 0 || database.writes.Load() != 0 || database.commits.Load() != 0 || database.begins.Load() != tc.begins || database.rollbacks.Load() != tc.begins {
				t.Fatal("rejected CLI alias crossed an admission boundary")
			}
		})
	}
}

// The real client uses synthetic credentials and a local Gateway fixture.
// Denied model permissions prove the captured CLI request passed schema and
// route validation without reserving quota or reaching an upstream provider.
func TestCPAAGYFlashRealCLIUsesCanonicalPermissions(t *testing.T) {
	binary := os.Getenv("AGY_CLI_TEST_BINARY")
	if binary == "" {
		t.Skip("AGY_CLI_TEST_BINARY is not set")
	}
	h := newResponsesWebSocketTestHarness(t)
	h.server.config.AntigravityTransport = "cpa"
	h.server.config.UsagePricing = nativeGeminiPricing(t)
	h.server.config.AntigravityModelRoutes = map[string]string{
		"gemini-3.8-flash-high": "gemini-3.8-flash-high",
		"gemini-3.1-flash-lite": "gemini-3.1-flash-lite",
	}
	fixture := &geminiAdmissionTestConnector{auth: h.database}
	connector := &cpaAGYCLIConnector{geminiAdmissionTestConnector: fixture}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	h.server.store = store.New(db)
	gateway := httptest.NewServer(h.handler)
	defer gateway.Close()
	root := t.TempDir()
	cliHome, project := filepath.Join(root, "home"), filepath.Join(root, "project")
	settings := filepath.Join(cliHome, ".gemini", "antigravity-cli")
	for _, path := range []string{settings, project} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(settings, "settings.json"), []byte(`{"modelProvider":"gemini"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--print", "Reply with exactly OK. Do not use tools.",
		"--model", "gemini-3.8-flash-high", "--output-format", "stream-json", "--mode", "plan",
		"--print-timeout", "15s", "--disable-slash-commands", "--log-file", filepath.Join(root, "cli.log"))
	command.Dir = project
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + cliHome,
		"TMPDIR=" + root, "TERM=dumb", "AGY_CLI_DISABLE_AUTO_UPDATE=true",
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"), "GEMINI_API_KEY=" + h.apiKey,
		"GOOGLE_GEMINI_BASE_URL=" + gateway.URL}
	output, err := command.Output()
	if err == nil || ctx.Err() != nil {
		t.Fatalf("expected the fixture's denied model permission; error=%v", err)
	}
	var reportedPermissionError bool
	for _, line := range strings.Split(string(output), "\n") {
		var event struct {
			Result struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Result.Status == "ERROR" && strings.Contains(event.Result.Error, "403") {
			reportedPermissionError = true
		}
	}
	if !reportedPermissionError || connector.mainReads.Load() == 0 {
		t.Fatalf("CLI did not reach the canonical Flash permission: reported403=%t reads=%d", reportedPermissionError, connector.mainReads.Load())
	}
	if fixture.writes.Load() != 0 || fixture.commits.Load() != 0 || h.upstreamCalls.Load() != 0 {
		t.Fatal("denied CLI request reserved quota or reached the upstream")
	}
}

type cpaAGYCLIConnector struct {
	*geminiAdmissionTestConnector
	mainReads atomic.Int64
}

func (c *cpaAGYCLIConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.geminiAdmissionTestConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return cpaAGYCLIConn{geminiAdmissionTestConn: conn.(geminiAdmissionTestConn), connector: c}, nil
}

type cpaAGYCLIConn struct {
	geminiAdmissionTestConn
	connector *cpaAGYCLIConnector
}

func (c cpaAGYCLIConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "SELECT a.enabled") {
		return c.geminiAdmissionTestConn.QueryContext(ctx, query, args)
	}
	if len(args) != 2 || args[0].Value != c.database.userID {
		return nil, errors.New("unexpected CLI permission lookup")
	}
	switch args[1].Value {
	case "gemini-3.8-flash-high":
		c.connector.mainReads.Add(1)
	case "gemini-3.1-flash-lite":
	default:
		return nil, errors.New("CLI permission used an unexpected model")
	}
	c.database.recordQuery(query)
	return &responsesWebSocketTestRows{columns: []string{"enabled"}, values: [][]driver.Value{{false}}}, nil
}
