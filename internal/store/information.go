package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// InformationMaintenanceLock serializes explicit cleanup, aggregation, automatic
// retention, and user deletion across every gateway instance.
const InformationMaintenanceLock int64 = 72770313

var ErrBillingOperationCleaned = fmt.Errorf("billing operation history was cleaned; operation ID cannot be reused: %w", ErrConflict)

type InformationCleanupReport struct {
	Cutoff          time.Time        `json:"cutoff"`
	DeleteCounts    map[string]int64 `json:"delete_counts"`
	RetainedCounts  map[string]int64 `json:"retained_counts"`
	RetainedReasons map[string]int64 `json:"retained_reasons"`
}

type InformationCleanupParams struct {
	OperationID, ActorUserID, ActorSessionID, SourceIP, RequestID string
	RetentionDays                                                 int
	Cutoff                                                        time.Time
}

type InformationCleanupJob struct {
	ID            string                   `json:"id"`
	RetentionDays int                      `json:"retention_days"`
	Cutoff        time.Time                `json:"cutoff"`
	Status        string                   `json:"status"`
	CreatedAt     time.Time                `json:"created_at"`
	UpdatedAt     time.Time                `json:"updated_at"`
	CompletedAt   *time.Time               `json:"completed_at"`
	Report        InformationCleanupReport `json:"report"`
	Error         string                   `json:"error"`
}

// InformationCutoff deliberately uses calendar arithmetic, avoiding duration
// overflow and local daylight-saving transitions.
func InformationCutoff(now time.Time, retentionDays int) (time.Time, error) {
	if retentionDays <= 0 || retentionDays > 2_147_483_647 {
		return time.Time{}, fmt.Errorf("%w: retention days must be a positive integer", ErrInvalid)
	}
	now = now.UTC()
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -retentionDays)
	if cutoff.Year() < 1 {
		return time.Time{}, fmt.Errorf("%w: retention days exceed supported calendar range", ErrInvalid)
	}
	return cutoff, nil
}

func lockInformationMaintenanceTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, InformationMaintenanceLock)
	return mapDBError("lock information maintenance", err)
}

func newInformationReport(cutoff time.Time) InformationCleanupReport {
	return InformationCleanupReport{Cutoff: cutoff, DeleteCounts: map[string]int64{}, RetainedCounts: map[string]int64{}, RetainedReasons: map[string]int64{}}
}

const informationJobColumns = `id, retention_days, cutoff, status, created_at, updated_at, completed_at, report, last_error`

func scanInformationJob(row rowScanner) (InformationCleanupJob, error) {
	var job InformationCleanupJob
	var report []byte
	err := row.Scan(&job.ID, &job.RetentionDays, &job.Cutoff, &job.Status, &job.CreatedAt, &job.UpdatedAt, &job.CompletedAt, &report, &job.Error)
	if err != nil {
		return job, mapDBError("read information cleanup job", err)
	}
	job.Report = newInformationReport(job.Cutoff)
	if err := json.Unmarshal(report, &job.Report); err != nil {
		return job, fmt.Errorf("decode information cleanup report: %w", err)
	}
	return job, nil
}

func (s *Store) GetInformationCleanupJob(ctx context.Context, id string) (InformationCleanupJob, error) {
	return scanInformationJob(s.db.QueryRowContext(ctx, `SELECT `+informationJobColumns+` FROM information_cleanup_jobs WHERE id=$1`, id))
}

func (s *Store) LatestInformationCleanupJob(ctx context.Context) (InformationCleanupJob, error) {
	return scanInformationJob(s.db.QueryRowContext(ctx, `SELECT `+informationJobColumns+` FROM information_cleanup_jobs ORDER BY created_at DESC,id DESC LIMIT 1`))
}

func (s *Store) InformationCleanedBefore(ctx context.Context) (*time.Time, error) {
	var cutoff *time.Time
	err := s.db.QueryRowContext(ctx, `SELECT max(cutoff) FROM information_cleanup_jobs WHERE status='completed'
		OR EXISTS (SELECT 1 FROM jsonb_each_text(report->'delete_counts') c WHERE c.value::bigint>0)`).Scan(&cutoff)
	return cutoff, mapDBError("read information retention boundary", err)
}

