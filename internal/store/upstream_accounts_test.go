package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNormalizeUpstreamAccountSnapshot(t *testing.T) {
	t.Parallel()
	value, err := normalizeUpstreamAccountSnapshot(UpstreamAccountSnapshot{
		ID: "0123456789abcdef", MaskedEmail: " a***@example.com ",
		Plan: "ChatGPT Pro", Status: "active",
	})
	if err != nil {
		t.Fatalf("normalize snapshot: %v", err)
	}
	if value.ID != "0123456789abcdef" || value.MaskedEmail != "a***@example.com" ||
		value.Plan != "pro" || value.Status != UpstreamAccountStatusAvailable {
		t.Fatalf("normalized snapshot = %+v", value)
	}

	for name, snapshot := range map[string]UpstreamAccountSnapshot{
		"empty id":       {MaskedEmail: "a***@example.com", Plan: "plus", Status: "available"},
		"email id":       {ID: "alice@example.com", MaskedEmail: "a***@example.com", Plan: "plus", Status: "available"},
		"full email":     {ID: "0123456789abcdef", MaskedEmail: "alice@example.com", Plan: "plus", Status: "available"},
		"asterisk email": {ID: "0123456789abcdef", MaskedEmail: "alice*tag@example.com", Plan: "plus", Status: "available"},
		"mask in domain": {ID: "0123456789abcdef", MaskedEmail: "alice@e***.com", Plan: "plus", Status: "available"},
		"short mask":     {ID: "0123456789abcdef", MaskedEmail: "a***@x", Plan: "plus", Status: "available"},
		"unsafe plan":    {ID: "0123456789abcdef", MaskedEmail: "a***@example.com", Plan: "Plus Plan", Status: "available"},
		"secret plan":    {ID: "0123456789abcdef", MaskedEmail: "a***@example.com", Plan: "refresh_token_deadbeef", Status: "available"},
		"unknown status": {ID: "0123456789abcdef", MaskedEmail: "a***@example.com", Plan: "plus", Status: "healthy"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := normalizeUpstreamAccountSnapshot(snapshot); !errors.Is(err, ErrInvalid) {
				t.Fatalf("normalize error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestUpstreamAccountSlotDisplayName(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"primary", "account-2", "slot_3"} {
		value, err := normalizeUpstreamAccountSnapshot(UpstreamAccountSnapshot{
			ID: "0123456789abcdef", DisplayName: label, Status: "available",
		})
		if err != nil || value.DisplayName != label || value.MaskedEmail != "" || value.Plan != "unknown" {
			t.Fatalf("slot snapshot = %+v, %v", value, err)
		}
	}
	for _, label := range []string{"alice@example.com", "../auth", "Account", "a b", "123456789012345678901234567890123"} {
		if _, err := normalizeUpstreamAccountSnapshot(UpstreamAccountSnapshot{
			ID: "0123456789abcdef", DisplayName: label, MaskedEmail: "a***@example.com",
		}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe display name %q accepted: %v", label, err)
		}
	}
}

func TestWithUpstreamProviderIsolatesStoreClone(t *testing.T) {
	t.Parallel()
	base := New(nil)
	agy := base.WithUpstreamProvider(UpstreamProviderAntigravity)
	if agy == base || base.upstreamProviderName() != UpstreamProviderCodex || agy.upstreamProviderName() != UpstreamProviderAntigravity {
		t.Fatal("provider scoping mutated original store")
	}
	invalid := base.WithUpstreamProvider("unknown")
	ctx := context.Background()
	if err := invalid.SyncUpstreamAccounts(ctx, nil, time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown provider snapshot error = %v", err)
	}
	if _, err := invalid.SelectUpstreamAccount(ctx, "", nil, time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown provider selection error = %v", err)
	}
	if _, err := invalid.EligibleUpstreamAccounts(ctx, "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown provider eligibility error = %v", err)
	}
	if _, err := invalid.EligibleUpstreamAccountLimits(ctx, "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown provider limits error = %v", err)
	}
}

func TestNormalizeUpstreamAccountIDRequiresCanonicalStableIndex(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0123456789abcdef", "ffffffffffffffff", "0000000000000000"} {
		if normalized, err := normalizeUpstreamAccountID(value); err != nil || normalized != value {
			t.Errorf("normalize %q = %q, %v", value, normalized, err)
		}
	}
	for _, value := range []string{"0", "acct-01", "sha256:abcdef", "ABCDEF0123456789", "alice@example.com", "../account"} {
		if _, err := normalizeUpstreamAccountID(value); !errors.Is(err, ErrInvalid) {
			t.Errorf("normalize unsafe %q error = %v, want ErrInvalid", value, err)
		}
	}
}
