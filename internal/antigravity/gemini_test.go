package antigravity

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const nativeText = `{"contents":[{"role":"user","parts":[{"text":"Hello 世界"}]}]}`
const nativeTools = `{"contents":[{"role":"user","parts":[{"text":"List tasks"}]}],"tools":[{"functionDeclarations":[{"name":"manage_task","parametersJsonSchema":{"type":"object","properties":{"Action":{"type":"string","enum":["list"]}},"required":["Action"],"additionalProperties":false}}]}]}`

type nativeFixture struct {
	Name string          `json:"name"`
	Path string          `json:"path"`
	Body json.RawMessage `json:"body"`
}

func nativeFixtures(t *testing.T) []nativeFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/agy-1.2.4-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []nativeFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestDecodeGeminiCapturedRequests(t *testing.T) {
	for _, fixture := range nativeFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			if fixture.Name == "title" {
				return
			}
			request, failure := DecodeGeminiRequest(PublicModel, fixture.Body)
			if failure != nil || request.Model != PublicModel || request.Stream {
				t.Fatalf("request=%+v failure=%+v", request, failure)
			}
			_, encoded, ok := strings.Cut(request.Prompt, "\n")
			var transcript struct {
				Instructions string                       `json:"instructions"`
				Contents     []map[string]json.RawMessage `json:"contents"`
			}
			if !ok || json.Unmarshal([]byte(encoded), &transcript) != nil || len(transcript.Contents) == 0 || !strings.Contains(transcript.Instructions, "Help with local programming tasks.") {
				t.Fatalf("invalid transcript: %s", request.Prompt)
			}
			if fixture.Name != "text" {
				for _, field := range []string{"parametersJsonSchema", "additionalProperties", "required", "ArtifactMetadata", "RequestFeedback", "manage_task"} {
					if !strings.Contains(transcript.Instructions, field) {
						t.Fatalf("lost schema field %s", field)
					}
				}
			}
			if strings.HasPrefix(fixture.Name, "tool_result") {
				for _, field := range []string{"probe_call_1", "functionResponse", "No background tasks", "toolAction", "toolSummary"} {
					if !strings.Contains(request.Prompt, field) {
						t.Fatalf("lost tool history field %s", field)
					}
				}
				if strings.Contains(request.Prompt, "external-session-signature") {
					t.Fatal("provider signature entered CLI prompt")
				}
			}
		})
	}
}

func TestDecodeGeminiSupportsBothSchemaFormats(t *testing.T) {
	for _, field := range []string{"parameters", "parametersJsonSchema"} {
		body := strings.ReplaceAll(nativeTools, "parametersJsonSchema", field)
		request, failure := DecodeGeminiRequest(PublicModel, []byte(body))
		if failure != nil || !strings.Contains(request.Prompt, "additionalProperties") {
			t.Fatalf("%s: failure=%+v prompt=%s", field, failure, request.Prompt)
		}
	}
}

