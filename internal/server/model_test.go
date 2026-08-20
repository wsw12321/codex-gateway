package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

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

func TestExtractTopLevelRouting(t *testing.T) {
	routing, err := extractTopLevelRouting([]byte(`{"input":[],"service_tier":"priority","model":"gpt-5.6-sol"}`))
	if err != nil {
		t.Fatal(err)
	}
	if routing.Model != "gpt-5.6-sol" || routing.ServiceTier != "priority" {
		t.Fatalf("routing = %+v", routing)
	}
	for _, raw := range []string{
		`{"model":"gpt-5.6-sol","service_tier":"ultra/fast"}`,
		`{"model":"gpt-5.6-sol","model":"gpt-5.4"}`,
		`{"model":"gpt-5.6-sol","service_tier":"flex","service_tier":"priority"}`,
	} {
		if _, err := extractTopLevelRouting([]byte(raw)); err == nil {
			t.Fatalf("accepted ambiguous routing: %s", raw)
		}
	}
}

func TestPrepareModelBodyScansRoutingAfterFormerPrefix(t *testing.T) {
	server := &Server{}
	largeInput := strings.Repeat("x", maxModelPrefix+1024)
	payload := `{"model":"gpt-5.6-sol","input":"` + largeInput + `","service_tier":"ultrafast"}`
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(payload))
	routing, body, err := server.prepareModelBody(httptest.NewRecorder(), request, int64(len(payload)+1))
	if err != nil {
		t.Fatal(err)
	}
	if routing.Model != "gpt-5.6-sol" || routing.ServiceTier != "ultrafast" {
		t.Fatalf("routing = %+v", routing)
	}
	temporary, ok := body.closer.(*temporaryRequestFile)
	if !ok {
		t.Fatalf("closer type = %T", body.closer)
	}
	if info, err := os.Stat(temporary.path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("temporary request file: info=%v err=%v", info, err)
	}
	replayed, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(replayed) != payload || body.bytes != int64(len(payload)) {
		t.Fatalf("replayed bytes = %d, want %d", body.bytes, len(payload))
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temporary.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary request file still exists: %v", err)
	}
}

func TestPrepareModelBodyRejectsLateDuplicateRouting(t *testing.T) {
	largeInput := strings.Repeat("x", maxModelPrefix+1024)
	for _, test := range []struct {
		name    string
		payload string
	}{
		{
			name:    "duplicate service tier",
			payload: `{"model":"gpt-5.6-sol","service_tier":"default","input":"` + largeInput + `","service_tier":"ultrafast"}`,
		},
		{
			name:    "duplicate model",
			payload: `{"model":"gpt-5.6-sol","input":"` + largeInput + `","model":"gpt-5.4"}`,
		},
		{
			name:    "escaped service tier key",
			payload: `{"model":"gpt-5.6-sol","input":"` + largeInput + `","service_\u0074ier":"ultrafast"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{}
			request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(test.payload))
			_, body, err := server.prepareModelBody(httptest.NewRecorder(), request, int64(len(test.payload)+1))
			if !errors.Is(err, errModelNotFound) || body != nil {
				t.Fatalf("body=%v error=%v", body, err)
			}
			// Malformed bodies must release their spool slot on every error path.
			for index := 0; index < maxConcurrentRequestSpools+1; index++ {
				valid := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol"}`))
				_, replay, err := server.prepareModelBody(httptest.NewRecorder(), valid, 1024)
				if err != nil {
					t.Fatalf("valid request %d after malformed body: %v", index, err)
				}
				if err := replay.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRequestBodySpoolConcurrencyIsBounded(t *testing.T) {
	server := &Server{}
	bodies := make([]*countingBody, 0, maxConcurrentRequestSpools)
	for index := 0; index < maxConcurrentRequestSpools; index++ {
		request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol"}`))
		_, body, err := server.prepareModelBody(httptest.NewRecorder(), request, 1024)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
	}
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol"}`))
	if _, _, err := server.prepareModelBody(httptest.NewRecorder(), request, 1024); !errors.Is(err, errRequestBodySpoolBusy) {
		t.Fatalf("fifth concurrent spool error = %v", err)
	}
	if err := bodies[0].Close(); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol"}`))
	_, replacement, err := server.prepareModelBody(httptest.NewRecorder(), request, 1024)
	if err != nil {
		t.Fatalf("replacement spool: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies[1:] {
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrepareModelBodyReleasesSlotAfterStreamingLimitError(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"too large"}`))
	request.ContentLength = -1
	_, body, err := server.prepareModelBody(httptest.NewRecorder(), request, 8)
	var maxBytes *http.MaxBytesError
	if !errors.As(err, &maxBytes) || body != nil {
		t.Fatalf("body=%v error=%v", body, err)
	}
	for index := 0; index < maxConcurrentRequestSpools+1; index++ {
		valid := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol"}`))
		_, replay, err := server.prepareModelBody(httptest.NewRecorder(), valid, 1024)
		if err != nil {
			t.Fatalf("valid request %d after limit error: %v", index, err)
		}
		if err := replay.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrepareModelBodyLimitBoundary(t *testing.T) {
	const limit = int64(1024)
	prefix := `{"model":"gpt-5.6-sol","input":"`
	suffix := `"}`
	payload := prefix + strings.Repeat("x", int(limit)-len(prefix)-len(suffix)) + suffix
	if int64(len(payload)) != limit {
		t.Fatalf("payload length = %d, want %d", len(payload), limit)
	}
	server := &Server{}
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(payload))
	_, body, err := server.prepareModelBody(httptest.NewRecorder(), request, limit)
	if err != nil {
		t.Fatalf("exact limit rejected: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(payload+" "))
	_, body, err = server.prepareModelBody(httptest.NewRecorder(), request, limit)
	var maxBytes *http.MaxBytesError
	if !errors.As(err, &maxBytes) || body != nil {
		t.Fatalf("one byte over limit: body=%v error=%v", body, err)
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

func TestRecordedUpstreamModelRejectsMalformedValues(t *testing.T) {
	if got := recordedUpstreamModel("gpt-5.2-codex"); got != "gpt-5.2-codex" {
		t.Fatalf("valid upstream model = %q", got)
	}
	for _, model := range []string{"", " gpt-5.2-codex ", "../../model", strings.Repeat("a", 129)} {
		if got := recordedUpstreamModel(model); got != "" {
			t.Fatalf("malformed upstream model %q was recorded as %q", model, got)
		}
	}
}

func TestRecordedUpstreamServiceTierPreservesUnknownSafeValue(t *testing.T) {
	for _, value := range []string{"default", "flex", "priority", "ultrafast"} {
		if got := recordedUpstreamServiceTier(value); got != value {
			t.Fatalf("service tier %q recorded as %q", value, got)
		}
	}
	for _, value := range []string{"", " priority ", "../../fast", strings.Repeat("a", 33)} {
		if got := recordedUpstreamServiceTier(value); got != "" {
			t.Fatalf("malformed service tier %q recorded as %q", value, got)
		}
	}
}
