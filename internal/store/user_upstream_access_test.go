package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestUserUpstreamAccessValidationBeforeDatabase(t *testing.T) {
	t.Parallel()
	base := SetUserUpstreamAccessParams{UserID: "00000000-0000-0000-0000-000000000001", Mode: "selected", Reason: "scope", ActorUserID: "actor"}
	for name, edit := range map[string]func(*SetUserUpstreamAccessParams){
		"missing user":      func(p *SetUserUpstreamAccessParams) { p.UserID = "" },
		"noncanonical user": func(p *SetUserUpstreamAccessParams) { p.UserID = "00000000000000000000000000000001" },
		"missing actor":     func(p *SetUserUpstreamAccessParams) { p.ActorUserID = " " },
		"missing reason":    func(p *SetUserUpstreamAccessParams) { p.Reason = "\n " },
		"long reason":       func(p *SetUserUpstreamAccessParams) { p.Reason = strings.Repeat("因", 501) },
		"invalid mode":      func(p *SetUserUpstreamAccessParams) { p.Mode = "shared" },
		"too many accounts": func(p *SetUserUpstreamAccessParams) { p.AccountIDs = make([]string, 10001) },
		"invalid account":   func(p *SetUserUpstreamAccessParams) { p.AccountIDs = []string{"0123456789abcdeF"} },
		"duplicate account": func(p *SetUserUpstreamAccessParams) { p.AccountIDs = []string{"0123456789abcdef", "0123456789abcdef"} },
		"all with accounts": func(p *SetUserUpstreamAccessParams) {
			p.Mode = "all"
			p.AccountIDs = []string{"0123456789abcdef"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			params := base
			edit(&params)
			if _, err := New(nil).SetUserUpstreamAccess(context.Background(), params); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid parameters error = %v", err)
			}
		})
	}
	if _, err := New(nil).GetUserUpstreamAccess(context.Background(), "invalid"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid get error = %v", err)
	}
	invalidProvider := New(nil).WithUpstreamProvider("unknown")
	if _, err := invalidProvider.GetUserUpstreamAccess(context.Background(), base.UserID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid provider get = %v", err)
	}
	if _, err := invalidProvider.SetUserUpstreamAccess(context.Background(), base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid provider set = %v", err)
	}
}

func TestNormalizeUserUpstreamAccessPreservesEmptySelectedAndCopiesInput(t *testing.T) {
	t.Parallel()
	base := SetUserUpstreamAccessParams{UserID: "00000000-0000-0000-0000-000000000001", Reason: " scope ", ActorUserID: "actor"}
	for _, mode := range []string{"all", "selected"} {
		base.Mode = mode
		got, err := normalizeUserUpstreamAccess(base)
		if err != nil || got.Mode != mode || got.Reason != "scope" || got.AccountIDs == nil || len(got.AccountIDs) != 0 {
			t.Fatalf("empty %s = %+v, %v", mode, got, err)
		}
	}
	base.AccountIDs = []string{"ffffffffffffffff", "0123456789abcdef"}
	got, err := normalizeUserUpstreamAccess(base)
	if err != nil || !reflect.DeepEqual(got.AccountIDs, []string{"0123456789abcdef", "ffffffffffffffff"}) {
		t.Fatalf("normalized accounts = %+v, %v", got, err)
	}
	got.AccountIDs[0] = "0000000000000000"
	if !reflect.DeepEqual(base.AccountIDs, []string{"ffffffffffffffff", "0123456789abcdef"}) {
		t.Fatal("normalizing or changing the result mutated the caller's accounts")
	}
}