func (s *Store) CreateInformationCleanupJob(ctx context.Context, params InformationCleanupParams) (InformationCleanupJob, error) {
	var job InformationCleanupJob
	if params.OperationID == "" || params.ActorUserID == "" {
		return job, fmt.Errorf("%w: cleanup operation and actor are required", ErrInvalid)
	}
	expected, err := InformationCutoff(s.now(), params.RetentionDays)
	if err != nil {
		return job, err
	}
	// A preview from an earlier UTC day remains safe to confirm; a later or
	// non-midnight cutoff cannot silently remove more than the chosen retention.
	if params.Cutoff.IsZero() || params.Cutoff.After(expected) || !params.Cutoff.Equal(params.Cutoff.UTC().Truncate(24*time.Hour)) {
		return job, fmt.Errorf("%w: cleanup cutoff must be a confirmed UTC midnight", ErrInvalid)
	}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockInformationMaintenanceTx(ctx, tx); err != nil {
			return err
		}
		var actor string
		existing, err := scanInformationJob(tx.QueryRowContext(ctx, `SELECT `+informationJobColumns+` FROM information_cleanup_jobs WHERE id=$1`, params.OperationID))
		if err == nil {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(actor_user_id::text,'') FROM information_cleanup_jobs WHERE id=$1`, params.OperationID).Scan(&actor); err != nil {
				return err
			}
			if actor != params.ActorUserID || existing.RetentionDays != params.RetentionDays || !existing.Cutoff.Equal(params.Cutoff) {
				return fmt.Errorf("cleanup operation replay mismatch: %w", ErrConflict)
			}
			job = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		report, _ := json.Marshal(newInformationReport(params.Cutoff))
		job, err = scanInformationJob(tx.QueryRowContext(ctx, `INSERT INTO information_cleanup_jobs
			(id,actor_user_id,retention_days,cutoff,report) VALUES ($1,$2,$3,$4,$5)
			RETURNING `+informationJobColumns, params.OperationID, params.ActorUserID, params.RetentionDays, params.Cutoff, report))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(actor_user_id,actor_session_id,event_type,source_ip,subject_type,subject_id,request_id,metadata)
			VALUES ($1,$2,'information.cleanup_created',$3::inet,'information_cleanup',$4,$5,
			jsonb_build_object('retention_days',$6::integer,'cutoff',$7::timestamptz))`,
			params.ActorUserID, valueOrNil(params.ActorSessionID), valueOrNil(params.SourceIP), params.OperationID,
			valueOrNil(params.RequestID), params.RetentionDays, params.Cutoff)
		return mapDBError("audit information cleanup", err)
	})
	return job, err
}

func validateInformationCutoff(cutoff time.Time) error {
	if cutoff.IsZero() || !cutoff.Equal(cutoff.UTC().Truncate(24*time.Hour)) {
		return fmt.Errorf("%w: cutoff must be UTC midnight", ErrInvalid)
	}
	return nil
}

func (s *Store) PreviewInformationCleanup(ctx context.Context, cutoff time.Time) (InformationCleanupReport, error) {
	report := newInformationReport(cutoff)
	if err := validateInformationCutoff(cutoff); err != nil {
		return report, err
	}
	err := s.withTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(tx *sql.Tx) error {
		if err := lockInformationMaintenanceTx(ctx, tx); err != nil {
			return err
		}
		if err := prepareInformationCandidates(ctx, tx, cutoff, 2_147_483_647); err != nil {
			return err
		}
		return informationReportTx(ctx, tx, cutoff, &report)
	})
	return report, err
}

