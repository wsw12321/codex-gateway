//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGroupRenamePostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	billingIntegrationModelScopes(t, ctx, s, "billing-priced-model")
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	owner := globalUsageIntegrationUser(t, ctx, s, "rename-owner", UserRoleOwner)
	write := func(reason string) BillingWriteParams { return billingIntegrationWrite(t, owner.ID, reason, now) }
	limit := "20"
	group, err := s.PutGroup(ctx, PutGroupParams{BillingWriteParams: write("create"), Name: "旧名称", LimitUSD: "100", MemberLimitUSD: &limit, Period: "week"})
	if err != nil {
		t.Fatal(err)
	}

	params := RenameGroupParams{BillingWriteParams: write("规范命名"), GroupID: group.ID, Name: "  研发 团队  "}
	renamed, err := s.RenameGroup(ctx, params)
	if err != nil || renamed.Name != "研发 团队" || renamed.ID != group.ID {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	// Only the name changes: limits, period, models and the current period stay.
	if renamed.LimitUSD != group.LimitUSD || renamed.MemberLimitUSD == nil || *renamed.MemberLimitUSD != *group.MemberLimitUSD ||
		renamed.Period != group.Period || renamed.PeriodID != group.PeriodID || renamed.CurrentPeriodNumber != group.CurrentPeriodNumber ||
		strings.Join(renamed.ModelIDs, ",") != strings.Join(group.ModelIDs, ",") {
		t.Fatalf("rename changed other fields:\nbefore %+v\nafter  %+v", group, renamed)
	}
	if stored, err := s.GetGroup(ctx, group.ID); err != nil || stored.Name != "研发 团队" {
		t.Fatalf("stored name: %+v %v", stored, err)
	}

	var metadata []byte
	var events int
	if err := s.DB().QueryRowContext(ctx, `SELECT metadata, (SELECT count(*) FROM audit_events WHERE event_type='group.rename' AND subject_id=$1)
		FROM audit_events WHERE event_type='group.rename' AND subject_id=$1 ORDER BY occurred_at DESC LIMIT 1`, group.ID).Scan(&metadata, &events); err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	if err := json.Unmarshal(metadata, &audit); err != nil || events != 1 || audit["previous_name"] != "旧名称" || audit["name"] != "研发 团队" ||
		audit["reason"] != "规范命名" || audit["operation_id"] != params.OperationID {
		t.Fatalf("audit events=%d metadata=%s err=%v", events, metadata, err)
	}

	// Replays return the original response without a second write or audit row.
	if _, err := s.RenameGroup(ctx, RenameGroupParams{BillingWriteParams: write("later"), GroupID: group.ID, Name: "后来名称"}); err != nil {
		t.Fatal(err)
	}
	replay, err := s.RenameGroup(ctx, params)
	if err != nil || replay.Name != "研发 团队" {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if stored, err := s.GetGroup(ctx, group.ID); err != nil || stored.Name != "后来名称" {
		t.Fatalf("replay overwrote a later rename: %+v %v", stored, err)
	}
	conflicting := params
	conflicting.Name = "不同名称"
	if _, err := s.RenameGroup(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("operation ID reused for another name: %v", err)
	}

	for _, name := range []string{"", " \t ", strings.Repeat("组", 121)} {
		if _, err := s.RenameGroup(ctx, RenameGroupParams{BillingWriteParams: write("invalid"), GroupID: group.ID, Name: name}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	if _, err := s.RenameGroup(ctx, RenameGroupParams{BillingWriteParams: write("missing"), GroupID: "6f2a9d5e-0c39-4a5c-9c8e-1f2d3c4b5a69", Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing group: %v", err)
	}

	if _, err := s.ArchiveGroup(ctx, ArchiveGroupParams{BillingWriteParams: write("archive"), GroupID: group.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameGroup(ctx, RenameGroupParams{BillingWriteParams: write("archived"), GroupID: group.ID, Name: "归档后改名"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived group renamed: %v", err)
	}
	if stored, err := s.GetGroup(ctx, group.ID); err != nil || stored.Name != "后来名称" || stored.ArchivedAt == nil {
		t.Fatalf("archived group changed: %+v %v", stored, err)
	}
}
