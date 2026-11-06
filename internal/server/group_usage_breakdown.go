package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/wsw/codex-gateway/internal/store"
)

type groupUsageBreakdownRepository interface {
	GroupUsageBreakdown(ctx context.Context, groupID, periodID string) (store.GroupUsageBreakdown, error)
}

func (s *Server) groupUsageBreakdownStorage() groupUsageBreakdownRepository {
	if s.groupUsageBreakdownRepo != nil {
		return s.groupUsageBreakdownRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

// groupUsageBreakdownJSON backs the Owner's group usage chart: each user's
// share of one period's group quota (former members included) and, for the
// accounts in the group's selected pools or paid for by the group, members'
// cost versus everyone else's. period_id is optional and defaults to the
// current period; no other query parameter is accepted.
func (s *Server) groupUsageBreakdownJSON(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validGroupID(id) {
		s.groupStoreError(w, r, "read group usage breakdown", store.ErrNotFound)
		return
	}
	query := r.URL.Query()
	for key, values := range query {
		if key != "period_id" || len(values) != 1 || values[0] == "" {
			s.groupStoreError(w, r, "read group usage breakdown", store.ErrInvalid)
			return
		}
	}
	repository := s.groupUsageBreakdownStorage()
	if repository == nil {
		internalError(s, w, r, "read group usage breakdown", errors.New("group repository unavailable"))
		return
	}
	breakdown, err := repository.GroupUsageBreakdown(r.Context(), id, query.Get("period_id"))
	if err != nil {
		s.groupStoreError(w, r, "read group usage breakdown", err)
		return
	}
	writeJSON(w, http.StatusOK, breakdown)
}
