package antigravity

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeRequestTextConversation(t *testing.T) {
	for _, input := range []string{
		`"Hello 世界"`,
		`[{"role":"user","content":"Hello 世界"}]`,
		`[{"type":"message","role":"developer","content":[{"type":"input_text","text":"Be concise"}]},{"role":"assistant","content":[{"type":"output_text","text":"Ready"}]},{"role":"user","content":[{"type":"input_text","text":"Hello "},{"type":"input_text","text":"世界"}]}]`,
	} {
		request, failure := DecodeRequest([]byte(`{"model":"` + PublicModel + `","input":` + input + `,"instructions":"Be accurate","stream":true,"store":false,"service_tier":"default"}`))
		if failure != nil || !request.Stream || request.Model != PublicModel {
			t.Fatalf("request=%+v failure=%+v", request, failure)
		}
		_, encoded, ok := strings.Cut(request.Prompt, "\n")
		var transcript struct {
			Instructions string        `json:"instructions"`
			Messages     []textMessage `json:"messages"`
		}
		if !ok || json.Unmarshal([]byte(encoded), &transcript) != nil || transcript.Instructions != "Be accurate" || len(transcript.Messages) == 0 || transcript.Messages[len(transcript.Messages)-1].Content != "Hello 世界" {
			t.Fatalf("text conversation lost role or content: %s", request.Prompt)
		}
	}
	request, failure := DecodeRequest([]byte(`{"model":"` + PublicModel + `","input":"/logout"}`))
	if failure != nil || strings.HasPrefix(request.Prompt, "/") || !strings.Contains(request.Prompt, "/logout") {
		t.Fatalf("slash command was not safely wrapped: %+v %+v", request, failure)
	}
}

func TestDecodeRequestRejectsUnsupportedInput(t *testing.T) {
	for _, test := range []struct{ name, fields, code string }{
		{"tools", `"input":"x","tools":[]`, "tools"},
		{"continuation", `"input":"x","previous_response_id":"resp_1"`, "previous_response_id"},
		{"token limit", `"input":"x","max_output_tokens":100`, "max_output_tokens"},
		{"stored response", `"input":"x","store":true`, "store"},
		{"null store", `"input":"x","store":null`, "store"},
		{"tier", `"input":"x","service_tier":"priority"`, "service_tier"},
		{"null instructions", `"input":"x","instructions":null`, "instructions"},
		{"nonboolean stream", `"input":"x","stream":"true"`, "stream"},
		{"null stream", `"input":"x","stream":null`, "stream"},
		{"image", `"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/a"}]}]`, "input"},
		{"file", `"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]`, "input"},
		{"function", `"input":[{"type":"function_call_output","call_id":"call_1","output":"x"}]`, "input"},
		{"tool role", `"input":[{"role":"tool","content":"x"}]`, "input"},
		{"null input", `"input":null`, "input"},
		{"empty input array", `"input":[]`, "input"},
		{"null text", `"input":[{"role":"user","content":[{"type":"input_text","text":null}]}]`, "input"},
		{"unknown field", `"input":"x","attacker_supplied_secret":true`, "parameter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, failure := DecodeRequest([]byte(`{"model":"` + PublicModel + `",` + test.fields + `}`))
			if failure == nil || failure.Status != 400 || failure.Code != "antigravity_"+test.code+"_unsupported" {
				t.Fatalf("failure=%+v", failure)
			}
		})
	}
	for _, body := range []string{
		`{"model":"` + PublicModel + `","input":"x","input":"y"}`,
		`{"model":"` + PublicModel + `","input":[{"role":"user","role":"system","content":"x"}]}`,
		`{"model":"` + PublicModel + `","input":"x"} {}`,
		`{"model":`,
	} {
		if _, failure := DecodeRequest([]byte(body)); failure == nil || failure.Status != 400 || failure.Code != "antigravity_invalid_request" {
			t.Fatalf("invalid JSON accepted: %s; failure=%+v", body, failure)
		}
	}
}
