package gatewayplugin

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// conversationHashDomain separates this plugin's hashes from the patched
// image's, whose root identities were derived differently. A conversation in
// flight during the cutover simply starts a new hash.
const conversationHashDomain = "codex-gateway/conversation-root/v3\x00"

type rootKey struct{ scope, session string }

type rootEntry struct {
	key     rootKey
	parent  string
	expires time.Time
}

// RootRegistry remembers parent relations between sessions so a subagent,
// fork or side conversation resolves to its root. Relations are process-local
// and bounded; after a restart, expiry or eviction only relations visible in
// the current request are known.
type RootRegistry struct {
	mu       sync.Mutex
	entries  map[rootKey]*list.Element
	order    list.List
	ttl      time.Duration
	capacity int
	now      func() time.Time
}

const (
	rootRegistryTTL      = time.Hour
	rootRegistryCapacity = 65536
	maxSessionIDLength   = 4096
)

// NewRootRegistry creates an empty registry.
func NewRootRegistry() *RootRegistry {
	return &RootRegistry{entries: map[rootKey]*list.Element{}, ttl: rootRegistryTTL, capacity: rootRegistryCapacity, now: time.Now}
}

func validSessionID(id string) bool {
	if id == "" || len(id) > maxSessionIDLength {
		return false
	}
	for _, c := range id {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// hierarchyParent decodes the parent embedded in a nested agent session id.
func hierarchyParent(id string) string {
	if i := strings.LastIndex(id, ":agent:"); i > 0 {
		return id[:i]
	}
	return ""
}

// Resolve returns the root session of child within scope, recording the
// explicit parent when one is given. A cycle invalidates every edge on it and
// resolves to the child itself.
func (r *RootRegistry) Resolve(scope, child, parent string) string {
	if !validSessionID(child) {
		return ""
	}
	if parent != "" && !validSessionID(parent) {
		parent = ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for element := r.order.Front(); element != nil; element = r.order.Front() {
		if element.Value.(*rootEntry).expires.After(now) {
			break
		}
		r.removeLocked(element)
	}
	if parent == "" {
		parent = hierarchyParent(child)
	}
	if parent != "" {
		if parent == child {
			r.removeLocked(r.entries[rootKey{scope, child}])
			return child
		}
		r.setLocked(scope, child, parent, now)
		for ancestor := parent; ; {
			next := hierarchyParent(ancestor)
			if next == "" {
				break
			}
			if _, known := r.entries[rootKey{scope, ancestor}]; !known {
				r.setLocked(scope, ancestor, next, now)
			}
			ancestor = next
		}
	}
	current := child
	seen := map[string]bool{}
	for len(seen) <= r.capacity {
		if seen[current] {
			for session := range seen {
				r.removeLocked(r.entries[rootKey{scope, session}])
			}
			return child
		}
		seen[current] = true
		element := r.entries[rootKey{scope, current}]
		if element == nil {
			return current
		}
		entry := element.Value.(*rootEntry)
		entry.expires = now.Add(r.ttl)
		r.order.MoveToBack(element)
		current = entry.parent
	}
	return child
}

func (r *RootRegistry) setLocked(scope, child, parent string, now time.Time) {
	key := rootKey{scope, child}
	if element := r.entries[key]; element != nil {
		entry := element.Value.(*rootEntry)
		entry.parent, entry.expires = parent, now.Add(r.ttl)
		r.order.MoveToBack(element)
		return
	}
	for r.order.Len() >= r.capacity {
		r.removeLocked(r.order.Front())
	}
	r.entries[key] = r.order.PushBack(&rootEntry{key: key, parent: parent, expires: now.Add(r.ttl)})
}

func (r *RootRegistry) removeLocked(element *list.Element) {
	if element == nil {
		return
	}
	delete(r.entries, element.Value.(*rootEntry).key)
	r.order.Remove(element)
}

// ConversationHash derives the opaque identity the Gateway stores for a root
// conversation. Raw session ids never leave CPA.
func ConversationHash(scope, root string) string {
	if root == "" {
		return ""
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(conversationHashDomain))
	_, _ = digest.Write([]byte(scope))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(root))
	return "conv-" + hex.EncodeToString(digest.Sum(nil)[:16])
}
