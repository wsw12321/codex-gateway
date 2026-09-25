package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type identificationTransportFunc func(*http.Request) (*http.Response, error)

func (fn identificationTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func identificationInternalResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestIdentificationSidecarClientChecksAccountAndProbeEnvelope(t *testing.T) {
	accountID := "0123456789abcdef"
	base, _ := url.Parse("http://sidecar.test")
	var responseAccount = accountID
	client := NewWithHTTPClient(base, "secret", &http.Client{Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Cache-Control") != "no-store" {
			t.Error("internal request lacked bearer or no-store")
		}
		switch r.URL.Path {
		case "/internal/upstream-accounts/" + accountID + "/models":
			return identificationInternalResponse(200, `{"account_id":"`+responseAccount+`","models":["gpt-6-sol","codex/special"]}`), nil
		case "/internal/upstream-accounts/" + accountID + "/probe":
			var body struct {
				Model, Input    string
				MaxOutputTokens int `json:"max_output_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Model != "gpt-6-sol" || body.Input != "Prompt" || body.MaxOutputTokens != 16384 {
				t.Errorf("unexpected probe body: %+v", body)
			}
			return identificationInternalResponse(200, `{"account_id":"`+responseAccount+`","output_text":" 17, 82 \n"}`), nil
		}
		t.Errorf("unexpected path %q", r.URL.Path)
		return identificationInternalResponse(404, `{"error":"unknown"}`), nil
	})})
	models, err := client.ListUpstreamAccountModels(context.Background(), accountID)
	if err != nil || len(models) != 2 || models[1] != "codex/special" {
		t.Fatalf("models=%v err=%v", models, err)
	}
	text, err := client.ProbeUpstreamAccount(context.Background(), accountID, "gpt-6-sol", "Prompt")
	if err != nil || text != " 17, 82 \n" {
		t.Fatalf("reply=%q err=%v", text, err)
	}
	responseAccount = "ffffffffffffffff"
	_, err = client.ProbeUpstreamAccount(context.Background(), accountID, "gpt-6-sol", "Prompt")
	var internal *InternalAPIError
	if !errors.As(err, &internal) || internal.SafeCode() != "probe_account_mismatch" {
		t.Fatalf("mismatched account error=%v", err)
	}
	if _, err := client.ListUpstreamAccountModels(context.Background(), accountID); !errors.As(err, &internal) || internal.SafeCode() != "sidecar_invalid_response" {
		t.Fatalf("mismatched catalog account error=%v", err)
	}
}

func TestIdentificationSidecarClientNormalizesFixedErrors(t *testing.T) {
	base, _ := url.Parse("http://sidecar.test")
	for _, test := range []struct {
		status        int
		sidecar, want string
	}{
		{409, "probe_model_unavailable", "model_unavailable"},
		{409, "upstream_account_unavailable", "upstream_account_unavailable"},
		{502, "probe_account_mismatch", "probe_account_mismatch"},
		{504, "probe_timeout", "sidecar_timeout"},
	} {
		client := NewWithHTTPClient(base, "secret", &http.Client{Transport: identificationTransportFunc(func(*http.Request) (*http.Response, error) {
			return identificationInternalResponse(test.status, `{"error":"`+test.sidecar+`"}`), nil
		})})
		_, err := client.ProbeUpstreamAccount(context.Background(), "0123456789abcdef", "gpt-6-sol", "Prompt")
		var internal *InternalAPIError
		if !errors.As(err, &internal) || internal.SafeCode() != test.want {
			t.Errorf("sidecar %s: got %v, want %s", test.sidecar, err, test.want)
		}
	}
}
