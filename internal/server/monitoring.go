package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

// activeAttributionTTL is deliberately short.  A normal request clears its
// observation when forwarding completes; the TTL is only a safety net for a
// process crash, a cancelled handler, or a caller that forgets to clean up.
const activeAttributionTTL = 15 * time.Minute
const activeConversationTTL = 15 * time.Minute

type monitoringRepository interface {
	Monitoring(context.Context) (store.MonitoringSnapshot, error)
}

func (s *Server) monitoringStorage() monitoringRepository {
	if s.monitoringRepo != nil {
		return s.monitoringRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

type activeRequestAttribution struct {
	AccountID string
	UpdatedAt time.Time
}

type activeRequestConversation struct {
	Hash      string
	UpdatedAt time.Time
}

func (s *Server) rememberActiveConversation(requestID, conversationHash string) {
	requestID = strings.TrimSpace(requestID)
	conversationHash = strings.TrimSpace(conversationHash)
	if s == nil || requestID == "" || !store.ValidConversationHash(conversationHash) {
		return
	}
	now := time.Now().UTC()
	s.activeConversationsMu.Lock()
	defer s.activeConversationsMu.Unlock()
	if s.activeConversations == nil {
		s.activeConversations = make(map[string]activeRequestConversation)
	}
	s.pruneActiveConversationsLocked(now)
	s.activeConversations[requestID] = activeRequestConversation{Hash: conversationHash, UpdatedAt: now}
}

func (s *Server) clearActiveConversation(requestID string) {
	if s == nil || requestID == "" {
		return
	}
	s.activeConversationsMu.Lock()
	delete(s.activeConversations, requestID)
	s.activeConversationsMu.Unlock()
}

func (s *Server) pruneActiveConversationsLocked(now time.Time) {
	if len(s.activeConversations) == 0 {
		return
	}
	cutoff := now.Add(-activeConversationTTL)
	for requestID, conversation := range s.activeConversations {
		if conversation.UpdatedAt.Before(cutoff) || !store.ValidConversationHash(conversation.Hash) {
			delete(s.activeConversations, requestID)
		}
	}
}

func (s *Server) activeConversationSnapshot(now time.Time) map[string]string {
	result := make(map[string]string)
	if s == nil {
		return result
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	s.activeConversationsMu.Lock()
	s.pruneActiveConversationsLocked(now)
	for requestID, conversation := range s.activeConversations {
		result[requestID] = conversation.Hash
	}
	s.activeConversationsMu.Unlock()
	return result
}

// rememberActiveAttribution records the account reported by an upstream
// response header for a still-running request.  Account IDs are validated at
// this boundary as a second defence in depth; proxy validation already rejects
// malformed values before invoking the callback.
func (s *Server) rememberActiveAttribution(requestID, accountID string) {
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	if s == nil || requestID == "" || !validUpstreamAccountID(accountID) {
		return
	}
	now := time.Now().UTC()
	s.activeAttributionsMu.Lock()
	defer s.activeAttributionsMu.Unlock()
	if s.activeAttributions == nil {
		s.activeAttributions = make(map[string]activeRequestAttribution)
	}
	s.pruneActiveAttributionsLocked(now)
	s.activeAttributions[requestID] = activeRequestAttribution{AccountID: accountID, UpdatedAt: now}
}

// recordActiveRequestAttribution is an explicit alias used by forwarding code
// and tests.  Keeping the callback-facing name separate from the storage
// helper makes the lifecycle intent clear at call sites.
func (s *Server) recordActiveRequestAttribution(requestID, accountID string) {
	s.rememberActiveAttribution(requestID, accountID)
}

// setActiveRequestAttribution is retained as a small compatibility alias for
// callers that phrase the callback as a setter.
func (s *Server) setActiveRequestAttribution(requestID, accountID string) {
	s.rememberActiveAttribution(requestID, accountID)
}

func (s *Server) clearActiveAttribution(requestID string) {
	if s == nil || requestID == "" {
		return
	}
	s.activeAttributionsMu.Lock()
	delete(s.activeAttributions, requestID)
	s.activeAttributionsMu.Unlock()
}

func (s *Server) clearActiveRequestAttribution(requestID string) {
	s.clearActiveAttribution(requestID)
}

func (s *Server) pruneActiveAttributionsLocked(now time.Time) {
	if len(s.activeAttributions) == 0 {
		return
	}
	cutoff := now.Add(-activeAttributionTTL)
	for requestID, attribution := range s.activeAttributions {
		if attribution.UpdatedAt.Before(cutoff) || attribution.AccountID == "" {
			delete(s.activeAttributions, requestID)
		}
	}
}

func (s *Server) activeAttributionSnapshot(now time.Time) map[string]string {
	result := make(map[string]string)
	if s == nil {
		return result
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	s.activeAttributionsMu.Lock()
	s.pruneActiveAttributionsLocked(now)
	for requestID, attribution := range s.activeAttributions {
		result[requestID] = attribution.AccountID
	}
	s.activeAttributionsMu.Unlock()
	return result
}

type monitoringRequestDTO struct {
	RequestID           string     `json:"request_id"`
	ConversationHash    *string    `json:"conversation_hash,omitempty"`
	RequestedAt         time.Time  `json:"requested_at"`
	CompletedAt         *time.Time `json:"completed_at"`
	UserID              string     `json:"user_id"`
	Username            string     `json:"username"`
	DisplayName         string     `json:"display_name"`
	RequestedModel      *string    `json:"requested_model"`
	Model               string     `json:"model"`
	State               string     `json:"state"`
	HTTPStatus          *int       `json:"http_status"`
	ErrorCode           *string    `json:"error_code"`
	UpstreamAccountID   *string    `json:"upstream_account_id"`
	UpstreamMaskedEmail *string    `json:"upstream_masked_email"`
}

type monitoringResponse struct {
	SampledAt  time.Time              `json:"sampled_at"`
	InProgress []monitoringRequestDTO `json:"in_progress"`
	Recent     []monitoringRequestDTO `json:"recent"`
	Failures   []monitoringRequestDTO `json:"failures"`
}

func monitoringRequestDTOFromStore(row store.MonitoringRequest) monitoringRequestDTO {
	return monitoringRequestDTO{
		RequestID: row.RequestID, ConversationHash: row.ConversationHash, RequestedAt: row.RequestedAt, CompletedAt: row.CompletedAt,
		UserID: row.UserID, Username: row.Username, DisplayName: row.DisplayName,
		RequestedModel: row.RequestedModel, Model: row.Model, State: row.State, HTTPStatus: row.HTTPStatus,
		ErrorCode: row.ErrorCode, UpstreamAccountID: row.UpstreamAccountID,
		UpstreamMaskedEmail: row.UpstreamMaskedEmail,
	}
}

func monitoringDTOs(rows []store.MonitoringRequest) []monitoringRequestDTO {
	result := make([]monitoringRequestDTO, 0, len(rows))
	for _, row := range rows {
		result = append(result, monitoringRequestDTOFromStore(row))
	}
	return result
}

func overlayActiveAttribution(rows []store.MonitoringRequest, active map[string]string) {
	for index := range rows {
		row := &rows[index]
		if row.State != "in_progress" {
			continue
		}
		accountID := active[row.RequestID]
		if !validUpstreamAccountID(accountID) {
			continue
		}
		// Do not mutate terminal records.  For active rows, however, a fresh
		// response-header observation supersedes a stale/null durable value.
		if row.UpstreamAccountID == nil || *row.UpstreamAccountID != accountID {
			row.UpstreamMaskedEmail = nil
		}
		value := accountID
		row.UpstreamAccountID = &value
	}
}

func overlayActiveConversation(rows []store.MonitoringRequest, active map[string]string) {
	for index := range rows {
		row := &rows[index]
		if row.State != "in_progress" {
			continue
		}
		conversationHash := active[row.RequestID]
		if !store.ValidConversationHash(conversationHash) {
			continue
		}
		value := conversationHash
		row.ConversationHash = &value
	}
}

func (s *Server) monitoringJSON(w http.ResponseWriter, r *http.Request) {
	repository := s.monitoringStorage()
	if repository == nil {
		internalError(s, w, r, "list request monitoring", errors.New("monitoring repository is unavailable"))
		return
	}
	snapshot, err := repository.Monitoring(r.Context())
	if err != nil {
		internalError(s, w, r, "list request monitoring", err)
		return
	}
	active := s.activeAttributionSnapshot(snapshot.SampledAt)
	conversations := s.activeConversationSnapshot(snapshot.SampledAt)
	overlayActiveAttribution(snapshot.InProgress, active)
	overlayActiveAttribution(snapshot.Recent, active)
	overlayActiveConversation(snapshot.InProgress, conversations)
	overlayActiveConversation(snapshot.Recent, conversations)
	writeJSON(w, http.StatusOK, monitoringResponse{
		SampledAt:  snapshot.SampledAt,
		InProgress: monitoringDTOs(snapshot.InProgress),
		Recent:     monitoringDTOs(snapshot.Recent),
		Failures:   monitoringDTOs(snapshot.Failures),
	})
}
