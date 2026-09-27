package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/billing"
	"github.com/wsw/codex-gateway/internal/config"
)

func TestBridgeCachedCLIUsageAcrossProtocols(t *testing.T) {
	for _, test := range []struct {
		name, path, body string
		native, stream   bool
	}{
		{"Responses JSON", "/v1/responses", `{"model":"gemini-3.1-pro-preview","input":"hello"}`, false, false},
		{"Responses SSE", "/v1/responses", `{"model":"gemini-3.1-pro-preview","input":"hello","stream":true}`, false, true},
		{"Gemini JSON", "/v1beta/models/gemini-3.1-pro-preview:generateContent", nativeText, true, false},
		{"Gemini SSE", "/v1beta/models/gemini-3.1-pro-preview:streamGenerateContent?alt=sse", nativeText, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, _ := fakeRunner(t, fakeCLIConfig{Stream: initEvent + cachedResultEvent})
			server := NewServer(runner, testBridgeToken)
			server.Refresh(context.Background())
			response := bridgeRequest(server, "POST", test.path, test.body)
			if response.Code != 200 {
				t.Fatalf("response=%d %s", response.Code, response.Body)
			}
			data := response.Body.Bytes()
			if test.stream {
				data = nil
				for _, line := range strings.Split(response.Body.String(), "\n") {
					payload, found := strings.CutPrefix(line, "data: ")
					if !found || payload == "[DONE]" {
						continue
					}
					if test.native {
						data = []byte(payload)
						continue
					}
					var event struct {
						Type     string
						Response json.RawMessage
					}
					if err := json.Unmarshal([]byte(payload), &event); err != nil {
						t.Fatal(err)
					}
					if event.Type == "response.completed" {
						data = event.Response
					}
				}
			}
			var completed struct {
				Usage struct {
					Input        int64 `json:"input_tokens"`
					Output       int64 `json:"output_tokens"`
					Total        int64 `json:"total_tokens"`
					InputDetails struct {
						Cached int64 `json:"cached_tokens"`
					} `json:"input_tokens_details"`
					OutputDetails struct {
						Thinking int64 `json:"reasoning_tokens"`
					} `json:"output_tokens_details"`
				}
				UsageMetadata struct {
					PromptTokenCount, CandidatesTokenCount, ThoughtsTokenCount, CachedContentTokenCount, TotalTokenCount int64
				}
			}
			if err := json.Unmarshal(data, &completed); err != nil {
				t.Fatal(err)
			}
			if test.native {
				u := completed.UsageMetadata
				if u.PromptTokenCount != 13530 || u.CandidatesTokenCount != 2 || u.ThoughtsTokenCount != 173 || u.CachedContentTokenCount != 8089 || u.TotalTokenCount != 13705 {
					t.Fatalf("Gemini usage=%+v", u)
				}
			} else {
				u := completed.Usage
				if u.Input != 13530 || u.Output != 175 || u.Total != 13705 || u.InputDetails.Cached != 8089 || u.OutputDetails.Thinking != 173 {
					t.Fatalf("Responses usage=%+v", u)
				}
			}
			assertWorkspacesClean(t, runner)
		})
	}
}

func TestCLIUsageAccountingIncludesCacheInContextThreshold(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := config.ParseUsagePricing(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	snapshotJSON, _, found, err := pricing.ModelSnapshot(PublicModel)
	if err != nil || !found {
		t.Fatalf("model pricing: found=%t err=%v", found, err)
	}
	snapshot, err := config.ParsePricingSnapshot(snapshotJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, event, context, cost string
		input, cached, total       int64
	}{
		{"cache larger than uncached input", cachedResultEvent, config.ContextClassShort, "0.014599800000", 13530, 8089, 13705},
		{"at threshold including cache", resultWithUsage(`{"input_tokens":190000,"output_tokens":10100,"thinking_tokens":100,"cache_read_tokens":10000,"total_tokens":200100}`), config.ContextClassShort, "0.503200000000", 200000, 10000, 210100},
		{"cache crosses threshold", resultWithUsage(`{"input_tokens":190001,"output_tokens":10100,"thinking_tokens":100,"cache_read_tokens":10000,"total_tokens":200101}`), config.ContextClassLong, "0.945804000000", 200001, 10000, 210101},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, failure := parseStream(strings.NewReader(initEvent + test.event))
			if failure != nil {
				t.Fatal(failure)
			}
			u := result.Usage
			if u.InputTokens != test.input || u.CacheReadTokens != test.cached || u.TotalTokens != test.total {
				t.Fatalf("usage=%+v", u)
			}
			decision, err := snapshot.Select("default", u.InputTokens)
			if err != nil || decision.ContextClass != test.context {
				t.Fatalf("pricing=%+v err=%v", decision, err)
			}
			cost, err := billing.CalculateCostV2(u.InputTokens, u.CacheReadTokens, 0, u.OutputTokens,
				snapshot.Rule.CacheWriteMode, decision.InputUSDPerMillion, decision.CachedInputUSDPerMillion,
				decision.CacheWriteUSDPerMillion, decision.OutputUSDPerMillion)
			if err != nil || cost != test.cost {
				t.Fatalf("cost=%s want=%s err=%v", cost, test.cost, err)
			}
		})
	}
}
