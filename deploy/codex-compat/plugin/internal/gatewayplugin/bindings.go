package gatewayplugin

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

// bindingKey identifies one conversation on one provider and model. The
// caller scope comes from the authenticated Gateway identity, so two callers
// that present the same client session id never share a binding.
type bindingKey struct {
	scope, provider, session, model string
}

type bindingEntry struct {
	key     bindingKey
	account string
	expires time.Time
}

// Bindings is a bounded, expiring session-to-account map. It lives in process
// memory: a restart drops every binding and conversations are reallocated.
type Bindings struct {
	mu       sync.Mutex
	entries  map[bindingKey]*list.Element
	order    list.List
	ttl      time.Duration
	capacity int
	now      func() time.Time
}

const bindingCapacity = 65536

// NewBindings creates an empty map with the given lifetime.
func NewBindings(ttl time.Duration) *Bindings {
	return &Bindings{entries: map[bindingKey]*list.Element{}, ttl: ttl, capacity: bindingCapacity, now: time.Now}
}

func newBindingKey(scope, provider, session, model string) bindingKey {
	return bindingKey{scope: scope, provider: provider, session: session, model: strings.ToLower(strings.TrimSpace(model))}
}

// Get returns the bound account and refreshes its lifetime.
func (b *Bindings) Get(key bindingKey) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	element := b.entries[key]
	if element == nil {
		return "", false
	}
	entry := element.Value.(*bindingEntry)
	now := b.now()
	if !entry.expires.After(now) {
		b.removeLocked(element)
		return "", false
	}
	entry.expires = now.Add(b.ttl)
	b.order.MoveToBack(element)
	return entry.account, true
}

// Set binds key to account, evicting the least recently used entry when full.
func (b *Bindings) Set(key bindingKey, account string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if element := b.entries[key]; element != nil {
		entry := element.Value.(*bindingEntry)
		entry.account, entry.expires = account, now.Add(b.ttl)
		b.order.MoveToBack(element)
		return
	}
	for b.order.Len() >= b.capacity {
		b.removeLocked(b.order.Front())
	}
	b.entries[key] = b.order.PushBack(&bindingEntry{key: key, account: account, expires: now.Add(b.ttl)})
}

// Delete removes key only while it is still bound to account, so a concurrent
// rebind is not undone.
func (b *Bindings) Delete(key bindingKey, account string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if element := b.entries[key]; element != nil && element.Value.(*bindingEntry).account == account {
		b.removeLocked(element)
	}
}

func (b *Bindings) removeLocked(element *list.Element) {
	if element == nil {
		return
	}
	delete(b.entries, element.Value.(*bindingEntry).key)
	b.order.Remove(element)
}
