package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testConversationID = "6f66d8c4-20b7-4019-b4c0-d28f065e94bb"
const otherConversationID = "10d29ac3-b84c-41ee-b214-87a04ee8d74d"
const testConversationHash = "conv-5707ff5fa3162eba148ba5bf5b665d68"

func TestExtractGeminiConversationID(t *testing.T) {
	marker := "Conversation ID: " + testConversationID
	for _, test := range []struct {
		name  string
		parts []string
		want  string
	}{
		{"absent", nil, ""},
		{"title", []string{"Create a title for this conversation."}, ""},
		{"one_line", []string{marker}, testConversationID},
		{"surrounding_lines", []string{"System instructions\n" + marker + "\nContinue."}, testConversationID},
		{"whitespace_crlf", []string{"\t \r\n\t " + marker + " \t\r\n"}, testConversationID},
		{"uppercase_uuid", []string{"Conversation ID: " + strings.ToUpper(testConversationID)}, testConversationID},
		{"duplicate_lines", []string{marker + "\n" + marker}, testConversationID},
		{"duplicate_parts", []string{marker, "Conversation ID: " + strings.ToUpper(testConversationID)}, testConversationID},
		{"conflicting_lines", []string{marker + "\nConversation ID: " + otherConversationID}, ""},
		{"conflicting_parts", []string{marker, "Conversation ID: " + otherConversationID}, ""},
		{"empty_marker", []string{"Conversation ID: "}, ""},
		{"empty_after_valid", []string{marker, "Conversation ID: "}, ""},
		{"invalid_after_valid", []string{marker, "Conversation ID: not-a-uuid"}, ""},
		{"invalid_before_valid", []string{"Conversation ID: not-a-uuid", marker}, ""},
		{"invalid_hex", []string{"Conversation ID: gf66d8c4-20b7-4019-b4c0-d28f065e94bb"}, ""},
		{"invalid_hyphens", []string{"Conversation ID: 6f66d8c4_20b7-4019-b4c0-d28f065e94bb"}, ""},
		{"compact_uuid", []string{"Conversation ID: " + strings.ReplaceAll(testConversationID, "-", "")}, ""},
		{"uuid_urn", []string{"Conversation ID: urn:uuid:" + testConversationID}, ""},
		{"uuid_braces", []string{"Conversation ID: {" + testConversationID + "}"}, ""},
		{"inline_marker", []string{"Example " + marker}, ""},
		{"artifact_path", []string{"Artifact Directory Path: /tmp/agy/brain/" + testConversationID}, ""},
		{"unrelated_artifact_path", []string{marker, "Artifact Directory Path: /tmp/agy/brain/" + otherConversationID}, testConversationID},
		{"trailing_text", []string{marker + " additional text"}, ""},
		{"split_marker", []string{"Conversation ID:", testConversationID}, ""},
		{"wrong_case", []string{"conversation ID: " + testConversationID}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, failure := DecodeGeminiRequest(PublicModel, conversationBody(t, test.parts, ""))
			if failure != nil {
				t.Fatalf("valid request rejected: %+v", failure)
			}
			if request.conversationID != test.want {
				t.Fatalf("conversationID=%q want %q", request.conversationID, test.want)
			}
			_, transcript, _ := strings.Cut(request.Prompt, "\n")
			var decoded struct {
				Instructions string `json:"instructions"`
			}
			if err := json.Unmarshal([]byte(transcript), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Instructions != strings.Join(test.parts, "\n") {
				t.Fatalf("system instructions changed: got %q want %q", decoded.Instructions, strings.Join(test.parts, "\n"))
			}
		})
	}
}

func TestGeminiConversationIDOnlyUsesSystemText(t *testing.T) {
	toolLoop := `[{"role":"model","parts":[{"functionCall":{"name":"tool","id":"` + testConversationID + `","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"tool","id":"` + testConversationID + `","response":{}}}]}]`
	for _, contents := range []string{
		`[{"role":"user","parts":[{"text":"Conversation ID: ` + testConversationID + `"}]}]`,
		`[{"role":"model","parts":[{"text":"Conversation ID: ` + testConversationID + `"}]}]`,
		toolLoop,
	} {
		request, failure := DecodeGeminiRequest(PublicModel, conversationBody(t, nil, contents))
		if failure != nil || request.conversationID != "" {
			t.Fatalf("ordinary message or tool call became identity: request=%+v failure=%+v", request, failure)
		}
	}
	for _, contents := range []string{"", toolLoop} {
		request, failure := DecodeGeminiRequest(PublicModel, conversationBody(t, []string{"Conversation ID: " + testConversationID}, contents))
		if failure != nil || request.conversationID != testConversationID {
			t.Fatalf("main/tool request identity differs: request=%+v failure=%+v", request, failure)
		}
	}
	responseBody := `{"model":"` + PublicModel + `","input":"Conversation ID: ` + testConversationID + `","instructions":"Conversation ID: ` + testConversationID + `","conversation_id":"` + testConversationID + `"}`
	if request, failure := DecodeRequest([]byte(responseBody)); failure != nil || request.conversationID != "" {
		t.Fatalf("Responses request acquired a Gemini conversation identity: request=%+v failure=%+v", request, failure)
	}
}

