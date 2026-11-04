package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultUpstreamQuotaGuardPercent = 5
	MaxUpstreamQuotaGuardPercent     = 50

	UpstreamQuotaGuardAvailable    = "available"
	UpstreamQuotaGuardLimited      = "limited"
	UpstreamQuotaGuardSourceAuto   = "auto"
	UpstreamQuotaGuardSourceManual = "manual"
	UpstreamQuotaWindowFiveHour    = "five_hour"
	UpstreamQuotaWindowSevenDay    = "seven_day"

	// Automatic checks of one account are never closer than the minimum
	// interval, including after failures, restarts and across instances.
	UpstreamQuotaGuardMinInterval = 10 * time.Minute
	UpstreamQuotaGuardMaxInterval = 60 * time.Minute
	// A limited account returns this long after its window resets.
	UpstreamQuotaGuardRestoreDelay = 5 * time.Minute
	// A tripped window without a reset time is held this long, then rechecked.
	upstreamQuotaGuardUnknownResetHold = time.Hour
	// Two readings belong to the same window period when their reset times
	// differ by at most this much.
	upstreamQuotaGuardSamePeriod = 2 * time.Minute
)

// upstreamQuotaGuardHeld matches a guard row that keeps the account out of
// allocation at the time bound to the placeholder. Accounts without a row are
// open; a limited row reopens once restore_at passes.
func upstreamQuotaGuardHeld(accountColumn, atExpression string) string {
	return `EXISTS (SELECT 1 FROM upstream_quota_guard_states q WHERE q.account_id=` + accountColumn +
		` AND q.status='limited' AND (q.restore_at IS NULL OR q.restore_at > ` + atExpression + `))`
}

// UpstreamQuotaWindow is one normalized usage window. UsedPercent is 0-100.
type UpstreamQuotaWindow struct {
	UsedPercent int
	ResetAt     *time.Time
}

// UpstreamQuotaObservation is one reading of an account's five-hour and weekly
// windows. A nil window was not reported.
type UpstreamQuotaObservation struct {
	ObservedAt time.Time
	FiveHour   *UpstreamQuotaWindow
	SevenDay   *UpstreamQuotaWindow
}

// UpstreamQuotaGuardState is an account's guard state as of the read time:
// a limited row whose restore time has passed reads as available.
type UpstreamQuotaGuardState struct {
	AccountID     string
	Status        string
	Source        string
	LimitedWindow string
	ResetAt       *time.Time
	RestoreAt     *time.Time
	ChangedAt     *time.Time
	ObservedAt    *time.Time
	FiveHour      *UpstreamQuotaWindow
	SevenDay      *UpstreamQuotaWindow
	AutoCheckedAt *time.Time
	NextCheckAt   *time.Time
}

// NextAutomaticCheck is the earliest time another automatic check may start.
func (s UpstreamQuotaGuardState) NextAutomaticCheck() time.Time {
	var next time.Time
	if s.NextCheckAt != nil {
		next = *s.NextCheckAt
	}
	if s.AutoCheckedAt != nil {
		if floor := s.AutoCheckedAt.Add(UpstreamQuotaGuardMinInterval); floor.After(next) {
			next = floor
		}
	}
	return next
}

type UpstreamQuotaGuardSettings struct {
	SafetyPercent int
	Revision      int64
	UpdatedAt     time.Time
}

// UpstreamQuotaCheckClaim reports whether this caller may run an automatic
// check now. Provider is empty for an account the guard does not cover.
type UpstreamQuotaCheckClaim struct {
	Provider    string
	Claimed     bool
	NextCheckAt time.Time
}

type RecordUpstreamQuotaObservationParams struct {
	Provider    string
	AccountID   string
	Observation UpstreamQuotaObservation
	// Automatic readings may override a manual state. Other readings, such as
	// an Owner or member query, only update the snapshot of a manual state.
	Automatic bool
	At        time.Time
}