func TestDecodeGeminiRejectsUnsupportedAndAmbiguousInput(t *testing.T) {
	cases := []string{
		`null`, `[]`, `{}`, `{"contents":[]}`, `{"contents":null}`, nativeText + ` {}`,
		`{"contents":[],"contents":[{"role":"user","parts":[{"text":"x"}]}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"x","text":"y"}]}]}`,
		strings.Replace(nativeText, `"text":"Hello 世界"`, `"inlineData":{"mimeType":"image/png","data":"AAAA"}`, 1),
		strings.Replace(nativeText, `"text":"Hello 世界"`, `"text":null`, 1),
		strings.Replace(nativeText, `"user"`, `"system"`, 1),
		strings.Replace(nativeText, `"text":"Hello 世界"`, `"text":"x","functionCall":{"name":"tool","args":{}}`, 1),
		strings.Replace(nativeTools, `"functionDeclarations"`, `"googleSearch"`, 1),
		strings.Replace(nativeTools, `"manage_task"`, `"bad tool"`, 1),
		strings.Replace(nativeTools, `"type":"object"`, `"type":"object","type":"string"`, 1),
		strings.Replace(nativeTools, `"parametersJsonSchema":{`, `"parameters":{},"parametersJsonSchema":{`, 1),
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"tool","args":{},"id":"x"}}]}]}`,
		`{"contents":[{"role":"model","parts":[{"functionResponse":{"name":"tool","response":{},"id":"x"}}]}]}`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"tool","args":{},"id":"x"}}]},{"role":"model","parts":[{"functionResponse":{"name":"other","response":{},"id":"x"}}]}]}`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"tool","args":{},"id":"x"}}]},{"role":"user","parts":[{"functionResponse":{"name":"tool","response":{},"id":"y"}}]}]}`,
	}
	for _, fields := range []string{
		`"model":"bad"`, `"cachedContent":"cache/1"`, `"safetySettings":[]`, `"tools":null`,
		`"generationConfig":null`, `"generationConfig":{"temperature":0.2}`, `"generationConfig":{"temperature":1.0000000000000001}`,
		`"generationConfig":{"maxOutputTokens":1024}`, `"generationConfig":{"candidateCount":2}`, `"generationConfig":{"responseMimeType":"application/json"}`,
		`"generationConfig":{"thinkingConfig":{"thinkingBudget":1024}}`, `"generationConfig":{"thinkingConfig":{"includeThoughts":false}}`,
		`"generationConfig":{"stopSequences":["STOP"]}`, `"toolConfig":{"functionCallingConfig":{"mode":"ANY"}}`,
		`"systemInstruction":{"parts":[{"inlineData":{}}]}`,
	} {
		cases = append(cases, strings.TrimSuffix(nativeText, "}")+","+fields+"}")
	}
	for _, body := range cases {
		if _, failure := DecodeGeminiRequest(PublicModel, []byte(body)); failure == nil || failure.Status != 400 {
			t.Fatalf("accepted invalid request: %s; failure=%+v", body, failure)
		}
	}
}

func TestDecodeGeminiMultipleToolResults(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"tool","args":{"n":1},"id":"one"}},{"functionCall":{"name":"tool","args":{"n":2},"id":"two"}}]},{"role":"model","parts":[{"functionResponse":{"name":"tool","response":{"n":2},"id":"two"}},{"functionResponse":{"name":"tool","response":{"n":1},"id":"one"}}]}]}`
	if _, failure := DecodeGeminiRequest(PublicModel, []byte(body)); failure != nil {
		t.Fatalf("out-of-order matching IDs rejected: %+v", failure)
	}
	noID := `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"tool","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"tool","response":{}}}]}]}`
	if _, failure := DecodeGeminiRequest(PublicModel, []byte(noID)); failure != nil {
		t.Fatalf("name-matched history rejected: %+v", failure)
	}
}

func TestBridgeGeminiJSONAndSSE(t *testing.T) {
	for _, method := range []string{"generateContent", "streamGenerateContent?alt=sse"} {
		for _, model := range []string{PublicModel} {
			t.Run(model+"/"+method, func(t *testing.T) {
				runner, _ := fakeRunner(t, fakeCLIConfig{Stream: initEvent + textStep + resultEvent, FragmentSize: 3})
				server := NewServer(runner, testBridgeToken)
				server.Refresh(context.Background())
				response := bridgeRequest(server, "POST", "/v1beta/models/"+model+":"+method, nativeText)
				if response.Code != 200 {
					t.Fatalf("response=%d %s", response.Code, response.Body)
				}
				data := response.Body.Bytes()
				if strings.HasPrefix(method, "stream") {
					if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") || !strings.HasPrefix(string(data), "data: ") || !strings.HasSuffix(string(data), "\n\n") || strings.Contains(string(data), "[DONE]") {
						t.Fatalf("bad SSE framing: %s", data)
					}
					data = []byte(strings.TrimSuffix(strings.TrimPrefix(string(data), "data: "), "\n\n"))
				}
				var result struct {
					Candidates []struct {
						Content struct {
							Role  string
							Parts []struct{ Text string }
						}
						FinishReason string
						Index        int
					}
					UsageMetadata map[string]int64
					ModelVersion  string
				}
				if json.Unmarshal(data, &result) != nil || len(result.Candidates) != 1 || result.Candidates[0].Content.Role != "model" || result.Candidates[0].Content.Parts[0].Text != "Hello 世界" || result.Candidates[0].FinishReason != "STOP" || result.ModelVersion != PublicModel {
					t.Fatalf("bad Gemini response: %s", data)
				}
				for key, value := range map[string]int64{"promptTokenCount": 18528, "candidatesTokenCount": 41, "thoughtsTokenCount": 616, "cachedContentTokenCount": 8113, "totalTokenCount": 19185} {
					if result.UsageMetadata[key] != value {
						t.Fatalf("usage %s=%d want %d", key, result.UsageMetadata[key], value)
					}
				}
				assertWorkspacesClean(t, runner)
			})
		}
	}
}

