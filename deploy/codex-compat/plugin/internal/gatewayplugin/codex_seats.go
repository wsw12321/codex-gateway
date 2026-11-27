package gatewayplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	codexSeatsFile     = ".gateway-codex-seats"
	ambiguousWorkspace = "ambiguous"
	seatsOnly          = "seats-only"
)

// The one-time snapshot is taken from the credential volume before this
// plugin can start a login. The host's runtime listing may still be loading.
// Only opaque indexes are persisted, never the claims or the tokens.
type codexSeatState struct {
	Version     int               `json:"version"`
	Legacy      map[string]string `json:"legacy"`       // workspace index -> sole pre-upgrade seat or migration mode
	Pending     map[string]bool   `json:"pending"`      // ambiguous old seats awaiting their own registration
	LegacyFiles map[string]string `json:"legacy_files"` // hashed file name -> old workspace for memberless credentials
}

type codexSeats struct {
	mu    sync.RWMutex
	path  string
	state codexSeatState
}

func codexSeatIndex(workspace, member string) string {
	sum := sha256.Sum256([]byte("codex-gateway-seat:v1\x00" + workspace + "\x00" + member))
	return hex.EncodeToString(sum[:8])
}

func legacyFileKey(name string) string {
	digest := sha256.Sum256([]byte(name))
	return hex.EncodeToString(digest[:])
}

func loadCodexSeats(dir string) (*codexSeats, error) {
	seats := &codexSeats{path: filepath.Join(dir, codexSeatsFile)}
	data, exists, err := readControlFile(seats.path, 0600)
	if err != nil {
		return nil, ErrStateUnavailable
	}
	if exists {
		if _, err := decodeObject(data, "version", "legacy", "pending", "legacy_files"); err != nil || json.Unmarshal(data, &seats.state) != nil ||
			seats.state.Version != 1 || seats.state.Legacy == nil || seats.state.Pending == nil || seats.state.LegacyFiles == nil {
			return nil, ErrStateUnavailable
		}
		for workspace, seat := range seats.state.Legacy {
			if !validAccountID(workspace) || seat != ambiguousWorkspace && seat != seatsOnly && !validAccountID(seat) {
				return nil, ErrStateUnavailable
			}
		}
		for seat, pending := range seats.state.Pending {
			if !validAccountID(seat) || !pending {
				return nil, ErrStateUnavailable
			}
		}
		for file, workspace := range seats.state.LegacyFiles {
			if _, err := hex.DecodeString(file); err != nil || len(file) != sha256.Size*2 || !validAccountID(workspace) {
				return nil, ErrStateUnavailable
			}
		}
		return seats, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, ErrStateUnavailable
	}
	seen := map[string]map[string]bool{}
	legacyFiles := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		raw, _, err := readControlFile(filepath.Join(dir, entry.Name()), 0)
		if err != nil {
			return nil, ErrStateUnavailable
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(raw, &doc) != nil || !strings.EqualFold(stringField(doc, "type"), ProviderCodex) {
			continue
		}
		workspace, member, ok := codexIdentity(doc)
		if !ok {
			continue
		}
		legacy := CodexAccountIndex(workspace)
		if seen[legacy] == nil {
			seen[legacy] = map[string]bool{}
		}
		seat := ""
		if member != "" {
			seat = codexSeatIndex(workspace, member)
		} else {
			legacyFiles[legacyFileKey(entry.Name())] = legacy
		}
		seen[legacy][seat] = true
	}
	state := codexSeatState{Version: 1, Legacy: map[string]string{}, Pending: map[string]bool{}, LegacyFiles: legacyFiles}
	for legacy, group := range seen {
		for seat := range group {
			if seat == "" {
				continue
			}
			if len(group) == 1 {
				state.Legacy[legacy] = seat
			} else {
				state.Legacy[legacy] = ambiguousWorkspace
				state.Pending[seat] = true
			}
		}
	}
	if err := seats.save(state); err != nil {
		return nil, err
	}
	return seats, nil
}

func (s *codexSeats) save(next codexSeatState) error {
	data, err := json.Marshal(next)
	if err != nil || len(data) > 4<<20 || writeControlFile(s.path, data) != nil {
		return ErrStateUnavailable
	}
	s.state = next
	return nil
}

// Once a workspace has identified seats, only a grandfathered file may
// continue using its workspace-only identity. A mixed legacy workspace has
// no safe attribution until its individual members register.
func (state codexSeatState) allowsWorkspaceOnly(legacy, file string) bool {
	switch state.Legacy[legacy] {
	case "":
		return true
	case seatsOnly:
		return state.LegacyFiles[file] == legacy
	default:
		return false
	}
}

func (s *codexSeats) index(name string, doc map[string]json.RawMessage, registering bool) string {
	workspace, member, ok := codexIdentity(doc)
	if !ok {
		return ""
	}
	legacy := CodexAccountIndex(workspace)
	s.mu.RLock()
	defer s.mu.RUnlock()
	original := s.state.Legacy[legacy]
	if member == "" {
		if !s.state.allowsWorkspaceOnly(legacy, legacyFileKey(name)) {
			return ""
		}
		return legacy
	}
	seat := codexSeatIndex(workspace, member)
	if seat == original {
		return legacy
	}
	if !registering && s.state.Pending[seat] {
		return ""
	}
	return seat
}

// Only a successful registration can release an ambiguous seat. Remember new
// seat-aware workspaces so a refresh losing its member claim cannot introduce
// a second, workspace-wide identity.
func (s *codexSeats) confirm(name string, raw json.RawMessage, id string) (bool, error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return false, ErrAccountNotFound
	}
	workspace, member, ok := codexIdentity(doc)
	if !ok {
		return false, ErrAccountNotFound
	}
	legacy := CodexAccountIndex(workspace)
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state
	file := legacyFileKey(name)
	if member == "" {
		if !next.allowsWorkspaceOnly(legacy, file) {
			return false, ErrAccountNotFound
		}
		if next.LegacyFiles[file] == legacy {
			return false, nil
		}
		next.LegacyFiles = maps.Clone(next.LegacyFiles)
		next.LegacyFiles[file] = legacy
	} else {
		changed := false
		if next.Legacy[legacy] == "" {
			next.Legacy = maps.Clone(next.Legacy)
			next.Legacy[legacy] = seatsOnly
			changed = true
		}
		if next.Pending[id] {
			next.Pending = maps.Clone(next.Pending)
			delete(next.Pending, id)
			changed = true
		}
		if _, old := next.LegacyFiles[file]; old {
			next.LegacyFiles = maps.Clone(next.LegacyFiles)
			delete(next.LegacyFiles, file)
			changed = true
		}
		if !changed {
			return false, nil
		}
	}
	return true, s.save(next)
}
