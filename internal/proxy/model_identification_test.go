package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type identificationTransportFunc func(*http.Request) (*http.Response, error)

func (fn identificationTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func identificationInternalResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

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

// diagnosticClient builds a client the way the server does: with CPA's
// management key and an allocation ledger.
func diagnosticClient(httpClient *http.Client) (*Client, *AllocationLedger) {
	ledger := NewAllocationLedger()
	client := withManagement(NewWithHTTPClient(&url.URL{Scheme: "http", Host: "sidecar.test"}, "internal-secret", httpClient))
	client.UseAllocationLedger(ledger)
	return client, ledger
}

const diagnosticCompleted = `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":" 17, 82 \n"}]}]}`

// The capability and the model list are plugin routes behind the management
// key; the probe itself is an ordinary request with the data-plane key.
func TestModelIdentificationRoutesKeysAndDeadline(t *testing.T) {
	probe := diagnosticTestProbe()
	var calls []string
	var ledger *AllocationLedger
	client, ledger := diagnosticClient(&http.Client{
		Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
			calls = append(calls, r.Method+" "+r.URL.RequestURI())
			if r.Header.Get("Cache-Control") != "no-store" {
				t.Error("diagnostic request may be cached")
			}
			management := r.Header.Get("Authorization") == "Bearer "+testManagementKey
			switch r.URL.RequestURI() {
			case pluginPath("model-identification/capabilities"):
				if !management {
					t.Error("capability route without the management key")
				}
				return identificationInternalResponse(200, `{"protocol":"model_identification_cpa_v1"}`), nil
			case pluginPath("model-identification/accounts/models") + "?id=" + probe.AccountID:
				if !management {
					t.Error("model route without the management key")
				}
				return identificationInternalResponse(200, `{"account_id":"`+probe.AccountID+`","models":["gpt-6-sol","unpriced-model"]}`), nil
			case "/v1/responses":
				if r.Header.Get("Authorization") != "Bearer internal-secret" {
					t.Error("the probe must use the data-plane key")
				}
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > ModelIdentificationProbeTimeout || time.Until(deadline) < 180*time.Second {
					t.Error("incorrect diagnostic deadline")
				}
				reportAllocation(t, ledger, "codex", r, probe.AccountID, "")
				return identificationInternalResponse(200, diagnosticCompleted), nil
			default:
				t.Errorf("unexpected request %q", r.URL.RequestURI())
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
		// The removed source-patched image's protocol sent probes down a
		// path of its own; it is not this one.
		{"source-patched protocol", `{"protocol":"model_identification_direct_v1"}`, 200, "protocol_unsupported"},
		{"duplicate", `{"protocol":"old","protocol":"model_identification_cpa_v1"}`, 200, "probe_invalid_response"},
		{"extra", `{"protocol":"model_identification_cpa_v1","secret":"do not log"}`, 200, "probe_invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client, _ := diagnosticClient(&http.Client{Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != pluginPath("model-identification/capabilities") {
					t.Error("fell back to another route")
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

// Without the management key there is no route to ask, and without a ledger
// nothing pins a probe to its account: both refuse before any request.
func TestModelIdentificationRequiresManagementKeyAndLedger(t *testing.T) {
	transport := identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request %s", r.URL.Path)
		return nil, errors.New("unexpected")
	})
	base := &url.URL{Scheme: "http", Host: "sidecar.test"}
	unkeyed := NewWithHTTPClient(base, "internal-secret", &http.Client{Transport: transport})
	unkeyed.UseAllocationLedger(NewAllocationLedger())
	assertDiagnosticCode(t, unkeyed.RequireModelIdentificationCapability(context.Background()), "probe_unavailable")
	_, err := unkeyed.ListModelIdentificationModels(context.Background(), diagnosticTestProbe().AccountID)
	assertDiagnosticCode(t, err, "probe_unavailable")

	unpinned := withManagement(NewWithHTTPClient(base, "internal-secret", &http.Client{Transport: transport}))
	_, err = unpinned.ProbeModelIdentification(context.Background(), diagnosticTestProbe())
	assertDiagnosticCode(t, err, "probe_unavailable")
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
	client, _ := diagnosticClient(&http.Client{Transport: identificationTransportFunc(func(*http.Request) (*http.Response, error) {
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

func TestModelIdentificationWaitsBeyondOrdinaryHeaderTimeout(t *testing.T) {
	client := withManagement(New(&url.URL{Scheme: "http", Host: "sidecar.test"}, "key"))
	ordinary := client.http.Transport.(*http.Transport)
	diagnostic := client.diagnosticHTTP.Transport.(*http.Transport)
	if ordinary == diagnostic || ordinary.ResponseHeaderTimeout != 90*time.Second || diagnostic.ResponseHeaderTimeout != 0 {
		t.Fatal("diagnostic changed the ordinary transport or retained its shorter deadline")
	}
	synctest.Test(t, func(t *testing.T) {
		probe := diagnosticTestProbe()
		var ledger *AllocationLedger
		client, ledger := diagnosticClient(&http.Client{Timeout: 90 * time.Second, Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
			select {
			case <-time.After(91 * time.Second):
				reportAllocation(t, ledger, "codex", r, probe.AccountID, "")
				return identificationInternalResponse(200, diagnosticCompleted), nil
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
				client, _ := diagnosticClient(&http.Client{Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
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
	client := withManagement(NewWithHTTPClient(base, "secret", &http.Client{}))
	client.UseAllocationLedger(NewAllocationLedger())
	_, err := client.ProbeModelIdentification(context.Background(), diagnosticTestProbe())
	assertDiagnosticCode(t, err, "probe_upstream_rejected")
	assertDiagnosticCode(t, client.RequireModelIdentificationCapability(context.Background()), "probe_unavailable")
	if downstream.Load() != 0 {
		t.Fatal("leaked diagnostic credentials through redirect")
	}
	// An ordinary caller can supply neither a dispatch token nor an identity,
	// so it cannot make its request look like a pinned probe.
	out := make(http.Header)
	client.copyAllowedHeaders(out, http.Header{dispatchHeader: {strings.Repeat("A", 43)}, gatewayUserHeader: {"00000000-0000-0000-0000-000000000009"}})
	if out.Get(dispatchHeader) != "" || out.Get(gatewayUserHeader) != "" {
		t.Fatal("ordinary callers can inject probe metadata")
	}
}

func TestModelIdentificationNetworkErrorsNeverExposeRequestDetails(t *testing.T) {
	client, _ := diagnosticClient(&http.Client{Transport: identificationTransportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret URL or credential") })})
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
				client, _ := diagnosticClient(&http.Client{Transport: identificationTransportFunc(func(r *http.Request) (*http.Response, error) {
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
