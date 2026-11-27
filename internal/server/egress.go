package server

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/egress"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func (s *Server) egressRoutes() {
	read := func(h http.HandlerFunc) http.Handler { return s.requireSession(s.ownerOnly(h)) }
	write := func(h http.HandlerFunc) http.Handler {
		return s.browserOrigin(s.requireRecentVerification(s.ownerOnly(h)))
	}
	s.mux.Handle("GET /admin/egress-servers", read(s.egressServersJSON))
	s.mux.Handle("POST /admin/egress-servers", write(s.saveEgressServer))
	s.mux.Handle("PUT /admin/egress-servers/{id}", write(s.saveEgressServer))
	s.mux.Handle("DELETE /admin/egress-servers/{id}", write(s.deleteEgressServer))
	s.mux.Handle("GET /admin/egress-allowed-domains", read(s.egressAllowedDomainsJSON))
	s.mux.Handle("PUT /admin/egress-allowed-domains", write(s.replaceEgressAllowedDomains))
	// A connectivity test changes nothing, so it needs a session and the
	// Origin check but not the five-minute re-verification of writes.
	s.mux.Handle("POST /admin/egress-servers/{id}/test", s.browserOrigin(s.requireSession(s.ownerOnly(http.HandlerFunc(s.testEgressServer)))))
	for _, path := range []string{"upstream-accounts", "antigravity-accounts", "anthropic-accounts"} {
		s.mux.Handle("PUT /admin/"+path+"/{id}/egress", write(s.setAccountEgress))
	}
	s.mux.HandleFunc("POST /internal/upstream-accounts/resolve-egress", s.resolveEgress)
	s.mux.HandleFunc("POST /internal/upstream-accounts/register-egress", s.registerEgress)
}

func egressActor(r *http.Request) store.EgressActor {
	return store.EgressActor{UserID: userFrom(r.Context()).ID, SessionID: sessionFrom(r.Context()).ID, SourceIP: safeIP(r.Context()), RequestID: httpx.RequestID(r.Context())}
}
func (s *Server) egressEnabled(w http.ResponseWriter, r *http.Request) bool {
	if s.config.EgressControlKey == "" || s.store == nil {
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "upstream_error", "egress_not_configured", "账号出口服务尚未配置")
		return false
	}
	return true
}

type egressServerDTO struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Protocol             string `json:"protocol"`
	Server               string `json:"server"`
	Port                 int    `json:"port"`
	Cipher               string `json:"cipher"`
	Username             string `json:"username"`
	UDP                  bool   `json:"udp"`
	ServerName           string `json:"tls_server_name,omitempty"`
	HasOptions           bool   `json:"has_options,omitempty"`
	HasPassword          bool   `json:"has_password"`
	Revision             int64  `json:"revision"`
	BoundAccounts        int    `json:"bound_accounts"`
	PendingRegistrations int    `json:"pending_registrations"`
	CreatedAt            any    `json:"created_at"`
	UpdatedAt            any    `json:"updated_at"`
}

func publicEgressStatus(status egress.Status) egress.Status { status.RouteIDs = nil; return status }

func egressNodeDTO(n store.EgressServer) egressServerDTO {
	return egressServerDTO{ID: n.ID, Name: n.Name, Protocol: n.Protocol, Server: n.Server, Port: n.Port, Cipher: n.Cipher, Username: n.Username, UDP: n.UDP, ServerName: n.ServerName, HasOptions: egress.AdvancedProtocol(n.Protocol) && len(n.PasswordCiphertext) > 0, HasPassword: !egress.AdvancedProtocol(n.Protocol) && len(n.PasswordCiphertext) > 0, Revision: n.Revision, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt}
}

