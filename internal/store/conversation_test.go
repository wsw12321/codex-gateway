package store

import "testing"

func TestValidConversationHash(t *testing.T) {
	valid := "conv-0123456789abcdef0123456789abcdef"
	for _, value := range []string{valid, "conv-abcdefabcdefabcdefabcdefabcdefab"} {
		if !ValidConversationHash(value) {
			t.Errorf("ValidConversationHash(%q) = false", value)
		}
	}
	for _, value := range []string{
		"",
		"conv-0123456789abcdef0123456789abcde",
		"conv-0123456789abcdef0123456789abcdef0",
		"conv-0123456789abcdef0123456789ABCDEf",
		"session-0123456789abcdef0123456789abcdef",
	} {
		if ValidConversationHash(value) {
			t.Errorf("ValidConversationHash(%q) = true", value)
		}
	}
}
