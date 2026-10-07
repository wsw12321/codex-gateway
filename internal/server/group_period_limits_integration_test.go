//go:build integration

package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGroupPeriodLimitsHTTPPostgresIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	starts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	body := func() map[string]any {
		return map[string]any{
			"operation_id": uuid.NewString(), "reason": "Group period limits HTTP regression",
			"name": "Period limits", "limit_usd": "20", "period": "day", "starts_at": starts.Format(time.RFC3339),
		}
	}
	createBody := body()
	created := invitationResponse(t, f.send(t, http.MethodPost, "/admin/groups", createBody, f.cookie, http.StatusOK))
	id, periodID := created["id"].(string), created["period_id"].(string)
	path := "/admin/groups/" + id
	assertLimits := func(response map[string]any, count int) {
		t.Helper()
		if response["id"] != id || response["period_count"] != float64(count) || response["current_period_number"] != float64(1) || response["period_id"] != periodID {
			t.Fatalf("unexpected period limits: %+v", response)
		}
		expires, exists := response["expires_at"]
		if !exists {
			t.Fatalf("missing expires_at: %+v", response)
		}
		if count == 0 {
			if expires != nil {
				t.Fatalf("unlimited group has an expiry: %+v", response)
			}
			return
		}
		end, err := time.Parse(time.RFC3339Nano, response["period_ends_at"].(string))
		if err != nil {
			t.Fatal(err)
		}
		actual, err := time.Parse(time.RFC3339Nano, expires.(string))
		if err != nil || !actual.Equal(end.Add(time.Duration(count-1)*24*time.Hour)) {
			t.Fatalf("expiry does not follow saved period: %+v error=%v", response, err)
		}
	}
	assertLimits(created, 1)
	f.send(t, http.MethodPut, path+"/members", map[string]any{
		"operation_id": uuid.NewString(), "reason": "Add owner for personal summary regression", "action": "add", "user_ids": []string{f.owner.ID},
	}, f.cookie, http.StatusOK)
	if _, err := f.s.store.DB().ExecContext(ctx, `UPDATE group_usage_periods SET used_usd=3 WHERE id=$1`, periodID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.DB().ExecContext(ctx, `INSERT INTO group_member_usage(period_id,user_id,used_usd) VALUES($1,$2,3)`, periodID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{4, 0, 99, 1} {
		edit := body()
		edit["period_count"] = count
		updated := invitationResponse(t, f.send(t, http.MethodPut, path, edit, f.cookie, http.StatusOK))
		assertLimits(updated, count)
		for _, key := range []string{"period_starts_at", "period_ends_at", "starts_at", "limit_usd"} {
			if updated[key] != created[key] {
				t.Fatalf("count edit changed %s: before=%v after=%v", key, created[key], updated[key])
			}
		}
		if updated["used_usd"] != "3.000000000000" || updated["members"].([]any)[0].(map[string]any)["used_usd"] != "3.000000000000" {
			t.Fatalf("count edit lost group or member usage: %+v", updated)
		}
		omitted := invitationResponse(t, f.send(t, http.MethodPut, path, body(), f.cookie, http.StatusOK))
		assertLimits(omitted, count)
		replay := invitationResponse(t, f.send(t, http.MethodPut, path, edit, f.cookie, http.StatusOK))
		assertLimits(replay, count)
		edit["period_count"] = (count + 1) % 100
		f.send(t, http.MethodPut, path, edit, f.cookie, http.StatusConflict)
		personal := invitationResponse(t, f.send(t, http.MethodGet, "/admin/billing/me", nil, f.cookie, http.StatusOK))
		assertLimits(personal["group"].(map[string]any), count)
		if personal["group_member_used_usd"] != "3.000000000000" {
			t.Fatalf("personal count update lost own usage: %+v", personal)
		}
	}
	// A replay of the original omitted-count request retains its initial snapshot,
	// even after later changes to the group's count and counters.
	replay := invitationResponse(t, f.send(t, http.MethodPost, "/admin/groups", createBody, f.cookie, http.StatusOK))
	assertLimits(replay, 1)
	if replay["used_usd"] != created["used_usd"] {
		t.Fatalf("create replay did not preserve its response snapshot: %+v", replay)
	}
	for _, value := range []any{nil, -1, 100, "1", 1.5, true} {
		invalid := body()
		invalid["period_count"] = value
		f.send(t, http.MethodPut, path, invalid, f.cookie, http.StatusBadRequest)
	}
}
