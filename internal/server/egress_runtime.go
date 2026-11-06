package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/wsw/codex-gateway/internal/egress"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

var errEgressUnavailable = errors.New("account egress is unavailable")

const accountEgressProxy = "http://egress-allowlist:3129"

type accountEgressRuntime struct {
	mu     sync.Mutex
	client *http.Client
	status egress.Status
}

func (s *Server) accountEgressRuntime() *accountEgressRuntime {
	s.egressOnce.Do(func() {
		s.egressRuntime = &accountEgressRuntime{client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	})
	return s.egressRuntime
}

func egressRouteID(kind, id string, bindingRevision int64, n store.EgressServer) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d/%s/%d", kind, id, bindingRevision, n.ID, n.Revision)))
	return hex.EncodeToString(digest[:])
}

func (s *Server) runtimeSnapshot(state store.EgressState) (egress.Snapshot, error) {
	snapshot := egress.Snapshot{Version: state.Version, Nodes: []egress.Node{}, Routes: []egress.Route{}}
	nodes := make(map[string]store.EgressServer, len(state.Nodes))
	for _, n := range state.Nodes {
		password, err := security.DecryptEgressPassword(s.config.APIKeyEncryptionKey, n.ID, n.PasswordCiphertext)
		if err != nil {
			return snapshot, errEgressUnavailable
		}
		nodes[n.ID] = n
		snapshot.Nodes = append(snapshot.Nodes, egress.Node{ID: n.ID, Revision: n.Revision, Server: n.Server, Port: n.Port, Cipher: n.Cipher, Password: password, UDP: n.UDP})
	}
	add := func(kind, id string, revision int64, nodeID string) {
		if n, ok := nodes[nodeID]; ok {
			routeID := egressRouteID(kind, id, revision, n)
			snapshot.Routes = append(snapshot.Routes, egress.Route{ID: routeID, Token: egress.RouteToken(s.config.EgressControlKey, routeID), NodeID: n.ID, NodeRevision: n.Revision})
		}
	}
	for _, b := range state.Bindings {
		add(b.Provider, b.AccountID, b.Revision, b.NodeID)
	}
	for _, r := range state.Registrations {
		if r.State != "committed" {
			add("registration", r.ID, r.Revision, r.NodeID)
			if r.State == "prepared" && !r.Preserve && r.AccountID != "" {
				add(r.Provider, r.AccountID, r.Revision, r.NodeID)
			}
		}
	}
	return snapshot, nil
}

// The runtime bounds each probe at ten seconds; allow for queueing and the
// control round trip without letting a stuck runtime hold the request.
const egressProbeClientTimeout = 20 * time.Second

func (s *Server) egressControl(ctx context.Context, method, path string, input, output any) error {
	return s.egressControlVia(ctx, s.accountEgressRuntime().client, method, path, input, output)
}

// egressProbe runs outside runtime.mu so a slow node never delays the
// synchronization of other nodes or account route resolution.
func (s *Server) egressProbe(ctx context.Context, request egress.ProbeRequest, result *egress.ProbeResult) error {
	client := *s.accountEgressRuntime().client
	client.Timeout = egressProbeClientTimeout
	return s.egressControlVia(ctx, &client, http.MethodPost, "/probe", request, result)
}

func (s *Server) egressControlVia(ctx context.Context, client *http.Client, method, path string, input, output any) error {
	if s.config.EgressControlKey == "" || s.config.EgressControlURL == nil {
		return errEgressUnavailable
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return errEgressUnavailable
		}
		body = bytes.NewReader(data)
	}
	target := *s.config.EgressControlURL
	target.Path = path
	target.RawPath = ""
	target.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return errEgressUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+s.config.EgressControlKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return errEgressUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errEgressUnavailable
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil
	}
	// Runtime permits 65,536 routes; their public IDs alone exceed 4 MiB.
	const maxStatusBytes = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes+1))
	if err != nil || len(data) > maxStatusBytes || json.Unmarshal(data, output) != nil {
		return errEgressUnavailable
	}
	return nil
}

func (s *Server) syncAccountEgress(ctx context.Context) (store.EgressState, egress.Status, error) {
	if s.store == nil {
		return store.EgressState{}, egress.Status{}, errEgressUnavailable
	}
	runtime := s.accountEgressRuntime()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := s.store.EgressSnapshot(ctx)
	if err != nil {
		return state, runtime.status, err
	}
	var status egress.Status
	if s.egressControl(ctx, http.MethodGet, "/status", nil, &status) != nil {
		runtime.status = egress.Status{ErrorCode: "egress_control_unavailable"}
		return state, runtime.status, errEgressUnavailable
	}
	if !status.Ready || status.AppliedVersion != state.Version {
		snapshot, err := s.runtimeSnapshot(state)
		if err != nil {
			status.ErrorCode = "egress_config_invalid"
			runtime.status = status
			return state, status, err
		}
		if err = s.egressControl(ctx, http.MethodPut, "/config", snapshot, nil); err != nil {
			// An apply may roll back to the previous healthy listeners. A fresh
			// status permits unchanged route IDs while edited routes stay blocked.
			if refreshErr := s.egressControl(ctx, http.MethodGet, "/status", nil, &status); refreshErr != nil {
				status = egress.Status{}
			}
			status.ErrorCode = "egress_apply_failed"
			runtime.status = status
			return state, runtime.status, err
		}
		if err = s.egressControl(ctx, http.MethodGet, "/status", nil, &status); err != nil {
			runtime.status = egress.Status{ErrorCode: "egress_control_unavailable"}
			return state, runtime.status, err
		}
	}
	if status.InstanceID == "" || !status.Ready || status.AppliedVersion != state.Version {
		status.ErrorCode = "egress_apply_pending"
		runtime.status = status
		return state, runtime.status, errEgressUnavailable
	}
	runtime.status = status
	runtime.status.ErrorCode = ""
	return state, status, nil
}

