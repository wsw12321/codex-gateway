package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RunInformationCleanupBatch commits both deletions and progress together.
// An interrupted batch rolls back; another process can immediately resume it.
// Failures remain retryable rather than dropping the durable job.
func (s *Store) RunInformationCleanupBatch(ctx context.Context, limit int) (*InformationCleanupJob, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 500
	}
	var job *InformationCleanupJob
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockInformationMaintenanceTx(ctx, tx); err != nil {
			return err
		}
		value, err := scanInformationJob(tx.QueryRowContext(ctx, `SELECT `+informationJobColumns+`
			FROM information_cleanup_jobs WHERE status IN ('pending','running') ORDER BY created_at LIMIT 1 FOR UPDATE`))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		job = &value
		if _, err := tx.ExecContext(ctx, `UPDATE information_cleanup_jobs SET status='running',updated_at=now(),last_error='' WHERE id=$1`, job.ID); err != nil {
			return err
		}
		job.Status = "running"
		// Admission/settlement take the global quota lock before account locks.
		// Holding that same first lock prevents a terminal request from racing its
		// quota settlement while we check the complete dependency graph.
		if _, err := tx.ExecContext(ctx, `INSERT INTO quota_locks(scope_type,scope_id) VALUES ('global','global') ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM quota_locks WHERE scope_type='global' AND scope_id='global' FOR UPDATE`); err != nil {
			return err
		}
		// All monetary writers lock accounts before sources. The batch only
		// removes historical sources and never recomputes current balances.
		if _, err := tx.ExecContext(ctx, `SELECT user_id FROM billing_accounts ORDER BY user_id FOR UPDATE`); err != nil {
			return err
		}
		if err := prepareInformationCandidates(ctx, tx, job.Cutoff, limit); err != nil {
			return err
		}
		var candidateCount int64
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM info_requests)+(SELECT count(*) FROM info_lots)+
			(SELECT count(*) FROM info_ledger)+(SELECT count(*) FROM info_periods)+(SELECT count(*) FROM info_subscriptions)`).Scan(&candidateCount); err != nil {
			return err
		}
		if candidateCount > 0 {
			if err := deleteInformationCandidates(ctx, tx, job); err != nil {
				return err
			}
		} else {
			finished, err := finishInformationSummaries(ctx, tx, job, limit)
			if err != nil {
				return err
			}
			if finished {
				retained := newInformationReport(job.Cutoff)
				if err := informationReportTx(ctx, tx, job.Cutoff, &retained); err != nil {
					return err
				}
				job.Report.RetainedCounts = retained.RetainedCounts
				job.Report.RetainedReasons = retained.RetainedReasons
				job.Status = "completed"
				at := s.now().UTC()
				job.CompletedAt = &at
				if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(event_type,subject_type,subject_id,metadata)
				VALUES ('information.cleanup_completed','information_cleanup',$1,jsonb_build_object('cutoff',$2::timestamptz))`, job.ID, job.Cutoff); err != nil {
					return mapDBError("audit completed information cleanup", err)
				}
			}
		}
		report, err := json.Marshal(job.Report)
		if err != nil {
			return err
		}
		job.Error = ""
		job.UpdatedAt = s.now().UTC()
		_, err = tx.ExecContext(ctx, `UPDATE information_cleanup_jobs SET status=$2,report=$3,updated_at=$4,completed_at=$5,last_error='' WHERE id=$1`,
			job.ID, job.Status, report, job.UpdatedAt, job.CompletedAt)
		return mapDBError("save information cleanup progress", err)
	})
	if err != nil && job != nil {
		// Never persist raw SQL error strings, credentials, or parameter values
		// in an administrator-visible status response.
		errorCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_, _ = s.db.ExecContext(errorCtx, `UPDATE information_cleanup_jobs SET last_error='cleanup batch failed; the task will retry',updated_at=now() WHERE id=$1 AND status<>'completed'`, job.ID)
	}
	return job, err
}

func informationDelete(ctx context.Context, tx *sql.Tx, report *InformationCleanupReport, table, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return mapDBError("clean information "+table, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count cleaned %s: %w", table, err)
	}
	report.DeleteCounts[table] += count
	return nil
}