func (s *Server) egressServersJSON(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_request", "出口配置不支持查询参数")
		return
	}
	if s.store == nil {
		egressError(w, r, errEgressUnavailable)
		return
	}
	state, err := s.store.EgressSnapshot(r.Context())
	if err != nil {
		egressError(w, r, err)
		return
	}
	runtime := s.accountEgressRuntime()
	runtime.mu.Lock()
	status := runtime.status
	runtime.mu.Unlock()
	if s.config.EgressControlKey == "" {
		status = egress.Status{ErrorCode: "egress_not_configured"}
	} else {
		state, status, _ = s.syncAccountEgress(r.Context())
	}
	nodes := make([]egressServerDTO, 0, len(state.Nodes))
	owner := requestTenant(r)
	for _, n := range state.Nodes {
		// The Owner's page lists platform nodes, a member's only their own.
		if n.OwnerUserID != owner {
			continue
		}
		dto := egressNodeDTO(n)
		for _, b := range state.Bindings {
			if b.NodeID == n.ID {
				dto.BoundAccounts++
			}
		}
		for _, a := range state.Registrations {
			if a.State != "committed" && a.NodeID == n.ID && time.Since(a.CreatedAt) < 24*time.Hour {
				dto.PendingRegistrations++
			}
		}
		nodes = append(nodes, dto)
	}
	runtimeStatus := publicEgressStatus(status)
	if len(runtimeStatus.NodeRevisions) > 0 {
		// Only the revisions of the nodes this page lists.
		visible := make(map[string]int64, len(nodes))
		for _, node := range nodes {
			if revision, ok := runtimeStatus.NodeRevisions[node.ID]; ok {
				visible[node.ID] = revision
			}
		}
		runtimeStatus.NodeRevisions = visible
	}
	response := map[string]any{"servers": nodes, "protocol_fields": egress.ProtocolFields(owner == ""), "runtime": runtimeStatus, "version": state.Version, "configured": s.config.EgressControlKey != ""}
	// Members never see the allowlist; saving a node tells them whether its
	// host name is allowed.
	if owner == "" {
		allowedDomains, err := s.store.EgressAllowedDomains(r.Context())
		if err != nil {
			egressError(w, r, err)
			return
		}
		response["allowed_domains"], response["max_allowed_domains"] = allowedDomains, egress.MaxAllowedDomains
	}
	writeJSON(w, 200, response)
}

func (s *Server) egressAllowedDomainsJSON(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		egressError(w, r, errEgressUnavailable)
		return
	}
	domains, err := s.store.EgressAllowedDomains(r.Context())
	if err != nil {
		egressError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": domains, "max_domains": egress.MaxAllowedDomains})
}

// replaceEgressAllowedDomains sets the host names member egress servers may
// use. Each entry is an exact name or a "*." subdomain wildcard and covers
// every port.
func (s *Server) replaceEgressAllowedDomains(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		egressError(w, r, errEgressUnavailable)
		return
	}
	var input struct {
		Domains []string `json:"domains"`
	}
	if !cpaJSON(w, r, &input, 64<<10, "domains") {
		return
	}
	if input.Domains == nil {
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_domain", "请提供域名列表")
		return
	}
	if _, err := store.NormalizeEgressAllowedDomains(input.Domains); err != nil {
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_domain", fmt.Sprintf("域名格式无效或超过 %d 条。每行一个公网域名，可用 *.example.com 允许全部子域名，不含协议、端口或路径", egress.MaxAllowedDomains))
		return
	}
	domains, err := s.store.ReplaceEgressAllowedDomains(r.Context(), input.Domains, egressActor(r))
	var inUse *store.EgressDomainInUseError
	if errors.As(err, &inUse) {
		httpx.WriteError(w, r, http.StatusConflict, "upstream_error", "egress_domain_in_use", "以下域名仍被成员出口节点使用，请先让成员修改或删除这些节点："+strings.Join(inUse.Hosts, "、"))
		return
	}
	if err != nil {
		egressError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": domains, "max_domains": egress.MaxAllowedDomains})
}

func tenantEgressDomainRefused(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, 400, "invalid_request_error", "egress_domain_not_allowed", "该域名不被允许用作出口服务器地址，请改用公网 IPv4 地址或联系管理员")
}