func (s *Server) appliedRouteResolution(kind, id string, revision int64, nodeID string, state store.EgressState, status egress.Status) (egress.Resolution, error) {
	if nodeID != "" && s.config.EgressControlKey == "" {
		return egress.Resolution{}, errEgressUnavailable
	}
	resolution, err := s.routeResolution(kind, id, revision, nodeID, state)
	if err != nil || resolution.Mode == "global" {
		return resolution, err
	}
	if status.Ready && status.InstanceID != "" {
		for _, routeID := range status.RouteIDs {
			if routeID == resolution.Revision {
				return resolution, nil
			}
		}
	}
	return egress.Resolution{}, errEgressUnavailable
}

// RunEgressSync restores a restarted egress instance and applies new revisions.
// Unbound account resolution remains independent of this worker's availability.
func (s *Server) RunEgressSync(ctx context.Context) {
	if s.config.EgressControlKey == "" {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, err := s.syncAccountEgress(attempt)
		cancel()
		if err != nil && ctx.Err() == nil && s.logger != nil {
			s.logger.Warn("account egress synchronization pending", "code", "egress_sync_unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) routeResolution(kind, id string, revision int64, nodeID string, state store.EgressState) (egress.Resolution, error) {
	if nodeID == "" {
		return egress.Resolution{Mode: "global", Revision: "global:" + strconv.FormatInt(revision, 10)}, nil
	}
	for _, n := range state.Nodes {
		if n.ID == nodeID {
			routeID := egressRouteID(kind, id, revision, n)
			u, _ := url.Parse(accountEgressProxy)
			u.User = url.UserPassword(routeID, egress.RouteToken(s.config.EgressControlKey, routeID))
			return egress.Resolution{Mode: "account", ProxyURL: u.String(), Revision: routeID}, nil
		}
	}
	return egress.Resolution{}, errEgressUnavailable
}

func (s *Server) egressRegistrationResolution(ctx context.Context, registrationID string) (egress.Resolution, error) {
	if s.store == nil {
		return egress.Resolution{}, errEgressUnavailable
	}
	state, err := s.store.EgressSnapshot(ctx)
	if err != nil {
		return egress.Resolution{}, err
	}
	for _, r := range state.Registrations {
		if r.ID == registrationID {
			if r.State == "committed" {
				return s.resolveAccountEgress(ctx, r.Provider, r.AccountID, "")
			}
			if r.NodeID == "" {
				return s.routeResolution("registration", r.ID, r.Revision, "", state)
			}
			applied, status, _ := s.syncAccountEgress(ctx)
			return s.appliedRouteResolution("registration", r.ID, r.Revision, r.NodeID, applied, status)
		}
	}
	return egress.Resolution{}, store.ErrNotFound
}

func (s *Server) resolveAccountEgress(ctx context.Context, provider, accountID, registrationID string) (egress.Resolution, error) {
	if s.store == nil {
		if registrationID == "" {
			return egress.Resolution{Mode: "global", Revision: "global:0"}, nil
		}
		return egress.Resolution{}, errEgressUnavailable
	}
	state, err := s.store.EgressSnapshot(ctx)
	if err != nil {
		return egress.Resolution{}, err
	}
	if registrationID != "" {
		for _, r := range state.Registrations {
			if r.ID == registrationID {
				if r.Provider != provider || r.AccountID != "" && accountID != "" && r.AccountID != accountID {
					return egress.Resolution{}, store.ErrConflict
				}
				return s.egressRegistrationResolution(ctx, registrationID)
			}
		}
		return egress.Resolution{}, store.ErrNotFound
	}
	for _, b := range state.Bindings {
		if b.AccountID == accountID {
			if b.Provider != provider {
				return egress.Resolution{}, store.ErrConflict
			}
			if b.NodeID != "" {
				applied, status, _ := s.syncAccountEgress(ctx)
				for _, current := range applied.Bindings {
					if current.AccountID == accountID && current.Provider == provider {
						return s.appliedRouteResolution(provider, accountID, current.Revision, current.NodeID, applied, status)
					}
				}
				return egress.Resolution{}, errEgressUnavailable
			}
			return s.routeResolution(provider, accountID, b.Revision, "", state)
		}
	}
	return s.routeResolution(provider, accountID, 0, "", state)
}
