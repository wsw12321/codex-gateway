package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func diagnosticTestProbe() ModelIdentificationProbe {
	return ModelIdentificationProbe{
		RunID: "00000000-0000-0000-0000-000000000002", UserID: "00000000-0000-0000-0000-000000000001",
		AccountID: "0123456789abcdef", Model: "gpt-6-sol", Prompt: "Prompt", ProbeIndex: 1,
	}
}

func assertDiagnosticCode(t *testing.T, err error, want string) *ModelIdentificationError {
	t.Helper()
	var diagnostic *ModelIdentificationError
	if !errors.As(err, &diagnostic) || diagnostic.SafeCode() != want {
		t.Fatalf("diagnostic error=%v, want %s", err, want)
	}
	return diagnostic
}

func TestModelIdentificationDirectProtocolAndTrustedMetadata(t *testing.T) {
	probe := diagnosticTestProbe()
	var calls []string
	client := NewWithHTTPClient(&url.URL{Scheme: "http", Host: "sidecar.test"}, "internal-secret", &http.Client{
		Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
			calls = append(calls, r.Method+" "+r.URL.Path)
			if r.Header.Get("Authorization") != "Bearer internal-secret" || r.Header.Get("Cache-Control") != "no-store" {
				t.Error("missing trusted internal authentication")
			}
			switch r.URL.Path {
			case modelIdentificationPath + "/capabilities":
				return identificationInternalResponse(200, `{"protocol":"model_identification_direct_v1"}`), nil
			case modelIdentificationPath + "/accounts/" + probe.AccountID + "/models":
				return identificationInternalResponse(200, `{"account_id":"`+probe.AccountID+`","models":["gpt-6-sol","unpriced-model"]}`), nil
			case modelIdentificationPath + "/accounts/" + probe.AccountID + "/probe":
				if r.Header.Get(gatewayUserHeader) != probe.UserID || r.Header.Get(diagnosticRunHeader) != probe.RunID || r.Header.Get(diagnosticProbeHeader) != "1" {
					t.Error("untrusted diagnostic identity")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if len(body) != 3 || body["model"] != probe.Model || body["input"] != probe.Prompt || body["max_output_tokens"] != float64(16384) {
					t.Error("unexpected probe body")
				}
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > ModelIdentificationProbeTimeout || time.Until(deadline) < 180*time.Second {
					t.Error("incorrect diagnostic deadline")
				}
				return identificationInternalResponse(200, `{"account_id":"`+probe.AccountID+`","output_text":" 17, 82 \n"}`), nil
			default:
				t.Errorf("unexpected ordinary-routing endpoint %q", r.URL.Path)
				return identificationInternalResponse(404, `{}`), nil
			}
		}),
	})
	if err := client.RequireModelIdentificationCapability(context.Background()); err != nil {
		t.Fatal(err)
	}
	models, err := client.ListModelIdentificationModels(context.Background(), probe.AccountID)
	if err != nil || len(models) != 2 || models[1] != "unpriced-model" {
		t.Fatalf("models=%v error=%v", models, err)
	}
	reply, err := client.ProbeModelIdentification(context.Background(), probe)
	if err != nil || reply != " 17, 82 \n" || len(calls) != 3 {
		t.Fatalf("probe error=%v calls=%v", err, calls)
	}
}

func TestModelIdentificationCapabilityFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"old version", `{"protocol":"old"}`, 200, "protocol_unsupported"},
		{"no route", `not found`, 404, "protocol_unsupported"},
		{"ordinary capability", `{"protocol":"upstream_account_access_v1"}`, 200, "protocol_unsupported"},
		{"duplicate", `{"protocol":"old","protocol":"model_identification_direct_v1"}`, 200, "probe_invalid_response"},
		{"extra", `{"protocol":"model_identification_direct_v1","secret":"do not log"}`, 200, "probe_invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := NewWithHTTPClient(&url.URL{Scheme: "http", Host: "sidecar.test"}, "key", &http.Client{Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != modelIdentificationPath+"/capabilities" {
					t.Error("fell back to legacy probe")
				}
				return identificationInternalResponse(tc.status, tc.body), nil
			})})
			assertDiagnosticCode(t, client.RequireModelIdentificationCapability(context.Background()), tc.want)
			if calls != 1 {
				t.Fatal("retried failed capability")
			}
		})
	}
}

