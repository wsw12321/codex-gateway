package server

import (
	"bytes"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	upstreamAllocationRequestBytes = 8 << 10
	upstreamWeightRequestBytes     = 256
)

// Only the compatibility service may choose a new binding. Candidate discovery
// belongs to that service; this handler deliberately never calls it back.
func (s *Server) selectUpstreamAccount(w http.ResponseWriter, r *http.Request) {
	s.upstreamAccountSelection(w, r, false)
}

func (s *Server) eligibleUpstreamAccounts(w http.ResponseWriter, r *http.Request) {
	s.upstreamAccountSelection(w, r, true)
}

func (s *Server) upstreamAccountSelection(w http.ResponseWriter, r *http.Request, eligibility bool) {
	authorization := r.Header.Values("Authorization")
	if s.config.SidecarToken == "" || len(authorization) != 1 ||
		!hmac.Equal([]byte(authorization[0]), []byte("Bearer "+s.config.SidecarToken)) {
		httpx.WriteError(w, r, http.StatusUnauthorized, "authentication_error", "invalid_sidecar_token", "内部服务认证失败")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_selection_request", "选路请求必须为 JSON，且不能包含查询参数")
		return
	}
	var ids []string
	var userID string
	if err := decodeExactJSONFields(r, upstreamAllocationRequestBytes, map[string]any{"account_ids": &ids, "user_id": &userID}); err != nil {
		badJSON(w, r, err)
		return
	}
	seen := make(map[string]bool, len(ids))
	parsedUser, parseErr := uuid.Parse(userID)
	valid := parseErr == nil && parsedUser.String() == userID && len(ids) > 0 && len(ids) <= store.MaxUpstreamAllocationCandidates
	for _, id := range ids {
		if !validUpstreamAccountID(id) || seen[id] {
			valid = false
		}
		seen[id] = true
	}
	if !valid {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_candidates", "候选账号列表无效")
		return
	}
	if eligibility {
		allowed, err := s.store.EligibleUpstreamAccountLimits(r.Context(), userID, ids)
		if err != nil {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "upstream_allocation_unavailable", "暂时无法查询上游账号权限")
			return
		}
		accounts := make([]map[string]any, 0, len(allowed))
		for _, account := range allowed {
			accounts = append(accounts, map[string]any{"id": account.ID, "concurrent_limit": account.ConcurrentLimit})
		}
		writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
		return
	}
	id, err := s.store.SelectUpstreamAccount(r.Context(), userID, ids, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrNoUpstreamAccount) {
			httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_error", "upstream_concurrency_exceeded", "暂无可用的上游账号，请稍后重试")
			return
		}
		if s.logger != nil {
			s.logger.Error("select upstream account failed", "request_id", httpx.RequestID(r.Context()))
		}
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "upstream_allocation_unavailable", "暂时无法分配上游账号")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"account_id": id})
}

func (s *Server) setUpstreamAccountConcurrentLimit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUpstreamAccountID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_concurrency_request", "并发对话数量请求必须为 JSON，且不能包含查询参数")
		return
	}
	var limit *int64
	if err := decodeSingleJSONField(r, upstreamWeightRequestBytes, "concurrent_limit", &limit); err != nil {
		badJSON(w, r, err)
		return
	}
	if limit == nil || *limit < 1 || *limit > store.MaxUpstreamConcurrentLimit {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_concurrency_limit", "并发对话数量必须是 1 至 2147483647 的整数")
		return
	}
	account, err := s.store.SetUpstreamAccountConcurrentLimit(r.Context(), store.SetUpstreamAccountConcurrentLimitParams{
		AccountID: id, Limit: int(*limit), At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "set upstream concurrent limit", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": account.ID, "concurrent_limit": account.ConcurrentLimit})
}

func (s *Server) setUpstreamAccountAccess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUpstreamAccountID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_access_request", "账号权限请求必须为 JSON，且不能包含查询参数")
		return
	}
	var mode, reason string
	var users []string
	if err := decodeExactJSONFields(r, 512<<10, map[string]any{"mode": &mode, "user_ids": &users, "reason": &reason}); err != nil {
		badJSON(w, r, err)
		return
	}
	account, err := s.store.SetUpstreamAccountAccess(r.Context(), store.SetUpstreamAccountAccessParams{
		AccountID: id, Mode: mode, UserIDs: users, Reason: reason, At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "set upstream account access", err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

func (s *Server) setUpstreamAccountAllocationWeight(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUpstreamAccountID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_weight_request", "分配系数请求必须为 JSON，且不能包含查询参数")
		return
	}
	var weight *int64
	if err := decodeSingleJSONField(r, upstreamWeightRequestBytes, "weight", &weight); err != nil {
		badJSON(w, r, err)
		return
	}
	if weight == nil || *weight < 0 || *weight > store.MaxUpstreamAllocationWeight {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_allocation_weight", "分配系数必须是 0 至 2147483647 的整数")
		return
	}
	account, err := s.store.SetUpstreamAccountAllocationWeight(r.Context(), store.SetUpstreamAccountAllocationWeightParams{
		AccountID: id, Weight: int(*weight), At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "set upstream allocation weight", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": account.ID, "allocation_weight": account.AllocationWeight})
}

func strictJSONRequest(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json" && r.URL.RawQuery == ""
}

// Decode the exact one-field protocol, rejecting duplicate/case-folded keys,
// oversized bodies (including chunked requests), and additional JSON values.
func decodeSingleJSONField(r *http.Request, limit int64, field string, destination any) error {
	return decodeExactJSONFields(r, limit, map[string]any{field: destination})
}

func decodeExactJSONFields(r *http.Request, limit int64, fields map[string]any) error {
	if r.ContentLength > limit {
		return &http.MaxBytesError{Limit: limit}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return &http.MaxBytesError{Limit: limit}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return errors.New("request must be an object")
	}
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || fields[name] == nil || seen[name] {
			return errors.New("invalid request field")
		}
		seen[name] = true
		if err := decoder.Decode(fields[name]); err != nil {
			return err
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return errors.New("request must be an object")
	}
	if len(seen) != len(fields) {
		return errors.New("request field missing")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}
