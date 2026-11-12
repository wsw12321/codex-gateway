package gatewayplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Identity of this plugin towards the host.
const (
	PluginID      = "codex-gateway"
	PluginVersion = "1.0.0"
	pluginAuthor  = "codex-gateway"
	pluginRepo    = "https://github.com/wsw12321/codex-gateway"
)

// Plugin is the Gateway integration. Its methods are safe for concurrent use.
type Plugin struct {
	cfg Config
	// unavailable is set when configuration or a control document could not be
	// loaded. The plugin then stays registered and refuses everything, which
	// is closed and observable, instead of leaving CPA to route on its own.
	unavailable error

	host        Host
	alloc       Allocator
	inventory   *Inventory
	control     *ControlState
	concurrency *Concurrency
	bindings    *Bindings
	roots       *RootRegistry
	requests    *requestTable
	loopback    *http.Client
	now         func() time.Time
	// applyProxy writes a credential's egress; see loopbackApplyProxy.
	applyProxy func(context.Context, Account, string) error
	// observeCredential reads passive quota observations; see
	// loopbackObserveCredential.
	observeCredential func(context.Context, Account) (credentialObservation, error)

	// reconcileStop ends the background egress passes; see startReconcile.
	reconcileMu   sync.Mutex
	reconcileStop chan struct{}
}

// New builds the plugin from the environment. It never fails: an unusable
// environment yields a plugin that rejects every request.
func New(host Host) *Plugin {
	cfg, err := LoadConfig(nil, nil)
	if err != nil {
		return &Plugin{unavailable: err, host: host, requests: newRequestTable(), now: time.Now}
	}
	return NewWithConfig(cfg, host, NewGatewayClient(cfg.GatewayURL, cfg.InternalKey, cfg.EgressProxyHost, cfg.CallbackTimeout))
}

