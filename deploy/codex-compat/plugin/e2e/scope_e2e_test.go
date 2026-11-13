//go:build e2e

package e2e

import (
	"net/http"
	"testing"
)

// TestProviderSessionsAreCallerScoped checks the property of unmodified CPA
// that the plugin's principal exists for: a session identity CPA derives for a
// provider belongs to one caller. Two Gateway API keys sending the same
// request through the same upstream account must not share it, or they would
// share whatever CPA and the provider key by it. A CPA upgrade that derives
// the identity differently fails here.
func TestProviderSessionsAreCallerScoped(t *testing.T) {
	h := newHarness(t)
	h.addAccount("a", nil)
	h.addClaudeAccountWithToken("c", "0c1a0de000000001", "sk-ant-oat01-e2e-c")
	h.start()

	// session returns the session identity one request presented upstream.
	session := func(request call, host, header string) string {
		t.Helper()
		h.resetObservations()
		if r := h.do(request); r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", host, r.status, r.body)
		}
		value := ""
		for _, seen := range h.provider.seen() {
			if seen.Host == host {
				value = seen.Headers.Get(header)
			}
		}
		return value
	}
	for _, provider := range []struct {
		name, host, header string
		request            call
	}{
		// No session header, prompt_cache_key or metadata: CPA derives the
		// identity from the caller scope and the conversation's opening.
		{"codex", "chatgpt.com", "Session-Id", call{}},
		{"claude", "api.anthropic.com", "X-Claude-Code-Session-Id", call{
			path: "/v1/messages", body: `{"model":"claude-opus-5","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`,
		}},
	} {
		first, second := provider.request, provider.request
		first.affinity, second.affinity = 1, 2
		mine, again, theirs := session(first, provider.host, provider.header), session(first, provider.host, provider.header), session(second, provider.host, provider.header)
		if mine == "" || mine != again {
			t.Fatalf("%s: one caller's conversation presented %q and then %q", provider.name, mine, again)
		}
		if theirs == "" || theirs == mine {
			t.Fatalf("%s: two callers share the provider session %q", provider.name, mine)
		}
		if mine == scope(1) || theirs == scope(2) {
			t.Fatalf("%s: the caller scope reached the provider", provider.name)
		}
	}
}