// All request-related retention uses admission time. If automatic request
// retention already removed the detail, the reservation is the durable
// admission-time record. A missing attribution is never synthesized.
const informationRequestCandidates = `
	WITH ids AS (
		SELECT request_id FROM usage_requests WHERE requested_at < $1
		UNION SELECT request_id FROM billing_reservations WHERE created_at < $1
		UNION SELECT request_id FROM quota_reservations WHERE created_at < $1
		UNION SELECT request_id FROM billing_ledger_entries WHERE request_id IS NOT NULL AND COALESCE(usage_requested_at,created_at) < $1
	)
	SELECT i.request_id FROM ids i
	LEFT JOIN usage_requests u USING(request_id)
	LEFT JOIN billing_reservations b USING(request_id)
	LEFT JOIN quota_reservations q USING(request_id)
	WHERE COALESCE(u.requested_at,(SELECT min(l.usage_requested_at) FROM billing_ledger_entries l WHERE l.request_id=i.request_id),b.created_at,q.created_at,
		(SELECT min(COALESCE(l.usage_requested_at,l.created_at)) FROM billing_ledger_entries l WHERE l.request_id=i.request_id)) < $1
	  AND (u.request_id IS NULL OR (u.state <> 'in_progress' AND u.completed_at IS NOT NULL))
	  AND (b.request_id IS NULL OR b.state IN ('settled','released'))
	  AND (q.request_id IS NULL OR q.state IN ('settled','released'))
	  AND NOT EXISTS (SELECT 1 FROM concurrency_leases c WHERE c.request_id=i.request_id AND c.lease_expires_at > now())
	ORDER BY i.request_id LIMIT $2`

func prepareInformationCandidates(ctx context.Context, tx *sql.Tx, cutoff time.Time, limit int) error {
	statements := []string{
		`CREATE TEMP TABLE info_requests ON COMMIT DROP AS ` + informationRequestCandidates,
		`CREATE UNIQUE INDEX ON info_requests(request_id)`,
		`CREATE TEMP TABLE info_lots ON COMMIT DROP AS SELECT l.id FROM billing_cash_credit_lots l
		 WHERE l.created_at < $1 AND l.remaining_usd=0
		 AND NOT EXISTS (SELECT 1 FROM billing_charge_allocations a WHERE a.cash_credit_lot_id=l.id AND NOT EXISTS (SELECT 1 FROM info_requests r WHERE r.request_id=a.request_id))
		 AND NOT EXISTS (SELECT 1 FROM billing_reservations b WHERE b.user_id=l.user_id AND b.state='reserved' AND b.cash_lot_cutoff >= l.lot_sequence)
		 ORDER BY l.id LIMIT $2`,
		`CREATE TEMP TABLE info_ledger ON COMMIT DROP AS SELECT l.id FROM billing_ledger_entries l
		 WHERE ((l.request_id IS NULL AND l.created_at < $1) OR EXISTS (SELECT 1 FROM info_requests r WHERE r.request_id=l.request_id))
		 AND NOT EXISTS (SELECT 1 FROM billing_cash_credit_lots c WHERE c.source_ledger_entry_id=l.id AND NOT EXISTS (SELECT 1 FROM info_lots x WHERE x.id=c.id))
		 AND (l.entry_type='usage_charge' OR NOT EXISTS (SELECT 1 FROM billing_subscriptions s WHERE s.enabled AND s.current_period_id=l.subscription_period_id))
		 AND NOT EXISTS (SELECT 1 FROM billing_subscription_operation_snapshots o JOIN billing_subscriptions s ON s.id=o.subscription_id WHERE o.ledger_entry_id=l.id AND s.enabled)
		 ORDER BY (l.request_id IS NOT NULL) DESC,l.id LIMIT ($2::bigint + (SELECT count(*) FROM billing_ledger_entries WHERE request_id IN (SELECT request_id FROM info_requests)))`,
		`CREATE TEMP TABLE info_periods ON COMMIT DROP AS SELECT p.id FROM billing_subscription_periods p
		 WHERE p.ends_at < $1 AND p.closed_at IS NOT NULL
		 AND NOT EXISTS (SELECT 1 FROM billing_subscriptions s WHERE s.enabled AND s.current_period_id=p.id)
		 AND NOT EXISTS (SELECT 1 FROM billing_reservations b WHERE p.id IN (b.day_period_id,b.week_period_id,b.month_period_id) AND NOT EXISTS (SELECT 1 FROM info_requests r WHERE r.request_id=b.request_id))
		 AND NOT EXISTS (SELECT 1 FROM billing_charge_allocations a WHERE a.subscription_period_id=p.id AND NOT EXISTS (SELECT 1 FROM info_requests r WHERE r.request_id=a.request_id))
		 AND NOT EXISTS (SELECT 1 FROM billing_ledger_entries l WHERE l.subscription_period_id=p.id AND NOT EXISTS (SELECT 1 FROM info_ledger x WHERE x.id=l.id))
		 ORDER BY p.id LIMIT $2`,
		`CREATE TEMP TABLE info_subscriptions ON COMMIT DROP AS SELECT s.id FROM billing_subscriptions s
		 WHERE NOT s.enabled AND s.disabled_at < $1
		 AND NOT EXISTS (SELECT 1 FROM billing_subscription_periods p WHERE p.subscription_id=s.id AND NOT EXISTS (SELECT 1 FROM info_periods x WHERE x.id=p.id))
		 AND NOT EXISTS (SELECT 1 FROM billing_subscription_operation_snapshots o WHERE o.subscription_id=s.id AND NOT EXISTS (SELECT 1 FROM info_ledger x WHERE x.id=o.ledger_entry_id))
		 ORDER BY s.id LIMIT $2`,
	}
	for _, statement := range statements {
		var err error
		if statement == statements[1] {
			_, err = tx.ExecContext(ctx, statement)
		} else {
			_, err = tx.ExecContext(ctx, statement, cutoff, limit)
		}
		if err != nil {
			return mapDBError("prepare information cleanup candidates", err)
		}
	}
	return nil
}