func (s *Server) saveEgressServer(w http.ResponseWriter, r *http.Request) {
	if !s.egressEnabled(w, r) {
		return
	}
	var input struct {
		Name       string          `json:"name"`
		Protocol   string          `json:"protocol"`
		Server     string          `json:"server"`
		Port       int             `json:"port"`
		Cipher     string          `json:"cipher"`
		Username   string          `json:"username"`
		ServerName string          `json:"tls_server_name"`
		Options    json.RawMessage `json:"options"`
		Password   string          `json:"password"`
		UDP        bool            `json:"udp"`
	}
	if !cpaJSON(w, r, &input, 64<<10, "tls_server_name", "options", "name", "protocol", "server", "port", "cipher", "username", "password", "udp") {
		return
	}
	create := r.Method == http.MethodPost
	id := r.PathValue("id")
	if create {
		id = uuid.NewString()
	}
	if !egress.SupportedProtocol(input.Protocol) {
		egressError(w, r, store.ErrInvalid)
		return
	}
	n := store.EgressServer{ID: id, Name: strings.TrimSpace(input.Name), Protocol: egress.NormalizeProtocol(input.Protocol), Server: strings.ToLower(strings.TrimSpace(input.Server)), Port: input.Port, ServerName: strings.ToLower(strings.TrimSpace(input.ServerName)), Cipher: input.Cipher, Username: input.Username, UDP: input.UDP}
	validate := store.ValidateEgressServer
	if requestTenant(r) != "" {
		validate = store.ValidateTenantEgressServer
	}
	if err := validate(n); err != nil {
		if requestTenant(r) != "" {
			httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_request", "出口服务器地址须为公网 IPv4 地址或管理员允许的域名，端口 1–65535")
			return
		}
		egressError(w, r, err)
		return
	}
	if requestTenant(r) != "" && egress.IsHostName(n.Server) {
		// Checked again inside the save transaction; this early refusal keeps
		// a name off the allowlist from reaching the runtime's validator.
		allowed, err := s.store.EgressAllowedDomains(r.Context())
		if err != nil {
			egressError(w, r, err)
			return
		}
		if !egress.DomainAllowed(allowed, n.Server) {
			tenantEgressDomainRefused(w, r)
			return
		}
	}
	advanced := egress.AdvancedProtocol(n.Protocol)
	if advanced && input.Password != "" || !advanced && len(input.Options) != 0 {
		egressError(w, r, store.ErrInvalid)
		return
	}
	supplied := input.Password
	if advanced {
		supplied = string(input.Options)
		if supplied != "" {
			options, err := egress.DecodeOptions(n.Protocol, supplied)
			if err != nil {
				invalidOutboundOptions(w, r)
				return
			}
			compact, err := json.Marshal(options)
			if err != nil || len(compact) > egress.MaxOptionsBytes {
				invalidOutboundOptions(w, r)
				return
			}
			supplied = string(compact)
		}
	} else if supplied != "" {
		valid := egress.ValidPassword(n.Cipher, supplied)
		if egress.OptionalAuth(n.Protocol) {
			valid = egress.ValidProxyCredentials(n.Protocol, n.Username, supplied)
		}
		if !valid {
			egressPasswordError(w, r, n, false)
			return
		}
	}
	needsPassword := egress.NeedsCredential(n.Protocol, n.Username)
	if create && needsPassword && supplied == "" {
		egressError(w, r, store.ErrInvalid)
		return
	}
	credential := supplied
	if !create && needsPassword && supplied == "" {
		// Bind retention to the checked protocol and row revision.
		current, err := s.egressStore(r).EgressServer(r.Context(), id)
		if err != nil {
			egressError(w, r, err)
			return
		}
		if current.Protocol != n.Protocol || len(current.PasswordCiphertext) == 0 {
			egressPasswordError(w, r, n, true)
			return
		}
		if advanced || egress.IsSS2022(n.Cipher) {
			retained, err := security.DecryptEgressPassword(s.config.APIKeyEncryptionKey, egressCredentialID(n), current.PasswordCiphertext)
			if err != nil {
				egressError(w, r, errEgressUnavailable)
				return
			}
			credential = retained
			valid := advanced || egress.ValidPassword(n.Cipher, retained)
			retained = ""
			if !valid {
				egressPasswordError(w, r, n, true)
				return
			}
		}
		n.Revision = current.Revision
	}
	if advanced {
		node := egressRuntimeNode(n, credential)
		node.Revision = 1 // Isolated candidate has no database revision yet.
		var result struct {
			Valid bool `json:"valid"`
		}
		if err := s.egressControl(r.Context(), http.MethodPost, "/validate", node, &result); err != nil {
			httpx.WriteError(w, r, 503, "upstream_error", "egress_validation_unavailable", "出口服务暂时无法校验节点，请稍后重试")
			return
		}
		if !result.Valid {
			invalidOutboundOptions(w, r)
			return
		}
	}
	if supplied != "" {
		ciphertext, err := security.EncryptEgressPassword(s.config.APIKeyEncryptionKey, egressCredentialID(n), supplied)
		if err != nil {
			egressError(w, r, err)
			return
		}
		n.PasswordCiphertext = ciphertext
	}
	saved, err := s.egressStore(r).SaveEgressServer(r.Context(), n, create, egressActor(r))
	if errors.Is(err, store.ErrEgressDomainNotAllowed) {
		tenantEgressDomainRefused(w, r)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		httpx.WriteError(w, r, http.StatusConflict, "upstream_error", "egress_node_changed", "节点已被其他操作修改，请刷新后重试")
		return
	}
	if err != nil {
		egressError(w, r, err)
		return
	}
	applied, status, _ := s.syncAccountEgress(r.Context())
	// Editing a node changes the route of every account on it.
	s.pushPluginEgress(r.Context(), nodeBindings(applied, saved.ID))
	code := http.StatusOK
	if create {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"server": egressNodeDTO(saved), "runtime": publicEgressStatus(status), "saved": true})
}

