package antigravity

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
)

const initEvent = `{"event":"init","init":{"model":"gemini-3.1-pro-high","permission_mode":"strict","tools":["read_file","run_command"]}}` + "\n"
const resultEvent = `{"event":"result","result":{"status":"SUCCESS","response":"Hello 世界","num_turns":1,"usage":{"input_tokens":10415,"output_tokens":657,"thinking_tokens":616,"cache_read_tokens":8113,"total_tokens":11072}}}` + "\n"
const textStep = `{"event":"step_update","step_update":{"step_type":"agent_response","state":"ACTIVE","text_delta":"untrusted partial text"}}` + "\n"

// Captured token counts from a successful AGY 1.2.4 production generation.
// Only public protocol metadata and synthetic response text are retained.
const cachedResultEvent = `{"event":"result","result":{"status":"SUCCESS","response":"Hello 世界","num_turns":1,"usage":{"input_tokens":5441,"output_tokens":175,"thinking_tokens":173,"cache_read_tokens":8089,"total_tokens":5616}}}` + "\n"

func resultWithUsage(usage string) string {
	return fmt.Sprintf("{\"event\":\"result\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"Hello 世界\",\"num_turns\":1,\"usage\":%s}}\n", usage)
}

type fragmentReader struct {
	reader io.Reader
	size   int
}

func (r fragmentReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.reader.Read(p)
}

func TestParseStreamFragmentedNDJSONUsesFinalResult(t *testing.T) {
	for _, size := range []int{1, 2, 7, 1024} {
		result, failure := parseStream(fragmentReader{strings.NewReader(initEvent + textStep + resultEvent), size})
		if failure != nil || result.Response != "Hello 世界" || result.Usage.InputTokens != 18528 || result.Usage.OutputTokens != 657 || result.Usage.ThinkingTokens != 616 || result.Usage.CacheReadTokens != 8113 || result.Usage.TotalTokens != 19185 {
			t.Fatalf("fragment size%d: result=%+v failure=%+v", size, result, failure)
		}
	}
}

func TestParseStreamNormalizesCLIUsageOnce(t *testing.T) {
	for _, test := range []struct {
		name, event string
		want        Usage
	}{
		{"cache larger than uncached input", cachedResultEvent, Usage{13530, 175, 173, 8089, 13705}},
		{"uncached plus cached input", resultEvent, Usage{18528, 657, 616, 8113, 19185}},
		{"no cache", resultWithUsage(`{"input_tokens":101,"output_tokens":37,"thinking_tokens":29,"cache_read_tokens":0,"total_tokens":138}`), Usage{101, 37, 29, 0, 138}},
		{"fully cached input", resultWithUsage(`{"input_tokens":0,"output_tokens":37,"thinking_tokens":29,"cache_read_tokens":101,"total_tokens":37}`), Usage{101, 37, 29, 101, 138}},
		{"zero usage", resultWithUsage(`{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}`), Usage{}},
		{"maximum normalized total", resultWithUsage(`{"input_tokens":9223372036854775707,"output_tokens":1,"thinking_tokens":1,"cache_read_tokens":99,"total_tokens":9223372036854775708}`), Usage{math.MaxInt64 - 1, 1, 1, 99, math.MaxInt64}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, failure := parseStream(strings.NewReader(initEvent + test.event))
			if failure != nil || result.Usage == nil || *result.Usage != test.want {
				t.Fatalf("usage=%+v want=%+v failure=%+v", result.Usage, test.want, failure)
			}
		})
	}
}

func TestParseStreamRequiresEveryCLIUsageField(t *testing.T) {
	for _, field := range []string{"input_tokens", "output_tokens", "thinking_tokens", "cache_read_tokens", "total_tokens"} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%t", field, missing), func(t *testing.T) {
				fields := map[string]any{"input_tokens": 101, "output_tokens": 37, "thinking_tokens": 29, "cache_read_tokens": 31, "total_tokens": 138}
				if missing {
					delete(fields, field)
				} else {
					fields[field] = nil
				}
				usage, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				_, failure := parseStream(strings.NewReader(initEvent + resultWithUsage(string(usage))))
				if failure == nil || failure.Code != "upstream_protocol_error" {
					t.Fatalf("failure=%+v", failure)
				}
			})
		}
	}
}