type SetUpstreamQuotaGuardStatusParams struct {
	AccountID      string
	Limited        bool
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

type SetUpstreamQuotaGuardSettingsParams struct {
	SafetyPercent  int
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

func guardedUpstreamProvider(provider string) bool {
	return provider == UpstreamProviderCodex || provider == UpstreamProviderAnthropic
}

type upstreamQuotaGuardDecision struct {
	limited     bool
	window      string
	resetAt     *time.Time
	restoreAt   *time.Time
	nextCheckAt time.Time
}

// evaluateUpstreamQuotaGuard decides the guard state for one observation. A
// window trips when its remaining share is at most percent; windows whose reset
// time has passed are ignored. The account stays limited until every tripped
// window has reset. Otherwise the next check moves closer as the smallest
// remaining margin shrinks or usage climbs quickly, within the fixed bounds.
func evaluateUpstreamQuotaGuard(observation UpstreamQuotaObservation, previous *UpstreamQuotaObservation, percent int, now time.Time) upstreamQuotaGuardDecision {
	type window struct {
		name              string
		current, previous *UpstreamQuotaWindow
	}
	windows := []window{
		{name: UpstreamQuotaWindowFiveHour, current: observation.FiveHour},
		{name: UpstreamQuotaWindowSevenDay, current: observation.SevenDay},
	}
	if previous != nil {
		windows[0].previous, windows[1].previous = previous.FiveHour, previous.SevenDay
	}
	var decision upstreamQuotaGuardDecision
	unknownReset := false
	margin := -1
	interval := UpstreamQuotaGuardMaxInterval
	for _, item := range windows {
		current := item.current
		if current == nil || current.ResetAt != nil && !current.ResetAt.After(now) {
			continue
		}
		remaining := 100 - current.UsedPercent
		if remaining <= percent {
			decision.limited = true
			if decision.window == "" {
				decision.window = item.name
			}
			if current.ResetAt == nil {
				unknownReset = true
			} else if decision.resetAt == nil || current.ResetAt.After(*decision.resetAt) {
				reset := current.ResetAt.UTC()
				decision.resetAt, decision.window = &reset, item.name
			}
			continue
		}
		windowMargin := remaining - percent
		if margin < 0 || windowMargin < margin {
			margin = windowMargin
		}
		// Usage that rose within the same period predicts when the margin runs
		// out; check again before half of that time has passed.
		prior := item.previous
		if previous != nil && prior != nil && prior.ResetAt != nil && current.ResetAt != nil &&
			current.UsedPercent > prior.UsedPercent && observation.ObservedAt.After(previous.ObservedAt) {
			if gap := current.ResetAt.Sub(*prior.ResetAt); gap <= upstreamQuotaGuardSamePeriod && gap >= -upstreamQuotaGuardSamePeriod {
				elapsed := observation.ObservedAt.Sub(previous.ObservedAt)
				untilLimit := elapsed * time.Duration(windowMargin) / time.Duration(current.UsedPercent-prior.UsedPercent)
				interval = min(interval, untilLimit/2)
			}
		}
	}
	if decision.limited {
		restore := now.Add(upstreamQuotaGuardUnknownResetHold)
		if decision.resetAt != nil {
			if afterReset := decision.resetAt.Add(UpstreamQuotaGuardRestoreDelay); !unknownReset || afterReset.After(restore) {
				restore = afterReset
			}
		}
		restore = restore.UTC()
		decision.restoreAt, decision.nextCheckAt = &restore, restore
		return decision
	}
	switch {
	case margin < 0:
		// Nothing current was reported; look again as early as allowed.
		interval = UpstreamQuotaGuardMinInterval
	case margin <= 10:
		interval = min(interval, UpstreamQuotaGuardMinInterval)
	case margin <= 20:
		interval = min(interval, 15*time.Minute)
	case margin <= 35:
		interval = min(interval, 30*time.Minute)
	}
	interval = max(UpstreamQuotaGuardMinInterval, min(UpstreamQuotaGuardMaxInterval, interval))
	decision.nextCheckAt = now.Add(interval).UTC()
	return decision
}

// soonestUpstreamQuotaReset returns the earliest future reset among the
// snapshot windows, used when an Owner limits an account by hand.
func soonestUpstreamQuotaReset(state upstreamQuotaGuardRow, now time.Time) (string, *time.Time) {
	var name string
	var soonest *time.Time
	for _, item := range []struct {
		name   string
		window *UpstreamQuotaWindow
	}{{UpstreamQuotaWindowFiveHour, state.fiveHour}, {UpstreamQuotaWindowSevenDay, state.sevenDay}} {
		if item.window == nil || item.window.ResetAt == nil || !item.window.ResetAt.After(now) {
			continue
		}
		if soonest == nil || item.window.ResetAt.Before(*soonest) {
			reset := item.window.ResetAt.UTC()
			name, soonest = item.name, &reset
		}
	}
	return name, soonest
}

func validUpstreamQuotaObservation(observation UpstreamQuotaObservation) bool {
	for _, window := range []*UpstreamQuotaWindow{observation.FiveHour, observation.SevenDay} {
		if window != nil && (window.UsedPercent < 0 || window.UsedPercent > 100) {
			return false
		}
	}
	return true
}

// upstreamQuotaGuardRow is the stored row before applying the read time.
type upstreamQuotaGuardRow struct {
	accountID     string
	status        string
	source        string
	limitedWindow sql.NullString
	resetAt       *time.Time
	restoreAt     *time.Time
	changedAt     *time.Time
	observedAt    *time.Time
	fiveHour      *UpstreamQuotaWindow
	sevenDay      *UpstreamQuotaWindow
	autoCheckedAt *time.Time
	nextCheckAt   *time.Time
}

const upstreamQuotaGuardColumns = `g.account_id, g.status, g.source, g.limited_window, g.reset_at, g.restore_at,
	g.changed_at, g.observed_at, g.five_hour_used_percent, g.five_hour_reset_at,
	g.seven_day_used_percent, g.seven_day_reset_at, g.auto_checked_at, g.next_check_at`

func scanUpstreamQuotaGuardRow(row rowScanner) (upstreamQuotaGuardRow, error) {
	var value upstreamQuotaGuardRow
	var reset, restore, changed, observed, fiveReset, sevenReset, checked, next sql.NullTime
	var fiveUsed, sevenUsed sql.NullInt16
	if err := row.Scan(&value.accountID, &value.status, &value.source, &value.limitedWindow, &reset, &restore,
		&changed, &observed, &fiveUsed, &fiveReset, &sevenUsed, &sevenReset, &checked, &next); err != nil {
		return upstreamQuotaGuardRow{}, err
	}
	value.resetAt, value.restoreAt, value.changedAt = nullTimePointer(reset), nullTimePointer(restore), nullTimePointer(changed)
	value.observedAt, value.autoCheckedAt, value.nextCheckAt = nullTimePointer(observed), nullTimePointer(checked), nullTimePointer(next)
	if fiveUsed.Valid {
		value.fiveHour = &UpstreamQuotaWindow{UsedPercent: int(fiveUsed.Int16), ResetAt: nullTimePointer(fiveReset)}
	}
	if sevenUsed.Valid {
		value.sevenDay = &UpstreamQuotaWindow{UsedPercent: int(sevenUsed.Int16), ResetAt: nullTimePointer(sevenReset)}
	}
	return value, nil
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time.UTC()
	return &at
}

func (row upstreamQuotaGuardRow) limitedAt(at time.Time) bool {
	return row.status == UpstreamQuotaGuardLimited && (row.restoreAt == nil || row.restoreAt.After(at))
}

func (row upstreamQuotaGuardRow) snapshot() *UpstreamQuotaObservation {
	if row.observedAt == nil {
		return nil
	}
	return &UpstreamQuotaObservation{ObservedAt: *row.observedAt, FiveHour: row.fiveHour, SevenDay: row.sevenDay}
}

func (row upstreamQuotaGuardRow) state(at time.Time) UpstreamQuotaGuardState {
	state := UpstreamQuotaGuardState{
		AccountID: row.accountID, Status: UpstreamQuotaGuardAvailable, Source: row.source,
		ChangedAt: row.changedAt, ObservedAt: row.observedAt, FiveHour: row.fiveHour, SevenDay: row.sevenDay,
		AutoCheckedAt: row.autoCheckedAt, NextCheckAt: row.nextCheckAt,
	}
	if row.limitedAt(at) {
		state.Status, state.LimitedWindow = UpstreamQuotaGuardLimited, row.limitedWindow.String
		state.ResetAt, state.RestoreAt = row.resetAt, row.restoreAt
	}
	return state
}

func normalizedGuardTime(at time.Time, now func() time.Time) time.Time {
	if at.IsZero() {
		return now().UTC()
	}
	return at.UTC()
}

// UpstreamQuotaGuardSettings returns the global safety percentage.
func (s *Store) UpstreamQuotaGuardSettings(ctx context.Context) (UpstreamQuotaGuardSettings, error) {
	var settings UpstreamQuotaGuardSettings
	err := s.db.QueryRowContext(ctx, `SELECT safety_percent, revision, updated_at
		FROM upstream_quota_guard_settings WHERE singleton`).Scan(&settings.SafetyPercent, &settings.Revision, &settings.UpdatedAt)
	return settings, mapDBError("read upstream quota guard settings", err)
}

// ListUpstreamQuotaGuards returns the settings and the guard state of every
// account of this store's provider that has one, as of at.
func (s *Store) ListUpstreamQuotaGuards(ctx context.Context, at time.Time) (UpstreamQuotaGuardSettings, []UpstreamQuotaGuardState, error) {
	at = normalizedGuardTime(at, s.now)
	settings, err := s.UpstreamQuotaGuardSettings(ctx)
	if err != nil {
		return settings, nil, err
	}
	if !guardedUpstreamProvider(s.upstreamProviderName()) {
		return settings, []UpstreamQuotaGuardState{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+upstreamQuotaGuardColumns+`
		FROM upstream_quota_guard_states g JOIN upstream_accounts a ON a.id = g.account_id
		WHERE a.provider = $1 ORDER BY g.account_id`, s.upstreamProviderName())
	if err != nil {
		return settings, nil, mapDBError("list upstream quota guards", err)
	}
	defer rows.Close()
	states := make([]UpstreamQuotaGuardState, 0)
	for rows.Next() {
		row, err := scanUpstreamQuotaGuardRow(rows)
		if err != nil {
			return settings, nil, fmt.Errorf("scan upstream quota guard: %w", err)
		}
		states = append(states, row.state(at))
	}
	if err := rows.Err(); err != nil {
		return settings, nil, fmt.Errorf("iterate upstream quota guards: %w", err)
	}
	return settings, states, nil
}

// ClaimUpstreamQuotaCheck atomically records an automatic check attempt when
// the account is due and the last attempt is at least the minimum interval old.
func (s *Store) ClaimUpstreamQuotaCheck(ctx context.Context, accountID string, at time.Time) (UpstreamQuotaCheckClaim, error) {
	if !upstreamAccountIDPattern.MatchString(accountID) {
		return UpstreamQuotaCheckClaim{}, fmt.Errorf("%w: invalid upstream account", ErrInvalid)
	}
	at = normalizedGuardTime(at, s.now)
	var claim UpstreamQuotaCheckClaim
	var next, checked sql.NullTime
	// The data-modifying CTE runs once; the outer read sees the row as it was
	// before this statement, which is what an unclaimed caller needs.
	err := s.db.QueryRowContext(ctx, `WITH account AS (
			SELECT id, provider FROM upstream_accounts WHERE id = $1 AND provider IN ('codex', 'anthropic')
		), claimed AS (
			INSERT INTO upstream_quota_guard_states AS g (account_id, auto_checked_at)
			SELECT id, $2 FROM account
			ON CONFLICT (account_id) DO UPDATE SET auto_checked_at = EXCLUDED.auto_checked_at
			WHERE (g.next_check_at IS NULL OR g.next_check_at <= $2)
			  AND (g.auto_checked_at IS NULL OR g.auto_checked_at <= $3)
			RETURNING g.account_id
		)
		SELECT a.provider, EXISTS (SELECT 1 FROM claimed), g.next_check_at, g.auto_checked_at
		FROM account a LEFT JOIN upstream_quota_guard_states g ON g.account_id = a.id`,
		accountID, at, at.Add(-UpstreamQuotaGuardMinInterval)).Scan(&claim.Provider, &claim.Claimed, &next, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return UpstreamQuotaCheckClaim{}, nil
	}
	if err != nil {
		return UpstreamQuotaCheckClaim{}, mapDBError("claim upstream quota check", err)
	}
	if !claim.Claimed {
		claim.NextCheckAt = UpstreamQuotaGuardState{NextCheckAt: nullTimePointer(next), AutoCheckedAt: nullTimePointer(checked)}.NextAutomaticCheck()
	}
	return claim, nil
}

// lockUpstreamQuotaGuardRowTx creates the account's guard row when missing and
// locks it. ErrNotFound means the account does not exist for provider.
func lockUpstreamQuotaGuardRowTx(ctx context.Context, tx *sql.Tx, accountID, provider string) (upstreamQuotaGuardRow, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO upstream_quota_guard_states (account_id)
		SELECT id FROM upstream_accounts WHERE id = $1 AND provider = $2
		ON CONFLICT (account_id) DO NOTHING`, accountID, provider); err != nil {
		return upstreamQuotaGuardRow{}, mapDBError("create upstream quota guard", err)
	}
	row, err := scanUpstreamQuotaGuardRow(tx.QueryRowContext(ctx, `SELECT `+upstreamQuotaGuardColumns+`
		FROM upstream_quota_guard_states g JOIN upstream_accounts a ON a.id = g.account_id
		WHERE g.account_id = $1 AND a.provider = $2 FOR UPDATE OF g`, accountID, provider))
	return row, mapDBError("lock upstream quota guard", err)
}

func windowValues(window *UpstreamQuotaWindow) (any, any) {
	if window == nil {
		return nil, nil
	}
	return window.UsedPercent, timeOrNil(window.ResetAt)
}

func appendUpstreamQuotaGuardAuditTx(ctx context.Context, tx *sql.Tx, at time.Time, actorUserID, actorSessionID, sourceIP, requestID, eventType, accountID string, metadata map[string]any) error {
	data, err := marshalSafeMetadata(metadata)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
		(occurred_at, actor_user_id, actor_session_id, event_type, severity,
			success, source_ip, subject_type, subject_id, request_id, metadata)
		VALUES ($1,$2,$3,$4,'info',true,$5::inet,'upstream_account',$6,$7,$8::jsonb)`,
		at, valueOrNil(actorUserID), valueOrNil(actorSessionID), eventType,
		valueOrNil(sourceIP), accountID, valueOrNil(requestID), data)
	return mapDBError("audit upstream quota guard", err)
}

func guardAuditWindowMetadata(metadata map[string]any, observation UpstreamQuotaObservation) map[string]any {
	if observation.FiveHour != nil {
		metadata["five_hour_used_percent"] = observation.FiveHour.UsedPercent
	}
	if observation.SevenDay != nil {
		metadata["seven_day_used_percent"] = observation.SevenDay.UsedPercent
	}
	return metadata
}

// RecordUpstreamQuotaObservation stores a reading and applies the guard rule.
// Transitions are audited without an actor because the rule, not a person,
// made them.
func (s *Store) RecordUpstreamQuotaObservation(ctx context.Context, params RecordUpstreamQuotaObservationParams) (UpstreamQuotaGuardState, error) {
	if !guardedUpstreamProvider(params.Provider) || !upstreamAccountIDPattern.MatchString(params.AccountID) ||
		!validUpstreamQuotaObservation(params.Observation) {
		return UpstreamQuotaGuardState{}, fmt.Errorf("%w: invalid upstream quota observation", ErrInvalid)
	}
	at := normalizedGuardTime(params.At, s.now)
	observation := params.Observation
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = at
	}
	observation.ObservedAt = observation.ObservedAt.UTC()
	var result UpstreamQuotaGuardState
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var percent int
		if err := tx.QueryRowContext(ctx, `SELECT safety_percent FROM upstream_quota_guard_settings
			WHERE singleton FOR SHARE`).Scan(&percent); err != nil {
			return mapDBError("read upstream quota guard settings", err)
		}
		current, err := lockUpstreamQuotaGuardRowTx(ctx, tx, params.AccountID, params.Provider)
		if err != nil {
			return err
		}
		fiveUsed, fiveReset := windowValues(observation.FiveHour)
		sevenUsed, sevenReset := windowValues(observation.SevenDay)
		if !params.Automatic && current.source == UpstreamQuotaGuardSourceManual {
			// A manual state lasts until the next automatic check.
			if _, err := tx.ExecContext(ctx, `UPDATE upstream_quota_guard_states SET observed_at = $2,
				five_hour_used_percent = $3, five_hour_reset_at = $4,
				seven_day_used_percent = $5, seven_day_reset_at = $6
				WHERE account_id = $1`, params.AccountID, observation.ObservedAt,
				fiveUsed, fiveReset, sevenUsed, sevenReset); err != nil {
				return mapDBError("record upstream quota snapshot", err)
			}
		} else {
			decision := evaluateUpstreamQuotaGuard(observation, current.snapshot(), percent, at)
			wasLimited := current.limitedAt(at)
			status := UpstreamQuotaGuardAvailable
			var window any
			if decision.limited {
				status, window = UpstreamQuotaGuardLimited, decision.window
			}
			changedAt := timeOrNil(current.changedAt)
			if wasLimited != decision.limited || current.source != UpstreamQuotaGuardSourceAuto {
				changedAt = at
			}
			if _, err := tx.ExecContext(ctx, `UPDATE upstream_quota_guard_states SET
				status = $2, source = 'auto', limited_window = $3, reset_at = $4, restore_at = $5,
				changed_at = $6, observed_at = $7,
				five_hour_used_percent = $8, five_hour_reset_at = $9,
				seven_day_used_percent = $10, seven_day_reset_at = $11, next_check_at = $12
				WHERE account_id = $1`, params.AccountID, status, window, timeOrNil(decision.resetAt),
				timeOrNil(decision.restoreAt), changedAt, observation.ObservedAt,
				fiveUsed, fiveReset, sevenUsed, sevenReset, decision.nextCheckAt); err != nil {
				return mapDBError("record upstream quota guard", err)
			}
			if wasLimited != decision.limited {
				via := "observation"
				if params.Automatic {
					via = "automatic"
				}
				eventType := "upstream_quota_guard.restored"
				metadata := map[string]any{"via": via, "safety_percent": percent}
				if decision.limited {
					eventType = "upstream_quota_guard.limited"
					metadata["window"] = decision.window
					if decision.resetAt != nil {
						metadata["reset_at"] = decision.resetAt.Format(time.RFC3339)
					}
				}
				if err := appendUpstreamQuotaGuardAuditTx(ctx, tx, at, "", "", "", "", eventType,
					params.AccountID, guardAuditWindowMetadata(metadata, observation)); err != nil {
					return err
				}
			}
		}
		updated, err := scanUpstreamQuotaGuardRow(tx.QueryRowContext(ctx, `SELECT `+upstreamQuotaGuardColumns+`
			FROM upstream_quota_guard_states g WHERE g.account_id = $1`, params.AccountID))
		if err != nil {
			return mapDBError("read upstream quota guard", err)
		}
		result = updated.state(at)
		return nil
	})
	return result, err
}

