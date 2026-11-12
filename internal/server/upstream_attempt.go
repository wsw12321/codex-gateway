package server

import (
	"bytes"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
)

const upstreamAttemptRequestBytes = 1 << 10

// attemptProvider names the provider of an internal accounts route in the
// allocation ledger's vocabulary.
func attemptProvider(r *http.Request) string {
	switch {
	case isAnthropicAccountRequest(r):
		return "anthropic"
	case isAntigravityAccountRequest(r):
		return "antigravity"
	}
	return "codex"
}

// recordUpstreamAttempt receives one allocation decision from the CPA plugin.
// The report only attributes a dispatch this process is already waiting on; it
// never creates one. The account is checked against the user's entitlement
// again here, so even a faulty plugin cannot attribute, and therefore bill, an
// account the user may not use.
func (s *Server) recordUpstreamAttempt(w http.ResponseWriter, r *http.Request) {
	authorization := r.Header.Values("Authorization")
	if s.config.SidecarToken == "" || len(authorization) != 1 ||
		!hmac.Equal([]byte(authorization[0]), []byte("Bearer "+s.config.SidecarToken)) {
		httpx.WriteError(w, r, http.StatusUnauthorized, "authentication_error", "invalid_sidecar_token", "内部服务认证失败")
		return
	}
	if s.allocations == nil {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_attempt_unsupported", "当前集成方式不接收分配上报")
		return
	}
	if !strictJSONRequest(r) || r.ContentLength > upstreamAttemptRequestBytes {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_attempt", "分配上报无效")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, upstreamAttemptRequestBytes+1))
	if err != nil || len(body) > upstreamAttemptRequestBytes || rejectDuplicateJSON(body) != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_attempt", "分配上报无效")
		return
	}
	var wire struct {
		RequestToken     string `json:"request_token"`
		UserID           string `json:"user_id"`
		AccountID        string `json:"account_id"`
		ConversationHash string `json:"conversation_hash"`
		Rejected         string `json:"rejected"`
	}
	if decodeStrictBody(body, &wire) != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_attempt", "分配上报无效")
		return
	}
	report := gatewayproxy.AllocationReport{
		Token: wire.RequestToken, UserID: wire.UserID, AccountID: wire.AccountID,
		ConversationHash: wire.ConversationHash, Rejected: wire.Rejected,
	}
	if !report.Valid() {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_attempt", "分配上报无效")
		return
	}
	if report.AccountID != "" {
		allowed, errEligible := s.accountStore(r).EligibleUpstreamAccountLimits(r.Context(), report.UserID, []string{report.AccountID})
		if errEligible != nil {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "upstream_allocation_unavailable", "暂时无法查询上游账号权限")
			return
		}
		if len(allowed) != 1 || allowed[0].ID != report.AccountID {
			httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "upstream_account_not_permitted", "用户无权使用该上游账号")
			return
		}
	}
	switch err = s.allocations.Record(attemptProvider(r), report); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, gatewayproxy.ErrAccountOutsideScope):
		httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "upstream_account_not_permitted", "该上游账号不在本次请求的账号池内")
	case errors.Is(err, gatewayproxy.ErrUnknownDispatch):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "unknown_upstream_dispatch", "没有与该上报对应的进行中请求")
	default:
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_attempt", "分配上报无效")
	}
}

// decodeStrictBody decodes exactly one JSON object with no unknown fields.
func decodeStrictBody(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}
