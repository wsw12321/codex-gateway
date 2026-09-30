package cpaprotocol

import (
	"errors"
	"strings"
	"testing"
)

func TestNativeGeminiToolsSignaturesAndControls(t *testing.T) {
	valid := `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"a","name":"lookup","args":{"query":"x"}},"thoughtSignature":"opaque-provider-signature"}]},{"role":"user","parts":[{"functionResponse":{"id":"a","name":"lookup","response":{"result":"yes"}}}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":{"type":"object","properties":{"query":{"type":"string"}}}}]}],"generationConfig":{"temperature":0.3,"maxOutputTokens":1234,"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"LOW"}}}`
	if err := GeminiRequest([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	missing := strings.Replace(valid, `,"thoughtSignature":"opaque-provider-signature"`, "", 1)
	if err := GeminiRequest([]byte(missing)); !errors.Is(err, ErrNewSession) {
		t.Fatalf("unsigned history = %v", err)
	}
	for _, body := range []string{
		strings.Replace(valid, `"args":{"query":"x"}`, `"args":{"query":"x","query":"y"}`, 1),
		strings.Replace(valid, `"functionDeclarations":`, `"googleSearch":{},"functionDeclarations":`, 1),
		strings.Replace(valid, `"functionResponse":{"id":"a"`, `"functionResponse":{"id":"different"`, 1),
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"abc"}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"x"}]}],"generationConfig":{"responseModalities":["AUDIO"]}}`,
		`{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"codeExecution":{}}]}`,
	} {
		if err := GeminiRequest([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestNativeResponsesOnlyTextAndClientFunctions(t *testing.T) {
	valid := `{"model":"gemini-pro-agent","input":[{"role":"user","content":"x"},{"type":"function_call","call_id":"a","name":"lookup","arguments":"{\"q\":1}"},{"type":"function_call_output","call_id":"a","output":"yes"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":true}],"reasoning":{"effort":"low"}}`
	if err := ResponsesRequest([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"model":"gemini-pro-agent","input":"x","tools":[{"type":"web_search"}]}`,
		`{"model":"gemini-pro-agent","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/a.png"}]}]}`,
		`{"model":"gemini-pro-agent","input":"x","service_tier":"priority"}`,
		`{"model":"gemini-pro-agent","input":"x","background":true}`,
	} {
		if err := ResponsesRequest([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if err := ResponsesRequest([]byte(`{"model":"gemini-pro-agent","input":"x","previous_response_id":"legacy-123"}`)); !errors.Is(err, ErrNewSession) {
		t.Fatalf("legacy continuation = %v", err)
	}
}

func TestObjectRejectsAmbiguity(t *testing.T) {
	for _, raw := range []string{`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `{"a":1} {}`, `[]`, "{\"a\":\"\xff\"}"} {
		if _, err := Object([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestNativeGeminiParallelToolSignatureAndSignatureOnlyReplay(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"a","name":"lookup","args":{}},"thoughtSignature":"opaque"},{"functionCall":{"id":"b","name":"lookup","args":{}}},{"thoughtSignature":"trailing"}]},{"role":"user","parts":[{"functionResponse":{"id":"a","name":"lookup","response":{}}},{"functionResponse":{"id":"b","name":"lookup","response":{}}}]}],"tools":[{"functionDeclarations":[{"name":"lookup"}]}]}`
	if err := GeminiRequest([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := GeminiRequest([]byte(strings.Replace(body, `,"thoughtSignature":"opaque"`, "", 1))); !errors.Is(err, ErrNewSession) {
		t.Fatalf("missing first signature=%v", err)
	}
	if err := GeminiRequest([]byte(strings.Replace(body, `{"role":"user","parts":[{"functionResponse"`, `{"role":"model","parts":[{"functionResponse"`, 1))); err == nil {
		t.Fatal("model supplied tool result accepted")
	}
	if err := GeminiRequest([]byte(`{"contents":[{"parts":[{"text":"hello"}]}]}`)); err != nil {
		t.Fatalf("optional role rejected: %v", err)
	}
}

func TestNativeResponsesControlsCheckedWithStringInput(t *testing.T) {
	for _, controls := range []string{`"stream":"true"`, `"max_output_tokens":-1`, `"reasoning":{"effort":"bogus"}`, `"text":{"format":{"type":"image"}}`, `"include":["web_search_call.action.sources"]`, `"parallel_tool_calls":{}`, `"metadata":{"key":42}`} {
		if err := ResponsesRequest([]byte(`{"model":"gemini-pro-agent","input":"hello",` + controls + `}`)); err == nil {
			t.Errorf("unchecked control %s", controls)
		}
	}
	if err := ResponsesRequest([]byte(`{"model":"gemini-pro-agent","input":"hello","temperature":null,"max_output_tokens":null,"reasoning":{"effort":"low"},"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}},"include":["reasoning.encrypted_content"]}`)); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	if err := ResponsesRequest([]byte(`{"model":"gemini-pro-agent","input":[{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"result"}]}]}`)); err != nil {
		t.Fatalf("text tool result rejected: %v", err)
	}
	if err := ResponsesRequest([]byte(`{"model":"gemini-pro-agent","input":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"a\":1,\"a\":2}"}],"tools":[{"type":"function","name":"lookup"}]}`)); err == nil {
		t.Fatal("ambiguous encoded tool arguments accepted")
	}
}
