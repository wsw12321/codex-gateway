package server

import "testing"

func TestExtractTopLevelModel(t *testing.T) {
	for _, test := range []struct {
		raw   string
		model string
	}{
		{`{"model":"gpt-5","input":"hello"}`, "gpt-5"},
		{`{"input":[{"text":"fake \\\"model\\\":\\\"evil\\\""}],"model":"gpt-5.1-codex"}`, "gpt-5.1-codex"},
		{` { "stream": true, "nested": {"model":"evil"}, "model" : "o3" }`, "o3"},
	} {
		got, err := extractTopLevelModel([]byte(test.raw))
		if err != nil || got != test.model {
			t.Fatalf("extract %s: got %q, %v", test.raw, got, err)
		}
	}
}

func TestRejectsNestedOrEscapedModel(t *testing.T) {
	for _, raw := range []string{
		`{"input":{"model":"evil"}}`,
		`{"model":"../../secret"}`,
		`{"mo\u0064el":"gpt-5"}`,
	} {
		if _, err := extractTopLevelModel([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