func TestModelIdentificationErrorEnvelopeIsBoundedAndSafe(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"upstream auth", `{"error":{"code":"probe_authentication_failed","stage":"probing","upstream_status":401}}`, "probe_authentication_failed"},
		{"limit", `{"error":{"code":"probe_rate_limited","stage":"probing","upstream_status":429,"retry_after":12}}`, "probe_rate_limited"},
		{"credential", `{"error":{"code":"probe_credential_unavailable","stage":"preflight"}}`, "probe_credential_unavailable"},
		{"incomplete", `{"error":{"code":"probe_response_incomplete","stage":"validating"}}`, "probe_response_incomplete"},
		{"unknown", `{"error":{"code":"secret_credential","stage":"probing"}}`, "probe_unavailable"},
		{"extra text", `{"error":{"code":"probe_rate_limited","stage":"probing","message":"secret_credential"}}`, "probe_unavailable"},
		{"wrong stage", `{"error":{"code":"probe_rate_limited","stage":"secret_credential"}}`, "probe_unavailable"},
		{"bad status", `{"error":{"code":"probe_rate_limited","stage":"probing","upstream_status":900}}`, "probe_unavailable"},
		{"negative delay", `{"error":{"code":"probe_rate_limited","stage":"probing","retry_after":-1}}`, "probe_unavailable"},
		{"huge delay", `{"error":{"code":"probe_rate_limited","stage":"probing","retry_after":3601}}`, "probe_unavailable"},
		{"fractional delay", `{"error":{"code":"probe_rate_limited","stage":"probing","retry_after":1.1}}`, "probe_unavailable"},
		{"duplicate", `{"error":{"code":"secret_credential","code":"probe_rate_limited","stage":"probing"}}`, "probe_unavailable"},
		{"trailing", `{"error":{"code":"probe_rate_limited","stage":"probing"}} {}`, "probe_unavailable"},
		{"oversized", strings.Repeat("secret_credential", 100), "probe_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := diagnosticStatusError(context.Background(), identificationInternalResponse(502, tc.body))
			failure := assertDiagnosticCode(t, err, tc.want)
			if strings.Contains(err.Error(), "secret_credential") || failure.Cause != nil {
				t.Fatal("retained unsafe upstream data")
			}
			if tc.name == "limit" && (failure.RetryAfter != 12 || failure.UpstreamStatus != 429) {
				t.Fatal("lost safe retry metadata")
			}
		})
	}
}

func TestModelIdentificationRejectsInvalidSelectionsWithoutDispatch(t *testing.T) {
	client := NewWithHTTPClient(&url.URL{Scheme: "http", Host: "sidecar.test"}, "key", &http.Client{Transport: identificationTransportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("invalid selection dispatched")
		return nil, errors.New("unexpected")
	})})
	for _, mutate := range []func(*ModelIdentificationProbe){
		func(p *ModelIdentificationProbe) { p.UserID = "" }, func(p *ModelIdentificationProbe) { p.RunID = "arbitrary" },
		func(p *ModelIdentificationProbe) { p.ProbeIndex = 4 }, func(p *ModelIdentificationProbe) { p.AccountID = "../other" },
		func(p *ModelIdentificationProbe) { p.Model = "model\n" }, func(p *ModelIdentificationProbe) { p.Prompt = "a\x00b" },
	} {
		probe := diagnosticTestProbe()
		mutate(&probe)
		if _, err := client.ProbeModelIdentification(context.Background(), probe); err == nil {
			t.Fatal("accepted invalid probe")
		}
	}
}

func TestModelIdentificationRejectsMalformedSuccess(t *testing.T) {
	probe := diagnosticTestProbe()
	for _, tc := range []struct{ name, body, want string }{
		{"wrong account", `{"account_id":"ffffffffffffffff","output_text":"answer"}`, "probe_account_mismatch"},
		{"empty", `{"account_id":"` + probe.AccountID + `","output_text":" \n"}`, "probe_empty_output"},
		{"large", `{"account_id":"` + probe.AccountID + `","output_text":"` + strings.Repeat("x", (16<<10)+1) + `"}`, "probe_output_too_large"},
		{"trailing", `{"account_id":"` + probe.AccountID + `","output_text":"answer"} {}`, "probe_invalid_response"},
		{"unexpected field", `{"account_id":"` + probe.AccountID + `","output_text":"answer","debug":"secret"}`, "probe_invalid_response"},
		{"invalid UTF-8", "{\"account_id\":\"" + probe.AccountID + "\",\"output_text\":\"\xff\"}", "probe_invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewWithHTTPClient(&url.URL{Scheme: "http", Host: "sidecar.test"}, "key", &http.Client{Transport: identificationTransportFunc(func(*http.Request) (*http.Response, error) { return identificationInternalResponse(200, tc.body), nil })})
			_, err := client.ProbeModelIdentification(context.Background(), probe)
			assertDiagnosticCode(t, err, tc.want)
		})
	}
}

