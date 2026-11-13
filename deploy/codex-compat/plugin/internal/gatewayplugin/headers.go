package gatewayplugin

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Trusted request headers. The Gateway sets them after authenticating its own
// API key; this plugin consumes them and removes them before any upstream call.
const (
	HeaderUser     = "X-Codex-Gateway-User"
	HeaderAffinity = "X-Codex-Gateway-Affinity"
	HeaderGroups   = "X-Codex-Gateway-Groups"
	// HeaderRequest carries the Gateway's one-time request token. Allocation
	// callbacks echo it so the Gateway can attribute the request without
	// depending on anything CPA renders in the response.
	HeaderRequest = "X-Codex-Gateway-Request"

	personalLayerID   = "00000000-0000-0000-0000-000000000000"
	maxGroupsHeader   = 8192
	maxLayers         = 64
	maxLayerAccounts  = 256
	requestTokenBytes = 32
)

// Layer is one admission funding layer in billing order.
type Layer struct {
	GroupID    string
	AccountIDs []string
	// All marks the personal layer, which covers every account the user is
	// individually entitled to.
	All bool
}

// Scope is the decoded account-pool capability of one request.
type Scope struct {
	// Present is false when the header is absent: the pool is then the user's
	// own entitlement, exactly as with a lone personal layer.
	Present bool
	// Restricted is true unless a personal layer is present.
	Restricted bool
	// IDs is the union of every group layer's accounts.
	IDs    map[string]bool
	Layers []Layer
}

// Trusted is the validated identity the Gateway attached to a request.
type Trusted struct {
	UserID   string
	Affinity string
	Token    string
	Scope    Scope
}

// singleHeader returns the only value of name. A repeated header is rejected
// rather than resolved, so two layers cannot disagree about which one counts.
func singleHeader(headers http.Header, name string) (value string, present, ok bool) {
	values := headers.Values(name)
	switch len(values) {
	case 0:
		return "", false, true
	case 1:
		if name == HeaderGroups {
			// The capability is a JSON document; its own decoder is strict.
			return values[0], true, true
		}
		return values[0], true, values[0] == strings.TrimSpace(values[0]) && !strings.Contains(values[0], ",")
	default:
		return "", true, false
	}
}

// ParseTrusted validates the trusted headers of a request that the frontend
// provider has already authenticated. requireIdentity is set for generation
// requests, which cannot be allocated without a user, a request token and the
// caller's own scope: a request without the scope runs under the principal
// every such request shares, and CPA keys its per-caller state by it.
func ParseTrusted(headers http.Header, requireIdentity bool) (Trusted, bool) {
	var trusted Trusted
	user, present, ok := singleHeader(headers, HeaderUser)
	if !ok || present && !validUUID(user) || requireIdentity && !present {
		return Trusted{}, false
	}
	trusted.UserID = user

	affinity, present, ok := singleHeader(headers, HeaderAffinity)
	if !ok || present && !validAffinity(affinity) || requireIdentity && !present {
		return Trusted{}, false
	}
	trusted.Affinity = affinity

	token, present, ok := singleHeader(headers, HeaderRequest)
	if !ok || present && !validRequestToken(token) || requireIdentity && !present {
		return Trusted{}, false
	}
	trusted.Token = token

	groups, present, ok := singleHeader(headers, HeaderGroups)
	if !ok {
		return Trusted{}, false
	}
	if present {
		scope, valid := decodeScope(groups)
		if !valid {
			return Trusted{}, false
		}
		trusted.Scope = scope
	}
	return trusted, true
}

// validAffinity accepts exactly the unpadded base64url form of a 32-byte HMAC.
func validAffinity(value string) bool {
	if len(value) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

// validRequestToken accepts the unpadded base64url form of 32 random bytes.
func validRequestToken(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(requestTokenBytes) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == requestTokenBytes && base64.RawURLEncoding.EncodeToString(decoded) == value
}

// validUUID accepts only the canonical lowercase hyphenated form.
func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// validAccountID accepts the 16-character lowercase hexadecimal account index.
func validAccountID(value string) bool {
	if len(value) != 16 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// decodeScope parses the admission capability. An explicit empty scope stays
// distinct from an absent header so it can never widen into the whole pool.
// A group layer is exactly {group_id, account_ids}; the personal layer is
// exactly {group_id: nil UUID, all_accounts: true} and is only valid last.
func decodeScope(raw string) (Scope, bool) {
	if len(raw) == 0 || len(raw) > maxGroupsHeader {
		return Scope{}, false
	}
	var groups []json.RawMessage
	if json.Unmarshal([]byte(raw), &groups) != nil || groups == nil || len(groups) > maxLayers {
		return Scope{}, false
	}
	scope := Scope{Present: true, Restricted: true, IDs: map[string]bool{}, Layers: make([]Layer, 0, len(groups))}
	seenGroups := map[string]bool{}
	for position, group := range groups {
		decoder := json.NewDecoder(strings.NewReader(string(group)))
		start, err := decoder.Token()
		if err != nil || start != json.Delim('{') {
			return Scope{}, false
		}
		seen := map[string]bool{}
		var groupID string
		var accountIDs []string
		allAccounts := false
		for decoder.More() {
			key, errKey := decoder.Token()
			name, isString := key.(string)
			if errKey != nil || !isString || seen[name] {
				return Scope{}, false
			}
			seen[name] = true
			switch name {
			case "group_id":
				if decoder.Decode(&groupID) != nil {
					return Scope{}, false
				}
			case "account_ids":
				if decoder.Decode(&accountIDs) != nil {
					return Scope{}, false
				}
			case "all_accounts":
				if decoder.Decode(&allAccounts) != nil || !allAccounts {
					return Scope{}, false
				}
			default:
				return Scope{}, false
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') || len(seen) != 2 || !seen["group_id"] || decoder.Decode(new(any)) != io.EOF {
			return Scope{}, false
		}
		if !validUUID(groupID) || seenGroups[groupID] || len(accountIDs) > maxLayerAccounts {
			return Scope{}, false
		}
		seenGroups[groupID] = true
		personal := groupID == personalLayerID
		if personal != allAccounts || personal != seen["all_accounts"] || personal && position != len(groups)-1 {
			return Scope{}, false
		}
		if personal {
			scope.Restricted = false
			scope.Layers = append(scope.Layers, Layer{GroupID: groupID, All: true})
			continue
		}
		local := map[string]bool{}
		layer := Layer{GroupID: groupID, AccountIDs: make([]string, 0, len(accountIDs))}
		for _, id := range accountIDs {
			if !validAccountID(id) || local[id] {
				return Scope{}, false
			}
			local[id] = true
			layer.AccountIDs = append(layer.AccountIDs, id)
			scope.IDs[id] = true
		}
		scope.Layers = append(scope.Layers, layer)
	}
	return scope, true
}

// bearerMatches compares an Authorization header with the expected key in
// constant time.
func bearerMatches(headers http.Header, key string) bool {
	values := headers.Values("Authorization")
	if key == "" || len(values) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(values[0]), []byte("Bearer "+key)) == 1
}

// internalHeader reports whether name must never reach an upstream provider.
func internalHeader(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, "x-codex-gateway-") || lower == "x-codex-upstream-account" ||
		lower == "x-codex-conversation-hash" || strings.HasPrefix(lower, "x-codex-diagnostic-")
}