// egressProbeTargets maps a provider choice to the provider host the probe
// opens through the node. Each host is on the runtime's CONNECT allowlist.
var egressProbeTargets = map[string]string{
	"codex":       "chatgpt.com",
	"antigravity": "cloudcode-pa.googleapis.com",
	"anthropic":   "api.anthropic.com",
}

func (s *Server) testEgressServer(w http.ResponseWriter, r *http.Request) {
	if !s.egressEnabled(w, r) {
		return
	}
	var input struct {
		Target string `json:"target"`
	}
	if !cpaJSON(w, r, &input, 256, "target") {
		return
	}
	if input.Target == "" {
		input.Target = "codex"
	}
	host, known := egressProbeTargets[input.Target]
	id := r.PathValue("id")
	if parsed, err := uuid.Parse(id); !known || err != nil || parsed.String() != id {
		egressError(w, r, store.ErrInvalid)
		return
	}
	// Synchronizing first lets a test right after saving use the new revision.
	state, status, err := s.syncAccountEgress(r.Context())
	if err != nil && !errors.Is(err, errEgressUnavailable) {
		egressError(w, r, err)
		return
	}
	var node *store.EgressServer
	for i := range state.Nodes {
		if state.Nodes[i].ID == id && state.Nodes[i].OwnerUserID == requestTenant(r) {
			node = &state.Nodes[i]
		}
	}
	if node == nil {
		egressError(w, r, store.ErrNotFound)
		return
	}
	if !status.Ready || status.InstanceID == "" || status.NodeRevisions[id] != node.Revision {
		httpx.WriteError(w, r, http.StatusConflict, "upstream_error", "egress_node_not_applied", "节点配置尚未应用到出口服务，请稍后再测试")
		return
	}
	var result egress.ProbeResult
	if err := s.egressProbe(r.Context(), egress.ProbeRequest{NodeID: id, NodeRevision: node.Revision, Target: host}, &result); err != nil {
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "upstream_error", "egress_probe_unavailable", "出口服务暂时无法执行连通性测试，请稍后重试")
		return
	}
	code := result.Code
	if !egress.KnownProbeCode(code) {
		code = egress.ProbeHandshakeFailed
	}
	latency := result.LatencyMS
	if latency < 0 || latency > egressProbeClientTimeout.Milliseconds() {
		latency = 0
	}
	writeJSON(w, 200, map[string]any{"id": id, "revision": node.Revision, "target": host, "ok": result.OK && code == egress.ProbeOK, "code": code, "latency_ms": latency, "tested_at": time.Now().UTC()})
}