func deleteInformationCandidates(ctx context.Context, tx *sql.Tx, job *InformationCleanupJob) error {
	// Billing operation IDs are serialized independently of the mutable
	// operation row, so deleting that row cannot permit an in-flight replay.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('billing.operation.'||operation_id::text,0))
		FROM billing_operations WHERE result_ledger_entry_id IN (SELECT id FROM info_ledger) ORDER BY operation_id`); err != nil {
		return mapDBError("lock cleaned billing operation IDs", err)
	}
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS billing_operations_result_ledger_fk,billing_ledger_entries_operation_id_fkey DEFERRED`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO information_cleanup_authorizations(transaction_id,job_id,table_name,row_id)
		SELECT txid_current(),$1::uuid,'billing_ledger_entries',id FROM info_ledger
		UNION ALL SELECT txid_current(),$1::uuid,'billing_subscription_operation_snapshots',ledger_entry_id
		FROM billing_subscription_operation_snapshots WHERE ledger_entry_id IN (SELECT id FROM info_ledger)`, job.ID); err != nil {
		return mapDBError("authorize selected information deletion", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_operation_tombstones(operation_id)
		SELECT operation_id FROM billing_operations WHERE result_ledger_entry_id IN (SELECT id FROM info_ledger) ON CONFLICT DO NOTHING`); err != nil {
		return mapDBError("retain cleaned operation IDs", err)
	}
	if err := informationDelete(ctx, tx, &job.Report, "audit_events", `DELETE FROM audit_events a
		WHERE a.occurred_at < $1 AND a.severity='info' AND a.success
		AND a.event_type IN ('billing.rate_updated','billing.recharged','billing.adjusted','billing.subscription_updated','billing.subscription_disabled')
		AND EXISTS (SELECT 1 FROM billing_operations o JOIN info_ledger l ON l.id=o.result_ledger_entry_id WHERE a.metadata->>'operation_id'=o.operation_id::text)`, job.Cutoff); err != nil {
		return err
	}
	for _, statement := range []struct{ table, query string }{
		{"billing_charge_allocations", `DELETE FROM billing_charge_allocations WHERE request_id IN (SELECT request_id FROM info_requests)`},
		{"billing_reservations", `DELETE FROM billing_reservations WHERE request_id IN (SELECT request_id FROM info_requests)`},
		{"quota_reservations", `DELETE FROM quota_reservations WHERE request_id IN (SELECT request_id FROM info_requests)`},
		{"concurrency_leases", `DELETE FROM concurrency_leases WHERE request_id IN (SELECT request_id FROM info_requests)`},
		{"billing_cash_credit_lots", `DELETE FROM billing_cash_credit_lots WHERE id IN (SELECT id FROM info_lots)`},
		{"billing_subscription_operation_snapshots", `DELETE FROM billing_subscription_operation_snapshots WHERE ledger_entry_id IN (SELECT id FROM info_ledger)`},
		{"billing_operations", `DELETE FROM billing_operations WHERE result_ledger_entry_id IN (SELECT id FROM info_ledger)`},
		{"billing_ledger_entries", `DELETE FROM billing_ledger_entries WHERE id IN (SELECT id FROM info_ledger)`},
		{"usage_requests", `DELETE FROM usage_requests WHERE request_id IN (SELECT request_id FROM info_requests)`},
	} {
		if err := informationDelete(ctx, tx, &job.Report, statement.table, statement.query); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET current_period_id=NULL
		WHERE NOT enabled AND current_period_id IN (SELECT id FROM info_periods)`); err != nil {
		return mapDBError("detach ended subscription history", err)
	}
	if err := informationDelete(ctx, tx, &job.Report, "billing_subscription_periods", `DELETE FROM billing_subscription_periods WHERE id IN (SELECT id FROM info_periods)`); err != nil {
		return err
	}
	if err := informationDelete(ctx, tx, &job.Report, "billing_subscriptions", `DELETE FROM billing_subscriptions WHERE id IN (SELECT id FROM info_subscriptions)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM information_cleanup_authorizations WHERE transaction_id=txid_current()`)
	return mapDBError("close information deletion authorization", err)
}

func finishInformationSummaries(ctx context.Context, tx *sql.Tx, job *InformationCleanupJob, limit int) (bool, error) {
	previous := job.Report.DeleteCounts["usage_daily"] + job.Report.DeleteCounts["usage_monthly"]
	if err := informationDelete(ctx, tx, &job.Report, "usage_daily", `DELETE FROM usage_daily WHERE id IN
		(SELECT id FROM usage_daily WHERE usage_day < $1::date ORDER BY usage_day,id LIMIT $2)`, job.Cutoff, limit); err != nil {
		return false, err
	}
	if err := informationDelete(ctx, tx, &job.Report, "usage_monthly", `DELETE FROM usage_monthly WHERE ctid IN
		(SELECT ctid FROM usage_monthly WHERE usage_month < date_trunc('month',$1::timestamptz AT TIME ZONE 'UTC')::date ORDER BY usage_month LIMIT $2)`, job.Cutoff, limit); err != nil {
		return false, err
	}
	if job.Report.DeleteCounts["usage_daily"]+job.Report.DeleteCounts["usage_monthly"] > previous {
		return false, nil
	}
	return true, rebuildInformationMonthTx(ctx, tx, job.Cutoff, "UTC")
}

// Rebuild additive metrics from retained daily rows. Days lacking a summary
// are supplied by detail rows, so a cleanup of the current month remains
// complete. Daily p95 values cannot be combined into a monthly percentile.
func rebuildInformationMonthTx(ctx context.Context, tx *sql.Tx, cutoff time.Time, timezone string) error {
	month := monthBucket(cutoff)
	// Persist the chosen daily population in the same transaction. Historical
	// daily totals can outlive their raw rows; late completions must therefore
	// become part of that durable population before their own detail expires.
	if err := persistInformationMonthDaysTx(ctx, tx, cutoff, timezone); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_monthly WHERE usage_month=$1::date`, month); err != nil {
		return mapDBError("clear retained monthly usage", err)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_monthly (
		usage_month,user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,status_class,error_code,
		request_count,error_count,input_tokens,cached_input_tokens,cache_write_tokens,output_tokens,reasoning_tokens,
		request_bytes,response_bytes,p95_ttft_ms,p95_duration_ms)
		SELECT $1::date,user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,status_class,error_code,
		sum(request_count),sum(error_count),sum(input_tokens),sum(cached_input_tokens),sum(cache_write_tokens),sum(output_tokens),sum(reasoning_tokens),
		sum(request_bytes),sum(response_bytes),NULL,NULL
		FROM usage_daily WHERE usage_day >= $2::date AND usage_day >= $1::date AND usage_day < ($1::date+interval '1 month')::date
		GROUP BY user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,status_class,error_code`, month, cutoff)
	if err != nil {
		return mapDBError("rebuild retained monthly usage", err)
	}
	// Exact p95 is available only for dimensions whose complete retained
	// request population is still present. Never substitute an average p95.
	_, err = tx.ExecContext(ctx, `WITH exact AS (
		SELECT user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,
		COALESCE(http_status/100,0)::smallint status_class,error_code,count(*) n,
		ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms))::bigint ttft,
		ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms))::bigint duration
		FROM usage_requests WHERE state<>'in_progress' AND completed_at IS NOT NULL AND requested_at >= $2
		AND date_trunc('month',requested_at AT TIME ZONE $3)::date=$1::date
		GROUP BY user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,COALESCE(http_status/100,0)::smallint,error_code)
		UPDATE usage_monthly m SET p95_ttft_ms=e.ttft,p95_duration_ms=e.duration FROM exact e
		WHERE m.usage_month=$1::date AND m.request_count=e.n AND m.user_id=e.user_id AND m.device_id=e.device_id AND m.api_key_id=e.api_key_id
		AND m.project_id IS NOT DISTINCT FROM e.project_id AND m.upstream_account_id IS NOT DISTINCT FROM e.upstream_account_id
		AND m.model=e.model AND m.endpoint=e.endpoint AND m.status_class=e.status_class AND m.error_code IS NOT DISTINCT FROM e.error_code`, month, cutoff, timezone)
	return mapDBError("restore exact retained monthly percentiles", err)
}

// Existing daily rows plus observed later completions and complete raw daily
// populations compete within each full dimension tuple. Choosing the largest
// population preserves already-pruned history; equal complete raw populations
// also permit exact daily percentiles. The event-time watermark only advances
// over rows present in this statement's snapshot.
func persistInformationMonthDaysTx(ctx context.Context, tx *sql.Tx, cutoff time.Time, timezone string) error {
	_, err := tx.ExecContext(ctx, `WITH raw AS (
		SELECT u.*,d.id snapshot_id,d.updated_at snapshot_at FROM usage_requests u LEFT JOIN usage_daily d
		ON d.usage_day=(u.requested_at AT TIME ZONE $3)::date AND u.user_id=d.user_id AND u.device_id=d.device_id AND u.api_key_id=d.api_key_id
		AND u.project_id IS NOT DISTINCT FROM d.project_id AND u.upstream_account_id IS NOT DISTINCT FROM d.upstream_account_id
		AND u.model=d.model AND u.endpoint=d.endpoint AND COALESCE(u.http_status/100,0)::smallint=d.status_class
		AND u.error_code IS NOT DISTINCT FROM d.error_code
		WHERE u.state<>'in_progress' AND u.completed_at IS NOT NULL AND u.requested_at >= $2
		AND date_trunc('month',u.requested_at AT TIME ZONE $3)::date=$1::date
	), detail AS (
		SELECT (requested_at AT TIME ZONE $3)::date usage_day,user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,
		COALESCE(http_status/100,0)::smallint status_class,error_code,count(*)::bigint request_count,
		count(*) FILTER (WHERE state IN ('failed', 'cancelled') OR http_status>=400 OR error_code IS NOT NULL)::bigint error_count,
		sum(input_tokens)::bigint input_tokens,sum(cached_input_tokens)::bigint cached_input_tokens,
		sum(cache_write_tokens)::bigint cache_write_tokens,sum(output_tokens)::bigint output_tokens,
		sum(reasoning_tokens)::bigint reasoning_tokens,sum(request_bytes)::bigint request_bytes,sum(response_bytes)::bigint response_bytes,
		count(ttft_ms)::bigint ttft_count,COALESCE(sum(ttft_ms),0)::numeric ttft_sum_ms,
		ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms))::bigint p95_ttft_ms,
		count(duration_ms)::bigint duration_count,COALESCE(sum(duration_ms),0)::numeric duration_sum_ms,
		ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms))::bigint p95_duration_ms,
		GREATEST(now(),max(completed_at)) updated_at
		FROM raw GROUP BY (requested_at AT TIME ZONE $3)::date,user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,COALESCE(http_status/100,0)::smallint,error_code
	), late AS (
		SELECT snapshot_id,count(*)::bigint request_count,
		count(*) FILTER (WHERE state IN ('failed', 'cancelled') OR http_status>=400 OR error_code IS NOT NULL)::bigint error_count,
		sum(input_tokens)::bigint input_tokens,sum(cached_input_tokens)::bigint cached_input_tokens,
		sum(cache_write_tokens)::bigint cache_write_tokens,sum(output_tokens)::bigint output_tokens,
		sum(reasoning_tokens)::bigint reasoning_tokens,sum(request_bytes)::bigint request_bytes,sum(response_bytes)::bigint response_bytes,
		count(ttft_ms)::bigint ttft_count,COALESCE(sum(ttft_ms),0)::numeric ttft_sum_ms,
		count(duration_ms)::bigint duration_count,COALESCE(sum(duration_ms),0)::numeric duration_sum_ms,max(completed_at) completed_through
		FROM raw WHERE snapshot_id IS NOT NULL AND completed_at>snapshot_at GROUP BY snapshot_id
	), candidates AS (
		SELECT d.usage_day,d.user_id,d.device_id,d.api_key_id,d.project_id,d.upstream_account_id,d.model,d.endpoint,d.status_class,d.error_code,
		d.request_count+COALESCE(l.request_count,0) request_count,d.error_count+COALESCE(l.error_count,0) error_count,d.input_tokens+COALESCE(l.input_tokens,0) input_tokens,
		d.cached_input_tokens+COALESCE(l.cached_input_tokens,0) cached_input_tokens,d.cache_write_tokens+COALESCE(l.cache_write_tokens,0) cache_write_tokens,
		d.output_tokens+COALESCE(l.output_tokens,0) output_tokens,d.reasoning_tokens+COALESCE(l.reasoning_tokens,0) reasoning_tokens,
		d.request_bytes+COALESCE(l.request_bytes,0) request_bytes,d.response_bytes+COALESCE(l.response_bytes,0) response_bytes,
		d.ttft_count+COALESCE(l.ttft_count,0) ttft_count,d.ttft_sum_ms+COALESCE(l.ttft_sum_ms,0) ttft_sum_ms,
		CASE WHEN l.snapshot_id IS NULL THEN d.p95_ttft_ms ELSE NULL END p95_ttft_ms,
		d.duration_count+COALESCE(l.duration_count,0) duration_count,d.duration_sum_ms+COALESCE(l.duration_sum_ms,0) duration_sum_ms,
		CASE WHEN l.snapshot_id IS NULL THEN d.p95_duration_ms ELSE NULL END p95_duration_ms,
		GREATEST(d.updated_at,l.completed_through) updated_at,0 preference
		FROM usage_daily d LEFT JOIN late l ON l.snapshot_id=d.id
		WHERE d.usage_day >= $2::date AND d.usage_day >= $1::date AND d.usage_day < ($1::date+interval '1 month')::date
		UNION ALL SELECT detail.*,1 preference FROM detail
	), retained AS (
		SELECT *,row_number() OVER (PARTITION BY usage_day,user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,status_class,error_code
		ORDER BY request_count DESC,preference DESC) selected FROM candidates
	) INSERT INTO usage_daily (
		usage_day,user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,status_class,error_code,
		request_count,error_count,input_tokens,cached_input_tokens,cache_write_tokens,output_tokens,reasoning_tokens,request_bytes,response_bytes,
		ttft_count,ttft_sum_ms,p95_ttft_ms,duration_count,duration_sum_ms,p95_duration_ms,updated_at)
		SELECT usage_day,user_id,device_id,api_key_id,project_id,upstream_account_id,model,endpoint,status_class,error_code,
		request_count,error_count,input_tokens,cached_input_tokens,cache_write_tokens,output_tokens,reasoning_tokens,request_bytes,response_bytes,
		ttft_count,ttft_sum_ms,p95_ttft_ms,duration_count,duration_sum_ms,p95_duration_ms,updated_at FROM retained WHERE selected=1
		ON CONFLICT ON CONSTRAINT usage_daily_dimensions_key DO UPDATE SET
		request_count=EXCLUDED.request_count,error_count=EXCLUDED.error_count,input_tokens=EXCLUDED.input_tokens,
		cached_input_tokens=EXCLUDED.cached_input_tokens,cache_write_tokens=EXCLUDED.cache_write_tokens,output_tokens=EXCLUDED.output_tokens,
		reasoning_tokens=EXCLUDED.reasoning_tokens,request_bytes=EXCLUDED.request_bytes,response_bytes=EXCLUDED.response_bytes,
		ttft_count=EXCLUDED.ttft_count,ttft_sum_ms=EXCLUDED.ttft_sum_ms,p95_ttft_ms=EXCLUDED.p95_ttft_ms,
		duration_count=EXCLUDED.duration_count,duration_sum_ms=EXCLUDED.duration_sum_ms,p95_duration_ms=EXCLUDED.p95_duration_ms,updated_at=EXCLUDED.updated_at`,
		monthBucket(cutoff), cutoff, timezone)
	return mapDBError("persist retained daily usage", err)
}
