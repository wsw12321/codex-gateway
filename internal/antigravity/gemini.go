package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/wsw/codex-gateway/internal/config"
)

// DecodeGeminiRequest validates the native body before quota admission and
// builds a transcript for the same isolated, single-turn process as Responses.
// The HTTP handler supplies the model and streaming mode from the URL.
func DecodeGeminiRequest(model string, body []byte) (Request, *Failure) {
	out := Request{Model: model}
	if !config.IsLegacyAntigravityModel(model) {
		return out, unsupported("model")
	}
	if uniqueJSON(body) != nil {
		return out, &Failure{400, "antigravity_invalid_request", "Request must be one JSON object without duplicate keys"}
	}
	fields, ok := object(body)
	if !ok || !onlyKeys(fields, "contents", "systemInstruction", "tools", "toolConfig", "generationConfig") {
		return out, unsupported("parameter")
	}
	if raw, exists := fields["generationConfig"]; exists && !defaultGeminiGeneration(model, raw) {
		return out, unsupported("generation_config")
	}
	if raw, exists := fields["toolConfig"]; exists {
		config, ok := object(raw)
		if !ok || !onlyKeys(config, "functionCallingConfig") {
			return out, unsupported("tool_config")
		}
		if raw, exists := config["functionCallingConfig"]; exists {
			calling, ok := object(raw)
			if !ok || !onlyKeys(calling, "mode") || !jsonEqual(calling["mode"], `"AUTO"`) {
				return out, unsupported("tool_config")
			}
		}
	}
	declarations, names, failure := decodeGeminiTools(fields["tools"])
	if failure != nil {
		return out, failure
	}
	out.toolNames = names
	var system []string
	if raw, exists := fields["systemInstruction"]; exists {
		instruction, ok := object(raw)
		if !ok || !onlyKeys(instruction, "role", "parts") {
			return out, unsupported("system_instruction")
		}
		if raw, exists := instruction["role"]; exists {
			role, ok := stringValue(raw)
			if !ok || (role != "system" && role != "user" && role != "") {
				return out, unsupported("system_instruction")
			}
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(instruction["parts"], &parts) != nil || len(parts) == 0 {
			return out, unsupported("system_instruction")
		}
		for _, part := range parts {
			text, ok := stringValue(part["text"])
			if !ok || !onlyKeys(part, "text") {
				return out, unsupported("system_instruction")
			}
			system = append(system, text)
		}
	}
	out.conversationID = extractConversationID(system)
	contents, failure := decodeGeminiContents(fields["contents"])
	if failure != nil {
		return out, failure
	}
	transcript := struct {
		Instructions string            `json:"instructions,omitempty"`
		Contents     []json.RawMessage `json:"contents"`
	}{strings.Join(system, "\n") + clientToolPrompt(declarations), contents}
	encoded, _ := json.Marshal(transcript)
	out.Prompt = "Answer the following Gemini conversation. Follow its instructions and roles (model means assistant). functionCall records a previous client tool invocation; functionResponse records its matching result, even when its role is model. Return only the assistant's answer. Client tools run on the user's computer. Do not use server tools, access files, run commands, browse URLs, or delegate to agents.\n" + string(encoded)
	return out, nil
}

func object(raw []byte) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	err := json.Unmarshal(raw, &fields)
	return fields, err == nil && fields != nil
}

