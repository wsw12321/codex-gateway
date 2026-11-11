package gatewayplugin

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"time"
)

// AccountStateFile keeps the patched image's file name and schema, so either
// image can take over the other's credential volume during a rollback.
const AccountStateFile = ".gateway-account-state"

var (
	// ErrAccountNotFound reports an account index with no credential behind it.
	ErrAccountNotFound = errors.New("gateway account not found")
	// ErrStateUnavailable reports a control document that could not be read or
	// durably written.
	ErrStateUnavailable = errors.New("gateway account state unavailable")
)

type accountState struct {
	Enabled        bool      `json:"enabled"`
	ManualDisabled bool      `json:"manual_disabled"`
	QuotaExhausted bool      `json:"quota_exhausted"`
	Version        uint64    `json:"version"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type accountStateDocument struct {
	Version  int                     `json:"version"`
	Accounts map[string]accountState `json:"accounts"`
}

// ControlState holds the two durable Gateway controls of every account: the
// Owner's manual switch and the quota lock. Missing state means both are
// healthy.
type ControlState struct {
	mu       sync.RWMutex
	path     string
	accounts map[string]accountState
	now      func() time.Time
	write    func(string, []byte) error
}

// LoadControlState reads the document from authDir. A missing file starts
// empty; an unreadable or malformed one is an error and must fail closed.
func LoadControlState(authDir string) (*ControlState, error) {
	path := filepath.Join(authDir, AccountStateFile)
	state := &ControlState{path: path, accounts: map[string]accountState{}, now: time.Now, write: writeControlFile}
	data, exists, err := readControlFile(path, 0)
	if err != nil {
		return nil, ErrStateUnavailable
	}
	if !exists {
		return state, nil
	}
	accounts, err := decodeAccountState(data)
	if err != nil {
		return nil, ErrStateUnavailable
	}
	state.accounts = accounts
	return state, nil
}

func decodeAccountState(data []byte) (map[string]accountState, error) {
	root, err := decodeObject(data, "version", "accounts")
	if err != nil || string(root["version"]) != "1" {
		return nil, errStrictJSON
	}
	raw, err := decodeObject(root["accounts"])
	if err != nil {
		return nil, err
	}
	accounts := make(map[string]accountState, len(raw))
	for id, item := range raw {
		if !validAccountID(id) {
			return nil, errStrictJSON
		}
		fields, err := decodeObject(item, "enabled", "manual_disabled", "quota_exhausted", "version", "updated_at")
		if err != nil {
			return nil, err
		}
		for _, field := range []string{"enabled", "manual_disabled", "quota_exhausted"} {
			if value := string(fields[field]); value != "true" && value != "false" {
				return nil, errStrictJSON
			}
		}
		var state accountState
		if err = json.Unmarshal(item, &state); err != nil || state.Version == 0 || state.UpdatedAt.IsZero() ||
			state.Enabled == (state.ManualDisabled || state.QuotaExhausted) {
			return nil, errStrictJSON
		}
		accounts[id] = state
	}
	return accounts, nil
}

// Blocked reports whether an account must not receive new requests.
func (s *ControlState) Blocked(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, exists := s.accounts[id]
	return exists && (!state.Enabled || state.ManualDisabled || state.QuotaExhausted)
}

// Statuses returns the manual and quota controls in the Gateway's vocabulary.
func (s *ControlState) Statuses(id string) (manual, quota string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	manual, quota = "enabled", "available"
	if state, exists := s.accounts[id]; exists {
		if state.ManualDisabled {
			manual = "manual_disabled"
		}
		if state.QuotaExhausted {
			quota = "quota_exhausted"
		}
	}
	return manual, quota
}

// Version returns the control version of an account; zero means no state.
func (s *ControlState) Version(id string) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accounts[id].Version
}

// SetEnabled persists the Owner's manual switch before acknowledging it.
// Enabling also clears the quota lock: the account is retried and locks again
// if the upstream still reports exhaustion.
func (s *ControlState) SetEnabled(id string, enabled bool) error {
	if !validAccountID(id) {
		return ErrAccountNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.accounts[id]
	next := accountState{Enabled: enabled, ManualDisabled: !enabled, Version: previous.Version + 1, UpdatedAt: s.now().UTC()}
	s.accounts[id] = next
	if err := s.persistLocked(); err != nil {
		// A failed enable must never route. A failed disable stays in memory
		// while the caller learns that durability was not confirmed.
		next.Enabled = false
		next.ManualDisabled = true
		next.QuotaExhausted = previous.QuotaExhausted
		s.accounts[id] = next
		_ = s.persistLocked()
		return ErrStateUnavailable
	}
	return nil
}

// MarkQuotaExhausted locks an account after the upstream reported its usage
// limit. version is the control version observed when the failed attempt was
// selected: a result that predates a newer Owner action is ignored, so a late
// failure cannot undo a re-enable.
func (s *ControlState) MarkQuotaExhausted(id string, version uint64) (changed bool, err error) {
	if !validAccountID(id) {
		return false, ErrAccountNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.accounts[id]
	if previous.Version != version || previous.QuotaExhausted {
		return false, nil
	}
	next := accountState{ManualDisabled: previous.ManualDisabled, QuotaExhausted: true, Version: previous.Version + 1, UpdatedAt: s.now().UTC()}
	s.accounts[id] = next
	if err = s.persistLocked(); err != nil {
		// Keep the lock in memory: it is the conservative state.
		return true, ErrStateUnavailable
	}
	return true, nil
}

func (s *ControlState) persistLocked() error {
	data, err := json.Marshal(accountStateDocument{Version: 1, Accounts: s.accounts})
	if err != nil {
		return err
	}
	return s.write(s.path, data)
}
