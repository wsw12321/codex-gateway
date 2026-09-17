package antigravity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
)

const maxOutputBytes = 4 << 20
const maxProtocolBytes = 16 << 20

type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

func (u *Usage) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"input_tokens", "output_tokens", "thinking_tokens", "cache_read_tokens", "total_tokens"} {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("missing numeric usage field")
		}
	}
	type plainUsage Usage
	return json.Unmarshal(raw, (*plainUsage)(u))
}

type Result struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Error    string `json:"error"`
	NumTurns int    `json:"num_turns"`
	Usage    *Usage `json:"usage"`
}

func (u Usage) valid() bool {
	// The official stream-json examples report output_tokens INCLUDING
	// thinking_tokens, and total_tokens == input_tokens + output_tokens.
	// Adding thinking again would overcharge every reasoning response.
	return u.InputTokens >= 0 && u.OutputTokens >= 0 && u.ThinkingTokens >= 0 &&
		u.ThinkingTokens <= u.OutputTokens && u.CacheReadTokens >= 0 &&
		u.CacheReadTokens <= u.InputTokens && u.InputTokens <= math.MaxInt64-u.OutputTokens &&
		u.TotalTokens == u.InputTokens+u.OutputTokens
}

// Consume the complete stream before exposing it. In particular a valid result
// followed by a tool event, a second result, or a process failure is not success.
func parseStream(reader io.Reader) (Result, *Failure) {
	scanner := bufio.NewScanner(io.LimitReader(reader, maxProtocolBytes+1))
	scanner.Buffer(make([]byte, 64<<10), maxOutputBytes+64<<10)
	initialized, finished, total := false, false, 0
	var result Result
	for scanner.Scan() {
		line := scanner.Bytes()
		total += len(line) + 1
		if total > maxProtocolBytes || len(bytes.TrimSpace(line)) == 0 || uniqueJSON(line) != nil {
			return result, protocolFailure()
		}
		var event struct {
			Event string `json:"event"`
			Init  *struct {
				Model          string `json:"model"`
				PermissionMode string `json:"permission_mode"`
			} `json:"init"`
			Step *struct {
				Type         string          `json:"step_type"`
				State        string          `json:"state"`
				Text         string          `json:"text_delta"`
				ToolName     string          `json:"tool_name"`
				ToolInfo     json.RawMessage `json:"tool_info"`
				SubagentInfo json.RawMessage `json:"subagent_info"`
			} `json:"step_update"`
			Result *Result `json:"result"`
		}
		if json.Unmarshal(line, &event) != nil || finished {
			return result, protocolFailure()
		}
		switch event.Event {
		case "init":
			if initialized || event.Init == nil || event.Step != nil || event.Result != nil ||
				event.Init.Model != CLIModel || event.Init.PermissionMode != "strict" {
				return result, protocolFailure()
			}
			initialized = true
		case "step_update":
			if !initialized || event.Step == nil || event.Init != nil || event.Result != nil {
				return result, protocolFailure()
			}
			step := event.Step
			if step.ToolName != "" || len(step.ToolInfo) != 0 || len(step.SubagentInfo) != 0 {
				return result, protocolFailure()
			}
			if step.State != "ACTIVE" && step.State != "DONE" {
				return result, protocolFailure()
			}
			switch step.Type {
			case "user_input", "checkpoint", "agent_response":
			default:
				return result, protocolFailure()
			}
		case "result":
			if event.Result == nil || event.Init != nil || event.Step != nil {
				return result, protocolFailure()
			}
			result = *event.Result
			if result.Status != "SUCCESS" {
				// Startup failures (e.g. expired authentication) can precede init.
				switch result.Status {
				case "ERROR", "CANCELED", "INTERRUPTED", "INVALID", "WAITING", "RUNNING":
					return result, classifyFailure(result.Error)
				default:
					return result, protocolFailure()
				}
			}
			if !initialized || result.Error != "" || result.NumTurns != 1 || result.Usage == nil || !result.Usage.valid() || len(result.Response) > maxOutputBytes {
				return result, protocolFailure()
			}
			// Keep the completed Responses event below the gateway's 4 MiB
			// event limit, including JSON escaping and response metadata.
			encoded, _ := json.Marshal(result.Response)
			if len(encoded) > 2<<20 {
				return result, protocolFailure()
			}
			finished = true
		default:
			return result, protocolFailure()
		}
	}
	if scanner.Err() != nil || !finished {
		return result, protocolFailure()
	}
	return result, nil
}

func classifyFailure(diagnostic string) *Failure {
	text := strings.ToLower(diagnostic)
	switch {
	case strings.Contains(text, "authentication required"), strings.Contains(text, "unauthenticated"), strings.Contains(text, "invalid_grant"), strings.Contains(text, "token expired"), strings.Contains(text, "credentials expired"), strings.Contains(text, "not authenticated"), strings.Contains(text, "login required"), strings.Contains(text, "please sign in"), strings.Contains(text, "invalid authentication credentials"):
		return &Failure{503, "upstream_unavailable", "Antigravity authentication is unavailable"}
	case strings.Contains(text, "quota exceeded"), strings.Contains(text, "quota exhausted"), strings.Contains(text, "resource_exhausted"), strings.Contains(text, "rate limit"), strings.Contains(text, "too many requests"), strings.Contains(text, "subscription limit"), strings.Contains(text, "usage limit"):
		return &Failure{429, "upstream_rate_limited", "Antigravity subscription quota is exhausted"}
	case strings.Contains(text, "deadline exceeded"), strings.Contains(text, "deadline_exceeded"), strings.Contains(text, "timed out"), strings.Contains(text, "timeout"):
		return &Failure{504, "upstream_timeout", "Antigravity request timed out"}
	default:
		return &Failure{502, "upstream_process_error", "Antigravity process failed"}
	}
}