// SetUpstreamQuotaGuardStatus records an Owner's choice. It changes only the
// current state: the next automatic check applies the rule again. A manual
// limit lasts until the soonest known reset plus the restore delay, or until
// the Owner lifts it when no future reset is known.
func (s *Store) SetUpstreamQuotaGuardStatus(ctx context.Context, params SetUpstreamQuotaGuardStatusParams) (UpstreamQuotaGuardState, error) {
	provider := s.upstreamProviderName()
	if !guardedUpstreamProvider(provider) || !upstreamAccountIDPattern.MatchString(params.AccountID) ||
		strings.TrimSpace(params.ActorUserID) == "" {
		return UpstreamQuotaGuardState{}, fmt.Errorf("%w: invalid quota guard account or actor", ErrInvalid)
	}
	at := normalizedGuardTime(params.At, s.now)
	var result UpstreamQuotaGuardState
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		current, err := lockUpstreamQuotaGuardRowTx(ctx, tx, params.AccountID, provider)
		if err != nil {
			return err
		}
		wasLimited := current.limitedAt(at)
		if wasLimited == params.Limited {
			result = current.state(at)
			return nil
		}
		metadata := map[string]any{"limited": params.Limited}
		var window, reset, restore, next any
		status := UpstreamQuotaGuardAvailable
		if params.Limited {
			status = UpstreamQuotaGuardLimited
			if name, soonest := soonestUpstreamQuotaReset(current, at); soonest != nil {
				restoreAt := soonest.Add(UpstreamQuotaGuardRestoreDelay)
				window, reset, restore, next = name, *soonest, restoreAt, restoreAt
				metadata["window"], metadata["reset_at"] = name, soonest.Format(time.RFC3339)
			}
		} else {
			following := at
			if current.autoCheckedAt != nil && current.autoCheckedAt.Add(UpstreamQuotaGuardMinInterval).After(following) {
				following = current.autoCheckedAt.Add(UpstreamQuotaGuardMinInterval)
			}
			next = following
		}
		if _, err := tx.ExecContext(ctx, `UPDATE upstream_quota_guard_states SET
			status = $2, source = 'manual', limited_window = $3, reset_at = $4, restore_at = $5,
			changed_at = $6, next_check_at = $7 WHERE account_id = $1`,
			params.AccountID, status, window, reset, restore, at, next); err != nil {
			return mapDBError("set upstream quota guard", err)
		}
		if err := appendUpstreamQuotaGuardAuditTx(ctx, tx, at, params.ActorUserID, params.ActorSessionID,
			params.SourceIP, params.RequestID, "upstream_quota_guard.manual_set", params.AccountID, metadata); err != nil {
			return err
		}
		updated, err := scanUpstreamQuotaGuardRow(tx.QueryRowContext(ctx, `SELECT `+upstreamQuotaGuardColumns+`
			FROM upstream_quota_guard_states g WHERE g.account_id = $1`, params.AccountID))
		if err != nil {
			return mapDBError("read upstream quota guard", err)
		}
		result = updated.state(at)
		return nil
	})
	return result, err
}

