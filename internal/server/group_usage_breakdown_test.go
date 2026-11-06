package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/store"
)

type fakeGroupUsageBreakdownRepository struct {
	calls  [][2]string
	result store.GroupUsageBreakdown
	err    error
}

func (f *fakeGroupUsageBreakdownRepository) GroupUsageBreakdown(_ context.Context, groupID, periodID string) (store.GroupUsageBreakdown, error) {
	f.calls = append(f.calls, [2]string{groupID, periodID})
	return f.result, f.err
}

func TestGroupUsageBreakdownIsOwnerOnlyAndPassesThePeriod(t *testing.T) {
	const groupID = "11111111-1111-4111-8111-111111111111"
	const periodID = "22222222-2222-4222-8222-222222222222"
	member, _ := newBillingSourceTestServer(t, store.UserRoleMember, nil)
	memberRepo := &fakeGroupUsageBreakdownRepository{}
	member.groupUsageBreakdownRepo = memberRepo
	w := httptest.NewRecorder()
	member.Handler().ServeHTTP(w, usageModelsRequest(t, "/admin/groups/"+groupID+"/usage-breakdown"))
	if w.Code != http.StatusForbidden || len(memberRepo.calls) != 0 {
		t.Fatalf("member: status=%d calls=%v", w.Code, memberRepo.calls)
	}

	owner, _ := newBillingSourceTestServer(t, store.UserRoleOwner, nil)
	repo := &fakeGroupUsageBreakdownRepository{result: store.GroupUsageBreakdown{GroupID: groupID, LimitUSD: "100.000000000000",
		Members:  []store.GroupUsageMember{{UserID: "u1", Username: "alice", CurrentMember: true, GroupUsedUSD: "1.000000000000"}},
		Accounts: []store.GroupUsageAccount{}, Periods: []store.GroupUsagePeriodRef{}, DataComplete: true}}
	owner.groupUsageBreakdownRepo = repo
	for _, tc := range []struct {
		name, path string
		status     int
		code       string
		call       *[2]string
	}{
		{name: "current period", path: "/admin/groups/" + groupID + "/usage-breakdown", status: http.StatusOK, call: &[2]string{groupID, ""}},
		{name: "selected period", path: "/admin/groups/" + groupID + "/usage-breakdown?period_id=" + periodID, status: http.StatusOK, call: &[2]string{groupID, periodID}},
		{name: "unknown parameter", path: "/admin/groups/" + groupID + "/usage-breakdown?user_id=u1", status: http.StatusBadRequest, code: "invalid_group_operation"},
		{name: "repeated period", path: "/admin/groups/" + groupID + "/usage-breakdown?period_id=" + periodID + "&period_id=" + periodID, status: http.StatusBadRequest, code: "invalid_group_operation"},
		{name: "empty period", path: "/admin/groups/" + groupID + "/usage-breakdown?period_id=", status: http.StatusBadRequest, code: "invalid_group_operation"},
		{name: "invalid group", path: "/admin/groups/not-a-group/usage-breakdown", status: http.StatusNotFound, code: "group_resource_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(repo.calls)
			w := httptest.NewRecorder()
			owner.Handler().ServeHTTP(w, usageModelsRequest(t, tc.path))
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.call == nil {
				if len(repo.calls) != before {
					t.Fatalf("rejected request reached the store: %v", repo.calls)
				}
				return
			}
			if len(repo.calls) != before+1 || repo.calls[before] != *tc.call {
				t.Fatalf("calls = %v", repo.calls)
			}
			var body store.GroupUsageBreakdown
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.GroupID != groupID || len(body.Members) != 1 || !body.DataComplete {
				t.Fatalf("body = %s (%v)", w.Body.String(), err)
			}
		})
	}

	for err, status := range map[error]int{store.ErrNotFound: http.StatusNotFound, store.ErrInvalid: http.StatusBadRequest, errors.New("private database detail"): http.StatusInternalServerError} {
		repo.err = err
		w := httptest.NewRecorder()
		owner.Handler().ServeHTTP(w, usageModelsRequest(t, "/admin/groups/"+groupID+"/usage-breakdown"))
		if w.Code != status || strings.Contains(w.Body.String(), "private database detail") {
			t.Fatalf("%v: status=%d body=%s", err, w.Code, w.Body.String())
		}
	}
}
