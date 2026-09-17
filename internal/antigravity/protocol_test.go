package antigravity

import (
	"io"
	"strings"
	"testing"
)

const initEvent = `{"event":"init","init":{"model":"gemini-3.1-pro-high","permission_mode":"strict","tools":["read_file","run_command"]}}` + "\n"
const resultEvent = `{"event":"result","result":{"status":"SUCCESS","response":"Hello 世界","num_turns":1,"usage":{"input_tokens":10415,"output_tokens":657,"thinking_tokens":616,"cache_read_tokens":8113,"total_tokens":11072}}}` + "\n"
const textStep = `{"event":"step_update","step_update":{"step_type":"agent_response","state":"ACTIVE","text_delta":"untrusted partial text"}}` + "\n"

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
		if failure != nil || result.Response != "Hello 世界" || result.Usage.InputTokens != 10415 || result.Usage.OutputTokens != 657 || result.Usage.ThinkingTokens != 616 || result.Usage.CacheReadTokens != 8113 || result.Usage.TotalTokens != 11072 {
			t.Fatalf("fragment size%d: result=%+v failure=%+v", size, result, failure)
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
		{"thinking exceeds output", initEvent + strings.Replace(resultEvent, `"thinking_tokens":616`, `"thinking_tokens":658`, 1)},
		{"cache exceeds input", initEvent + strings.Replace(resultEvent, `"cache_read_tokens":8113`, `"cache_read_tokens":10416`, 1)},
		{"numeric overflow", initEvent + strings.Replace(resultEvent, `"input_tokens":10415`, `"input_tokens":9223372036854775807`, 1)},
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
