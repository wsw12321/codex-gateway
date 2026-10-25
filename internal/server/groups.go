package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	decimal "github.com/wsw/codex-gateway/internal/billing"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type groupRepository interface {
	ListGroups(context.Context) ([]store.GroupSummary, error)
	GetGroup(context.Context, string) (store.Group, error)
	PutGroup(context.Context, store.PutGroupParams) (store.Group, error)
	SetGroupMembers(context.Context, store.SetGroupMembersParams) (store.Group, error)
	ArchiveGroup(context.Context, store.ArchiveGroupParams) (store.Group, error)
}

type groupInput struct {
	billingOperationInput
	Name           string          `json:"name"`
	LimitUSD       string          `json:"limit_usd"`
	MemberLimitUSD json.RawMessage `json:"member_limit_usd"`
	ModelIDs       json.RawMessage `json:"model_ids"`
	Period         string          `json:"period"`
	PeriodCount    json.RawMessage `json:"period_count"`
	CustomDays     int             `json:"custom_days"`
	StartsAt       *time.Time      `json:"starts_at"`
}

type groupMembersInput struct {
	billingOperationInput
	Action  string   `json:"action"`
	UserIDs []string `json:"user_ids"`
}

func (s *Server) groupStorage() groupRepository {
	if s.groupRepo != nil {
		return s.groupRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

func (s *Server) groupsJSON(w http.ResponseWriter, r *http.Request) {
	repository := s.groupStorage()
	if repository == nil {
		internalError(s, w, r, "list groups", errors.New("group repository unavailable"))
		return
	}
	groups, err := repository.ListGroups(r.Context())
	if err != nil {
		s.groupStoreError(w, r, "list groups", err)
		return
	}
	if groups == nil {
		groups = []store.GroupSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) groupJSON(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validGroupID(id) {
		s.groupStoreError(w, r, "read group", store.ErrNotFound)
		return
	}
	repository := s.groupStorage()
	if repository == nil {
		internalError(s, w, r, "read group", errors.New("group repository unavailable"))
		return
	}
	group, err := repository.GetGroup(r.Context(), id)
	if err != nil {
		s.groupStoreError(w, r, "read group", err)
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) putGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method != http.MethodPost && !validGroupID(id) {
		s.groupStoreError(w, r, "update group", store.ErrNotFound)
		return
	}
	var input groupInput
	if err := decodeJSON(w, r, &input, 16<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	if err := validateBillingOperation(input.OperationID, input.Reason); err != nil {
		s.groupStoreError(w, r, "update group", store.ErrInvalid)
		return
	}
	// model_ids is optional for backwards compatibility. An omitted field
	// leaves the existing range unchanged on edit and initializes a new group
	// from the current manageable catalog in the store. Once present, however,
	// it must be a non-empty list of distinct, well-formed model IDs; the store
	// checks membership in its current model catalog before committing.
	var modelIDs []string
	modelIDsSet := len(input.ModelIDs) != 0
	if len(input.ModelIDs) != 0 {
		if string(input.ModelIDs) == "null" || json.Unmarshal(input.ModelIDs, &modelIDs) != nil || !s.validManageableBillingModels(modelIDs) {
			s.groupStoreError(w, r, "update group", store.ErrInvalid)
			return
		}
	}
	var memberLimit *string
	memberLimitSet := len(input.MemberLimitUSD) != 0
	if memberLimitSet && string(input.MemberLimitUSD) != "null" {
		var amount string
		if json.Unmarshal(input.MemberLimitUSD, &amount) != nil {
			s.groupStoreError(w, r, "update group", store.ErrInvalid)
			return
		}
		if _, err := decimal.ParseInput(amount, false, true); err != nil {
			s.groupStoreError(w, r, "update group", store.ErrInvalid)
			return
		}
		memberLimit = &amount
	}
	var periodCount *int
	if len(input.PeriodCount) != 0 {
		if json.Unmarshal(input.PeriodCount, &periodCount) != nil || periodCount == nil || *periodCount < 0 || *periodCount > 99 {
			s.groupStoreError(w, r, "update group", store.ErrInvalid)
			return
		}
	}
	repository := s.groupStorage()
	if repository == nil {
		internalError(s, w, r, "update group", errors.New("group repository unavailable"))
		return
	}
	group, err := repository.PutGroup(r.Context(), store.PutGroupParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, strings.TrimSpace(input.Reason)),
		GroupID:            id, Name: input.Name, LimitUSD: input.LimitUSD,
		MemberLimitUSD: memberLimit, MemberLimitSet: memberLimitSet,
		ModelIDs: modelIDs, ModelIDsSet: modelIDsSet,
		Period: input.Period, PeriodCount: periodCount, CustomDays: input.CustomDays, StartsAt: input.StartsAt,
	})
	if err != nil {
		s.groupStoreError(w, r, "update group", err)
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) setGroupMembers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validGroupID(id) {
		s.groupStoreError(w, r, "set group members", store.ErrNotFound)
		return
	}
	var input groupMembersInput
	if err := decodeJSON(w, r, &input, 512<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	validMembers := false
	switch input.Action {
	case "add", "remove":
		validMembers = len(input.UserIDs) > 0 && len(input.UserIDs) <= 5000
	case "remove_all":
		validMembers = len(input.UserIDs) == 0
	}
	if validateBillingOperation(input.OperationID, input.Reason) != nil || !validMembers {
		s.groupStoreError(w, r, "set group members", store.ErrInvalid)
		return
	}
	repository := s.groupStorage()
	if repository == nil {
		internalError(s, w, r, "set group members", errors.New("group repository unavailable"))
		return
	}
	group, err := repository.SetGroupMembers(r.Context(), store.SetGroupMembersParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, strings.TrimSpace(input.Reason)),
		GroupID:            id, Action: input.Action, UserIDs: input.UserIDs,
	})
	if err != nil {
		s.groupStoreError(w, r, "set group members", err)
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) archiveGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validGroupID(id) {
		s.groupStoreError(w, r, "archive group", store.ErrNotFound)
		return
	}
	var input billingOperationInput
	if err := decodeJSON(w, r, &input, 16<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	if validateBillingOperation(input.OperationID, input.Reason) != nil {
		s.groupStoreError(w, r, "archive group", store.ErrInvalid)
		return
	}
	repository := s.groupStorage()
	if repository == nil {
		internalError(s, w, r, "archive group", errors.New("group repository unavailable"))
		return
	}
	group, err := repository.ArchiveGroup(r.Context(), store.ArchiveGroupParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, strings.TrimSpace(input.Reason)), GroupID: id,
	})
	if err != nil {
		s.groupStoreError(w, r, "archive group", err)
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func validGroupID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func (s *Server) groupStoreError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, store.ErrInvalid):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_group_operation", "群组操作参数无效")
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "group_resource_not_found", "群组或用户不存在")
	case errors.Is(err, store.ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "group_operation_conflict", "群组操作冲突：成员可能已变化、用户已加入其他群组、群组仍有成员或已归档，或操作 ID 已用于不同请求。请刷新后重试")
	default:
		internalError(s, w, r, operation, err)
	}
}