func TestBridgeGeminiCapturedToolLoop(t *testing.T) {
	responseText := "```json\n" + `{"type":"function_call","name":"manage_task","arguments":{"Action":"list","toolAction":"Listing tasks","toolSummary":"Task list"},"id":"probe_call_1"}` + "\n```"
	encoded, _ := json.Marshal(responseText)
	toolEvent := strings.Replace(resultEvent, `"Hello 世界"`, string(encoded), 1)
	runner, capturePath := fakeRunner(t, fakeCLIConfig{Stream: initEvent + toolEvent})
	server := NewServer(runner, testBridgeToken)
	server.Refresh(context.Background())
	for _, fixture := range nativeFixtures(t) {
		if fixture.Name == "text" {
			continue
		}
		path := fixture.Path
		if fixture.Name != "title" {
			// Captured client payloads remain useful, but their legacy aliases
			// must be replaced with the exact public model before forwarding.
			path = "/v1beta/models/" + PublicModel + ":streamGenerateContent?alt=sse"
		}
		response := bridgeRequest(server, "POST", path, string(fixture.Body))
		if fixture.Name == "title" {
			if response.Code != 404 {
				t.Fatalf("title=%d %s", response.Code, response.Body)
			}
			continue
		}
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"functionCall"`) || !strings.Contains(response.Body.String(), `"id":"probe_call_1"`) || strings.Contains(response.Body.String(), "thoughtSignature") {
			t.Fatalf("tool response=%d %s", response.Code, response.Body)
		}
		capture := readCapture(t, capturePath)
		if !capture.WorkspaceEmpty || !capture.SafePolicy || !capture.SecretsAbsent || !strings.Contains(capture.Prompt, "parametersJsonSchema") {
			t.Fatalf("unsafe or incomplete process invocation: %+v", capture)
		}
	}
}

func TestBridgeGeminiRejectsInvalidResultsBeforeSSE(t *testing.T) {
	for _, test := range []struct{ name, stream string }{
		{"duplicate result", initEvent + resultEvent + resultEvent},
		{"malformed tail", initEvent + resultEvent + "malformed\n"},
		{"server tool tail", initEvent + resultEvent + `{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"run_command"}}` + "\n"},
		{"unknown client tool", initEvent + strings.Replace(resultEvent, `"Hello 世界"`, `"{\"type\":\"function_call\",\"name\":\"undeclared\",\"arguments\":{}}"`, 1)},
		{"invalid client arguments", initEvent + strings.Replace(resultEvent, `"Hello 世界"`, `"{\"type\":\"function_call\",\"name\":\"manage_task\",\"arguments\":[]}"`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, _ := fakeRunner(t, fakeCLIConfig{Stream: test.stream})
			server := NewServer(runner, testBridgeToken)
			server.Refresh(context.Background())
			response := bridgeRequest(server, "POST", "/v1beta/models/"+PublicModel+":streamGenerateContent?alt=sse", nativeTools)
			if response.Code != 502 || strings.Contains(response.Body.String(), "data: ") || strings.Contains(response.Body.String(), "Hello 世界") {
				t.Fatalf("invalid result leaked: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestBridgeGeminiLimitsAndQuery(t *testing.T) {
	server := NewServer(&blockingExecutor{}, testBridgeToken)
	path := "/v1beta/models/" + PublicModel
	for _, suffix := range []string{":generateContent?alt=sse", ":streamGenerateContent?alt=json", ":streamGenerateContent?alt=sse&alt=sse", ":streamGenerateContent?secret=value", ":streamGenerateContent?bad;query"} {
		if response := bridgeRequest(server, "POST", path+suffix, nativeText); response.Code != 400 {
			t.Fatalf("query=%s status=%d", suffix, response.Code)
		}
	}
	if response := bridgeRequest(server, "POST", path+":generateContent", strings.Repeat(" ", 1<<20)+nativeText); response.Code != 413 {
		t.Fatalf("oversized=%d", response.Code)
	}
}

func TestBridgeGeminiSharesResponsesProcessSlot(t *testing.T) {
	executor := &blockingExecutor{entered: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(executor, testBridgeToken)
	server.Refresh(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- bridgeRequest(server, "POST", "/v1beta/models/"+PublicModel+":generateContent", nativeText)
	}()
	select {
	case <-executor.entered:
	case <-time.After(time.Second):
		t.Fatal("native request did not acquire process slot")
	}
	for _, path := range []string{"/v1/responses", "/v1beta/models/" + PublicModel + ":streamGenerateContent?alt=sse"} {
		body := &failOnRead{}
		request := httptest.NewRequest("POST", path, body)
		request.Header.Set("Authorization", "Bearer "+testBridgeToken)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 429 || response.Header().Get("Retry-After") != "1" || body.read.Load() {
			close(executor.release)
			t.Fatalf("process slot not shared or busy body read: path=%s status=%d", path, response.Code)
		}
	}
	close(executor.release)
	select {
	case response := <-done:
		if response.Code != 200 {
			t.Fatalf("initial native response=%d %s", response.Code, response.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("native process slot was not released")
	}
}

func TestBridgeGeminiTimeoutAndCancellation(t *testing.T) {
	runner, _ := fakeRunner(t, fakeCLIConfig{Hang: true})
	runner.Timeout = 30 * time.Millisecond
	server := NewServer(runner, testBridgeToken)
	server.Refresh(context.Background())
	path := "/v1beta/models/" + PublicModel + ":streamGenerateContent?alt=sse"
	response := bridgeRequest(server, "POST", path, nativeText)
	if response.Code != 504 || strings.Contains(response.Body.String(), "data: ") {
		t.Fatalf("timeout=%d %s", response.Code, response.Body)
	}
	assertWorkspacesClean(t, runner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest("POST", path, strings.NewReader(nativeText)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testBridgeToken)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, request)
	if w.Body.Len() != 0 {
		t.Fatalf("canceled request wrote response: %s", w.Body)
	}
	assertWorkspacesClean(t, runner)
}

func TestResponsesPreserveSchemaAndCallID(t *testing.T) {
	body := `{"model":"` + PublicModel + `","input":[{"type":"function_call","name":"exec","call_id":"same_call","arguments":{"cmd":"ls"}},{"type":"function_call_output","call_id":"same_call","output":"main.go"}],"tools":[{"type":"function","function":{"name":"exec","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"],"additionalProperties":false}}}]}`
	request, failure := DecodeRequest([]byte(body))
	if failure != nil || strings.Count(request.Prompt, "same_call") != 2 || !strings.Contains(request.Prompt, "additionalProperties") || !strings.Contains(request.Prompt, "required") {
		t.Fatalf("lost schema or call ID: failure=%+v prompt=%s", failure, request.Prompt)
	}
}

func TestClientFunctionCallValidation(t *testing.T) {
	names := map[string]struct{}{"exec": {}}
	for _, body := range []string{
		`{"type":"function_call","name":"other","arguments":{}}`,
		`{"type":"function_call","name":"exec","arguments":null}`,
		`{"type":"function_call","name":"exec","arguments":"{}"}`,
		`{"type":"function_call","name":"exec","arguments":{"x":1,"x":2}}`,
		`{"type":"function_call","name":"exec","arguments":{},"id":"a","call_id":"b"}`,
		`{"type":"function_call","name":"exec","arguments":{},"id":""}`,
	} {
		if _, failure := decodeClientFunctionCall(body, names); failure == nil {
			t.Fatalf("accepted invalid tool call: %s", body)
		}
	}
	call, failure := decodeClientFunctionCall(`{"type":"function_call","name":"exec","arguments":{"cmd":"ls"}}`, names)
	if failure != nil || call == nil || !validCallID(call.ID) {
		t.Fatalf("missing generated ID: %+v %+v", call, failure)
	}
}