func (s *Server) deleteEgressServer(w http.ResponseWriter, r *http.Request) {
	if !s.egressEnabled(w, r) {
		return
	}
	// An explicit empty JSON object keeps deletion under the same strict parser.
	var input struct{}
	if !cpaJSON(w, r, &input, 128) {
		return
	}
	if err := s.egressStore(r).DeleteEgressServer(r.Context(), r.PathValue("id"), egressActor(r)); err != nil {
		egressError(w, r, err)
		return
	}
	_, status, _ := s.syncAccountEgress(r.Context())
	writeJSON(w, 200, map[string]any{"deleted": true, "runtime": publicEgressStatus(status)})
}

func (s *Server) setAccountEgress(w http.ResponseWriter, r *http.Request) {
	if !s.egressEnabled(w, r) {
		return
	}
	var selected *string
	if !strictJSONRequest(r) || decodeSingleJSONField(r, 512, "egress_server_id", &selected) != nil {
		egressError(w, r, store.ErrInvalid)
		return
	}
	_, supplier, err := s.supplierAccount(r, r.PathValue("id"))
	if err != nil {
		egressError(w, r, err)
		return
	}
	nodeID := ""
	if selected != nil {
		nodeID = *selected
	}
	binding, err := s.accountStore(r).SetEgressBinding(r.Context(), r.PathValue("id"), nodeID, egressActor(r))
	if err != nil {
		egressError(w, r, err)
		return
	}
	_, status, _ := s.syncAccountEgress(r.Context())
	if binding.Provider == "" {
		binding.Provider = attemptProvider(r)
	}
	if !supplier {
		s.pushPluginEgress(r.Context(), []store.EgressBinding{binding})
	}
	writeJSON(w, 200, map[string]any{"id": binding.AccountID, "egress_server_id": binding.NodeID, "egress_revision": binding.Revision, "runtime": publicEgressStatus(status), "saved": true})
}

func (s *Server) createEgressRegistration(r *http.Request, provider string, nodeID *string) (store.EgressRegistration, error) {
	if s.config.EgressControlKey == "" || s.store == nil {
		return store.EgressRegistration{}, errEgressUnavailable
	}
	return s.egressStore(r).CreateEgressRegistration(r.Context(), provider, nodeID, egressActor(r))
}

func (s *Server) egressInternalAuth(w http.ResponseWriter, r *http.Request) bool {
	authorization := r.Header.Values("Authorization")
	if s.config.SidecarToken == "" || len(authorization) != 1 || !hmac.Equal([]byte(authorization[0]), []byte("Bearer "+s.config.SidecarToken)) {
		httpx.WriteError(w, r, 401, "authentication_error", "invalid_sidecar_token", "内部服务认证失败")
		return false
	}
	return true
}

func (s *Server) resolveEgress(w http.ResponseWriter, r *http.Request) {
	if !s.egressInternalAuth(w, r) {
		return
	}
	var input struct {
		Provider       string `json:"provider"`
		AccountID      string `json:"account_id"`
		RegistrationID string `json:"registration_id,omitempty"`
	}
	if !cpaJSON(w, r, &input, 1024, "provider", "account_id", "registration_id") {
		return
	}
	if !cpaProvider(input.Provider) || input.RegistrationID == "" && !validUpstreamAccountID(input.AccountID) || input.AccountID != "" && !validUpstreamAccountID(input.AccountID) {
		egressError(w, r, store.ErrInvalid)
		return
	}
	resolution, err := s.resolveAccountEgress(r.Context(), input.Provider, input.AccountID, input.RegistrationID)
	if err != nil {
		egressError(w, r, err)
		return
	}
	writeJSON(w, 200, resolution)
}

