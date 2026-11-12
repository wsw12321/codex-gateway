package server

import (
	"crypto/hmac"
	"errors"
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
	UDP                  bool   `json:"udp"`
	HasPassword          bool   `json:"has_password"`
	Revision             int64  `json:"revision"`
	BoundAccounts        int    `json:"bound_accounts"`
	PendingRegistrations int    `json:"pending_registrations"`
	CreatedAt            any    `json:"created_at"`
	UpdatedAt            any    `json:"updated_at"`
}

func publicEgressStatus(status egress.Status) egress.Status { status.RouteIDs = nil; return status }

// The rollback bridge has no account egress transport. Block its data plane
// when any AGY account has a binding, including direct/single-account paths
// which do not consult the allocation callbacks.
func (s *Server) legacyEgressAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.config.AntigravityTransport != "legacy-bridge" {
		return true
	}
	if s.store != nil {
		bound, err := s.store.HasProviderEgressBinding(r.Context(), store.UpstreamProviderAntigravity)
		if err == nil && !bound {
			return true
		}
	}
	httpx.WriteError(w, r, 503, "upstream_error", "egress_legacy_unsupported", "旧版 Antigravity 桥接不支持已绑定的账号出口，请切回 CPA 或先解除出口绑定")
	return false
}

func egressNodeDTO(n store.EgressServer) egressServerDTO {
	return egressServerDTO{ID: n.ID, Name: n.Name, Protocol: "ss", Server: n.Server, Port: n.Port, Cipher: n.Cipher, UDP: n.UDP, HasPassword: len(n.PasswordCiphertext) > 0, Revision: n.Revision, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt}
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
	for _, n := range state.Nodes {
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
	writeJSON(w, 200, map[string]any{"servers": nodes, "runtime": publicEgressStatus(status), "version": state.Version, "configured": s.config.EgressControlKey != ""})
}

func (s *Server) saveEgressServer(w http.ResponseWriter, r *http.Request) {
	if !s.egressEnabled(w, r) {
		return
	}
	var input struct {
		Name     string `json:"name"`
		Protocol string `json:"protocol"`
		Server   string `json:"server"`
		Port     int    `json:"port"`
		Cipher   string `json:"cipher"`
		Password string `json:"password"`
		UDP      bool   `json:"udp"`
	}
	if !cpaJSON(w, r, &input, 16<<10, "name", "protocol", "server", "port", "cipher", "password", "udp") {
		return
	}
	create := r.Method == http.MethodPost
	id := r.PathValue("id")
	if create {
		id = uuid.NewString()
	}
	if input.Protocol != "" && input.Protocol != "ss" || create && input.Password == "" {
		egressError(w, r, store.ErrInvalid)
		return
	}
	n := store.EgressServer{ID: id, Name: strings.TrimSpace(input.Name), Server: strings.ToLower(strings.TrimSpace(input.Server)), Port: input.Port, Cipher: input.Cipher, UDP: input.UDP}
	if err := store.ValidateEgressServer(n); err != nil {
		egressError(w, r, err)
		return
	}
	if input.Password != "" && !egress.ValidPassword(n.Cipher, input.Password) {
		input.Password = ""
		egressPasswordError(w, r, n.Cipher, false)
		return
	}
	if !create && input.Password == "" && egress.IsSS2022(n.Cipher) {
		// A retained password may predate the switch to SS2022. Check that it is
		// a usable key, then save only if the node is still at that revision.
		current, err := s.store.EgressServer(r.Context(), id)
		if err != nil {
			egressError(w, r, err)
			return
		}
		retained, err := security.DecryptEgressPassword(s.config.APIKeyEncryptionKey, id, current.PasswordCiphertext)
		if err != nil {
			egressError(w, r, errEgressUnavailable)
			return
		}
		valid := egress.ValidPassword(n.Cipher, retained)
		retained = ""
		if !valid {
			egressPasswordError(w, r, n.Cipher, true)
			return
		}
		n.Revision = current.Revision
	}
	if input.Password != "" {
		ciphertext, err := security.EncryptEgressPassword(s.config.APIKeyEncryptionKey, id, input.Password)
		if err != nil {
			egressError(w, r, err)
			return
		}
		n.PasswordCiphertext = ciphertext
	}
	input.Password = ""
	saved, err := s.store.SaveEgressServer(r.Context(), n, create, egressActor(r))
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
		if state.Nodes[i].ID == id {
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
	if err := s.store.DeleteEgressServer(r.Context(), r.PathValue("id"), egressActor(r)); err != nil {
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
	nodeID := ""
	if selected != nil {
		nodeID = *selected
	}
	if nodeID != "" && isAntigravityAccountRequest(r) && s.config.AntigravityTransport == "legacy-bridge" {
		httpx.WriteError(w, r, 503, "upstream_error", "egress_legacy_unsupported", "旧版 Antigravity 桥接不支持账号出口绑定，请先切回 CPA")
		return
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
	s.pushPluginEgress(r.Context(), []store.EgressBinding{binding})
	writeJSON(w, 200, map[string]any{"id": binding.AccountID, "egress_server_id": binding.NodeID, "egress_revision": binding.Revision, "runtime": publicEgressStatus(status), "saved": true})
}

func (s *Server) createEgressRegistration(r *http.Request, provider string, nodeID *string) (store.EgressRegistration, error) {
	if s.config.EgressControlKey == "" || s.store == nil {
		return store.EgressRegistration{}, errEgressUnavailable
	}
	return s.store.CreateEgressRegistration(r.Context(), provider, nodeID, egressActor(r))
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

func egressPasswordError(w http.ResponseWriter, r *http.Request, cipher string, retained bool) {
	switch {
	case retained:
		httpx.WriteError(w, r, 400, "invalid_request_error", "egress_password_required", "现有密码不是有效的 "+cipher+" 密钥，请重新填写 Base64 密钥")
	case egress.IsSS2022(cipher):
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_password", cipher+" 的密码须为 Base64 编码的 16 字节密钥，多个密钥用英文冒号分隔")
	default:
		httpx.WriteError(w, r, 400, "invalid_request_error", "invalid_egress_password", "SS 密码须为非空单行文本，最多 4096 字节")
	}
}

func egressError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusServiceUnavailable, "egress_unavailable", "出口配置已保存但暂未应用，请稍后刷新；绑定账号不会回退全局出口"
	switch {
	case errors.Is(err, store.ErrInvalid):
		status, code, message = 400, "invalid_egress_request", "SS 节点或出口绑定参数无效"
	case errors.Is(err, store.ErrNotFound):
		status, code, message = 404, "egress_not_found", "出口节点、账号或注册流程不存在"
	case errors.Is(err, store.ErrConflict):
		status, code, message = 409, "egress_conflict", "出口仍被引用，或账号与注册流程不匹配"
	}
	httpx.WriteError(w, r, status, "upstream_error", code, message)
}
