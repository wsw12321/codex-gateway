package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/egress"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

type supplierInput struct {
	URL          string   `json:"url"`
	APIKey       string   `json:"api_key"`
	Models       []string `json:"models"`
	EgressNodeID *string  `json:"egress_server_id"`
}

func supplierVersion(provider string) string {
	if provider == store.UpstreamProviderAntigravity {
		return "v1beta"
	}
	return "v1"
}

func (s *Server) supplierModels(provider string) []string {
	var models []string
	for _, model := range s.config.UsagePricing.ManageableModelNames() {
		switch provider {
		case store.UpstreamProviderAntigravity:
			if s.configuredGeminiModel(model) {
				models = append(models, model)
			}
		case store.UpstreamProviderAnthropic:
			if strings.HasPrefix(model, "claude-") {
				models = append(models, model)
			}
		case store.UpstreamProviderCodex:
			if !strings.HasPrefix(model, "claude-") && !strings.HasPrefix(model, "gemini-") && !config.IsAntigravityModel(model) {
				models = append(models, model)
			}
		}
	}
	return models
}

func (s *Server) supplierTransport(account, host string, route egress.Resolution) (*http.Transport, error) {
	if s.config.EgressControlURL == nil || len(s.config.EgressControlKey) < 32 {
		return nil, errEgressUnavailable
	}
	routeID := "global"
	if route.Mode == "account" {
		routeID = route.Revision
	} else if route.Mode != "global" {
		return nil, errEgressUnavailable
	}
	transport := &http.Transport{
		Proxy: func(request *http.Request) (*url.URL, error) {
			if request.URL.Scheme != "https" || request.URL.Host != host || request.Host != host {
				return nil, errEgressUnavailable
			}
			proxyURL := *s.config.EgressControlURL
			proxyURL.User = url.UserPassword(egress.SupplierProxyUser, egress.SupplierTicket(s.config.EgressControlKey, account, host, routeID, time.Now()))
			return &proxyURL, nil
		},
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 12 * time.Second, ResponseHeaderTimeout: 90 * time.Second,
		DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10,
	}
	return transport, nil
}

func supplierHTTP(transport *http.Transport) *http.Client {
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func supplierHeaders(header http.Header, provider, key string) {
	switch provider {
	case store.UpstreamProviderAnthropic:
		header.Set("X-Api-Key", key)
		if header.Get("Anthropic-Version") == "" {
			header.Set("Anthropic-Version", "2023-06-01")
		}
	case store.UpstreamProviderAntigravity:
		header.Set("X-Goog-Api-Key", key)
	default:
		header.Set("Authorization", "Bearer "+key)
	}
}

func discoverSupplierModels(ctx context.Context, client *http.Client, base *url.URL, provider, key string, allowed []string) ([]string, bool, error) {
	const maxListing = 2 << 20
	target := *base
	target.Path += "/models"
	var matches []string
	var after string
	for page := 0; page < 8; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, false, err
		}
		supplierHeaders(request.Header, provider, key)
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return nil, false, err
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			if page == 0 && (response.StatusCode == 404 || response.StatusCode == 405 || response.StatusCode == 501) {
				return nil, true, nil
			}
			return nil, false, errors.New("supplier model list rejected")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, maxListing+1))
		_ = response.Body.Close()
		var listing struct {
			Data   []struct{ ID string } `json:"data"`
			Models []struct {
				Name    string   `json:"name"`
				Methods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			HasMore   bool   `json:"has_more"`
			LastID    string `json:"last_id"`
			PageToken string `json:"nextPageToken"`
		}
		if err != nil || len(data) > maxListing || json.Unmarshal(data, &listing) != nil {
			return nil, false, errors.New("invalid supplier model list")
		}
		remember := func(id string) {
			if slices.Contains(allowed, id) && !slices.Contains(matches, id) {
				matches = append(matches, id)
			}
		}
		var field, next string
		if provider == store.UpstreamProviderAntigravity && listing.Models != nil {
			for _, model := range listing.Models {
				if model.Methods == nil || slices.Contains(model.Methods, "generateContent") {
					remember(strings.TrimPrefix(model.Name, "models/"))
				}
			}
			field, next = "pageToken", listing.PageToken
		} else if provider != store.UpstreamProviderAntigravity && listing.Data != nil {
			for _, model := range listing.Data {
				remember(model.ID)
			}
			if provider == store.UpstreamProviderAnthropic && listing.HasMore {
				field, next = "after_id", listing.LastID
				if next == "" {
					return nil, false, errors.New("invalid supplier pagination")
				}
			}
		} else {
			return nil, false, errors.New("invalid supplier model list shape")
		}
		if next == "" {
			slices.Sort(matches)
			return matches, len(matches) == 0, nil
		}
		if next == after || len(next) > 1024 {
			return nil, false, errors.New("invalid supplier pagination")
		}
		after = next
		target.RawQuery = url.Values{field: {next}}.Encode()
	}
	return nil, false, errors.New("supplier model list too large")
}

