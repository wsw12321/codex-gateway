// Package egress defines the private Gateway-to-egress control protocol.
// These types contain secrets and must never be returned to a browser or logged.
package egress

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"time"
)

type Node struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Cipher   string `json:"cipher"`
	Password string `json:"password"`
	UDP      bool   `json:"udp"`
}

type Route struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	NodeID       string `json:"node_id"`
	NodeRevision int64  `json:"node_revision"`
}

type Snapshot struct {
	Version int64   `json:"version"`
	Nodes   []Node  `json:"nodes"`
	Routes  []Route `json:"routes"`
}

type Status struct {
	InstanceID     string           `json:"instance_id"`
	AppliedVersion int64            `json:"applied_version"`
	Ready          bool             `json:"ready"`
	ErrorCode      string           `json:"error_code,omitempty"`
	UpdatedAt      time.Time        `json:"updated_at"`
	RouteIDs       []string         `json:"route_ids,omitempty"`
	NodeRevisions  map[string]int64 `json:"node_revisions,omitempty"`
}

// RouteToken binds an opaque route identifier to this installation's control
// credential. The root credential is never disclosed to CPA.
func RouteToken(key, routeID string) string {
	m := hmac.New(sha256.New, []byte(key))
	_, _ = m.Write([]byte("codex-gateway/egress-route/v1\x00"))
	_, _ = m.Write([]byte(routeID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

type Resolution struct {
	Mode     string `json:"mode"`
	ProxyURL string `json:"proxy_url,omitempty"`
	Revision string `json:"revision"`
}
