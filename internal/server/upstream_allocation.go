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
	if err := decodeSingleJSONField(r, upstreamAllocationRequestBytes, "account_ids", &ids); err != nil {
		badJSON(w, r, err)
		return
	}
	seen := make(map[string]bool, len(ids))
	valid := len(ids) > 0 && len(ids) <= store.MaxUpstreamAllocationCandidates
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
	id, err := s.store.SelectUpstreamAccount(r.Context(), ids, time.Now().UTC())
	if err != nil {
		if s.logger != nil && !errors.Is(err, store.ErrNoUpstreamAccount) {
			s.logger.Error("select upstream account failed", "request_id", httpx.RequestID(r.Context()))
		}
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "upstream_allocation_unavailable", "暂时无法分配上游账号")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"account_id": id})
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
	key, err := decoder.Token()
	if err != nil || key != field {
		return errors.New("request field missing")
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return errors.New("request must contain exactly one field")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}