func TestParseStreamRejectsUnsafeOrAmbiguousProtocol(t *testing.T) {
	for _, test := range []struct{ name, stream string }{
		{"missing init", resultEvent},
		{"missing result", initEvent + textStep},
		{"duplicate init", initEvent + initEvent + resultEvent},
		{"duplicate result", initEvent + resultEvent + resultEvent},
		{"malformed", initEvent + "{\n"},
		{"blank line", initEvent + "\n" + resultEvent},
		{"wrong model", strings.Replace(initEvent, CLIModel, "different-model", 1) + resultEvent},
		{"missing model", strings.Replace(initEvent, `"model":"`+CLIModel+`",`, "", 1) + resultEvent},
		{"non-strict permissions", strings.Replace(initEvent, "strict", "request-review", 1) + resultEvent},
		{"unsafe permissions", strings.Replace(initEvent, "strict", "always-proceed", 1) + resultEvent},
		{"tool step", initEvent + `{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"read_file","text_delta":"secret"}}` + "\n" + resultEvent},
		{"concealed tool", initEvent + `{"event":"step_update","step_update":{"step_type":"agent_response","state":"DONE","tool_info":{}}}` + "\n" + resultEvent},
		{"subagent", initEvent + `{"event":"step_update","step_update":{"step_type":"agent_response","state":"DONE","subagent_info":{"subagents":[]}}}` + "\n" + resultEvent},
		{"tool after result", initEvent + resultEvent + `{"event":"step_update","step_update":{"step_type":"tool","state":"DONE"}}` + "\n"},
		{"unknown event", initEvent + `{"event":"future_event"}` + "\n" + resultEvent},
		{"duplicate fields", initEvent + strings.Replace(resultEvent, `"status":"SUCCESS"`, `"status":"ERROR","status":"SUCCESS"`, 1)},
		{"multiple turns", initEvent + strings.Replace(resultEvent, `"num_turns":1`, `"num_turns":2`, 1)},
		{"missing usage", initEvent + strings.Replace(resultEvent, `"usage":{`, `"unused":{`, 1)},
		{"missing numeric input", initEvent + strings.Replace(resultEvent, `"input_tokens":10415,`, "", 1)},
		{"empty usage", initEvent + `{"event":"result","result":{"status":"SUCCESS","response":"x","num_turns":1,"usage":{}}}` + "\n"},
		{"null tokens", initEvent + `{"event":"result","result":{"status":"SUCCESS","response":"x","num_turns":1,"usage":{"input_tokens":null,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}}}` + "\n"},
		{"incorrect total", initEvent + strings.Replace(resultEvent, `"total_tokens":11072`, `"total_tokens":11688`, 1)},
		{"negative input", initEvent + strings.Replace(resultEvent, `"input_tokens":10415`, `"input_tokens":-1`, 1)},
		{"negative output", initEvent + strings.Replace(resultEvent, `"output_tokens":657`, `"output_tokens":-1`, 1)},
		{"negative thinking", initEvent + strings.Replace(resultEvent, `"thinking_tokens":616`, `"thinking_tokens":-1`, 1)},
		{"negative cache", initEvent + strings.Replace(resultEvent, `"cache_read_tokens":8113`, `"cache_read_tokens":-1`, 1)},
		{"negative total", initEvent + strings.Replace(resultEvent, `"total_tokens":11072`, `"total_tokens":-1`, 1)},
		{"thinking exceeds output", initEvent + strings.Replace(resultEvent, `"thinking_tokens":616`, `"thinking_tokens":658`, 1)},
		{"numeric overflow", initEvent + strings.Replace(resultEvent, `"input_tokens":10415`, `"input_tokens":9223372036854775807`, 1)},
		{"raw total overflow", initEvent + resultWithUsage(`{"input_tokens":9223372036854775807,"output_tokens":1,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}`)},
		{"normalized input overflow", initEvent + resultWithUsage(`{"input_tokens":9223372036854775807,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":1,"total_tokens":9223372036854775807}`)},
		{"normalized total overflow", initEvent + resultWithUsage(`{"input_tokens":9223372036854775806,"output_tokens":1,"thinking_tokens":0,"cache_read_tokens":1,"total_tokens":9223372036854775807}`)},
		{"out of range integer", initEvent + strings.Replace(resultEvent, `"input_tokens":10415`, `"input_tokens":9223372036854775808`, 1)},
		{"fractional count", initEvent + strings.Replace(resultEvent, `"cache_read_tokens":8113`, `"cache_read_tokens":1.5`, 1)},
		{"unknown terminal status", `{"event":"result","result":{"status":"FUTURE","error":"quota exceeded"}}` + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, failure := parseStream(strings.NewReader(test.stream))
			if failure == nil || failure.Status != 502 || failure.Code != "upstream_protocol_error" {
				t.Fatalf("failure=%+v", failure)
			}
		})
	}
}