var informationCandidateQueries = map[string]string{
	"usage_requests":                           `SELECT count(*) FROM usage_requests WHERE request_id IN (SELECT request_id FROM info_requests)`,
	"billing_reservations":                     `SELECT count(*) FROM billing_reservations WHERE request_id IN (SELECT request_id FROM info_requests)`,
	"quota_reservations":                       `SELECT count(*) FROM quota_reservations WHERE request_id IN (SELECT request_id FROM info_requests)`,
	"billing_charge_allocations":               `SELECT count(*) FROM billing_charge_allocations WHERE request_id IN (SELECT request_id FROM info_requests)`,
	"billing_ledger_entries":                   `SELECT count(*) FROM info_ledger`,
	"billing_operations":                       `SELECT count(*) FROM billing_operations WHERE result_ledger_entry_id IN (SELECT id FROM info_ledger)`,
	"billing_subscription_operation_snapshots": `SELECT count(*) FROM billing_subscription_operation_snapshots WHERE ledger_entry_id IN (SELECT id FROM info_ledger)`,
	"billing_cash_credit_lots":                 `SELECT count(*) FROM info_lots`,
	"billing_subscription_periods":             `SELECT count(*) FROM info_periods`,
	"billing_subscriptions":                    `SELECT count(*) FROM info_subscriptions`,
}