func (s *Server) saveModelSupplier(w http.ResponseWriter, r *http.Request) {
	provider, id := r.PathValue("provider"), r.PathValue("id")
	if !s.config.SuppliersEnabled || s.store == nil || !cpaProvider(provider) || id != "" && !validUpstreamAccountID(id) {
		http.NotFound(w, r)
		return
	}
	var input supplierInput
	if !cpaJSON(w, r, &input, 24<<10, "url", "api_key", "models", "egress_server_id") {
		return
	}
	if len(input.APIKey) == 0 || len(input.APIKey) > 4096 || strings.IndexFunc(input.APIKey, func(value rune) bool { return value <= ' ' || value > '~' }) >= 0 {
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_supplier_key", "API Key 必须是非空单行文本")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 22*time.Second)
	defer cancel()
	repo := s.store.WithUpstreamProvider(provider).WithUpstreamOwner(requestTenant(r))
	value := store.ModelSupplier{Provider: provider}
	var err error
	if id == "" {
		if !s.tenantAccountSlotAvailable(w, r) {
			return
		}
		var entropy [8]byte
		if _, err = rand.Read(entropy[:]); err != nil {
			cpaError(w, r)
			return
		}
		value.ID = hex.EncodeToString(entropy[:])
		value.BaseURL = input.URL
	} else {
		value, err = s.findScopedModelSupplier(r.WithContext(ctx), provider, id)
		if err != nil {
			s.storeWriteError(w, r, "locate API Key supplier", err)
			return
		}
		if input.URL != "" || input.EgressNodeID != nil {
			httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_supplier_update", "换 Key 时不能修改地址或出口")
			return
		}
	}
	base, ok := egress.SupplierBaseURL(value.BaseURL, supplierVersion(provider))
	if !ok {
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_supplier_url", "请输入公网 HTTPS Base URL，不包含凭据、其他端口、查询参数或片段")
		return
	}
	value.BaseURL = base.String()
	nodeID := ""
	route := egress.Resolution{Mode: "global"}
	if id != "" {
		route, err = s.resolveAccountEgress(ctx, provider, id, "")
	} else if input.EgressNodeID != nil && *input.EgressNodeID != "" {
		nodeID = *input.EgressNodeID
		var registration store.EgressRegistration
		registration, err = s.createEgressRegistration(r.WithContext(ctx), provider, input.EgressNodeID)
		if err == nil {
			route, err = s.egressRegistrationResolution(ctx, registration.ID)
		}
	}
	if err != nil {
		egressError(w, r, err)
		return
	}
	transport, err := s.supplierTransport(value.ID, base.Hostname(), route)
	if err != nil {
		egressError(w, r, err)
		return
	}
	defer transport.CloseIdleConnections()
	allowed := s.supplierModels(provider)
	models, manual, err := discoverSupplierModels(ctx, supplierHTTP(transport), base, provider, input.APIKey, allowed)
	if err != nil {
		httpx.WriteError(w, r, 502, "upstream_error", "supplier_verification_failed", "无法验证供应商模型列表，请检查 API Key、地址和出口")
		return
	}
	if manual {
		if len(input.Models) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"status": "models_required", "models": allowed})
			return
		}
		for _, model := range input.Models {
			if !slices.Contains(allowed, model) || slices.Contains(models, model) {
				httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_supplier_models", "只能选择该协议已定价的模型")
				return
			}
			models = append(models, model)
		}
		slices.Sort(models)
	}
	value.Models = models
	value.Ciphertext, err = security.EncryptSupplierKey(s.config.APIKeyEncryptionKey, value.ID, input.APIKey)
	if err != nil {
		cpaError(w, r)
		return
	}
	if id == "" {
		err = repo.CreateModelSupplier(ctx, value, nodeID, egressActor(r))
	} else {
		err = repo.ReplaceModelSupplierKey(ctx, value)
	}
	if code := tenantRefusalCode(err); code != "" {
		writeTenantRefusal(w, r, code)
		return
	}
	if err != nil {
		s.storeWriteError(w, r, "save API Key supplier", err)
		return
	}
	s.supplierRuntime.clearCooldown(value.ID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "account_id": value.ID, "models": models})
}