func onlyKeys(fields map[string]json.RawMessage, allowed ...string) bool {
	for key := range fields {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func stringValue(raw []byte) (string, bool) {
	var value string
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func jsonEqual(raw []byte, expected string) bool {
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return false
	}
	return compact.String() == expected
}

// These are the defaults actually emitted by AGY 1.2.4. They describe its
// ordinary main conversation and do not override the server's fixed model.
// Custom controls cannot be honored by the CLI and must fail before billing.
func defaultGeminiGeneration(model string, raw []byte) bool {
	fields, ok := object(raw)
	if !ok {
		return false
	}
	defaults := map[string]string{
		"candidateCount": "1", "maxOutputTokens": "65535", "temperature": "1", "topK": "50", "topP": "1",
		"stopSequences": `["<|user|>","<|bot|>","<|context_request|>","<|endoftext|>","<|end_of_turn|>"]`,
	}
	for key, value := range fields {
		// AGY 1.2.12's Flash presets use a 65536 output limit. Medium also
		// emits a 4000 thinking budget; the exact CLI model enforces that preset.
		if key == "maxOutputTokens" && (model == "gemini-3.8-flash-high" || model == "gemini-3.8-flash-medium") && jsonEqual(value, "65536") {
			continue
		}
		if key == "thinkingConfig" {
			thinking, ok := object(value)
			if !ok || !onlyKeys(thinking, "includeThoughts", "thinkingBudget") {
				return false
			}
			for key, raw := range thinking {
				if (key == "includeThoughts" && !jsonEqual(raw, "true")) ||
					(key == "thinkingBudget" && !jsonEqual(raw, "-1") && !(model == "gemini-3.8-flash-medium" && jsonEqual(raw, "4000"))) {
					return false
				}
			}
			continue
		}
		if expected, exists := defaults[key]; !exists || !jsonEqual(value, expected) {
			return false
		}
	}
	return true
}

func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i, c := range name {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || i > 0 && (c >= '0' && c <= '9' || c == '.' || c == ':' || c == '-') {
			continue
		}
		return false
	}
	return true
}

func validCallID(id string) bool {
	if len(id) == 0 || len(id) > 256 || strings.TrimSpace(id) != id {
		return false
	}
	for _, c := range id {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func decodeGeminiTools(raw []byte) ([]map[string]json.RawMessage, map[string]struct{}, *Failure) {
	names := map[string]struct{}{}
	if len(raw) == 0 {
		return nil, names, nil
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || tools == nil {
		return nil, nil, unsupported("tools")
	}
	var declarations []map[string]json.RawMessage
	for _, tool := range tools {
		var functions []map[string]json.RawMessage
		if !onlyKeys(tool, "functionDeclarations") || json.Unmarshal(tool["functionDeclarations"], &functions) != nil || len(functions) == 0 {
			return nil, nil, unsupported("tools")
		}
		for _, function := range functions {
			if !validDeclaration(function, names) {
				return nil, nil, unsupported("tools")
			}
			declarations = append(declarations, function)
		}
	}
	return declarations, names, nil
}

func validDeclaration(function map[string]json.RawMessage, names map[string]struct{}) bool {
	name, ok := stringValue(function["name"])
	if !ok || !validToolName(name) || !onlyKeys(function, "name", "description", "parameters", "parametersJsonSchema") {
		return false
	}
	if _, exists := names[name]; exists {
		return false
	}
	if raw, exists := function["description"]; exists {
		if _, ok := stringValue(raw); !ok {
			return false
		}
	}
	_, parameters := function["parameters"]
	_, schema := function["parametersJsonSchema"]
	if parameters && schema {
		return false
	}
	for _, key := range []string{"parameters", "parametersJsonSchema"} {
		if raw, exists := function[key]; exists {
			if _, ok := object(raw); !ok {
				return false
			}
		}
	}
	names[name] = struct{}{}
	return true
}

func decodeResponseTools(raw []byte) ([]map[string]json.RawMessage, map[string]struct{}, *Failure) {
	names := map[string]struct{}{}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, names, nil
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, nil, unsupported("tools")
	}
	var declarations []map[string]json.RawMessage
	for _, tool := range tools {
		if !jsonEqual(tool["type"], `"function"`) {
			return nil, nil, unsupported("tools")
		}
		var function map[string]json.RawMessage
		if nested, exists := tool["function"]; exists {
			if !onlyKeys(tool, "type", "function") {
				return nil, nil, unsupported("tools")
			}
			function, _ = object(nested)
		} else {
			function = tool
			delete(function, "type")
		}
		if raw, exists := function["strict"]; exists {
			if !isBool(raw) {
				return nil, nil, unsupported("tools")
			}
			delete(function, "strict")
		}
		if !validDeclaration(function, names) {
			return nil, nil, unsupported("tools")
		}
		declarations = append(declarations, function)
	}
	return declarations, names, nil
}

func clientToolPrompt(declarations []map[string]json.RawMessage) string {
	if len(declarations) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(declarations)
	return "\nAvailable client tool declarations (including complete argument schemas):\n" + string(encoded) + "\nTo invoke a declared client tool, return ONLY a JSON code block containing {\"type\":\"function_call\",\"name\":\"<declared_tool_name>\",\"arguments\":{...}}. Tool arguments must follow its declared schema. Invoke one tool at a time; never invoke an undeclared tool. Otherwise return your answer directly as text."
}

func decodeGeminiContents(raw []byte) ([]json.RawMessage, *Failure) {
	var contents []map[string]json.RawMessage
	if json.Unmarshal(raw, &contents) != nil || len(contents) == 0 {
		return nil, unsupported("contents")
	}
	pendingIDs, usedIDs := map[string]string{}, map[string]bool{}
	pendingNames := map[string]int{}
	var transcript []json.RawMessage
	for _, content := range contents {
		role, ok := stringValue(content["role"])
		if !ok || (role != "user" && role != "model") || !onlyKeys(content, "role", "parts") {
			return nil, unsupported("contents")
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(content["parts"], &parts) != nil || len(parts) == 0 {
			return nil, unsupported("contents")
		}
		for _, part := range parts {
			_, text := part["text"]
			callRaw, call := part["functionCall"]
			responseRaw, response := part["functionResponse"]
			count := 0
			for _, present := range []bool{text, call, response} {
				if present {
					count++
				}
			}
			if count != 1 || !onlyKeys(part, "text", "functionCall", "functionResponse", "thoughtSignature") {
				return nil, unsupported("contents")
			}
			if signature, exists := part["thoughtSignature"]; exists {
				if _, ok := stringValue(signature); !ok || role != "model" || !call {
					return nil, unsupported("contents")
				}
				// Signatures belong to another provider session. They are neither
				// needed by the CLI transcript nor fabricated in our responses.
				delete(part, "thoughtSignature")
			}
			if text {
				if _, ok := stringValue(part["text"]); !ok {
					return nil, unsupported("contents")
				}
				continue
			}
			functionRaw := callRaw
			if response {
				functionRaw = responseRaw
			}
			function, ok := object(functionRaw)
			if !ok || !onlyKeys(function, "name", "id", map[bool]string{true: "args", false: "response"}[call]) {
				return nil, unsupported("contents")
			}
			name, ok := stringValue(function["name"])
			if !ok || !validToolName(name) || call && role != "model" {
				return nil, unsupported("contents")
			}
			payload := function["response"]
			if call {
				payload = function["args"]
			}
			if _, ok := object(payload); !ok {
				return nil, unsupported("contents")
			}
			id := ""
			if raw, exists := function["id"]; exists {
				id, ok = stringValue(raw)
				if !ok || !validCallID(id) {
					return nil, unsupported("contents")
				}
			}
			if call {
				if id != "" {
					if usedIDs[id] {
						return nil, unsupported("contents")
					}
					usedIDs[id], pendingIDs[id] = true, name
				} else {
					pendingNames[name]++
				}
			} else if id != "" {
				if pendingIDs[id] != name {
					return nil, unsupported("contents")
				}
				delete(pendingIDs, id)
			} else {
				if pendingNames[name] == 0 {
					return nil, unsupported("contents")
				}
				pendingNames[name]--
			}
		}
		content["parts"], _ = json.Marshal(parts)
		encoded, _ := json.Marshal(content)
		transcript = append(transcript, encoded)
	}
	if len(pendingIDs) != 0 {
		return nil, unsupported("contents")
	}
	for _, count := range pendingNames {
		if count != 0 {
			return nil, unsupported("contents")
		}
	}
	return transcript, nil
}

func geminiResponseObject(model string, result Result, call *clientFunctionCall) map[string]any {
	part := map[string]any{"text": result.Response}
	if call != nil {
		part = map[string]any{"functionCall": map[string]any{"name": call.Name, "args": call.Arguments, "id": call.ID}}
	}
	return map[string]any{
		"candidates":   []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}}, "finishReason": "STOP", "index": 0}},
		"modelVersion": model, "responseId": newID("resp_"),
		"usageMetadata": map[string]int64{
			"promptTokenCount":        result.Usage.InputTokens,
			"candidatesTokenCount":    result.Usage.OutputTokens - result.Usage.ThinkingTokens,
			"thoughtsTokenCount":      result.Usage.ThinkingTokens,
			"cachedContentTokenCount": result.Usage.CacheReadTokens,
			"totalTokenCount":         result.Usage.TotalTokens,
		},
	}
}

func writeGeminiSSE(w http.ResponseWriter, response map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	encoded, _ := json.Marshal(response)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
