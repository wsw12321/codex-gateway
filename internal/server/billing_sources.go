package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

const billingSourceStatusRequestBytes = 256

func (s *Server) setBillingSourceStatus(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	switch source {
	case "day", "week", "month", "cash":
	default:
		s.billingInputError(w, r)
		return
	}
	if !strictJSONRequest(r) {
		s.billingInputError(w, r)
		return
	}
	var disabled *bool
	if err := decodeSingleJSONField(r, billingSourceStatusRequestBytes, "disabled", &disabled); err != nil {
		badJSON(w, r, err)
		return
	}
	if disabled == nil {
		badJSON(w, r, errors.New("disabled must be a boolean"))
		return
	}
	if err := s.store.SetBillingSourceDisabled(r.Context(), store.SetBillingSourceDisabledParams{
		UserID: userFrom(r.Context()).ID, Source: source, Disabled: *disabled,
		ActorSessionID: sessionFrom(r.Context()).ID, RequestID: httpx.RequestID(r.Context()),
		SourceIP: safeIP(r.Context()), At: time.Now().UTC(),
	}); err != nil {
		s.billingStoreError(w, r, "set billing source status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": source, "disabled": *disabled})
}
