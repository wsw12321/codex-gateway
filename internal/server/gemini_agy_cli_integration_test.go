//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

// AGY_CLI_TEST_BINARY opts into the installed official 1.2.12 client. The test
// uses an isolated home and synthetic credentials against the real gateway and
// database lifecycle. Its only client tool is listing in-memory task state.
func testNativeAGYCLI(t *testing.T, handler http.Handler, repository *store.Store, userID, keyID, apiKey string, executor *nativeLifecycleExecutor) {
	t.Helper()
	binary := os.Getenv("AGY_CLI_TEST_BINARY")
	if binary == "" {
		t.Skip("AGY_CLI_TEST_BINARY is not set; real AGY 1.2.12 test is opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	home, project := filepath.Join(root, "用户 home"), filepath.Join(root, "project")
	settingsDir := filepath.Join(home, ".gemini", "antigravity-cli")
	for _, dir := range []string{settingsDir, project} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	settings := `{"modelProvider":"gemini","toolPermission":"strict","allowNonWorkspaceAccess":false,"enableTelemetry":false,"useG1Credits":false,"permissions":{"deny":["read_file(*)","write_file(*)","read_url(*)","execute_url(*)","command(*)","unsandboxed(*)","mcp(*)"],"allow":[],"ask":[]}}`
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PutSubscription(ctx, store.PutSubscriptionParams{
		BillingWriteParams: store.BillingWriteParams{OperationID: uuid.NewString(), ActorUserID: userID, Reason: "real AGY CLI synthetic protocol test"},
		UserID:             userID, Tier: store.BillingTierDay, AllowanceUSD: "1",
	}); err != nil {
		t.Fatal(err)
	}
	executor.started, executor.failure = nil, nil
	executor.responseForPrompt = func(prompt string) string {
		if strings.Contains(prompt, `"functionResponse":`) || !strings.Contains(prompt, "manage_task") {
			return "OK"
		}
		return `{"type":"function_call","name":"manage_task","arguments":{"Action":"list","toolSummary":"Task list","toolAction":"Listing tasks"}}`
	}
	type capture struct {
		path, requestID string
		toolResult      bool
	}
	var mu sync.Mutex
	var captures []capture
	clientServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		_ = r.Body.Close()
		if err != nil {
			t.Errorf("capture AGY body: %v", err)
			http.Error(w, "capture failed", 500)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		handler.ServeHTTP(w, r)
		mu.Lock()
		captures = append(captures, capture{r.URL.Path, w.Header().Get(httpx.RequestIDHeader), bytes.Contains(data, []byte(`"functionResponse":`))})
		mu.Unlock()
	}))
	defer clientServer.Close()
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "USER=agy-test", "TMPDIR=" + root,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"), "TERM=dumb", "AGY_CLI_DISABLE_AUTO_UPDATE=true",
		"GEMINI_API_KEY=" + apiKey, "GOOGLE_GEMINI_BASE_URL=" + clientServer.URL,
	}
	version := exec.CommandContext(ctx, binary, "--version")
	version.Env, version.Dir = env, project
	versionOut, err := version.CombinedOutput()
	if err != nil || strings.TrimSpace(string(versionOut)) != "1.2.12" {
		t.Fatalf("expected official AGY 1.2.12, got %q: %v", versionOut, err)
	}
	command := exec.CommandContext(ctx, binary,
		"--print", "Reply with exactly OK. Only listing in-memory task state is allowed for this local protocol test.",
		"--model", config.LegacyAntigravityPublicModel, "--output-format", "json", "--mode", "plan",
		"--print-timeout", "15s", "--disable-slash-commands", "--log-file", filepath.Join(root, "cli.log"))
	command.Env, command.Dir = env, project
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	clientServer.Close()
	if err != nil {
		t.Fatalf("AGY protocol loop failed: %v\n%s\n%s", err, output, &stderr)
	}
	var result struct {
		Status   string `json:"status"`
		Response string `json:"response"`
	}
	if err := json.Unmarshal(output, &result); err != nil || result.Status != "SUCCESS" || strings.TrimSpace(result.Response) != "OK" {
		t.Fatalf("unexpected AGY result: %s (%v)", output, err)
	}
	mu.Lock()
	defer mu.Unlock()
	var mainCalls, titleCalls, toolResults int
	for _, record := range captures {
		switch record.path {
		case "/v1beta/models/gemini-3.1-pro-preview:streamGenerateContent", "/v1beta/models/gemini-3.1-pro-preview-customtools:streamGenerateContent":
			mainCalls++
		case "/v1beta/models/gemini-3.1-flash-lite-preview:streamGenerateContent":
			titleCalls++
		default:
			t.Fatalf("unexpected AGY request: %s", record.path)
		}
		if record.toolResult {
			toolResults++
		}
		if err := repository.SettleRequest(ctx, record.requestID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		var model, state, cost string
		if err := repository.DB().QueryRowContext(ctx, `SELECT u.model,u.state,l.amount_usd::text FROM usage_requests u JOIN billing_ledger_entries l USING(request_id) WHERE u.request_id=$1 AND u.api_key_id=$2`, record.requestID, keyID).Scan(&model, &state, &cost); err != nil {
			t.Fatalf("missing AGY request settlement for %s: %v", record.path, err)
		}
		if model != config.LegacyAntigravityPublicModel || state != "completed" || cost != "0.000262000000" {
			t.Fatalf("AGY request settlement for %s: model=%s state=%s cost=%s", record.path, model, state, cost)
		}
	}
	if mainCalls != 2 || titleCalls != 1 || toolResults != 1 {
		t.Fatalf("incomplete AGY tool loop: main=%d title=%d tool_result=%d", mainCalls, titleCalls, toolResults)
	}
	t.Logf("AGY %s completed text, title and manage_task loop; all %d requests settled as %s", strings.TrimSpace(string(versionOut)), len(captures), config.LegacyAntigravityPublicModel)
}
