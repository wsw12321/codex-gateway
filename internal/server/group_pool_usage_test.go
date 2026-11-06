package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

type fakeGroupPoolUsageRepository struct {
	groupIDs []string
	accounts []store.GroupPoolAccount
	err      error
}

func (f *fakeGroupPoolUsageRepository) ListGroupPoolAccounts(_ context.Context, groupID string) ([]store.GroupPoolAccount, error) {
	f.groupIDs = append(f.groupIDs, groupID)
	return f.accounts, f.err
}

func TestGroupPoolAccountUsageIsOwnerOnlyAndNamesAccounts(t *testing.T) {
	const (
		groupID = "11111111-1111-4111-8111-111111111111"
		codexA  = "0123456789abcdef"
		codexB  = "1123456789abcdef"
		claude  = "aaaabbbbccccdddd"
	)
	path := "/admin/groups/" + groupID + "/account-usage"
	member, _ := newBillingSourceTestServer(t, store.UserRoleMember, nil)
	memberRepo := &fakeGroupPoolUsageRepository{}
	member.groupPoolUsageRepo = memberRepo
	w := httptest.NewRecorder()
	member.Handler().ServeHTTP(w, usageModelsRequest(t, path))
	if w.Code != http.StatusForbidden || len(memberRepo.groupIDs) != 0 {
		t.Fatalf("member: status=%d reads=%v", w.Code, memberRepo.groupIDs)
	}

	owner, _ := newBillingSourceTestServer(t, store.UserRoleOwner, nil)
	repo := &fakeGroupPoolUsageRepository{accounts: []store.GroupPoolAccount{
		{Provider: store.UpstreamProviderCodex, AccountID: codexA, Alias: "主力", DisplayName: "team-a", EmailMasked: "a***@example.test"},
		{Provider: store.UpstreamProviderCodex, AccountID: codexB},
		{Provider: store.UpstreamProviderAnthropic, AccountID: claude, DisplayName: "claude-a"},
	}}
	owner.groupPoolUsageRepo = repo
	observed := time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC)
	reads := &countingAccountUsageReader{read: func(_ string, accountID string) (accountUsage, string) {
		switch accountID {
		case codexA:
			five, week := 38, 64
			return accountUsage{Source: "live", UpdatedAt: &observed, FiveHour: &accountUsageWindow{UsedPercent: &five}, SevenDay: &accountUsageWindow{UsedPercent: &week}}, ""
		case claude:
			week := 18
			return accountUsage{Source: "observed", UpdatedAt: &observed, Limited: true, SevenDay: &accountUsageWindow{UsedPercent: &week}}, ""
		default:
			return accountUsage{}, "usage_rate_limited"
		}
	}}
	owner.accountUsageRead = reads.reader()

	for _, bad := range []struct{ path, code string }{
		{path + "?refresh=1", "invalid_group_operation"},
		{"/admin/groups/not-a-group/account-usage", "group_resource_not_found"},
	} {
		w := httptest.NewRecorder()
		owner.Handler().ServeHTTP(w, usageModelsRequest(t, bad.path))
		if !strings.Contains(w.Body.String(), bad.code) || len(repo.groupIDs) != 0 {
			t.Fatalf("%s: status=%d body=%s", bad.path, w.Code, w.Body.String())
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		w := httptest.NewRecorder()
		owner.Handler().ServeHTTP(w, usageModelsRequest(t, path))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var body groupPoolUsageResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.GroupID != groupID || body.Truncated || len(body.Accounts) != 3 {
			t.Fatalf("body=%+v", body)
		}
		first, failed, observedAccount := body.Accounts[0], body.Accounts[1], body.Accounts[2]
		if first.AccountID != codexA || first.Alias != "主力" || first.DisplayName != "team-a" || first.EmailMasked != "a***@example.test" ||
			first.Source != "live" || *first.FiveHour.UsedPercent != 38 || *first.SevenDay.UsedPercent != 64 || first.ErrorCode != "" {
			t.Fatalf("codex account=%+v", first)
		}
		if failed.AccountID != codexB || failed.ErrorCode != "usage_rate_limited" || failed.FiveHour != nil {
			t.Fatalf("failed account=%+v", failed)
		}
		if observedAccount.Provider != store.UpstreamProviderAnthropic || !observedAccount.Limited || observedAccount.FiveHour != nil || *observedAccount.SevenDay.UsedPercent != 18 {
			t.Fatalf("claude account=%+v", observedAccount)
		}
	}
	// The second look is served from the shared cache, failures included.
	for _, id := range []string{codexA, codexB} {
		if got := reads.count(store.UpstreamProviderCodex, id); got != 1 {
			t.Fatalf("%s reads=%d", id, got)
		}
	}
	if reads.count(store.UpstreamProviderAnthropic, claude) != 1 {
		t.Fatal("claude must be read once")
	}

	for err, status := range map[error]int{store.ErrNotFound: http.StatusNotFound, errors.New("private database detail"): http.StatusInternalServerError} {
		repo.err = err
		w := httptest.NewRecorder()
		owner.Handler().ServeHTTP(w, usageModelsRequest(t, path))
		if w.Code != status || strings.Contains(w.Body.String(), "private database detail") {
			t.Fatalf("%v: status=%d body=%s", err, w.Code, w.Body.String())
		}
	}
}
