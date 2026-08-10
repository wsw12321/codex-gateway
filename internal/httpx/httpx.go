// Package httpx contains the deliberately small, shared HTTP security layer.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

const RequestIDHeader = "X-Gateway-Request-ID"

type contextKey uint8

const (
	requestIDKey contextKey = iota
	clientIPKey
)

type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
}

func WriteError(w http.ResponseWriter, r *http.Request, status int, typ, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: ErrorDetail{
		Message: message, Type: typ, Code: code, RequestID: RequestID(r.Context()),
	}})
}

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

func ClientIP(ctx context.Context) net.IP {
	value, _ := ctx.Value(clientIPKey).(net.IP)
	return value
}

func RequestContext(trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := randomID()
			ip := resolveClientIP(r, trusted)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			ctx = context.WithValue(ctx, clientIPKey, ip)
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; connect-src 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func Recover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("http handler panic", "request_id", RequestID(r.Context()))
				WriteError(w, r, http.StatusInternalServerError, "server_error", "internal_error", "服务器暂时无法处理请求")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RequireBrowserOrigin rejects cross-site state changes. API bearer-token
// routes do not use this middleware and intentionally emit no CORS headers.
func RequireBrowserOrigin(origins []string, next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		allowed[strings.TrimRight(origin, "/")] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")
		if _, ok := allowed[origin]; !ok {
			WriteError(w, r, http.StatusForbidden, "invalid_request_error", "invalid_origin", "请求来源无效")
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			WriteError(w, r, http.StatusForbidden, "invalid_request_error", "cross_site_request", "不允许跨站请求")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func randomID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("operating system random source unavailable")
	}
	return hex.EncodeToString(raw[:])
}

func resolveClientIP(r *http.Request, trusted []*net.IPNet) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(strings.TrimSpace(host))
	if peer == nil || !isTrusted(peer, trusted) {
		return peer
	}

	// Walk from the closest proxy toward the client. The first untrusted hop
	// is the only address the application may safely attribute the request to.
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	current := peer
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(parts[i]))
		if candidate == nil {
			continue
		}
		current = candidate
		if !isTrusted(candidate, trusted) {
			return candidate
		}
	}
	return current
}

func isTrusted(ip net.IP, networks []*net.IPNet) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