func (s *Server) findModelSupplier(ctx context.Context, provider, id string) (store.ModelSupplier, error) {
	values, err := s.store.WithUpstreamProvider(provider).ModelSuppliers(ctx)
	if err != nil {
		return store.ModelSupplier{}, err
	}
	for _, value := range values {
		if value.ID == id {
			return value, nil
		}
	}
	return store.ModelSupplier{}, store.ErrNotFound
}

// findScopedModelSupplier finds a supplier of this request's owner: the
// member's own on /admin/me/ routes, a platform supplier otherwise.
func (s *Server) findScopedModelSupplier(r *http.Request, provider, id string) (store.ModelSupplier, error) {
	values, err := s.store.WithUpstreamProvider(provider).WithUpstreamOwner(requestTenant(r)).ModelSuppliers(r.Context())
	if err != nil {
		return store.ModelSupplier{}, err
	}
	for _, value := range values {
		if value.ID == id {
			return value, nil
		}
	}
	return store.ModelSupplier{}, store.ErrNotFound
}

func supplierManualStatus(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "manual_disabled"
}

func (s *Server) supplierAccount(r *http.Request, id string) (store.ModelSupplier, bool, error) {
	if !s.config.SuppliersEnabled || s.store == nil {
		return store.ModelSupplier{}, false, nil
	}
	value, err := s.findScopedModelSupplier(r, attemptProvider(r), id)
	if errors.Is(err, store.ErrNotFound) {
		return store.ModelSupplier{}, false, nil
	}
	return value, err == nil, err
}

func (s *Server) deleteSupplierCredential(w http.ResponseWriter, r *http.Request) bool {
	if s.store == nil {
		return false
	}
	provider, id := attemptProvider(r), r.PathValue("id")
	if _, err := s.findScopedModelSupplier(r, provider, id); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.storeWriteError(w, r, "locate supplier for deletion", err)
			return true
		}
		return false
	}
	repo := s.store.WithUpstreamProvider(provider).WithUpstreamOwner(requestTenant(r))
	s.supplierRuntime.mu.Lock()
	err := repo.SetModelSupplierEnabled(r.Context(), id, false)
	s.supplierRuntime.dropIdleLocked(id)
	s.supplierRuntime.mu.Unlock()
	if err != nil {
		s.storeWriteError(w, r, "disable API Key supplier", err)
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for s.supplierRuntime.active(id) > 0 {
		select {
		case <-ctx.Done():
			httpx.WriteError(w, r, 409, "server_error", "supplier_draining", "供应商已停用，仍有请求运行；请等待后重试删除")
			return true
		case <-ticker.C:
		}
	}
	s.supplierRuntime.mu.Lock()
	err = repo.DeleteModelSupplier(ctx, id)
	s.supplierRuntime.mu.Unlock()
	if err != nil {
		s.storeWriteError(w, r, "delete API Key supplier", err)
		return true
	}
	s.supplierRuntime.clearCooldown(id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	return true
}
