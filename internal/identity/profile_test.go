package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateUsernameRules(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"  ALIce_01-Z  ", "alice_01-z"}, {"abc", "abc"}, {strings.Repeat("a", 32), strings.Repeat("a", 32)},
	} {
		got, err := ValidateUsername(test.input)
		if err != nil || got != test.want {
			t.Errorf("ValidateUsername(%q) = %q, %v", test.input, got, err)
		}
	}
	for _, input := range []string{"", "  ", "ab", strings.Repeat("a", 33), "0abc", "_abc", "-abc", "a.b", "a b", "用户一", "alice@example.com", string([]byte{'a', 'b', 0xff})} {
		if _, err := ValidateUsername(input); !errors.Is(err, ErrInvalidUsername) {
			t.Errorf("accepted invalid username %q: %v", input, err)
		}
	}
}

func TestValidateDisplayNameRules(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"  用户 显示名称\t", "用户 显示名称"}, {" Alice ", "Alice"}, {strings.Repeat("名", 80), strings.Repeat("名", 80)},
	} {
		got, err := ValidateDisplayName(test.input)
		if err != nil || got != test.want {
			t.Errorf("ValidateDisplayName(%q) = %q, %v", test.input, got, err)
		}
	}
	for _, input := range []string{"", " \t\n\u3000", "name\x00hidden", strings.Repeat("名", 81), string([]byte{0xff})} {
		if _, err := ValidateDisplayName(input); !errors.Is(err, ErrInvalidDisplayName) {
			t.Errorf("accepted invalid display name %q: %v", input, err)
		}
	}
}