func TestConversationHashRequiresUnambiguousGatewayScope(t *testing.T) {
	scope := strings.Repeat("a", 43)
	for _, test := range []struct {
		name   string
		header http.Header
		id     string
		want   string
	}{
		{"valid", http.Header{affinityHeader: {scope}}, testConversationID, testConversationHash},
		{"uppercase_uuid", http.Header{affinityHeader: {scope}}, strings.ToUpper(testConversationID), testConversationHash},
		{"missing_scope", nil, testConversationID, ""},
		{"empty_scope", http.Header{affinityHeader: {""}}, testConversationID, ""},
		{"short_scope", http.Header{affinityHeader: {scope[:42]}}, testConversationID, ""},
		{"long_scope", http.Header{affinityHeader: {scope + "a"}}, testConversationID, ""},
		{"invalid_scope", http.Header{affinityHeader: {strings.Repeat("a", 42) + "+"}}, testConversationID, ""},
		{"whitespace_scope", http.Header{affinityHeader: {" " + scope}}, testConversationID, ""},
		{"duplicate_scope", http.Header{affinityHeader: {scope, scope}}, testConversationID, ""},
		{"combined_scope", http.Header{affinityHeader: {scope + "," + scope}}, testConversationID, ""},
		{"case_duplicate_scope", http.Header{affinityHeader: {scope}, strings.ToLower(affinityHeader): {scope}}, testConversationID, ""},
		{"empty_uuid", http.Header{affinityHeader: {scope}}, "", ""},
		{"invalid_uuid", http.Header{affinityHeader: {scope}}, "invalid", ""},
		{"forged_hash", http.Header{affinityHeader: {scope}, conversationHashHeader: {"conv-" + strings.Repeat("f", 32)}}, testConversationID, testConversationHash},
		{"forged_without_scope", http.Header{conversationHashHeader: {testConversationHash}}, testConversationID, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := conversationHash(test.header, test.id); got != test.want {
				t.Fatalf("hash=%q want %q", got, test.want)
			}
		})
	}
	for _, input := range []struct{ scope, id string }{
		{strings.Repeat("b", 43), testConversationID},
		{scope, otherConversationID},
	} {
		if got := conversationHash(http.Header{affinityHeader: {input.scope}}, input.id); got == "" || got == testConversationHash {
			t.Fatalf("different API key or conversation reused hash: %q", got)
		}
	}
}

type conversationExecutor struct {
	prompt string
}

func (*conversationExecutor) Check(context.Context) ([]string, error) {
	return []string{PublicModel}, nil
}

func (e *conversationExecutor) Run(_ context.Context, _, prompt string) (Result, *Failure) {
	e.prompt = prompt
	return Result{Status: "SUCCESS", Response: "ok", Usage: &Usage{}}, nil
}

func TestBridgeGeminiConversationHashJSONAndSSE(t *testing.T) {
	for _, method := range []string{"generateContent", "streamGenerateContent?alt=sse"} {
		t.Run(method, func(t *testing.T) {
			executor := &conversationExecutor{}
			server := NewServer(executor, testBridgeToken)
			server.Refresh(context.Background())
			for _, test := range []struct {
				name   string
				parts  []string
				scopes []string
				want   string
			}{
				{"main", []string{"Conversation ID: " + testConversationID}, []string{strings.Repeat("a", 43)}, testConversationHash},
				{"title", []string{"Create a title for this conversation."}, []string{strings.Repeat("a", 43)}, ""},
				{"no_scope", []string{"Conversation ID: " + testConversationID}, nil, ""},
				{"invalid_scope", []string{"Conversation ID: " + testConversationID}, []string{"invalid"}, ""},
				{"duplicate_scope", []string{"Conversation ID: " + testConversationID}, []string{strings.Repeat("a", 43), strings.Repeat("a", 43)}, ""},
			} {
				t.Run(test.name, func(t *testing.T) {
					body := conversationBody(t, test.parts, "")
					req := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+PublicModel+":"+method, strings.NewReader(string(body)))
					req.Header.Set("Authorization", "Bearer "+testBridgeToken)
					for _, scope := range test.scopes {
						req.Header.Add(affinityHeader, scope)
					}
					req.Header.Set(conversationHashHeader, "conv-"+strings.Repeat("f", 32))
					response := httptest.NewRecorder()
					server.ServeHTTP(response, req)
					if response.Code != http.StatusOK || response.Header().Get(conversationHashHeader) != test.want {
						t.Fatalf("response=%d hash=%q want %q body=%s", response.Code, response.Header().Get(conversationHashHeader), test.want, response.Body)
					}
					if strings.Contains(response.Body.String(), testConversationID) || strings.Contains(response.Body.String(), "conv-") {
						t.Fatalf("conversation identity leaked into response body: %s", response.Body)
					}
					decoded, failure := DecodeGeminiRequest(PublicModel, body)
					if failure != nil || decoded.Prompt != executor.prompt {
						t.Fatal("conversation extraction changed the process prompt")
					}
					wantContentType := "application/json"
					if strings.HasPrefix(method, "stream") {
						wantContentType = "text/event-stream"
					}
					if !strings.HasPrefix(response.Header().Get("Content-Type"), wantContentType) {
						t.Fatalf("unexpected content type: %q", response.Header().Get("Content-Type"))
					}
				})
			}
		})
	}
}

func conversationBody(t *testing.T, texts []string, contents string) []byte {
	t.Helper()
	if contents == "" {
		contents = `[{"role":"user","parts":[{"text":"hello"}]}]`
	}
	body := map[string]any{"contents": json.RawMessage(contents)}
	if len(texts) != 0 {
		parts := make([]map[string]string, len(texts))
		for i, text := range texts {
			parts[i] = map[string]string{"text": text}
		}
		body["systemInstruction"] = map[string]any{"parts": parts}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
