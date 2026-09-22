//go:build integration

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

func informationIntegrationDrain(t *testing.T, ctx context.Context, s *Store, actorID string, cutoff time.Time) InformationCleanupJob {
	t.Helper()
	id, _ := newUUID()
	days := int(s.now().UTC().Truncate(24*time.Hour).Sub(cutoff) / (24 * time.Hour))
	job, err := s.CreateInformationCleanupJob(ctx, InformationCleanupParams{OperationID: id, ActorUserID: actorID, RetentionDays: days, Cutoff: cutoff})
	if err != nil {
		t.Fatal(err)
	}
	for batch := 0; batch < 100 && job.Status != "completed"; batch++ {
		value, err := s.RunInformationCleanupBatch(ctx, 2)
		if err != nil || value == nil {
			t.Fatalf("run cleanup: %+v, %v", value, err)
		}
		job = *value
	}
	if job.Status != "completed" {
		t.Fatal("cleanup did not converge")
	}
	return job
}

func TestInformationMonthlyDeletionAccountingPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	cutoff, _ := InformationCutoff(now, 2)
	oldMonth := time.Date(cutoff.Year(), cutoff.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	actor := globalUsageIntegrationUser(t, ctx, s, "information-month-actor", UserRoleMember)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "information-month-accounting")
	billingIntegrationComplete(t, ctx, s, user, device, key, "information-expired-month-complete", oldMonth.Add(time.Hour), 20, "month-model")
	runningID := "information-expired-month-running"
	if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{RequestID: runningID, UserID: user.ID,
		DeviceID: device.ID, APIKeyID: key.ID, Model: "month-model", Endpoint: "responses", RequestedAt: oldMonth.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AggregateUsageDay(ctx, oldMonth, "UTC"); err != nil {
		t.Fatal(err)
	}
	if err := s.AggregateUsageMonth(ctx, oldMonth, "UTC"); err != nil {
		t.Fatal(err)
	}
	assertMonthlyCount := func(want int) {
		t.Helper()
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM usage_monthly WHERE usage_month=$1`, monthBucket(oldMonth)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("expired monthly rows = %d, want %d", count, want)
		}
	}
	assertMonthlyCount(1)
	id, _ := newUUID()
	job, err := s.CreateInformationCleanupJob(ctx, InformationCleanupParams{OperationID: id, ActorUserID: actor.ID, RetentionDays: 2, Cutoff: cutoff})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AggregateUsageMonth(ctx, oldMonth, "UTC"); err != nil {
		t.Fatal(err)
	}
	assertMonthlyCount(1)
	first, err := s.RunInformationCleanupBatch(ctx, 1)
	if err != nil || first == nil || first.Status != "running" {
		t.Fatalf("first batch = %+v, %v", first, err)
	}
	if err := s.AggregateUsageMonth(ctx, oldMonth, "UTC"); err != nil {
		t.Fatal(err)
	}
	assertMonthlyCount(1)
	for i := 0; i < 10 && job.Status != "completed"; i++ {
		next, err := s.RunInformationCleanupBatch(ctx, 1)
		if err != nil || next == nil {
			t.Fatalf("cleanup batch = %+v, %v", next, err)
		}
		job = *next
	}
	if job.Status != "completed" || job.Report.DeleteCounts["usage_monthly"] != 1 {
		t.Fatalf("monthly deletion missing from completed report: %+v", job)
	}
	assertMonthlyCount(0)
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: runningID,
		State: "completed", HTTPStatus: 200, CompletedAt: now, InputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.AggregateUsageMonth(ctx, oldMonth, "UTC"); err != nil {
		t.Fatal(err)
	}
	assertMonthlyCount(0)
}

func TestInformationLateCompletionAfterDetailRetentionPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	day := time.Date(now.Year(), now.Month()-5, 16, 0, 0, 0, 0, time.UTC)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "information-old-late-completion")
	for i := 0; i < 10; i++ {
		billingIntegrationComplete(t, ctx, s, user, device, key, fmt.Sprintf("information-pruned-detail-%02d", i), day, 10, "late-model")
	}
	requestID := "information-protected-late-detail"
	if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{RequestID: requestID, UserID: user.ID,
		DeviceID: device.ID, APIKeyID: key.ID, Model: "late-model", Endpoint: "responses", RequestedAt: day}); err != nil {
		t.Fatal(err)
	}
	newDimensionID := "information-protected-new-dimension"
	if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{RequestID: newDimensionID, UserID: user.ID,
		DeviceID: device.ID, APIKeyID: key.ID, Model: "new-late-model", Endpoint: "responses", RequestedAt: day}); err != nil {
		t.Fatal(err)
	}
	if err := s.AggregateUsageDay(ctx, day, "UTC"); err != nil {
		t.Fatal(err)
	}
	var snapshotAt time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT updated_at FROM usage_daily WHERE user_id=$1`, user.ID).Scan(&snapshotAt); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.DeleteUsageRequestsBefore(ctx, now.AddDate(0, 0, -90), 100); err != nil || deleted != 10 {
		t.Fatalf("ordinary detail retention = %d, %v", deleted, err)
	}
	firstToken := day.Add(500 * time.Millisecond)
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: requestID, State: "completed",
		HTTPStatus: 200, CompletedAt: snapshotAt.Add(time.Second), InputTokens: 7, CachedInputTokens: 2, OutputTokens: 3,
		ReasoningTokens: 1, RequestBytes: 13, ResponseBytes: 17, FirstTokenAt: &firstToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteUsageRequest(ctx, CompleteUsageRequestParams{RequestID: newDimensionID, State: "completed",
		HTTPStatus: 200, CompletedAt: snapshotAt.Add(2 * time.Second), InputTokens: 9}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if i == 2 {
			if deleted, err := s.DeleteUsageRequestsBefore(ctx, now.AddDate(0, 0, 91), 100); err != nil || deleted != 2 {
				t.Fatalf("late detail retention = %d, %v", deleted, err)
			}
		}
		if err := s.AggregateUsageMonth(ctx, day, "UTC"); err != nil {
			t.Fatal(err)
		}
		var requests, tokens int64
		var p95 sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT request_count,input_tokens,p95_duration_ms
			FROM usage_monthly WHERE user_id=$1 AND model='late-model'`, user.ID).Scan(&requests, &tokens, &p95); err != nil {
			t.Fatal(err)
		}
		if requests != 11 || tokens != 107 || p95.Valid {
			t.Fatalf("late rebuild %d = requests %d, tokens %d, p95 %v", i, requests, tokens, p95)
		}
		var ttftCount, ttftSum, durationCount, durationSum, requestBytes, responseBytes int64
		var dailyP95 sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT request_count,input_tokens,ttft_count,ttft_sum_ms::bigint,
			duration_count,duration_sum_ms::bigint,request_bytes,response_bytes,p95_duration_ms
			FROM usage_daily WHERE user_id=$1 AND model='late-model'`, user.ID).Scan(&requests, &tokens, &ttftCount, &ttftSum,
			&durationCount, &durationSum, &requestBytes, &responseBytes, &dailyP95); err != nil {
			t.Fatal(err)
		}
		if requests != 11 || tokens != 107 || ttftCount != 1 || ttftSum != 500 || durationCount != 11 || durationSum <= 10_000 || requestBytes != 13 || responseBytes != 17 || dailyP95.Valid {
			t.Fatalf("persisted late daily metrics %d = %d/%d ttft=%d/%d duration=%d/%d bytes=%d/%d p95=%v",
				i, requests, tokens, ttftCount, ttftSum, durationCount, durationSum, requestBytes, responseBytes, dailyP95)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT request_count,input_tokens FROM usage_monthly
			WHERE user_id=$1 AND model='new-late-model'`, user.ID).Scan(&requests, &tokens); err != nil || requests != 1 || tokens != 9 {
			t.Fatalf("persisted new dimension %d = %d/%d, %v", i, requests, tokens, err)
		}
	}
}

func TestInformationSubscriptionAndGroupPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	cutoff, _ := InformationCutoff(now, 5)
	old := cutoff.AddDate(0, 0, -5)
	actor := globalUsageIntegrationUser(t, ctx, s, "information-sub-actor", UserRoleMember)
	active, device, key := billingIntegrationPrincipal(t, ctx, s, "information-sub-active")
	ended, _, _ := billingIntegrationPrincipal(t, ctx, s, "information-sub-ended")
	sub, err := s.PutSubscription(ctx, PutSubscriptionParams{
		BillingWriteParams: billingIntegrationWrite(t, actor.ID, "active monthly subscription", old),
		UserID:             active.ID, Tier: BillingTierMonth, AllowanceUSD: "10", PeriodCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.PutGroup(ctx, PutGroupParams{
		BillingWriteParams: billingIntegrationWrite(t, actor.ID, "long group period", old),
		Name:               "Information group", LimitUSD: "100", Period: "custom", CustomDays: 365, StartsAt: &old,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupMembers(ctx, SetGroupMembersParams{
		BillingWriteParams: billingIntegrationWrite(t, actor.ID, "group member", old),
		GroupID:            group.ID, UserIDs: []string{active.ID}, Action: "add",
	}); err != nil {
		t.Fatal(err)
	}
	requestID := "information-subscription-charge"
	billingIntegrationReserveAndComplete(t, ctx, s, active, device, key, requestID, old.Add(time.Hour), 1_000_000, "1", "billing-priced-model")
	if _, err := s.SettleBilling(ctx, requestID, old.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	setWrite := billingIntegrationWrite(t, actor.ID, "ended subscription", old.AddDate(0, 0, -3))
	if _, err := s.PutSubscription(ctx, PutSubscriptionParams{BillingWriteParams: setWrite,
		UserID: ended.ID, Tier: BillingTierDay, AllowanceUSD: "1", PeriodCount: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSubscription(ctx, DeleteSubscriptionParams{
		BillingWriteParams: billingIntegrationWrite(t, actor.ID, "disable old subscription", old.AddDate(0, 0, -2)),
		UserID:             ended.ID, Tier: BillingTierDay,
	}); err != nil {
		t.Fatal(err)
	}
	// Exercise immutable legacy operation snapshots in the same dependency
	// graph as an ended subscription and its operation/result pair.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscription_operation_snapshots
		(ledger_entry_id,subscription_id,period_count,current_period_number,expires_at)
		SELECT l.id,s.id,s.period_count,s.current_period_number,s.expires_at
		FROM billing_ledger_entries l JOIN billing_subscriptions s ON s.user_id=l.user_id WHERE l.operation_id=$1`, setWrite.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM billing_subscription_operation_snapshots`); err == nil {
		t.Fatal("unguarded operation snapshot deletion accepted")
	}
	job := informationIntegrationDrain(t, ctx, s, actor.ID, cutoff)
	if job.Report.DeleteCounts["billing_subscription_periods"] != 1 || job.Report.DeleteCounts["billing_subscriptions"] != 1 || job.Report.DeleteCounts["billing_subscription_operation_snapshots"] != 1 {
		t.Fatalf("ended subscription history was not removed: %+v", job.Report)
	}
	var remaining, groupUsed string
	if err := s.db.QueryRowContext(ctx, `SELECT remaining_usd::text FROM billing_subscription_periods WHERE id=$1`, sub.PeriodID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT used_usd::text FROM group_usage_periods WHERE id=$1`, group.PeriodID).Scan(&groupUsed); err != nil {
		t.Fatal(err)
	}
	if remaining != "9.000000000000" || groupUsed != "1.000000000000" {
		t.Fatalf("current quota changed: subscription=%s group=%s", remaining, groupUsed)
	}
	var activeLedger, endedHistory int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM billing_ledger_entries WHERE user_id=$1`, active.ID).Scan(&activeLedger); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM billing_ledger_entries WHERE user_id=$1`, ended.ID).Scan(&endedHistory); err != nil {
		t.Fatal(err)
	}
	if activeLedger != 1 || endedHistory != 0 {
		t.Fatalf("required current subscription ledger=%d, ended history=%d", activeLedger, endedHistory)
	}
}

func TestInformationLateAggregationAndSettlementPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	cutoff, _ := InformationCutoff(now, 2)
	actor := globalUsageIntegrationUser(t, ctx, s, "information-race-actor", UserRoleMember)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "information-race")
	billingIntegrationComplete(t, ctx, s, user, device, key, "information-summary-first", cutoff, 10, "first-model")
	if err := s.AggregateUsageDay(ctx, cutoff, "UTC"); err != nil {
		t.Fatal(err)
	}
	billingIntegrationComplete(t, ctx, s, user, device, key, "information-summary-late-same", cutoff.Add(time.Hour), 20, "first-model")
	billingIntegrationComplete(t, ctx, s, user, device, key, "information-summary-late-other", cutoff.Add(time.Hour), 40, "second-model")
	old := cutoff.Add(-48 * time.Hour)
	billingIntegrationSetRate(t, ctx, s, actor.ID, "1", old)
	billingIntegrationRecharge(t, ctx, s, actor.ID, user.ID, "5", old)
	id := "information-concurrent-settle"
	billingIntegrationReserveAndComplete(t, ctx, s, user, device, key, id, old.Add(time.Hour), 1_000_000, "1", "billing-priced-model")
	operationID, _ := newUUID()
	if _, err := s.CreateInformationCleanupJob(ctx, InformationCleanupParams{OperationID: operationID, ActorUserID: actor.ID, RetentionDays: 2, Cutoff: cutoff}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsCh := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, err := s.SettleBilling(ctx, id, now)
		errorsCh <- err
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			job, err := s.RunInformationCleanupBatch(ctx, 1)
			if err != nil {
				errorsCh <- err
				return
			}
			if job == nil || job.Status == "completed" {
				break
			}
		}
	}()
	go func() {
		defer wg.Done()
		errorsCh <- s.AggregateUsageMonth(ctx, cutoff, "UTC")
	}()
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var balance string
	if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, user.ID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != "4.000000000000" {
		t.Fatalf("concurrent settlement balance = %s", balance)
	}
	for model, want := range map[string]int64{"first-model": 30, "second-model": 40} {
		var tokens int64
		if err := s.db.QueryRowContext(ctx, `SELECT input_tokens FROM usage_monthly WHERE user_id=$1 AND model=$2`, user.ID, model).Scan(&tokens); err != nil || tokens != want {
			t.Fatalf("late summary %s = %d, %v; want %d", model, tokens, err, want)
		}
	}
}

func TestInformationCleanupPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := informationIntegrationStore(t, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	// Fix a mid-month cutoff older than ordinary 90-day detail retention.
	cutoff := time.Date(now.Year(), now.Month()-5, 15, 0, 0, 0, 0, time.UTC)
	retentionDays := int(now.UTC().Truncate(24*time.Hour).Sub(cutoff) / (24 * time.Hour))
	old := cutoff.Add(-48 * time.Hour)
	actor := globalUsageIntegrationUser(t, ctx, s, "information-actor", UserRoleMember)
	user, device, key := billingIntegrationPrincipal(t, ctx, s, "information-history")
	active, activeDevice, activeKey := billingIntegrationPrincipal(t, ctx, s, "information-active")
	billingIntegrationSetRate(t, ctx, s, actor.ID, "1", old)
	write := billingIntegrationWrite(t, actor.ID, "old recharge", old)
	params := RechargeUserParams{BillingWriteParams: write, UserID: user.ID, CNYAmount: "1"}
	entry, err := s.RechargeUser(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	requestID := "information-old-request-0001"
	billingIntegrationReserveAndComplete(t, ctx, s, user, device, key, requestID, old.Add(time.Minute), 1_000_000, "1", "billing-priced-model")
	if _, err := s.SettleBilling(ctx, requestID, old.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Available cash and pending billing remain usable even when old.
	activeEntry := billingIntegrationRecharge(t, ctx, s, actor.ID, active.ID, "10", old)
	activeID := "information-unsettled-request"
	if _, err := s.ReserveBilling(ctx, BillingReservationParams{RequestID: activeID, UserID: active.ID, APIKeyID: activeKey.ID,
		Model: "billing-priced-model", InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0", Now: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginUsageRequest(ctx, BeginUsageRequestParams{RequestID: activeID, UserID: active.ID, DeviceID: activeDevice.ID,
		APIKeyID: activeKey.ID, Model: "billing-priced-model", Endpoint: "responses", RequestedAt: old}); err != nil {
		t.Fatal(err)
	}
	// A delayed settlement keeps the original request timestamp in its
	// ledger, even after both detail and reservations were independently aged
	// out. Cleanup must not substitute the later ledger creation time.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO billing_ledger_entries
		(user_id,entry_type,request_id,created_at,usage_requested_at)
		VALUES ($1,'usage_charge','information-orphan-late-ledger',$2,$3),
		($1,'usage_charge','information-orphan-boundary-ledger',$3,$4)`, user.ID, now, old, cutoff); err != nil {
		t.Fatal(err)
	}
	// The exact cutoff is retained; its daily summary must survive even when
	// ordinary request retention has removed its raw detail.
	boundaryID := "information-boundary-request"
	billingIntegrationComplete(t, ctx, s, user, device, key, boundaryID, cutoff, 321, "billing-priced-model")
	for _, day := range []time.Time{old, cutoff} {
		if err := s.AggregateUsageDay(ctx, day, "UTC"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AggregateUsageMonth(ctx, cutoff, "UTC"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteUsageRequestsBefore(ctx, now.AddDate(0, 0, -90), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAuditEvent(ctx, AppendAuditEventParams{OccurredAt: old, ActorUserID: actor.ID,
		EventType: "identity.security_test", Severity: "warning", Success: false}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewInformationCleanup(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if preview.DeleteCounts["billing_reservations"] != 1 || preview.DeleteCounts["billing_charge_allocations"] != 1 || preview.RetainedReasons["unsettled_billing"] != 1 || preview.RetainedReasons["available_cash"] != 1 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	for _, query := range []string{
		`DELETE FROM billing_ledger_entries WHERE id=` + strconv.FormatInt(entry.ID, 10),
		`UPDATE billing_ledger_entries SET reason='altered' WHERE id=` + strconv.FormatInt(entry.ID, 10),
	} {
		if _, err := s.db.ExecContext(ctx, query); err == nil {
			t.Fatalf("unguarded mutation accepted: %s", query)
		}
	}
	operationID, _ := newUUID()
	jobParams := InformationCleanupParams{OperationID: operationID, ActorUserID: actor.ID, RetentionDays: retentionDays, Cutoff: cutoff}
	job, err := s.CreateInformationCleanupJob(ctx, jobParams)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := s.CreateInformationCleanupJob(ctx, jobParams); err != nil || replay.ID != job.ID {
		t.Fatalf("cleanup replay = %+v, %v", replay, err)
	}
	otherID, _ := newUUID()
	jobParams.OperationID = otherID
	if _, err := s.CreateInformationCleanupJob(ctx, jobParams); !errors.Is(err, ErrConflict) {
		t.Fatalf("parallel cleanup job error = %v", err)
	}
	// A fresh Store object simulates process restart between committed batches.
	for batch := 0; batch < 30 && job.Status != "completed"; batch++ {
		runner := New(s.db)
		runner.now = s.now
		value, err := runner.RunInformationCleanupBatch(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if value == nil {
			t.Fatal("durable job disappeared")
		}
		job = *value
	}
	if job.Status != "completed" || job.Report.DeleteCounts["billing_reservations"] != 1 {
		t.Fatalf("cleanup did not finish correctly: %+v", job)
	}
	if _, err := s.RechargeUser(ctx, params); !errors.Is(err, ErrBillingOperationCleaned) {
		t.Fatalf("cleaned recharge replay = %v", err)
	}
	for _, check := range []struct {
		query string
		want  int64
	}{
		{`SELECT count(*) FROM billing_operation_tombstones WHERE operation_id='` + write.OperationID + `'`, 1},
		{`SELECT count(*) FROM billing_ledger_entries WHERE id=` + strconv.FormatInt(activeEntry.ID, 10), 1},
		{`SELECT count(*) FROM usage_requests WHERE request_id='` + activeID + `'`, 1},
		{`SELECT count(*) FROM audit_events WHERE event_type='identity.security_test'`, 1},
		{`SELECT count(*) FROM information_cleanup_authorizations`, 0},
		{`SELECT count(*) FROM billing_ledger_entries WHERE request_id='information-orphan-late-ledger'`, 0},
		{`SELECT count(*) FROM billing_ledger_entries WHERE request_id='information-orphan-boundary-ledger'`, 1},
		{`SELECT count(*) FROM usage_daily WHERE usage_day < '` + cutoff.Format("2006-01-02") + `'`, 0},
	} {
		var got int64
		if err := s.db.QueryRowContext(ctx, check.query).Scan(&got); err != nil || got != check.want {
			t.Fatalf("%s = %d, %v; want %d", check.query, got, err, check.want)
		}
	}
	var userBalance, activeBalance string
	if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, user.ID).Scan(&userBalance); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT balance_usd::text FROM billing_accounts WHERE user_id=$1`, active.ID).Scan(&activeBalance); err != nil {
		t.Fatal(err)
	}
	if userBalance != "0.000000000000" || activeBalance != "10.000000000000" {
		t.Fatalf("balances changed: zero=%s active=%s", userBalance, activeBalance)
	}
	assertMonthly := func() {
		t.Helper()
		var count, tokens int64
		var p95 sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT request_count,input_tokens,p95_duration_ms FROM usage_monthly WHERE user_id=$1 AND usage_month=$2`, user.ID, monthBucket(cutoff)).Scan(&count, &tokens, &p95); err != nil {
			t.Fatal(err)
		}
		if count != 1 || tokens != 321 || p95.Valid {
			t.Fatalf("retained monthly metrics = %d/%d/%v", count, tokens, p95)
		}
	}
	assertMonthly()
	if err := s.AggregateUsageMonth(ctx, cutoff, "UTC"); err != nil {
		t.Fatal(err)
	}
	assertMonthly()
	if err := s.AggregateUsageDay(ctx, old, "UTC"); err != nil {
		t.Fatal(err)
	}
	if boundary, err := s.InformationCleanedBefore(ctx); err != nil || boundary == nil || !boundary.Equal(cutoff) {
		t.Fatalf("cleaned boundary = %v, %v", boundary, err)
	}
	if _, err := s.db.ExecContext(ctx, `SELECT 1 FROM billing_accounts WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(fmt.Errorf("current empty account remains: %w", err))
	}
}