func informationReportTx(ctx context.Context, tx *sql.Tx, cutoff time.Time, report *InformationCleanupReport) error {
	for table, query := range informationCandidateQueries {
		var count int64
		if err := tx.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return mapDBError("count information cleanup candidates", err)
		}
		report.DeleteCounts[table] = count
	}
	for table, query := range map[string]string{
		"usage_daily":   `SELECT count(*) FROM usage_daily WHERE usage_day < $1::date`,
		"usage_monthly": `SELECT count(*) FROM usage_monthly WHERE usage_month < date_trunc('month',$1::timestamptz AT TIME ZONE 'UTC')::date`,
		"audit_events":  `SELECT count(*) FROM audit_events a WHERE a.occurred_at < $1 AND a.severity='info' AND a.success AND a.event_type IN ('billing.rate_updated','billing.recharged','billing.adjusted','billing.subscription_updated','billing.subscription_disabled') AND EXISTS (SELECT 1 FROM billing_operations o JOIN info_ledger l ON l.id=o.result_ledger_entry_id WHERE a.metadata->>'operation_id'=o.operation_id::text)`,
	} {
		var count int64
		if err := tx.QueryRowContext(ctx, query, cutoff).Scan(&count); err != nil {
			return mapDBError("count information cleanup summaries", err)
		}
		report.DeleteCounts[table] = count
	}
	for table, query := range map[string]string{
		"usage_requests":                           `SELECT count(*) FROM usage_requests WHERE requested_at < $1 AND request_id NOT IN (SELECT request_id FROM info_requests)`,
		"billing_reservations":                     `SELECT count(*) FROM billing_reservations WHERE created_at < $1 AND request_id NOT IN (SELECT request_id FROM info_requests)`,
		"quota_reservations":                       `SELECT count(*) FROM quota_reservations WHERE created_at < $1 AND request_id NOT IN (SELECT request_id FROM info_requests)`,
		"billing_ledger_entries":                   `SELECT count(*) FROM billing_ledger_entries WHERE created_at < $1 AND id NOT IN (SELECT id FROM info_ledger)`,
		"billing_cash_credit_lots":                 `SELECT count(*) FROM billing_cash_credit_lots WHERE created_at < $1 AND id NOT IN (SELECT id FROM info_lots)`,
		"billing_subscription_periods":             `SELECT count(*) FROM billing_subscription_periods WHERE starts_at < $1 AND id NOT IN (SELECT id FROM info_periods)`,
		"billing_subscriptions":                    `SELECT count(*) FROM billing_subscriptions WHERE created_at < $1 AND id NOT IN (SELECT id FROM info_subscriptions)`,
		"billing_operations":                       `SELECT count(*) FROM billing_operations WHERE created_at < $1 AND (result_ledger_entry_id IS NULL OR result_ledger_entry_id NOT IN (SELECT id FROM info_ledger))`,
		"billing_subscription_operation_snapshots": `SELECT count(*) FROM billing_subscription_operation_snapshots o JOIN billing_ledger_entries l ON l.id=o.ledger_entry_id WHERE l.created_at < $1 AND l.id NOT IN (SELECT id FROM info_ledger)`,
		"billing_charge_allocations":               `SELECT count(*) FROM billing_charge_allocations a JOIN billing_reservations b USING(request_id) WHERE b.created_at < $1 AND a.request_id NOT IN (SELECT request_id FROM info_requests)`,
		"audit_events":                             `SELECT count(*) FROM audit_events a WHERE a.occurred_at < $1 AND NOT (a.severity='info' AND a.success AND a.event_type IN ('billing.rate_updated','billing.recharged','billing.adjusted','billing.subscription_updated','billing.subscription_disabled') AND EXISTS (SELECT 1 FROM billing_operations o JOIN info_ledger l ON l.id=o.result_ledger_entry_id WHERE a.metadata->>'operation_id'=o.operation_id::text))`,
	} {
		var count int64
		if err := tx.QueryRowContext(ctx, query, cutoff).Scan(&count); err != nil {
			return mapDBError("count retained information", err)
		}
		report.RetainedCounts[table] = count
	}
	for reason, query := range map[string]string{
		"running_requests":     `SELECT count(*) FROM usage_requests WHERE requested_at < $1 AND state='in_progress'`,
		"unsettled_billing":    `SELECT count(*) FROM billing_reservations WHERE created_at < $1 AND state='reserved'`,
		"unsettled_quota":      `SELECT count(*) FROM quota_reservations WHERE created_at < $1 AND state='reserved'`,
		"available_cash":       `SELECT count(*) FROM billing_cash_credit_lots WHERE created_at < $1 AND remaining_usd > 0`,
		"active_subscriptions": `SELECT count(*) FROM billing_subscriptions WHERE created_at < $1 AND enabled`,
		"referenced_history":   `SELECT count(*) FROM billing_ledger_entries WHERE created_at < $1 AND id NOT IN (SELECT id FROM info_ledger)`,
	} {
		var count int64
		if err := tx.QueryRowContext(ctx, query, cutoff).Scan(&count); err != nil {
			return mapDBError("count information retention reasons", err)
		}
		report.RetainedReasons[reason] = count
	}
	return nil
}
