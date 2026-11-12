package server

import (
	"bytes"
	"context"
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
	// Forty characters may each arrive as a 12-byte escaped surrogate pair.
	upstreamAliasRequestBytes = 1 << 10
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
	token := s.config.SidecarToken
	if isAntigravityAccountRequest(r) && !s.config.UsesCPAAntigravity() {
		token = s.config.AntigravityBridgeToken
	}
	authorization := r.Header.Values("Authorization")
	if token == "" || len(authorization) != 1 ||
		!hmac.Equal([]byte(authorization[0]), []byte("Bearer "+token)) {
		httpx.WriteError(w, r, http.StatusUnauthorized, "authentication_error", "invalid_sidecar_token", "内部服务认证失败")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_selection_request", "选路请求必须为 JSON，且不能包含查询参数")
		return
	}
	request, err := decodeUpstreamSelectionRequest(r)
	if err != nil {
		badJSON(w, r, err)
		return
	}
	ids, userID := request.AccountIDs, request.UserID
	if !validUpstreamSelectionRequest(request) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_candidates", "候选账号列表无效")
		return
	}
	if isAntigravityAccountRequest(r) && !s.legacyEgressAllowed(w, r) {
		return
	}
	if pinned, ok := s.allocations.ProbeAccount(userID); ok {
		s.probeAccountSelection(w, r, request, pinned, eligibility)
		return
	}
	// account_ids is the sidecar's current model/attempt candidate set. Group
	// snapshots may contain unsupported or already-tried accounts, so neither
	// eligibility nor selection may expand beyond that set. The groups-only
	// protocol remains supported; an explicit empty/null set stays empty.
	if request.accountIDsProvided && len(request.Groups) > 0 {
		candidates := make(map[string]bool, len(ids))
		for _, id := range ids {
			candidates[id] = true
		}
		for i, group := range request.Groups {
			filtered := make([]string, 0, len(group.AccountIDs))
			for _, id := range group.AccountIDs {
				if candidates[id] {
					filtered = append(filtered, id)
				}
			}
			request.Groups[i].AccountIDs = filtered
		}
	}
	if eligibility {
		if len(request.Groups) == 0 {
			allowed, err := s.accountStore(r).EligibleUpstreamAccountLimits(r.Context(), userID, ids)
			if err != nil {
				httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "upstream_allocation_unavailable", "暂时无法查询上游账号权限")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"accounts": eligibilityRows(allowed)})
			return
		}
		groups := make([]map[string]any, 0, len(request.Groups))
		for _, group := range request.Groups {
			if len(group.AccountIDs) == 0 {
				groups = append(groups, map[string]any{"group_id": group.GroupID, "accounts": []map[string]any{}})
				continue
			}
			allowed, err := s.accountStore(r).EligibleUpstreamAccountLimits(r.Context(), userID, group.AccountIDs)
			if err != nil {
				httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "upstream_allocation_unavailable", "暂时无法查询上游账号权限")
				return
			}
			groups = append(groups, map[string]any{"group_id": group.GroupID, "accounts": eligibilityRows(allowed)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
		return
	}
	var id string
	if len(request.Groups) > 0 {
		// Groups are ordered by the caller's billing priority. Filter each group
		// through the same permission/concurrency admission query before asking
		// weighted allocation to choose an account. Empty or zero-weight groups
		// naturally fall through to the next group.
		for _, group := range request.Groups {
			if len(group.AccountIDs) == 0 {
				continue
			}
			allowed, e := s.accountStore(r).EligibleUpstreamAccountLimits(r.Context(), userID, group.AccountIDs)
			if e != nil {
				httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "upstream_allocation_unavailable", "暂时无法查询上游账号权限")
				return
			}
			candidates := make([]string, 0, len(allowed))
			for _, account := range allowed {
				candidates = append(candidates, account.ID)
			}
			if len(candidates) == 0 {
				continue
			}
			id, e = s.accountStore(r).SelectUpstreamAccount(r.Context(), userID, candidates, time.Now().UTC())
			if e == nil {
				break
			}
			if !errors.Is(e, store.ErrNoUpstreamAccount) {
				err = e
				break
			}
		}
		// A group-bound request must fail when its authorized pools are
		// exhausted. Falling back to account_ids would spend group funds on an
		// account outside the admission-time pool.
		if id == "" && err == nil {
			err = store.ErrNoUpstreamAccount
		}
	} else {
		id, err = s.accountStore(r).SelectUpstreamAccount(r.Context(), userID, ids, time.Now().UTC())
	}
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
	concurrencyName := "并发对话数量"
	id := r.PathValue("id")
	if !validUpstreamAccountID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_concurrency_request", concurrencyName+"请求必须为 JSON，且不能包含查询参数")
		return
	}
	var limit *int64
	if err := decodeSingleJSONField(r, upstreamWeightRequestBytes, "concurrent_limit", &limit); err != nil {
		badJSON(w, r, err)
		return
	}
	if limit == nil || *limit < 1 || *limit > store.MaxUpstreamConcurrentLimit {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_concurrency_limit", concurrencyName+"必须是 1 至 2147483647 的整数")
		return
	}
	account, err := s.accountStore(r).SetUpstreamAccountConcurrentLimit(r.Context(), store.SetUpstreamAccountConcurrentLimitParams{
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

func (s *Server) setUpstreamAccountAlias(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUpstreamAccountID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "upstream_account_not_found", "上游账号不存在")
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_alias_request", "账号别名请求必须为 JSON，且不能包含查询参数")
		return
	}
	var alias *string
	if err := decodeSingleJSONField(r, upstreamAliasRequestBytes, "alias", &alias); err != nil {
		badJSON(w, r, err)
		return
	}
	var err error
	if alias != nil {
		*alias, err = store.NormalizeUpstreamAccountAlias(*alias)
	}
	if alias == nil || err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_account_alias", "账号别名最多 40 个字符，不能包含 @、换行或控制字符")
		return
	}
	account, err := s.accountStore(r).SetUpstreamAccountAlias(r.Context(), store.SetUpstreamAccountAliasParams{
		AccountID: id, Alias: *alias, At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "set upstream account alias", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": account.ID, "alias": account.Alias})
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
	account, err := s.accountStore(r).SetUpstreamAccountAccess(r.Context(), store.SetUpstreamAccountAccessParams{
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
	account, err := s.accountStore(r).SetUpstreamAccountAllocationWeight(r.Context(), store.SetUpstreamAccountAllocationWeightParams{
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

// upstreamSelectionGroup carries one priority layer. AccountIDs are kept in
// caller order for deterministic validation; weighted selection itself is
// delegated to the store.
type upstreamSelectionGroup struct {
	GroupID    string   `json:"group_id"`
	ID         string   `json:"id"`
	AccountIDs []string `json:"account_ids"`
}
type upstreamSelectionRequest struct {
	UserID     string
	AccountIDs []string
	Groups     []upstreamSelectionGroup
	// Distinguish the groups-only protocol from an explicitly empty candidate
	// list, which must not restore the group's full snapshot.
	accountIDsProvided bool
}

func decodeUpstreamSelectionRequest(r *http.Request) (upstreamSelectionRequest, error) {
	var out upstreamSelectionRequest
	if r.ContentLength > upstreamAllocationRequestBytes {
		return out, &http.MaxBytesError{Limit: upstreamAllocationRequestBytes}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, upstreamAllocationRequestBytes+1))
	if err != nil {
		return out, err
	}
	if int64(len(body)) > upstreamAllocationRequestBytes {
		return out, &http.MaxBytesError{Limit: upstreamAllocationRequestBytes}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil || json.Valid(body) == false {
		return out, errors.New("request must be an object")
	}
	if err := rejectDuplicateJSON(body); err != nil {
		return out, err
	}
	for key := range fields {
		if key != "account_ids" && key != "user_id" && key != "groups" {
			return out, errors.New("invalid request field")
		}
	}
	if raw, ok := fields["user_id"]; !ok || json.Unmarshal(raw, &out.UserID) != nil {
		return out, errors.New("request field missing or invalid")
	}
	if raw, ok := fields["account_ids"]; ok {
		out.accountIDsProvided = true
		if json.Unmarshal(raw, &out.AccountIDs) != nil {
			return out, errors.New("request field missing or invalid")
		}
	}
	if raw, ok := fields["groups"]; ok {
		if json.Unmarshal(raw, &out.Groups) != nil {
			return out, errors.New("invalid groups")
		}
		for i := range out.Groups {
			if out.Groups[i].GroupID != "" && out.Groups[i].ID != "" && out.Groups[i].GroupID != out.Groups[i].ID {
				return out, errors.New("invalid group id")
			}
			if out.Groups[i].GroupID == "" {
				out.Groups[i].GroupID = out.Groups[i].ID
			}
		}
	}
	return out, nil
}

func rejectDuplicateJSON(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return errors.New("request must be an object")
	}
	seen := map[string]bool{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return errors.New("invalid request field")
		}
		seen[name] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}

func validUpstreamSelectionRequest(request upstreamSelectionRequest) bool {
	parsed, err := uuid.Parse(request.UserID)
	if err != nil || parsed.String() != request.UserID || (len(request.AccountIDs) == 0 && len(request.Groups) == 0) || len(request.AccountIDs) > store.MaxUpstreamAllocationCandidates {
		return false
	}
	seen := map[string]bool{}
	for _, id := range request.AccountIDs {
		if !validUpstreamAccountID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	if len(request.Groups) > 64 {
		return false
	}
	seenGroups := map[string]bool{}
	for _, group := range request.Groups {
		if group.GroupID == "" && group.ID != "" {
			group.GroupID = group.ID
		}
		parsedGroup, groupErr := uuid.Parse(group.GroupID)
		if groupErr != nil || parsedGroup.String() != group.GroupID || seenGroups[group.GroupID] || len(group.AccountIDs) > store.MaxUpstreamAllocationCandidates {
			return false
		}
		seenGroups[group.GroupID] = true
		local := map[string]bool{}
		for _, id := range group.AccountIDs {
			if !validUpstreamAccountID(id) || local[id] {
				return false
			}
			local[id] = true
		}
	}
	return true
}

func eligibilityRows(allowed []store.UpstreamAccountEligibility) []map[string]any {
	rows := make([]map[string]any, 0, len(allowed))
	for _, account := range allowed {
		rows = append(rows, map[string]any{"id": account.ID, "concurrent_limit": account.ConcurrentLimit})
	}
	return rows
}

// probeAccountSelection answers the allocation callbacks of a model
// identification probe. The probe identity is one the Gateway created for a
// probe in flight and is pinned to one account: that account is the only
// answer, whatever the Owner's own permissions, and only when CPA offers it.
// A probe never carries funding layers.
func (s *Server) probeAccountSelection(w http.ResponseWriter, r *http.Request, request upstreamSelectionRequest, pinned string, eligibility bool) {
	offered := false
	for _, id := range request.AccountIDs {
		offered = offered || id == pinned
	}
	if len(request.Groups) > 0 {
		offered = false
	}
	if eligibility {
		rows := []map[string]any{}
		if offered {
			lookup := s.probeAccountLimit
			if lookup == nil {
				lookup = s.storedAccountLimit
			}
			limit, err := lookup(r.Context(), r, pinned)
			if err != nil {
				httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "upstream_allocation_unavailable", "暂时无法查询上游账号权限")
				return
			}
			rows = append(rows, map[string]any{"id": pinned, "concurrent_limit": limit})
		}
		writeJSON(w, http.StatusOK, map[string]any{"accounts": rows})
		return
	}
	if !offered {
		httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_error", "upstream_concurrency_exceeded", "暂无可用的上游账号，请稍后重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"account_id": pinned})
}

// storedAccountLimit is the Owner-configured concurrency limit of one account,
// so a probe occupies the same slots as any other request on it.
func (s *Server) storedAccountLimit(ctx context.Context, r *http.Request, id string) (int64, error) {
	accounts, err := s.accountStore(r).ListUpstreamAccounts(ctx)
	if err != nil {
		return 0, err
	}
	limit := int64(1)
	for _, account := range accounts {
		if account.ID == id && account.ConcurrentLimit > 0 {
			limit = int64(account.ConcurrentLimit)
		}
	}
	return limit, nil
}
