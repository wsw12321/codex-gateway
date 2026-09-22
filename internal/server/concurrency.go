package server

import (
	"context"
	"net/http"
)

func (s *Server) upstreamAccountConcurrency(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), upstreamAccountSyncTimeout)
	defer cancel()
	snapshot, err := s.upstream.UpstreamAccountConcurrency(ctx)
	if err != nil {
		writeUpstreamManagementError(w, r, err, "上游账号实时并发暂不可用")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
