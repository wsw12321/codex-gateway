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

func TestDecodeRequestCodexCLI(t *testing.T) {
	body := `{"model":"` + PublicModel + `","input":[{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"hello codex"}]}],"tools":[{"type":"function","function":{"name":"read_file","description":"read a file"}}],"stream":true}`
	req, failure := DecodeRequest([]byte(body))
	if failure != nil {
		t.Fatalf("unexpected failure: %+v", failure)
	}
	if !req.Stream || !strings.Contains(req.Prompt, "read_file") || !strings.Contains(req.Prompt, "hello codex") {
		t.Fatalf("unexpected prompt: %s", req.Prompt)
	}
}

func TestDecodeRequestFunctionCallRoundTrip(t *testing.T) {
	body := `{"model":"` + PublicModel + `","input":[{"type":"function_call","name":"exec","arguments":"{\"cmd\":\"ls\"}"},{"type":"function_call_output","call_id":"call_1","output":"main.go"}]}`
	req, failure := DecodeRequest([]byte(body))
	if failure != nil {
		t.Fatalf("unexpected failure: %+v", failure)
	}
	if !strings.Contains(req.Prompt, "exec") || !strings.Contains(req.Prompt, "main.go") {
		t.Fatalf("unexpected prompt: %s", req.Prompt)
	}
}

func TestDecodeRequestRejectsUnsupportedInput(t *testing.T) {
	for _, test := range []struct{ name, fields, code string }{
		{"nonboolean stream", `"input":"x","stream":"true"`, "stream"},
		{"null stream", `"input":"x","stream":null`, "stream"},
		{"null input", `"input":null`, "input"},
		{"empty input array", `"input":[]`, "input"},
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
