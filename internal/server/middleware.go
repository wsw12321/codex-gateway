package server

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

type requestContextKey uint8

const (
	sessionContextKey requestContextKey = iota
	userContextKey
	apiKeyContextKey
	passwordLoginContextKey
)

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusRecorder) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Info("http request",
			"request_id", httpx.RequestID(r.Context()), "method", r.Method,
			"path", r.URL.Path, "status", status, "response_bytes", recorder.bytes,
			"duration_ms", time.Since(start).Milliseconds(), "client_ip", safeIP(r.Context()),
		)
	})
}

func safeIP(ctx context.Context) string {
	if ip := httpx.ClientIP(ctx); ip != nil {
		return ip.String()
	}
	return ""
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			httpx.WriteError(w, r, http.StatusUnauthorized, "authentication_error", "session_required", "请先登录")
			return
		}
		rawDigest, err := security.DigestOpaqueToken(security.SessionToken, cookie.Value)
		if err != nil {
			s.clearSessionCookie(w)
			httpx.WriteError(w, r, http.StatusUnauthorized, "authentication_error", "invalid_session", "会话已失效，请重新登录")
			return
		}
		digest, err := security.PepperTokenDigest(s.config.TokenPepper, rawDigest)
		if err != nil {
			internalError(s, w, r, "pepper session token", err)
			return
		}
		now := time.Now().UTC()
		session, err := s.store.GetActiveSession(r.Context(), digest[:], now)
		if err != nil {
			s.clearSessionCookie(w)
			httpx.WriteError(w, r, http.StatusUnauthorized, "authentication_error", "invalid_session", "会话已失效，请重新登录")
			return
		}
		user, err := s.store.GetUser(r.Context(), session.UserID)
		if err != nil || user.Status != store.StatusActive {
			httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "user_disabled", "账号已被禁用")
			return
		}
		if now.Sub(session.LastSeenAt) >= 5*time.Minute {
			_ = s.store.TouchSession(r.Context(), session.ID, now, s.config.SessionIdle)
		}
		ctx := context.WithValue(r.Context(), sessionContextKey, session)
		ctx = context.WithValue(ctx, userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requireRecentVerification(next http.Handler) http.Handler {
	return s.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := sessionFrom(r.Context())
		now := time.Now().UTC()
		if session.RecentlyVerifiedAt == nil || now.Sub(*session.RecentlyVerifiedAt) > s.config.ReauthMaxAge {
			httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "recent_identity_verification_required", "此操作需要在 5 分钟内再次验证身份")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) ownerOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := userFrom(r.Context())
		if user.Role != store.UserRoleOwner {
			httpx.WriteError(w, r, http.StatusForbidden, "permission_error", "owner_required", "仅 Owner 可执行此操作")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sessionFrom(ctx context.Context) store.Session {
	value, _ := ctx.Value(sessionContextKey).(store.Session)
	return value
}

func userFrom(ctx context.Context) store.User {
	value, _ := ctx.Value(userContextKey).(store.User)
	return value
}

func apiKeyFrom(ctx context.Context) store.APIKey {
	value, _ := ctx.Value(apiKeyContextKey).(store.APIKey)
	return value
}

func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r.Header)
		if !ok {
			s.rejectAPIKey(w, r, "missing_or_invalid_bearer")
			return
		}
		parsed, err := security.ParseAPIKey(raw)
		if err != nil {
			s.rejectAPIKey(w, r, "malformed_key")
			return
		}
		key, lookupErr := s.store.LookupAPIKey(r.Context(), parsed.PublicID)
		var expected security.APIKeyDigest
		if lookupErr == nil && len(key.KeyHash) == len(expected) {
			copy(expected[:], key.KeyHash)
		}
		_, verified, verifyErr := security.VerifyAPIKey(s.config.KeyPepper, raw, expected)
		if verifyErr != nil {
			s.logger.Error("API key verification configuration error", "request_id", httpx.RequestID(r.Context()))
			httpx.WriteError(w, r, http.StatusInternalServerError, "server_error", "authentication_unavailable", "认证服务暂时不可用")
			return
		}
		if lookupErr != nil || !verified || !hmac.Equal([]byte(key.PublicID), []byte(parsed.PublicID)) {
			s.rejectAPIKey(w, r, "unknown_key")
			return
		}
		now := time.Now().UTC()
		if key.Status == store.StatusRevoked || !key.ExpiresAt.After(now) {
			s.rejectAPIKey(w, r, "revoked_or_expired_key")
			return
		}
		if key.Status != store.StatusActive {
			httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "key_disabled", "API Key 已被禁用")
			return
		}
		if key.UserStatus != store.StatusActive {
			httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "user_disabled", "账号已被禁用")
			return
		}
		if key.DeviceStatus != store.StatusActive {
			httpx.WriteError(w, r, http.StatusForbidden, "authentication_error", "device_disabled", "设备已被禁用")
			return
		}
		ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) rejectAPIKey(w http.ResponseWriter, r *http.Request, reason string) {
	ip := safeIP(r.Context())
	if ok, retry := s.attempts.allow("api-key:"+ip, 30, time.Minute); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_error", "invalid_key_rate_limited", "无效 API Key 尝试次数过多")
		return
	}
	_, _ = s.store.AppendAuditEvent(r.Context(), store.AppendAuditEventParams{
		EventType: "api_key.authentication_failed", Severity: "warning", Success: false,
		SourceIP: ip, RequestID: httpx.RequestID(r.Context()), Metadata: map[string]any{"reason": reason},
	})
	httpx.WriteError(w, r, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "API Key 无效或已过期")
}

func bearerToken(header http.Header) (string, bool) {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	returnValue := ""
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		returnValue = parts[1]
	}
	return returnValue, returnValue != ""
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func integerString(value int) string { return strconv.Itoa(value) }

func durationMillis(start, end time.Time) int64 { return end.Sub(start).Milliseconds() }

func badJSON(w http.ResponseWriter, r *http.Request, err error) {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "请求体过大")
		return
	}
	httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_json", "请求 JSON 无效")
}

func internalError(s *Server, w http.ResponseWriter, r *http.Request, operation string, err error) {
	s.logger.Error(operation, "request_id", httpx.RequestID(r.Context()), "error", fmt.Sprintf("%T", err))
	httpx.WriteError(w, r, http.StatusInternalServerError, "server_error", "internal_error", "服务器暂时无法处理请求")
}
