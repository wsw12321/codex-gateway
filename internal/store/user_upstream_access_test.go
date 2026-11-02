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

func TestUserUpstreamAccessBatchValidationBeforeDatabase(t *testing.T) {
	t.Parallel()
	const user, account = "00000000-0000-0000-0000-000000000001", "0123456789abcdef"
	base := SetUserUpstreamAccessBatchParams{UserIDs: []string{user}, Action: UserUpstreamAccessBatchDisable, AccountIDs: []string{account}, Reason: "batch", ActorUserID: "actor"}
	for name, edit := range map[string]func(*SetUserUpstreamAccessBatchParams){
		"no users":                func(p *SetUserUpstreamAccessBatchParams) { p.UserIDs = nil },
		"too many users":          func(p *SetUserUpstreamAccessBatchParams) { p.UserIDs = make([]string, 1001) },
		"noncanonical user":       func(p *SetUserUpstreamAccessBatchParams) { p.UserIDs = []string{"00000000000000000000000000000001"} },
		"duplicate user":          func(p *SetUserUpstreamAccessBatchParams) { p.UserIDs = []string{user, user} },
		"missing actor":           func(p *SetUserUpstreamAccessBatchParams) { p.ActorUserID = " " },
		"missing reason":          func(p *SetUserUpstreamAccessBatchParams) { p.Reason = "\n " },
		"long reason":             func(p *SetUserUpstreamAccessBatchParams) { p.Reason = strings.Repeat("因", 501) },
		"unknown action":          func(p *SetUserUpstreamAccessBatchParams) { p.Action = "selected" },
		"too many accounts":       func(p *SetUserUpstreamAccessBatchParams) { p.AccountIDs = make([]string, 1001) },
		"invalid account":         func(p *SetUserUpstreamAccessBatchParams) { p.AccountIDs = []string{"0123456789abcdeF"} },
		"duplicate account":       func(p *SetUserUpstreamAccessBatchParams) { p.AccountIDs = []string{account, account} },
		"disable without account": func(p *SetUserUpstreamAccessBatchParams) { p.AccountIDs = nil },
		"enable with provider": func(p *SetUserUpstreamAccessBatchParams) {
			p.Action, p.Providers = UserUpstreamAccessBatchEnable, []string{UpstreamProviderCodex}
		},
		"all with account": func(p *SetUserUpstreamAccessBatchParams) {
			p.Action, p.Providers = UserUpstreamAccessBatchAll, []string{UpstreamProviderCodex}
		},
		"all without provider": func(p *SetUserUpstreamAccessBatchParams) {
			p.Action, p.AccountIDs = UserUpstreamAccessBatchAll, nil
		},
		"all with unknown provider": func(p *SetUserUpstreamAccessBatchParams) {
			p.Action, p.AccountIDs, p.Providers = UserUpstreamAccessBatchAll, nil, []string{"Codex"}
		},
		"all with duplicate provider": func(p *SetUserUpstreamAccessBatchParams) {
			p.Action, p.AccountIDs, p.Providers = UserUpstreamAccessBatchAll, nil, []string{UpstreamProviderCodex, UpstreamProviderCodex}
		},
	} {
		t.Run(name, func(t *testing.T) {
			params := base
			edit(&params)
			if _, err := New(nil).SetUserUpstreamAccessBatch(context.Background(), params); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid parameters error = %v", err)
			}
		})
	}
	input := SetUserUpstreamAccessBatchParams{
		UserIDs: []string{"00000000-0000-0000-0000-000000000002", user}, Action: UserUpstreamAccessBatchAll,
		Providers: []string{UpstreamProviderAnthropic, UpstreamProviderCodex}, Reason: " batch ", ActorUserID: "actor",
	}
	got, err := normalizeUserUpstreamAccessBatch(input)
	if err != nil || got.Reason != "batch" || !reflect.DeepEqual(got.UserIDs, []string{user, "00000000-0000-0000-0000-000000000002"}) ||
		!reflect.DeepEqual(got.Providers, []string{UpstreamProviderAnthropic, UpstreamProviderCodex}) {
		t.Fatalf("normalized batch = %+v, %v", got, err)
	}
	if input.UserIDs[0] != "00000000-0000-0000-0000-000000000002" {
		t.Fatal("normalizing mutated the caller's users")
	}
}

func TestNextUserUpstreamAccessAppliesBatchActions(t *testing.T) {
	t.Parallel()
	const a, b, c = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"
	registered := []string{a, b, c}
	all := UserUpstreamAccess{UserID: "u", Provider: UpstreamProviderCodex, Mode: "all", AccountIDs: []string{}}
	selectedB := UserUpstreamAccess{UserID: "u", Provider: UpstreamProviderCodex, Mode: "selected", AccountIDs: []string{b}}
	empty := UserUpstreamAccess{UserID: "u", Provider: UpstreamProviderCodex, Mode: "selected", AccountIDs: []string{}}
	for _, test := range []struct {
		name     string
		action   string
		previous UserUpstreamAccess
		selected []string
		mode     string
		want     []string
	}{
		{"enable keeps all", UserUpstreamAccessBatchEnable, all, []string{a}, "all", []string{}},
		{"enable adds to selected", UserUpstreamAccessBatchEnable, selectedB, []string{c, a, b}, "selected", []string{a, b, c}},
		{"enable reopens empty", UserUpstreamAccessBatchEnable, empty, []string{c}, "selected", []string{c}},
		{"disable converts all", UserUpstreamAccessBatchDisable, all, []string{b}, "selected", []string{a, c}},
		{"disable every account", UserUpstreamAccessBatchDisable, all, registered, "selected", []string{}},
		{"disable removes from selected", UserUpstreamAccessBatchDisable, selectedB, []string{a, b}, "selected", []string{}},
		{"disable keeps unrelated", UserUpstreamAccessBatchDisable, selectedB, []string{a}, "selected", []string{b}},
		{"all restores default", UserUpstreamAccessBatchAll, selectedB, nil, "all", []string{}},
		{"all keeps all", UserUpstreamAccessBatchAll, all, nil, "all", []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := map[string]bool{}
			for _, id := range test.selected {
				selected[id] = true
			}
			previousIDs := append([]string{}, test.previous.AccountIDs...)
			got := nextUserUpstreamAccess(test.action, test.previous, selected, registered)
			if got.UserID != "u" || got.Provider != UpstreamProviderCodex || got.Mode != test.mode || !reflect.DeepEqual(got.AccountIDs, test.want) {
				t.Fatalf("next = %+v; want %s %v", got, test.mode, test.want)
			}
			if !reflect.DeepEqual(test.previous.AccountIDs, previousIDs) {
				t.Fatal("next mutated the previous scope")
			}
		})
	}
}