func TestModelIdentificationWaitsBeyondOrdinaryHeaderTimeout(t *testing.T) {
	client := New(&url.URL{Scheme: "http", Host: "sidecar.test"}, "key")
	ordinary := client.http.Transport.(*http.Transport)
	diagnostic := client.diagnosticHTTP.Transport.(*http.Transport)
	if ordinary == diagnostic || ordinary.ResponseHeaderTimeout != 90*time.Second || diagnostic.ResponseHeaderTimeout != 0 {
		t.Fatal("diagnostic changed the ordinary transport or retained its shorter deadline")
	}
	synctest.Test(t, func(t *testing.T) {
		probe := diagnosticTestProbe()
		client := NewWithHTTPClient(&url.URL{Scheme: "http", Host: "sidecar.test"}, "key", &http.Client{Timeout: 90 * time.Second, Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
			select {
			case <-time.After(91 * time.Second):
				return identificationInternalResponse(200, `{"account_id":"`+probe.AccountID+`","output_text":"answer"}`), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})})
		if _, err := client.ProbeModelIdentification(context.Background(), probe); err != nil {
			t.Fatalf("prematurely interrupted slow probe: %v", err)
		}
	})
}

func TestModelIdentificationDeadlineAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				client := NewWithHTTPClient(&url.URL{Scheme: "http", Host: "sidecar.test"}, "key", &http.Client{Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
					if cancelEarly {
						cancel()
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})})
				_, err := client.ProbeModelIdentification(ctx, diagnosticTestProbe())
				want := "probe_timeout"
				cause := context.DeadlineExceeded
				if cancelEarly {
					want = "probe_canceled"
					cause = context.Canceled
				}
				assertDiagnosticCode(t, err, want)
				if !errors.Is(err, cause) {
					t.Fatalf("lost cancellation cause: %v", err)
				}
			})
		})
	}
}

func TestModelIdentificationNeverFollowsRedirectsOrForwardsUntrustedHeaders(t *testing.T) {
	var downstream atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downstream.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer sidecar.Close()
	base, _ := url.Parse(sidecar.URL)
	client := NewWithHTTPClient(base, "secret", &http.Client{})
	_, err := client.ProbeModelIdentification(context.Background(), diagnosticTestProbe())
	assertDiagnosticCode(t, err, "probe_unavailable")
	if downstream.Load() != 0 {
		t.Fatal("leaked diagnostic credentials through redirect")
	}
	out := make(http.Header)
	client.copyAllowedHeaders(out, http.Header{diagnosticRunHeader: {"forged"}, diagnosticProbeHeader: {"1"}})
	if out.Get(diagnosticRunHeader) != "" || out.Get(diagnosticProbeHeader) != "" {
		t.Fatal("ordinary callers can inject diagnostic metadata")
	}
}

func TestModelIdentificationNetworkErrorsNeverExposeRequestDetails(t *testing.T) {
	client := NewWithHTTPClient(&url.URL{Scheme: "http", Host: "sidecar.test"}, "key", &http.Client{Transport: identificationTransportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret URL or credential") })})
	_, err := client.ProbeModelIdentification(context.Background(), diagnosticTestProbe())
	assertDiagnosticCode(t, err, "probe_network_failed")
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("unsafe network error")
	}
}

type diagnosticBlockedBody struct {
	ctx    context.Context
	closed bool
}

func (b *diagnosticBlockedBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *diagnosticBlockedBody) Close() error { b.closed = true; return nil }

func TestModelIdentificationCancellationWhileReadingErrorBody(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var body *diagnosticBlockedBody
				client := NewWithHTTPClient(&url.URL{Scheme: "http", Host: "sidecar.test"}, "key", &http.Client{Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
					body = &diagnosticBlockedBody{ctx: r.Context()}
					if cancelEarly {
						cancel()
					}
					return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
				})})
				_, err := client.ProbeModelIdentification(ctx, diagnosticTestProbe())
				want, cause := "probe_timeout", context.DeadlineExceeded
				if cancelEarly {
					want, cause = "probe_canceled", context.Canceled
				}
				assertDiagnosticCode(t, err, want)
				if !errors.Is(err, cause) || !body.closed {
					t.Fatalf("lost cancellation or leaked response: %v", err)
				}
			})
		})
	}
}
