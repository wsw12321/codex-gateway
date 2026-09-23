package store

import "strings"

// ValidConversationHash accepts only the opaque identity returned by the
// trusted sidecar. Raw client session IDs must never enter durable metadata.
func ValidConversationHash(value string) bool {
	if len(value) != 37 || !strings.HasPrefix(value, "conv-") {
		return false
	}
	for _, ch := range value[5:] {
		if !(ch >= 'a' && ch <= 'f') && !(ch >= '0' && ch <= '9') {
			return false
		}
	}
	return true
}
