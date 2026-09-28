package antigravity

import (
	"context"
	"strings"
	"testing"
)

func TestBridgeGeminiFlashClientPresets(t *testing.T) {
	for _, tc := range []struct{ model, budget string }{
		{"gemini-3.8-flash-high", "-1"},
		{"gemini-3.8-flash-medium", "4000"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			runner, capturePath := fakeRunner(t, fakeCLIConfig{
				Models: tc.model + " Gemini model\n",
				Stream: strings.Replace(initEvent, CLIModel, tc.model, 1) + resultEvent,
			})
			server := NewServer(runner, testBridgeToken)
			server.Refresh(context.Background())
			body := strings.TrimSuffix(nativeText, "}") + `,"generationConfig":{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":` + tc.budget + `}}}`
			for _, action := range []string{"generateContent", "streamGenerateContent?alt=sse"} {
				response := bridgeRequest(server, "POST", "/v1beta/models/"+tc.model+":"+action, body)
				if response.Code != 200 || !strings.Contains(response.Body.String(), `"modelVersion":"`+tc.model+`"`) {
					t.Fatalf("response=%d %s", response.Code, response.Body)
				}
				if !strings.Contains(strings.Join(readCapture(t, capturePath).Args, " "), "--model "+tc.model+" ") {
					t.Fatal("Flash preset did not reach the CLI")
				}
			}
			for _, bad := range []string{
				strings.Replace(body, "65536", "65537", 1),
				strings.Replace(body, `"thinkingBudget":`+tc.budget, `"thinkingBudget":4001`, 1),
				strings.Replace(body, `"includeThoughts":true`, `"includeThoughts":false`, 1),
			} {
				if _, failure := DecodeGeminiRequest(tc.model, []byte(bad)); failure == nil || failure.Code != "antigravity_generation_config_unsupported" {
					t.Fatalf("custom Flash controls accepted: %s", bad)
				}
			}
			if _, failure := DecodeGeminiRequest(PublicModel, []byte(body)); failure == nil {
				t.Fatal("Flash defaults changed Pro validation")
			}
		})
	}
}