// NewWithConfig builds the plugin from explicit dependencies.
func NewWithConfig(cfg Config, host Host, alloc Allocator) *Plugin {
	p := &Plugin{
		cfg: cfg, host: host, alloc: alloc,
		concurrency: NewConcurrency(cfg.HoldTTL), bindings: NewBindings(cfg.BindingTTL),
		roots: NewRootRegistry(), requests: newRequestTable(), now: time.Now,
		loopback: &http.Client{
			Timeout:       30 * time.Second,
			Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	p.applyProxy = p.loopbackApplyProxy
	p.observeCredential = p.loopbackObserveCredential
	control, err := LoadControlState(cfg.AuthDir)
	if err != nil {
		p.unavailable = err
		return p
	}
	inventory, err := NewInventory(host, cfg.AuthDir, cfg.AntigravityEnabled)
	if err != nil {
		p.unavailable = err
		return p
	}
	p.control, p.inventory = control, inventory
	return p
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func okEnvelope(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return errorEnvelope("plugin_error", "plugin_error", http.StatusInternalServerError)
	}
	out, _ := json.Marshal(envelope{OK: true, Result: raw})
	return out
}

func errorEnvelope(code, message string, status int) []byte {
	out, _ := json.Marshal(envelope{Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
	return out
}

// allocationEnvelope renders an allocation refusal. The message is the stable
// outcome code and nothing else: CPA copies it into the response body.
func allocationEnvelope(err error) []byte {
	if errors.Is(err, ErrConcurrency) {
		return errorEnvelope(ErrConcurrency.Error(), ErrConcurrency.Error(), http.StatusTooManyRequests)
	}
	return errorEnvelope(ErrUnavailable.Error(), ErrUnavailable.Error(), http.StatusServiceUnavailable)
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]bool    `json:"capabilities"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata:      pluginapi.Metadata{Name: PluginID, Version: PluginVersion, Author: pluginAuthor, GitHubRepository: pluginRepo},
		Capabilities: map[string]bool{
			"frontend_auth_provider":               true,
			"frontend_auth_provider_exclusive":     true,
			"frontend_auth_provider_omit_body":     true,
			"scheduler":                            true,
			"scheduler_across_priorities":          true,
			"request_interceptor":                  true,
			"request_interceptor_omit_body_before": true,
			"request_interceptor_omit_body_after":  true,
			"request_lifecycle_plugin":             true,
			"usage_plugin":                         true,
			"management_api":                       true,
		},
	}
}

// Handle serves one host RPC and always returns an envelope. A panic anywhere
// below is converted into an error here: the host cannot recover a panic
// raised in this plugin's Go runtime, and an unrecovered one would take the
// whole CPA process down with every in-flight request.
func (p *Plugin) Handle(method string, request []byte) (response []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logf("plugin panic recovered in %s: %T", method, recovered)
			if method == pluginabi.MethodSchedulerPick {
				response = allocationEnvelope(ErrUnavailable)
				return
			}
			response = errorEnvelope("plugin_error", "plugin_error", http.StatusInternalServerError)
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		p.startReconcile()
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		p.stopReconcile()
		return okEnvelope(struct{}{})
	case pluginabi.MethodFrontendAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": PluginID})
	case pluginabi.MethodFrontendAuthAuthenticate:
		return p.handleFrontendAuth(request)
	case pluginabi.MethodSchedulerPick:
		return p.handlePick(request)
	case pluginabi.MethodRequestInterceptBefore:
		return p.handleInterceptBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return p.handleInterceptAfter(request)
	case pluginabi.MethodRequestComplete:
		return p.handleComplete(request)
	case pluginabi.MethodUsageHandle:
		return p.handleUsage(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(p.managementRoutes())
	case pluginabi.MethodManagementHandle:
		return p.handleManagement(request)
	}
	return errorEnvelope("unknown_method", "unknown_method", http.StatusNotImplemented)
}

func (p *Plugin) handleFrontendAuth(raw []byte) []byte {
	var request pluginapi.FrontendAuthRequest
	if json.Unmarshal(raw, &request) != nil {
		return okEnvelope(pluginapi.FrontendAuthResponse{DenyStatusCode: http.StatusBadRequest, DenyMessage: "invalid_request"})
	}
	return okEnvelope(p.Authenticate(request.Headers))
}

// Authenticate admits only the Gateway. The principal is the Gateway's opaque
// per-caller scope, which makes CPA's native caller scope differ per caller;
// requests without one (model listing, health checks) share a fixed principal
// and are never allocated an account.
func (p *Plugin) Authenticate(headers http.Header) pluginapi.FrontendAuthResponse {
	if p.unavailable != nil || !bearerMatches(headers, p.cfg.InternalKey) {
		return pluginapi.FrontendAuthResponse{DenyStatusCode: http.StatusUnauthorized, DenyMessage: "invalid_internal_key"}
	}
	trusted, ok := ParseTrusted(headers, false)
	if !ok {
		return pluginapi.FrontendAuthResponse{DenyStatusCode: http.StatusBadRequest, DenyMessage: "invalid_gateway_identity"}
	}
	principal := trusted.Affinity
	if principal == "" {
		principal = "gateway"
	}
	return pluginapi.FrontendAuthResponse{Authenticated: true, Principal: principal}
}

func (p *Plugin) handlePick(raw []byte) []byte {
	if p.unavailable != nil {
		return allocationEnvelope(ErrUnavailable)
	}
	var request pluginapi.SchedulerPickRequest
	if json.Unmarshal(raw, &request) != nil {
		return allocationEnvelope(ErrUnavailable)
	}
	// One pick makes at most a handful of bounded Gateway calls.
	ctx, cancel := context.WithTimeout(context.Background(), 4*p.cfg.CallbackTimeout)
	defer cancel()
	authID, err := p.Pick(ctx, request)
	if err != nil {
		return allocationEnvelope(err)
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: authID})
}

type interceptRequest struct {
	pluginapi.RequestInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (p *Plugin) handleInterceptBefore(raw []byte) []byte {
	var request interceptRequest
	if json.Unmarshal(raw, &request) != nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	if token, present, ok := singleHeader(request.Headers, HeaderRequest); present && ok && validRequestToken(token) {
		generate, _ := request.Metadata[metadataGenerate].(bool)
		p.requests.begin(token, request.RequestID, generate)
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

// handleInterceptAfter removes every internal header once the account is
// chosen. CPA does not forward unknown inbound headers to a provider, so this
// is the second of two independent barriers.
func (p *Plugin) handleInterceptAfter(raw []byte) []byte {
	var request interceptRequest
	if json.Unmarshal(raw, &request) != nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	response := pluginapi.RequestInterceptResponse{}
	for name := range request.Headers {
		if internalHeader(name) {
			response.ClearHeaders = append(response.ClearHeaders, name)
		}
	}
	return okEnvelope(response)
}

func (p *Plugin) handleComplete(raw []byte) []byte {
	var request pluginapi.RequestCompletion
	if json.Unmarshal(raw, &request) == nil {
		p.Complete(request.RequestID, request.Outcome)
	}
	return okEnvelope(struct{}{})
}

// Complete releases the request's slot. A successful generation keeps the
// conversation's hold; an account-attributed failure drops it and unbinds the
// conversation; anything else leaves an existing hold as it was.
func (p *Plugin) Complete(requestID string, outcome pluginapi.RequestCompletionOutcome) {
	state := p.requests.complete(requestID)
	if state == nil || state.current == nil {
		return
	}
	slot := SlotKeep
	switch {
	case outcome == pluginapi.RequestCompletionSucceeded && state.generate:
		slot = SlotHold
	case outcome == pluginapi.RequestCompletionFailed && p.requests.accountFailed(state.current):
		slot = SlotDrop
	}
	p.finishAttempt(state.current, slot)
}

func (p *Plugin) handleUsage(raw []byte) []byte {
	var record pluginapi.UsageRecord
	if json.Unmarshal(raw, &record) == nil && record.Failed {
		p.ObserveFailure(record.SelectionID, record.Failure.StatusCode, record.Failure.Body)
	}
	return okEnvelope(struct{}{})
}

// ObserveFailure applies one failed attempt. The body is inspected only for
// the structured usage-limit marker and is never logged or stored.
func (p *Plugin) ObserveFailure(selectionID string, status int, body string) {
	if p.unavailable != nil || selectionID == "" {
		return
	}
	failed, released := p.requests.selection(selectionID)
	if failed == nil {
		return
	}
	if failed.provider == ProviderCodex && usageLimitReached(status, body) {
		if changed, err := p.control.MarkQuotaExhausted(failed.account, failed.controlVersion); err != nil {
			logf("quota lock for account %s was not persisted: %T", failed.account, err)
		} else if changed {
			logf("account %s locked after an upstream usage limit", failed.account)
		}
	}
	if !accountAttributed(status) {
		return
	}
	if released {
		// The execution is already over; undo the hold and binding it left.
		if failed.bound {
			p.bindings.Delete(failed.bindKey, failed.account)
		}
		p.concurrency.DropHold(failed.account, failed.rootKey)
		return
	}
	p.requests.markAccountFailure(failed)
}

// accountAttributed reports whether a failure status blames the account
// rather than the request.
func accountAttributed(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests || status >= 500
}

// usageLimitReached recognizes the structured marker a provider sends when an
// account's plan allowance is exhausted. A message that merely contains the
// words does not count, and neither does an authentication failure.
func usageLimitReached(status int, body string) bool {
	if status != http.StatusTooManyRequests && status != http.StatusBadRequest {
		return false
	}
	var document struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &document) != nil {
		return false
	}
	return document.Error.Type == "usage_limit_reached" || document.Type == "usage_limit_reached"
}

// logSink receives operational messages. They carry fixed text, account
// indexes and Go type names only: never request content, tokens or raw
// upstream error text.
var logSink = func(string) {}

// SetLogSink installs the host logger.
func SetLogSink(sink func(string)) {
	if sink != nil {
		logSink = sink
	}
}

func logf(format string, args ...any) {
	logSink(fmt.Sprintf(format, args...))
}
