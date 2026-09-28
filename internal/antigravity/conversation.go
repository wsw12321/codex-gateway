package antigravity

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

const (
	affinityHeader         = "X-Codex-Gateway-Affinity"
	conversationHashHeader = "X-Codex-Conversation-Hash"
)

var affinityScopePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// AGY 1.2.12 includes this marker in Gemini system instructions. It is a
// compatibility hint, not a guaranteed protocol field. Ambiguous or malformed
// markers must leave a request independent for account concurrency admission.
func extractConversationID(parts []string) string {
	var conversationID string
	for _, part := range parts {
		for line := range strings.SplitSeq(part, "\n") {
			value, found := strings.CutPrefix(strings.TrimSpace(line), "Conversation ID:")
			if !found {
				continue
			}
			id := canonicalConversationID(strings.TrimSpace(value))
			if id == "" || conversationID != "" && conversationID != id {
				return ""
			}
			conversationID = id
		}
	}
	return conversationID
}

func canonicalConversationID(value string) string {
	if len(value) != 36 {
		return ""
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return ""
	}
	return id.String()
}

// The bridge token authenticates the gateway, which supplies this API-key
// namespace. Never derive identity from a client-provided conversation hash.
func conversationHash(header http.Header, conversationID string) string {
	id := canonicalConversationID(conversationID)
	if id == "" {
		return ""
	}
	var scopes []string
	for name, values := range header {
		if strings.EqualFold(name, affinityHeader) {
			scopes = append(scopes, values...)
		}
	}
	if len(scopes) != 1 || !affinityScopePattern.MatchString(scopes[0]) {
		return ""
	}
	digest := sha256.Sum256([]byte("agy-conversation-v1\x00" + scopes[0] + "\x00" + id))
	return "conv-" + hex.EncodeToString(digest[:16])
}
