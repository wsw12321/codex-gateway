package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

const protocolConversionRequestBytes = 1 << 10

type protocolConversionRepository interface {
	ProtocolConversionSettings(context.Context) (store.ProtocolConversionSettings, error)
	SetProtocolConversionSettings(context.Context, store.SetProtocolConversionSettingsParams) (store.ProtocolConversionSettings, error)
}

func (s *Server) protocolConversionStorage() protocolConversionRepository {
	if s.protocolConversionRepo != nil {
		return s.protocolConversionRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

// requireClaudeResponsesConversion admits a claude-* request on OpenAI
// Responses only while the Owner has enabled protocol conversion. It runs
// before admission and reads the setting only for this request shape, so
// native Messages and every non-Claude request are unaffected. An unreadable
// setting rejects the request.
func (s *Server) requireClaudeResponsesConversion(w http.ResponseWriter, r *http.Request, prepared preparedAPIRequest, model string) bool {
	repository := s.protocolConversionStorage()
	var settings store.ProtocolConversionSettings
	err := errors.New("protocol conversion storage unavailable")
	if repository != nil {
		settings, err = repository.ProtocolConversionSettings(r.Context())
	}
	if err != nil {
		s.logger.Error("read protocol conversion settings", "request_id", httpx.RequestID(r.Context()), "error", fmt.Sprintf("%T", err))
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "protocol_conversion_unavailable", "模型协议转换设置暂不可用，请稍后重试")
		return false
	}
	if settings.ClaudeResponsesEnabled {
		return true
	}
	if prepared.requestedModel != "" {
		writeModelRouteIncompatible(w, r, prepared.requestedModel, model, "管理员未开启模型协议转换")
		return false
	}
	httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "protocol_conversion_disabled", "管理员未开启模型协议转换，Claude 模型请通过 /v1/messages 调用")
	return false
}

func (s *Server) protocolConversionSettings(w http.ResponseWriter, r *http.Request) {
	repository := s.protocolConversionStorage()
	if repository == nil {
		internalError(s, w, r, "read protocol conversion settings", errors.New("protocol conversion storage unavailable"))
		return
	}
	settings, err := repository.ProtocolConversionSettings(r.Context())
	if err != nil {
		internalError(s, w, r, "read protocol conversion settings", err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) setProtocolConversionSettings(w http.ResponseWriter, r *http.Request) {
	repository := s.protocolConversionStorage()
	if repository == nil {
		internalError(s, w, r, "set protocol conversion settings", errors.New("protocol conversion storage unavailable"))
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_protocol_conversion_request", "协议转换设置请求必须为 JSON，且不能包含查询参数")
		return
	}
	var enabled *bool
	var revision *int64
	if err := decodeExactJSONFields(r, protocolConversionRequestBytes, map[string]any{
		"claude_responses_enabled": &enabled, "revision": &revision,
	}); err != nil {
		badJSON(w, r, err)
		return
	}
	if enabled == nil || revision == nil || *revision < 1 {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_protocol_conversion_settings", "请提供开关状态和当前设置版本")
		return
	}
	settings, err := repository.SetProtocolConversionSettings(r.Context(), store.SetProtocolConversionSettingsParams{
		ClaudeResponsesEnabled: *enabled, ExpectedRevision: *revision, At: time.Now().UTC(),
		ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if errors.Is(err, store.ErrConflict) {
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "protocol_conversion_revision_changed", "协议转换设置已被其他管理员修改，请刷新后重试")
		return
	}
	if err != nil {
		s.storeWriteError(w, r, "set protocol conversion settings", err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
