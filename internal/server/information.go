package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

type informationRepository interface {
	PreviewInformationCleanup(context.Context, time.Time) (store.InformationCleanupReport, error)
	CreateInformationCleanupJob(context.Context, store.InformationCleanupParams) (store.InformationCleanupJob, error)
	GetInformationCleanupJob(context.Context, string) (store.InformationCleanupJob, error)
	LatestInformationCleanupJob(context.Context) (store.InformationCleanupJob, error)
	InformationCleanedBefore(context.Context) (*time.Time, error)
	ListDeletableInformationUsers(context.Context, string, int, int) ([]store.DeletableUser, error)
	DeleteInformationUsers(context.Context, store.DeleteInformationUsersParams) (store.DeleteInformationUsersResult, error)
}

func (s *Server) informationStorage() informationRepository {
	if s.informationRepo != nil {
		return s.informationRepo
	}
	return s.store
}

func (s *Server) informationJSON(w http.ResponseWriter, r *http.Request) {
	repository := s.informationStorage()
	job, err := repository.LatestInformationCleanupJob(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.informationStoreError(w, r, err)
		return
	}
	var latest, active *store.InformationCleanupJob
	if err == nil {
		latest = &job
		if job.Status == "queued" || job.Status == "running" || job.Status == "pending" {
			active = &job
		}
	}
	cleaned, err := repository.InformationCleanedBefore(r.Context())
	if err != nil {
		s.informationStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"latest_job": latest, "active_job": active, "cleaned_before": cleaned})
}

type informationCleanupInput struct {
	RetentionDays *int      `json:"retention_days"`
	Cutoff        time.Time `json:"cutoff"`
	OperationID   string    `json:"operation_id"`
}

func informationRetentionDays(input informationCleanupInput) int {
	if input.RetentionDays == nil {
		return 90
	}
	return *input.RetentionDays
}

func (s *Server) previewInformationCleanup(w http.ResponseWriter, r *http.Request) {
	var input informationCleanupInput
	if err := decodeJSON(w, r, &input, 4096); err != nil {
		badJSON(w, r, err)
		return
	}
	days := informationRetentionDays(input)
	cutoff, err := store.InformationCutoff(time.Now().UTC(), days)
	if err != nil {
		s.informationStoreError(w, r, err)
		return
	}
	report, err := s.informationStorage().PreviewInformationCleanup(r.Context(), cutoff)
	if err != nil {
		s.informationStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		store.InformationCleanupReport
		RetentionDays int `json:"retention_days"`
	}{report, days})
}

func (s *Server) createInformationCleanupJob(w http.ResponseWriter, r *http.Request) {
	var input informationCleanupInput
	if err := decodeJSON(w, r, &input, 4096); err != nil {
		badJSON(w, r, err)
		return
	}
	days := informationRetentionDays(input)
	latest, err := store.InformationCutoff(time.Now().UTC(), days)
	if err != nil {
		s.informationStoreError(w, r, err)
		return
	}
	// Accept the exact UTC preview cutoff even if confirmation crosses midnight,
	// while refusing a cutoff that could delete more than the chosen retention.
	if !validInformationOperationID(input.OperationID) || input.Cutoff.IsZero() || input.Cutoff.After(latest) ||
		!input.Cutoff.Equal(input.Cutoff.UTC().Truncate(24*time.Hour)) {
		s.informationStoreError(w, r, store.ErrInvalid)
		return
	}
	job, err := s.informationStorage().CreateInformationCleanupJob(r.Context(), store.InformationCleanupParams{
		OperationID: input.OperationID, ActorUserID: userFrom(r.Context()).ID,
		ActorSessionID: sessionFrom(r.Context()).ID, SourceIP: safeIP(r.Context()), RequestID: httpx.RequestID(r.Context()),
		RetentionDays: days, Cutoff: input.Cutoff.UTC(),
	})
	if err != nil {
		s.informationStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func validInformationOperationID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func (s *Server) informationCleanupJobJSON(w http.ResponseWriter, r *http.Request) {
	if !validInformationOperationID(r.PathValue("id")) {
		s.informationStoreError(w, r, store.ErrNotFound)
		return
	}
	job, err := s.informationStorage().GetInformationCleanupJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.informationStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) deletableInformationUsersJSON(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, offset := 100, 0
	var err error
	if query.Has("limit") {
		limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil {
			s.informationStoreError(w, r, store.ErrInvalid)
			return
		}
	}
	if query.Has("offset") {
		offset, err = strconv.Atoi(query.Get("offset"))
		if err != nil {
			s.informationStoreError(w, r, store.ErrInvalid)
			return
		}
	}
	search := query.Get("search")
	if search == "" {
		search = query.Get("q")
	}
	repository := s.informationStorage()
	users, err := listSearchableDeletableUsers(r.Context(), repository, search, limit, offset)
	if err != nil {
		s.informationStoreError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(users))
	for _, user := range users {
		item := map[string]any{
			"id": user.ID, "username": user.Username, "display_name": user.DisplayName,
			"status": user.Status, "created_at": user.CreatedAt,
		}
		addUserSearchFields(item, user.Username, user.DisplayName)
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": items, "limit": limit, "offset": offset})
}

func listSearchableDeletableUsers(ctx context.Context, repository informationRepository, search string, limit, offset int) ([]store.DeletableUser, error) {
	search = strings.TrimSpace(search)
	if len([]rune(search)) > 200 {
		return nil, store.ErrInvalid
	}
	if search == "" {
		return repository.ListDeletableInformationUsers(ctx, "", limit, offset)
	}

	// PostgreSQL intentionally stores no pinyin columns. Scan the eligible-user
	// projection in bounded pages and apply the same derived search index used by
	// every other picker, so remote pagination has identical matching semantics.
	const pageSize = 100
	result := make([]store.DeletableUser, 0, limit)
	matched := 0
	for sourceOffset := 0; ; sourceOffset += pageSize {
		page, err := repository.ListDeletableInformationUsers(ctx, "", pageSize, sourceOffset)
		if err != nil {
			return nil, err
		}
		for _, user := range page {
			if !matchesDerivedUserSearch(user.ID, user.Username, user.DisplayName, search) {
				continue
			}
			if matched >= offset && len(result) < limit {
				result = append(result, user)
			}
			matched++
			if len(result) == limit {
				return result, nil
			}
		}
		if len(page) < pageSize {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
}

func (s *Server) deleteInformationUsers(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OperationID string   `json:"operation_id"`
		UserIDs     []string `json:"user_ids"`
	}
	if err := decodeJSON(w, r, &input, 16<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	if !validInformationOperationID(input.OperationID) {
		s.informationStoreError(w, r, store.ErrInvalid)
		return
	}
	result, err := s.informationStorage().DeleteInformationUsers(r.Context(), store.DeleteInformationUsersParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, "Owner 删除无账务用户"), UserIDs: input.UserIDs,
	})
	if err != nil {
		s.informationStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) informationStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var blocked *store.UserDeletionBlockedError
	switch {
	case errors.As(err, &blocked):
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"type": "conflict_error", "code": "users_not_deletable", "message": "部分用户已不满足删除条件，整批操作未执行"}, "blockers": blocked.Blockers})
	case errors.Is(err, store.ErrInvalid):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_information_request", "请检查保留天数、截止时间、操作 ID 和所选用户")
	case errors.Is(err, store.ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, "conflict_error", "information_conflict", "已有清理任务运行，或操作 ID 已用于其他请求；请刷新后重试")
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, "not_found_error", "information_not_found", "任务或用户不存在")
	default:
		internalError(s, w, r, "information management", err)
	}
}