func (s *Server) registerEgress(w http.ResponseWriter, r *http.Request) {
	if !s.egressInternalAuth(w, r) || !s.egressEnabled(w, r) {
		return
	}
	var input struct {
		RegistrationID string `json:"registration_id"`
		Provider       string `json:"provider"`
		AccountID      string `json:"account_id"`
	}
	if !cpaJSON(w, r, &input, 1024, "registration_id", "provider", "account_id") {
		return
	}
	if s.store == nil {
		egressError(w, r, errEgressUnavailable)
		return
	}
	registration, err := s.store.PrepareEgressRegistration(r.Context(), input.RegistrationID, input.Provider, input.AccountID)
	if err != nil {
		egressError(w, r, err)
		return
	}
	if registration.State != "committed" {
		expectedNodeRevision := int64(0)
		// Prepare the future route before changing an existing account's live
		// binding. Failed apply leaves its prior credentials and route intact.
		if !registration.Preserve && registration.NodeID != "" {
			state, status, _ := s.syncAccountEgress(r.Context())
			if _, err := s.appliedRouteResolution(registration.Provider, registration.AccountID, registration.Revision, registration.NodeID, state, status); err != nil {
				egressError(w, r, err)
				return
			}
			for _, node := range state.Nodes {
				if node.ID == registration.NodeID {
					expectedNodeRevision = node.Revision
				}
			}
		}
		registration, err = s.store.CommitEgressRegistration(r.Context(), input.RegistrationID, input.Provider, input.AccountID, expectedNodeRevision)
		if err != nil {
			egressError(w, r, err)
			return
		}
	}
	resolution, err := s.resolveAccountEgress(r.Context(), registration.Provider, registration.AccountID, "")
	if err != nil {
		egressError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"registration_id": registration.ID, "account_id": registration.AccountID, "status": "committed", "revision": resolution.Revision})
}

func invalidOutboundOptions(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_options", "协议参数无效，请检查字段、凭据格式和所选协议")
}

func egressPasswordError(w http.ResponseWriter, r *http.Request, n store.EgressServer, retained bool) {
	cipher := n.Cipher
	switch {
	case retained && egress.AdvancedProtocol(n.Protocol):
		httpx.WriteError(w, r, 400, "invalid_request_error", "egress_options_required", "切换协议时请填写新协议的节点参数")
	case retained && egress.OptionalAuth(n.Protocol):
		httpx.WriteError(w, r, 400, "invalid_request_error", "egress_password_required", "切换协议或启用代理认证时须重新填写密码")
	case retained && egress.IsSS2022(cipher):
		httpx.WriteError(w, r, 400, "invalid_request_error", "egress_password_required", "现有密码不是有效的 "+cipher+" 密钥，请重新填写 Base64 密钥")
	case retained:
		httpx.WriteError(w, r, 400, "invalid_request_error", "egress_password_required", "切换协议时须重新填写 SS 密码")
	case egress.HTTPProtocol(n.Protocol):
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_password", "HTTP 代理认证须同时填写用户名和密码，密码为单行文本，最多 4096 字节")
	case n.Protocol == egress.ProtocolSOCKS5:
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_password", "SOCKS5 用户名和密码须同时填写，均为单行文本，各不超过 255 字节")
	case egress.IsSS2022(cipher):
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_password", cipher+" 的密码须为 Base64 编码的 16 字节密钥，多个密钥用英文冒号分隔")
	default:
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_password", "SS 密码须为非空单行文本，最多 4096 字节")
	}
}

func egressError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusServiceUnavailable, "egress_unavailable", "出口配置已保存但暂未应用，请稍后刷新；绑定账号不会回退全局出口"
	// A login or import that reached someone else's account, or a tenant at
	// its limit. The plugin decodes exactly these codes.
	if refusal := tenantRefusalCode(err); refusal != "" {
		writeTenantRefusal(w, r, refusal)
		return
	}
	switch {
	case errors.Is(err, store.ErrInvalid):
		status, code, message = 400, "invalid_egress_request", "出口节点或出口绑定参数无效"
	case errors.Is(err, store.ErrNotFound):
		status, code, message = 404, "egress_not_found", "出口节点、账号或注册流程不存在"
	case errors.Is(err, store.ErrConflict):
		status, code, message = 409, "egress_conflict", "出口仍被引用，或账号与注册流程不匹配"
	}
	httpx.WriteError(w, r, status, "upstream_error", code, message)
}
