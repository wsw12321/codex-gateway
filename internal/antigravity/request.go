// Package antigravity adapts the official agy headless process to the narrow
// text-only Responses API. It never implements the provider's private protocol.
package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
		case "model", "input", "instructions", "stream", "store", "service_tier",
			"tools", "tool_choice", "parallel_tool_calls", "reasoning", "reasoning_effort",
			"max_output_tokens", "max_tokens", "temperature", "top_p", "metadata", "client_metadata",
			"user", "prompt_cache_key", "session_id", "conversation_id", "previous_response_id",
			"truncation", "background", "modalities", "stream_options", "text", "include":
		default:
			return out, unsupported("parameter")
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
	var instructions string
	if raw, ok := fields["instructions"]; ok {
		if len(raw) > 0 && raw[0] == '"' {
			_ = json.Unmarshal(raw, &instructions)
		}
	}
	var toolPrompt strings.Builder
	if rawTools, ok := fields["tools"]; ok && len(rawTools) > 0 && string(bytes.TrimSpace(rawTools)) != "null" {
		var toolList []map[string]json.RawMessage
		if json.Unmarshal(rawTools, &toolList) == nil && len(toolList) > 0 {
			toolPrompt.WriteString("\nAvailable client tools for this session:")
			for _, t := range toolList {
				var name, desc string
				if rawName, ok := t["name"]; ok {
					_ = json.Unmarshal(rawName, &name)
				}
				if rawDesc, ok := t["description"]; ok {
					_ = json.Unmarshal(rawDesc, &desc)
				}
				if rawFn, ok := t["function"]; ok {
					var fnObj map[string]json.RawMessage
					if json.Unmarshal(rawFn, &fnObj) == nil {
						if n, ok := fnObj["name"]; ok {
							_ = json.Unmarshal(n, &name)
						}
						if d, ok := fnObj["description"]; ok {
							_ = json.Unmarshal(d, &desc)
						}
					}
				}
				if name != "" {
					toolPrompt.WriteString(fmt.Sprintf("\n- %s: %s", name, desc))
				}
			}
			toolPrompt.WriteString("\nIf you want to invoke a tool, respond with ONLY a JSON code block in the format:\n```json\n{\"type\":\"function_call\",\"name\":\"<tool_name>\",\"arguments\":{...}}\n```\nOtherwise, provide your answer directly in markdown.")
		}
	}
	messages, failure := decodeInput(fields["input"])
	if failure != nil {
		return out, failure
	}
	fullInstructions := instructions
	if toolPrompt.Len() > 0 {
		if fullInstructions != "" {
			fullInstructions += "\n"
		}
		fullInstructions += toolPrompt.String()
	}
	transcript := struct {
		Instructions string        `json:"instructions,omitempty"`
		Messages     []textMessage `json:"messages"`
	}{fullInstructions, messages}
	encoded, _ := json.Marshal(transcript)
	out.Prompt = "Answer the following text conversation. Follow its instructions and message roles. Return only the assistant's answer. Do not use server tools, access files, run commands, browse URLs, or delegate to agents.\n" + string(encoded)
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
		var itemType string
		if rawType, ok := item["type"]; ok {
			_ = json.Unmarshal(rawType, &itemType)
		}
		switch itemType {
		case "function_call":
			var name, args string
			if rawName, ok := item["name"]; ok {
				_ = json.Unmarshal(rawName, &name)
			}
			if rawArgs, ok := item["arguments"]; ok {
				rawArgs = bytes.TrimSpace(rawArgs)
				if len(rawArgs) > 0 && rawArgs[0] == '"' {
					_ = json.Unmarshal(rawArgs, &args)
				} else {
					args = string(rawArgs)
				}
			}
			messages = append(messages, textMessage{
				Role:    "assistant",
				Content: fmt.Sprintf("[Assistant called tool %s with arguments: %s]", name, args),
			})
		case "function_call_output":
			var callID, output string
			if rawCallID, ok := item["call_id"]; ok {
				_ = json.Unmarshal(rawCallID, &callID)
			}
			if rawOutput, ok := item["output"]; ok {
				rawOutput = bytes.TrimSpace(rawOutput)
				if len(rawOutput) > 0 && rawOutput[0] == '"' {
					_ = json.Unmarshal(rawOutput, &output)
				} else {
					output = string(rawOutput)
				}
			}
			messages = append(messages, textMessage{
				Role:    "user",
				Content: fmt.Sprintf("[Tool output for %s]: %s", callID, output),
			})
		default:
			var role string
			if rawRole, ok := item["role"]; ok {
				_ = json.Unmarshal(rawRole, &role)
			}
			switch role {
			case "user", "assistant", "system", "developer":
			case "tool":
				role = "user"
			default:
				role = "user"
			}
			rawContent, hasContent := item["content"]
			if !hasContent {
				continue
			}
			rawContent = bytes.TrimSpace(rawContent)
			if len(rawContent) == 0 {
				continue
			}
			if rawContent[0] == '"' {
				var text string
				if json.Unmarshal(rawContent, &text) == nil {
					messages = append(messages, textMessage{Role: role, Content: text})
				}
			} else if rawContent[0] == '[' {
				var parts []map[string]json.RawMessage
				if json.Unmarshal(rawContent, &parts) != nil || len(parts) == 0 {
					return nil, unsupported("input")
				}
				var text strings.Builder
				for _, part := range parts {
					var pType string
					if rawPType, ok := part["type"]; ok {
						_ = json.Unmarshal(rawPType, &pType)
					}
					if pType != "input_text" && pType != "output_text" && pType != "text" {
						return nil, unsupported("input")
					}
					var pText string
					if rawPText, ok := part["text"]; ok {
						if len(rawPText) == 0 || rawPText[0] != '"' || json.Unmarshal(rawPText, &pText) != nil {
							return nil, unsupported("input")
						}
					}
					text.WriteString(pText)
				}
				messages = append(messages, textMessage{Role: role, Content: text.String()})
			}
		}
	}
	if len(messages) == 0 {
		return nil, unsupported("input")
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
