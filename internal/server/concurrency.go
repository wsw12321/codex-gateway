package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
)

func (s *Server) upstreamAccountConcurrency(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), upstreamAccountSyncTimeout)
	defer cancel()
	var snapshot gatewayproxy.UpstreamConcurrency
	var err error
	if client := s.accountClient(r); client != nil {
		snapshot, err = client.UpstreamAccountConcurrency(ctx)
	} else {
		err = errors.New("upstream concurrency is unavailable")
	}
	if s.config.SuppliersEnabled {
		values, supplierErr := s.accountStore(r).ModelSuppliers(r.Context())
		if supplierErr != nil {
			internalError(s, w, r, "read supplier concurrency", supplierErr)
			return
		}
		if len(values) > 0 {
			err = nil
			now := time.Now().UTC()
			snapshot.SampledAt = now
			s.supplierRuntime.mu.Lock()
			for _, value := range values {
				var occupied int64
				for _, slot := range s.supplierRuntime.slots[value.ID] {
					if slot.active > 0 || slot.until.After(now) {
						occupied++
					}
				}
				snapshot.Accounts = append(snapshot.Accounts, gatewayproxy.UpstreamAccountConcurrency{ID: value.ID, ActiveRequests: occupied})
			}
			s.supplierRuntime.mu.Unlock()
		}
	}
	if err != nil {
		writeUpstreamManagementError(w, r, err, "上游账号实时并发暂不可用")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
