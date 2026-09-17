// Package antigravity adapts the official agy headless process to the narrow
// text-only Responses API. It never implements the provider's private protocol.
package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/wsw/codex-gateway/internal/config"
)

const PublicModel = config.AntigravityPublicModel
const CLIModel = config.AntigravityCLIModel

type Failure struct {
	Status  int
	Code    string
	Message string
}

func unsupported(field string) *Failure {
	return &Failure{http.StatusBadRequest, "antigravity_" + field + "_unsupported", "Antigravity does not support this " + field + " value"}
}

func protocolFailure() *Failure {
	return &Failure{http.StatusBadGateway, "upstream_protocol_error", "Invalid Antigravity process output"}
}

type Request struct {
	Model  string
	Stream bool
	Prompt string
}

type textMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func DecodeRequest(body []byte) (Request, *Failure) {
	var out Request
	if err := uniqueJSON(body); err != nil {
		return out, &Failure{400, "antigravity_invalid_request", "Request must be one JSON object without duplicate keys"}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return out, unsupported("input")
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		case "model", "input", "instructions", "stream", "store", "service_tier":
		default:
			// Do not echo arbitrary user-controlled field names into error codes.
			switch key {
			case "tools", "tool_choice", "parallel_tool_calls", "previous_response_id", "max_output_tokens", "reasoning", "text", "temperature", "top_p", "include", "metadata", "background", "conversation", "truncation":
				return out, unsupported(key)
			default:
				return out, unsupported("parameter")
			}
		}
	}
	if json.Unmarshal(fields["model"], &out.Model) != nil || out.Model != PublicModel {
		return out, unsupported("model")
	}
	if raw, ok := fields["stream"]; ok {
		if !isBool(raw) || json.Unmarshal(raw, &out.Stream) != nil {
			return out, unsupported("stream")
		}
	}
	if raw, ok := fields["store"]; ok && string(bytes.TrimSpace(raw)) != "false" {
		return out, unsupported("store")
	}
	if raw, ok := fields["service_tier"]; ok {
		var tier string
		if json.Unmarshal(raw, &tier) != nil || tier != "default" {
			return out, unsupported("service_tier")
		}
	}
	var instructions string
	if raw, ok := fields["instructions"]; ok {
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &instructions) != nil {
			return out, unsupported("instructions")
		}
	}
	messages, failure := decodeInput(fields["input"])
	if failure != nil {
		return out, failure
	}
	transcript := struct {
		Instructions string        `json:"instructions,omitempty"`
		Messages     []textMessage `json:"messages"`
	}{instructions, messages}
	encoded, _ := json.Marshal(transcript)
	// The wrapper also prevents a user input beginning with / from becoming a
	// CLI slash command. No user content is ever passed as a command argument.
	out.Prompt = "Answer the following text conversation. Follow its instructions and message roles. Return only the assistant's answer. Do not use tools, access files, run commands, browse URLs, or delegate to agents.\n" + string(encoded)
	return out, nil
}

func isBool(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("true")) || bytes.Equal(bytes.TrimSpace(raw), []byte("false"))
}

func decodeInput(raw json.RawMessage) ([]textMessage, *Failure) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, unsupported("input")
	}
	if raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil, unsupported("input")
		}
		return []textMessage{{"user", value}}, nil
	}
	if raw[0] != '[' {
		return nil, unsupported("input")
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil, unsupported("input")
	}
	var messages []textMessage
	for _, item := range items {
		for key := range item {
			if key != "role" && key != "content" && key != "type" {
				return nil, unsupported("input")
			}
		}
		if kind, ok := item["type"]; ok && string(kind) != `"message"` {
			return nil, unsupported("input")
		}
		var message textMessage
		if json.Unmarshal(item["role"], &message.Role) != nil {
			return nil, unsupported("input")
		}
		switch message.Role {
		case "user", "assistant", "system", "developer":
		default:
			return nil, unsupported("input")
		}
		content := bytes.TrimSpace(item["content"])
		if len(content) == 0 {
			return nil, unsupported("input")
		}
		if content[0] == '"' {
			if json.Unmarshal(content, &message.Content) != nil {
				return nil, unsupported("input")
			}
		} else {
			if content[0] != '[' {
				return nil, unsupported("input")
			}
			var parts []map[string]json.RawMessage
			if json.Unmarshal(content, &parts) != nil || len(parts) == 0 {
				return nil, unsupported("input")
			}
			var text strings.Builder
			for _, part := range parts {
				if len(part) != 2 {
					return nil, unsupported("input")
				}
				var kind, value string
				if json.Unmarshal(part["type"], &kind) != nil || (kind != "input_text" && kind != "output_text") {
					return nil, unsupported("input")
				}
				if len(part["text"]) == 0 || part["text"][0] != '"' || json.Unmarshal(part["text"], &value) != nil {
					return nil, unsupported("input")
				}
				text.WriteString(value)
			}
			message.Content = text.String()
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// Reject duplicate fields at every level; encoding/json otherwise silently
// chooses the last occurrence at a security-sensitive protocol boundary.
func uniqueJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON key")
				}
				seen[name] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
