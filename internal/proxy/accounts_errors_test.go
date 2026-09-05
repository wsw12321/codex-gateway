package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestInternalQuotaErrorEnvelopeValidation(t *testing.T) {
	const valid = `{"error":"quota_schema_changed"}`
	const sensitive = "Bearer sensitive-canary"
	for _, test := range []struct {
		name        string
		body        string
		contentType string
		status      int
		wantCode    string
	}{
		{name: "known error", body: valid, contentType: "application/json", wantCode: "upstream_quota_schema_changed"},
		{name: "charset", body: valid, contentType: "application/json; charset=utf-8", wantCode: "upstream_quota_schema_changed"},
		{name: "exact size limit", body: valid + strings.Repeat(" ", maxInternalErrorBodyBytes-len(valid)), contentType: "application/json", wantCode: "upstream_quota_schema_changed"},
		{name: "oversized", body: valid + strings.Repeat(" ", maxInternalErrorBodyBytes) + sensitive, contentType: "application/json"},
		{name: "wrong status", body: valid, contentType: "application/json", status: http.StatusInternalServerError},
		{name: "unknown service failure", body: `{"error":"unknown"}`, contentType: "application/json", status: http.StatusServiceUnavailable},
		{name: "wrong content type", body: valid, contentType: "text/plain"},
		{name: "invalid content type", body: valid, contentType: "application/json; invalid"},
		{name: "missing content type", body: valid},
		{name: "unknown code", body: `{"error":"secret_shaped_code"}`, contentType: "application/json"},
		{name: "sensitive code", body: `{"error":"` + sensitive + `"}`, contentType: "application/json"},
		{name: "extra field", body: `{"error":"quota_schema_changed","access_token":"` + sensitive + `"}`, contentType: "application/json"},
		{name: "duplicate error", body: `{"error":"quota_schema_changed","error":"quota_schema_changed"}`, contentType: "application/json"},
		{name: "escaped duplicate", body: `{"error":"quota_schema_changed","\u0065rror":"quota_schema_changed"}`, contentType: "application/json"},
		{name: "case alias", body: `{"Error":"quota_schema_changed"}`, contentType: "application/json"},
		{name: "duplicate case alias", body: `{"error":"` + sensitive + `","Error":"quota_schema_changed"}`, contentType: "application/json"},
		{name: "multiple values", body: valid + ` {"token":"` + sensitive + `"}`, contentType: "application/json"},
		{name: "malformed", body: `{"error":"` + sensitive, contentType: "application/json"},
		{name: "nested object", body: `{"error":{"message":"` + sensitive + `"}}`, contentType: "application/json"},
		{name: "number", body: `{"error":502}`, contentType: "application/json"},
		{name: "null error", body: `{"error":null}`, contentType: "application/json"},
		{name: "missing error", body: `{}`, contentType: "application/json"},
		{name: "null body", body: `null`, contentType: "application/json"},
		{name: "array", body: `[{"error":"quota_schema_changed"}]`, contentType: "application/json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := test.status
			if status == 0 {
				status = http.StatusBadGateway
			}
			wantCode := test.wantCode
			if wantCode == "" {
				wantCode = "sidecar_request_failed"
			}
			body := &internalErrorCountingReader{Reader: strings.NewReader(test.body)}
			base, _ := url.Parse("http://sidecar.internal")
			client := NewWithHTTPClient(base, "secret", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: status, Body: io.NopCloser(body),
					Header: http.Header{"Content-Type": {test.contentType}, "Retry-After": {"7"}},
				}, nil
			})})
			_, err := client.QueryUpstreamAccountQuota(context.Background(), "0123456789abcdef")
			var internalErr *InternalAPIError
			if !errors.As(err, &internalErr) || internalErr.SafeCode() != wantCode || internalErr.StatusCode != status || internalErr.RetryAfter != 7 {
				t.Fatalf("error = %#v, want %s / %d", err, wantCode, status)
			}
			if internalErr.Cause != nil {
				t.Fatal("remote error retained a potentially sensitive decoder error")
			}
			if body.bytesRead > maxInternalErrorBodyBytes+1 {
				t.Fatalf("read %d error body bytes", body.bytesRead)
			}
			encoded, marshalErr := json.Marshal(internalErr)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, text := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%#v", err), string(encoded)} {
				if strings.Contains(text, sensitive) || strings.Contains(text, "secret_shaped_code") || strings.Contains(text, "access_token") {
					t.Fatal("remote error leaked sensitive or unrecognized data")
				}
			}
		})
	}
}

type internalErrorCountingReader struct {
	io.Reader
	bytesRead int
}

func (r *internalErrorCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytesRead += n
	return n, err
}

func TestInternalQuotaErrorReadFailureDoesNotLeak(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(internalErrorFailingReader{}),
	}
	err := internalStatusError(response)
	var internalErr *InternalAPIError
	if !errors.As(err, &internalErr) || internalErr.SafeCode() != "sidecar_request_failed" || internalErr.Cause != nil {
		t.Fatalf("error = %#v", err)
	}
}

type internalErrorFailingReader struct{}

func (internalErrorFailingReader) Read([]byte) (int, error) {
	return 0, errors.New("Bearer sensitive-canary")
}
