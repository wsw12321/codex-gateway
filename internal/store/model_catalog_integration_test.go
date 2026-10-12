//go:build integration

package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestUsageModelCatalogIncludesLedgerOnlyHistoryPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	first := globalUsageIntegrationUser(t, ctx, s, "catalog-first", UserRoleMember)
	second := globalUsageIntegrationUser(t, ctx, s, "catalog-second", UserRoleMember)
	empty := globalUsageIntegrationUser(t, ctx, s, "catalog-empty", UserRoleMember)
	// These financial rows deliberately have no raw request or aggregate rows.
	// Their owner is user_id; the actor may be a different account.
	insert := func(userID, actorID, entryType, requested, actual string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_ledger_entries(user_id,actor_user_id,entry_type,model,actual_model) VALUES($1,$2,$3,$4,$5)`, userID, actorID, entryType, valueOrNil(requested), valueOrNil(actual)); err != nil {
			t.Fatal(err)
		}
	}
	insert(first.ID, second.ID, "usage_charge", "gpt-retired", "")
	insert(first.ID, second.ID, "usage_charge", "gpt-retired", "")
	insert(first.ID, second.ID, "usage_charge", "requested-alias", "claude-retired-actual")
	insert(first.ID, second.ID, "usage_charge", "", "")
	insert(first.ID, second.ID, "adjustment", "non-usage-model", "")
	insert(second.ID, first.ID, "usage_charge", "gemini-other-user", "")
	for _, tc := range []struct {
		name, userID string
		want         []string
	}{
		{"personal history", first.ID, []string{"claude-retired-actual", "gpt-retired"}},
		{"other owner", second.ID, []string{"gemini-other-user"}},
		{"empty history", empty.ID, []string{}},
		{"global history", "", []string{"claude-retired-actual", "gemini-other-user", "gpt-retired"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListUsageModels(ctx, tc.userID)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("models=%v, want %v: %v", got, tc.want, err)
			}
		})
	}
}
