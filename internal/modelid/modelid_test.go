package modelid

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

func exampleReplies(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("testdata/example.json")
	if err != nil {
		t.Fatal(err)
	}
	var sample struct {
		Sets map[string][]struct {
			Text string `json:"text"`
		} `json:"sets"`
	}
	if err := json.Unmarshal(data, &sample); err != nil {
		t.Fatal(err)
	}
	outputs := sample.Sets["environment-05"]
	if len(outputs) != 3 {
		t.Fatalf("sample outputs: %d", len(outputs))
	}
	return []string{outputs[0].Text, outputs[1].Text, outputs[2].Text}
}

func TestOfficialExampleParity(t *testing.T) {
	result, err := Score(exampleReplies(t))
	if err != nil {
		t.Fatal(err)
	}
	if result.BankVersion != "2026.09.5" || result.Level != "match" || result.MatchedModel != "claude-fable-5-1" || result.ClosestModel != "claude-fable-5-1" || result.Family != "claude" {
		t.Fatalf("unexpected result: %+v", result)
	}
	// Produced by the official v2026.09.5 fingerprint-core.js, gates.js,
	// verdict.js and bank.json for the official environment-05 example.
	if math.Abs(result.Fit-0.3688934845339764) > 1e-10 || math.Abs(result.Margin-0.2720162953879786) > 1e-10 {
		t.Fatalf("official fit/margin differ: got %.15f / %.15f", result.Fit, result.Margin)
	}
	want := []string{"claude-fable-5-1", "claude-opus-4-6", "claude-opus-5-5", "qwen3.8-max-0902", "claude-opus-5"}
	for i, name := range want {
		if result.Candidates[i].Model != name {
			t.Errorf("candidate %d: got %q, want %q", i, result.Candidates[i].Model, name)
		}
	}
}

func TestOfficialRandomOpenSetParity(t *testing.T) {
	// The same deterministic replies were scored with the official JavaScript
	// modules and bank. Uniform numbers must retain the nearest candidate but
	// must not become an identity conclusion.
	seed := uint32(1234567)
	replies := make([]string, 3)
	for i, challenge := range Challenges() {
		var b strings.Builder
		for j := 0; j < challenge.RequestedCount; j++ {
			if j > 0 {
				b.WriteString(", ")
			}
			seed = seed*1664525 + 1013904223
			b.WriteString(strconv.FormatUint(uint64(1+seed%355), 10))
		}
		replies[i] = b.String()
	}
	result, err := Score(replies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Level != "insufficient" || result.MatchedModel != "" || result.ClosestModel != "gpt-5.6-sol" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if math.Abs(result.Fit-0.03517694256930998) > 1e-10 || math.Abs(result.Margin-(-0.003813972697052005)) > 1e-10 {
		t.Fatalf("official fit/margin differ: %.15f / %.15f", result.Fit, result.Margin)
	}
	want := []string{"gpt-5.6-sol", "gpt-6-astra", "gpt-5.5", "deepseek-v4-pro-0813", "claude-opus-4-7"}
	for i, name := range want {
		if result.Candidates[i].Model != name {
			t.Errorf("candidate %d: got %q, want %q", i, result.Candidates[i].Model, name)
		}
	}
}

func TestChallengeSelection(t *testing.T) {
	selected := Challenges()
	if len(selected) != 3 || selected[0].ID != "query-13" || selected[1].ID != "query-14" || selected[2].ID != "query-15" {
		t.Fatalf("unexpected challenges: %+v", selected)
	}
	if selected[0].RequestedCount != 297 || selected[1].RequestedCount != 315 || selected[2].RequestedCount != 331 {
		t.Fatalf("unexpected counts: %+v", selected)
	}
	selected[0].Prompt = "edited"
	if Challenges()[0].Prompt == "edited" {
		t.Fatal("challenge slice was not copied")
	}
}

func TestInputGates(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		rule   string
	}{
		{"empty", "", "empty"},
		{"constant", strings.Repeat("42,", 297), "constant"},
		{"monotonic", numberKey(sequence(1, 297)), "monotonic"},
		{"arithmetic", strings.Repeat("10,12,14,", 99), "arithmetic"},
		{"low_unique", strings.Repeat("10,20,50,100,170,260,", 50), "low_unique"},
		{"short", "17, 312, 51, 119", "too_short"},
		{"api_object", `{"choices":[]}`, "api_multiple"},
		{"array_fraction", `[1, 2.5, 3]`, "api_array"},
		{"array_multiple", `[[1, 2], [3, 4]]`, "api_multiple"},
		{"wrapped_number", "[12\n3,4]", "wrapped_number"},
	}
	base := exampleReplies(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replies := append([]string(nil), base...)
			replies[0] = tc.answer
			_, err := Score(replies)
			var replyError *ReplyError
			if !errors.As(err, &replyError) || replyError.Index != 0 || replyError.Rule != tc.rule {
				t.Fatalf("got %v, want rule %s", err, tc.rule)
			}
		})
	}
	_, err := Score([]string{base[0], base[0], base[2]})
	var replyError *ReplyError
	if !errors.As(err, &replyError) || replyError.Index != 1 || replyError.Rule != "duplicate" {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestCompletedResponseEnvelope(t *testing.T) {
	base := exampleReplies(t)
	envelope, err := json.Marshal(map[string]any{
		"object":  "chat.completion",
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": base[0]}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base[0] = string(envelope)
	result, err := Score(base)
	if err != nil {
		t.Fatal(err)
	}
	if result.Level != "match" || result.MatchedModel != "claude-fable-5-1" {
		t.Fatalf("unexpected wrapped result: %+v", result)
	}
}

func TestValidateRepliesPrefixSharesScoringGates(t *testing.T) {
	replies := exampleReplies(t)
	for n := 1; n <= len(replies); n++ {
		if err := ValidateRepliesPrefix(replies[:n]); err != nil {
			t.Fatalf("valid prefix of %d: %v", n, err)
		}
	}
	for _, prefix := range [][]string{nil, append(append([]string(nil), replies...), replies[0])} {
		if err := ValidateRepliesPrefix(prefix); err == nil {
			t.Fatal("accepted invalid prefix length")
		}
	}
	for _, prefix := range [][]string{{"17, 312, 51"}, {replies[0], replies[0]}} {
		err := ValidateRepliesPrefix(prefix)
		var replyError *ReplyError
		if !errors.As(err, &replyError) || replyError.Index != len(prefix)-1 {
			t.Fatalf("bad answer did not fail immediately: %v", err)
		}
		full := append(append([]string(nil), prefix...), replies[len(prefix):]...)
		_, scoreErr := Score(full)
		if scoreErr == nil || scoreErr.Error() != err.Error() {
			t.Fatalf("prefix and final scoring diverged: %v / %v", err, scoreErr)
		}
	}
}

func sequence(start, count int) []int {
	values := make([]int, count)
	for i := range values {
		values[i] = start + i
	}
	return values
}
