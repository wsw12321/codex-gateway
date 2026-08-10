package identity

import (
	"errors"
	"testing"
	"time"
)

func TestChallengeIsSingleUse(t *testing.T) {
	challenges := newChallengeStore(time.Minute)
	id, err := challenges.put(pendingCeremony{kind: pendingLogin, expiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := challenges.take(id, pendingLogin); err != nil {
		t.Fatal(err)
	}
	if _, err := challenges.take(id, pendingLogin); !errors.Is(err, ErrInvalidCeremony) {
		t.Fatalf("expected replay rejection, got %v", err)
	}
}

func TestChallengeKindCannotBeSwapped(t *testing.T) {
	challenges := newChallengeStore(time.Minute)
	id, err := challenges.put(pendingCeremony{kind: pendingReauthentication, expiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := challenges.take(id, pendingLogin); !errors.Is(err, ErrInvalidCeremony) {
		t.Fatalf("expected kind rejection, got %v", err)
	}
}

func TestValidateNames(t *testing.T) {
	if _, _, err := validateNames("alice-01", "Alice"); err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"ab", "0alice", "alice@example.com", "爱丽丝"} {
		if _, _, err := validateNames(username, "Alice"); !errors.Is(err, ErrInvalidUsername) {
			t.Fatalf("username %q was accepted", username)
		}
	}
}
