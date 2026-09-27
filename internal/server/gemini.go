package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/wsw/codex-gateway/internal/antigravity"
	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
)

// geminiAPI adapts only the credential envelope. Verification, lifecycle and
// permission checks are the same ones used by the Responses API.
func (s *Server) geminiAPI(next http.Handler) http.Handler {
	authenticated := s.requireAPIKey(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(httpx.GeminiErrors(r.Context()))
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_query", "请求查询参数无效")
			return
		}
		googleKeys := r.Header.Values("X-Goog-Api-Key")
		// Query credentials are deliberately unsupported, even alongside a
		// valid header. A second source must never be silently ignored.
		if query.Has("key") || query.Has("api_key") || query.Has("access_token") ||
			len(googleKeys) > 1 || (len(googleKeys) > 0 && len(r.Header.Values("Authorization")) > 0) {
			s.rejectAPIKey(w, r, "ambiguous_gemini_credentials")
			return
		}
		if len(googleKeys) == 1 {
			key := googleKeys[0]
			if strings.TrimSpace(key) != key || key == "" || strings.ContainsAny(key, ", \t\r\n") {
				s.rejectAPIKey(w, r, "invalid_gemini_credentials")
				return
			}
			r.Header = r.Header.Clone()
			r.Header.Del("X-Goog-Api-Key")
			r.Header.Set("Authorization", "Bearer "+key)
		}
		authenticated.ServeHTTP(w, r)
	})
}

func (s *Server) proxyGemini(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v1beta/models/"
	model, action, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), ":")
	if r.Method != http.MethodPost || r.URL.RawPath != "" || !strings.HasPrefix(r.URL.Path, prefix) || !ok ||
		(action != "generateContent" && action != "streamGenerateContent") {
		httpx.WriteError(w, r, 404, "invalid_request_error", "unsupported_endpoint", "不支持的 Gemini 接口")
		return
	}
	// AGY switches these two precise names during a tool conversation. Both
	// authorize and bill against the existing public model, never a new alias.
	switch model {
	case config.AntigravityPublicModel, "gemini-3.1-pro-preview-customtools":
		model = config.AntigravityPublicModel
	default:
		httpx.WriteError(w, r, 404, "invalid_request_error", "model_not_found", "未配置此 Gemini 模型")
		return
	}
	if _, configured := s.config.AntigravityModelRoutes[model]; !configured {
		httpx.WriteError(w, r, 404, "invalid_request_error", "model_not_found", "未配置此 Gemini 模型")
		return
	}
	if _, priced := s.config.UsagePricing.Models[model]; !priced {
		httpx.WriteError(w, r, 404, "invalid_request_error", "model_not_found", "未配置此 Gemini 模型")
		return
	}
	query := r.URL.Query() // geminiAPI already rejected malformed queries.
	for name, values := range query {
		if name != "alt" || action != "streamGenerateContent" || len(values) != 1 || values[0] != "sse" {
			httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_query", "仅流式生成支持 alt=sse")
			return
		}
	}
	const bodyLimit = 1 << 20
	if r.ContentLength > bodyLimit {
		httpx.WriteError(w, r, 413, "invalid_request_error", "antigravity_request_too_large", "Antigravity 请求体超过 1 MiB 限制")
		return
	}
	// Keep the permit through admission and forwarding, so requests waiting
	// on the database cannot accumulate unbounded retained body buffers.
	release, err := s.acquireRequestSpool()
	if err != nil {
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, r, 503, "server_error", "request_spool_busy", "网关正在处理过多请求，请稍后重试")
		return
	}
	defer release()
	original := http.MaxBytesReader(w, r.Body, bodyLimit)
	data, err := io.ReadAll(original)
	_ = original.Close()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.WriteError(w, r, 413, "invalid_request_error", "antigravity_request_too_large", "Antigravity 请求体超过 1 MiB 限制")
		} else {
			httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_request", "无法读取 Gemini 请求")
		}
		return
	}
	// Apply the bridge's complete schema/control validation before any quota
	// or billing reservation. The isolated bridge validates independently too.
	_, failure := antigravity.DecodeGeminiRequest(data)
	if failure != nil {
		httpx.WriteError(w, r, failure.Status, "invalid_request_error", failure.Code, failure.Message)
		return
	}
	replay := io.NopCloser(bytes.NewReader(data))
	body := &countingBody{reader: replay, closer: replay}
	r.Body, r.ContentLength = body, int64(len(data))
	s.executeAPIRequest(w, r, preparedAPIRequest{
		upstreamPath: prefix + model + ":" + action,
		endpoint:     "gemini." + action, model: model, body: body, gemini: true,
	})
}