// SetUpstreamQuotaGuardSettings changes the safety percentage and applies it
// at once to every automatic state from its stored snapshot. A stored reading
// can only overstate what remains, so this never limits too early; every open
// account is checked again on its next request, still subject to the minimum
// interval. Manual states are left alone.
func (s *Store) SetUpstreamQuotaGuardSettings(ctx context.Context, params SetUpstreamQuotaGuardSettingsParams) (UpstreamQuotaGuardSettings, error) {
	if params.SafetyPercent < 0 || params.SafetyPercent > MaxUpstreamQuotaGuardPercent || strings.TrimSpace(params.ActorUserID) == "" {
		return UpstreamQuotaGuardSettings{}, fmt.Errorf("%w: invalid quota guard settings", ErrInvalid)
	}
	at := normalizedGuardTime(params.At, s.now)
	var settings UpstreamQuotaGuardSettings
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var previous int
		if err := tx.QueryRowContext(ctx, `SELECT safety_percent FROM upstream_quota_guard_settings
			WHERE singleton FOR UPDATE`).Scan(&previous); err != nil {
			return mapDBError("lock upstream quota guard settings", err)
		}
		if err := tx.QueryRowContext(ctx, `UPDATE upstream_quota_guard_settings
			SET safety_percent = $1, revision = revision + 1, updated_at = $2 WHERE singleton
			RETURNING safety_percent, revision, updated_at`, params.SafetyPercent, at).Scan(
			&settings.SafetyPercent, &settings.Revision, &settings.UpdatedAt); err != nil {
			return mapDBError("update upstream quota guard settings", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+upstreamQuotaGuardColumns+`
			FROM upstream_quota_guard_states g WHERE g.source = 'auto' AND g.observed_at IS NOT NULL
			ORDER BY g.account_id FOR UPDATE`)
		if err != nil {
			return mapDBError("lock upstream quota guards", err)
		}
		var states []upstreamQuotaGuardRow
		for rows.Next() {
			row, err := scanUpstreamQuotaGuardRow(rows)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan upstream quota guard: %w", err)
			}
			states = append(states, row)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close upstream quota guards: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate upstream quota guards: %w", err)
		}
		limited, restored := 0, 0
		for _, row := range states {
			decision := evaluateUpstreamQuotaGuard(*row.snapshot(), nil, settings.SafetyPercent, at)
			wasLimited := row.limitedAt(at)
			status, changedAt := UpstreamQuotaGuardAvailable, timeOrNil(row.changedAt)
			var window, next any
			if decision.limited {
				status, window, next = UpstreamQuotaGuardLimited, decision.window, decision.nextCheckAt
			}
			if wasLimited != decision.limited {
				changedAt = at
				if decision.limited {
					limited++
				} else {
					restored++
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE upstream_quota_guard_states SET
				status = $2, limited_window = $3, reset_at = $4, restore_at = $5,
				changed_at = $6, next_check_at = $7 WHERE account_id = $1`,
				row.accountID, status, window, timeOrNil(decision.resetAt), timeOrNil(decision.restoreAt),
				changedAt, next); err != nil {
				return mapDBError("reapply upstream quota guard", err)
			}
		}
		metadata, err := marshalSafeMetadata(map[string]any{
			"previous_safety_percent": previous, "safety_percent": settings.SafetyPercent,
			"revision": settings.Revision, "limited_accounts": limited, "restored_accounts": restored,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity,
				success, source_ip, subject_type, subject_id, request_id, metadata)
			VALUES ($1,$2,$3,'upstream_quota_guard.settings_updated','info',true,
				$4::inet,'upstream_quota_guard_settings','global',$5,$6::jsonb)`,
			at, params.ActorUserID, valueOrNil(params.ActorSessionID),
			valueOrNil(params.SourceIP), valueOrNil(params.RequestID), metadata)
		return mapDBError("audit upstream quota guard settings", err)
	})
	return settings, err
}
