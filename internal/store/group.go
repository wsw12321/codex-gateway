package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	decimal "github.com/wsw/codex-gateway/internal/billing"
)

// GroupQuotaExceededError takes precedence over personal funding and request
// limits. Requests accepted before a group reaches its cap still settle in full.
type GroupQuotaExceededError struct {
	GroupID    string
	RetryAfter time.Duration
	NotStarted bool
}

func (e *GroupQuotaExceededError) Error() string { return "group quota unavailable" }
func (e *GroupQuotaExceededError) Unwrap() error { return ErrQuotaExceeded }

type GroupSummary struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	LimitUSD       string     `json:"limit_usd"`
	UsedUSD        string     `json:"used_usd"`
	RemainingUSD   string     `json:"remaining_usd"`
	Period         string     `json:"period"`
	CustomDays     int        `json:"custom_days"`
	StartsAt       time.Time  `json:"starts_at"`
	PeriodID       string     `json:"period_id"`
	PeriodStartsAt time.Time  `json:"period_starts_at"`
	PeriodEndsAt   time.Time  `json:"period_ends_at"`
	MemberCount    int        `json:"member_count"`
	ArchivedAt     *time.Time `json:"archived_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type GroupMember struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	UsedUSD     string `json:"used_usd"`
}

type Group struct {
	GroupSummary
	Members []GroupMember `json:"members"`
}

type PutGroupParams struct {
	BillingWriteParams
	GroupID    string
	Name       string
	LimitUSD   string
	Period     string
	CustomDays int
	StartsAt   *time.Time
}

type SetGroupMembersParams struct {
	BillingWriteParams
	GroupID string
	UserIDs []string
	Action  string
}

type ArchiveGroupParams struct {
	BillingWriteParams
	GroupID string
}

func groupPeriodDuration(period string, customDays int) (time.Duration, error) {
	if period == "custom" {
		// time.Duration must hold the complete period without overflowing.
		if customDays < 1 || customDays > 106751 {
			return 0, fmt.Errorf("%w: custom period days must be between 1 and 106751", ErrInvalid)
		}
		return time.Duration(customDays) * 24 * time.Hour, nil
	}
	if customDays != 0 {
		return 0, fmt.Errorf("%w: custom days require a custom period", ErrInvalid)
	}
	return billingPeriodDuration(period)
}

const groupSummaryColumns = `g.id, g.name, g.limit_usd::text, p.used_usd::text,
	GREATEST(g.limit_usd-p.used_usd,0)::numeric(30,12)::text, g.period, g.custom_days, g.starts_at,
	p.id, p.starts_at, p.ends_at,
	(SELECT count(*) FROM billing_accounts a WHERE a.group_id=g.id),
	g.archived_at, g.created_at, g.updated_at`

func scanGroupSummary(row rowScanner) (GroupSummary, error) {
	var g GroupSummary
	err := row.Scan(&g.ID, &g.Name, &g.LimitUSD, &g.UsedUSD, &g.RemainingUSD,
		&g.Period, &g.CustomDays, &g.StartsAt, &g.PeriodID, &g.PeriodStartsAt,
		&g.PeriodEndsAt, &g.MemberCount, &g.ArchivedAt, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

// lockGroupTx always follows billing-account locks, when any are needed.
func lockGroupTx(ctx context.Context, tx *sql.Tx, id string) error {
	var locked string
	err := tx.QueryRowContext(ctx, `SELECT id FROM user_groups WHERE id=$1 FOR UPDATE`, id).Scan(&locked)
	return mapDBError("lock group", err)
}

func readGroupSummaryTx(ctx context.Context, tx *sql.Tx, id string) (GroupSummary, error) {
	g, err := scanGroupSummary(tx.QueryRowContext(ctx, `SELECT `+groupSummaryColumns+`
		FROM user_groups g JOIN group_usage_periods p ON p.id=g.current_period_id WHERE g.id=$1`, id))
	return g, mapDBError("read group", err)
}

// rollGroupTx is called with the group lock held. Idle periods need no rows;
// moving directly to the period containing at retains the configured anchor.
func rollGroupTx(ctx context.Context, tx *sql.Tx, id string, at time.Time) (GroupSummary, error) {
	g, err := readGroupSummaryTx(ctx, tx, id)
	if err != nil || g.ArchivedAt != nil || at.Before(g.PeriodEndsAt) {
		return g, err
	}
	duration, err := groupPeriodDuration(g.Period, g.CustomDays)
	if err != nil {
		return g, err
	}
	start := groupPeriodStart(g.PeriodStartsAt, at, duration)
	if err := replaceGroupPeriodTx(ctx, tx, g, start, start.Add(duration), g.PeriodEndsAt, at); err != nil {
		return g, err
	}
	return readGroupSummaryTx(ctx, tx, id)
}

// Use seconds rather than Duration for elapsed time: a valid timestamp anchor
// can be more than Duration's 292-year range before the current request.
func groupPeriodStart(anchor, at time.Time, duration time.Duration) time.Time {
	if at.Before(anchor) {
		return anchor
	}
	elapsed := at.Unix() - anchor.Unix()
	if at.Nanosecond() < anchor.Nanosecond() {
		elapsed--
	}
	seconds := int64(duration / time.Second)
	return time.Unix(anchor.Unix()+(elapsed/seconds)*seconds, int64(anchor.Nanosecond())).UTC()
}

func canonicalGroupID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return "", fmt.Errorf("%w: invalid group or user ID", ErrInvalid)
	}
	return id.String(), nil
}

func replaceGroupPeriodTx(ctx context.Context, tx *sql.Tx, g GroupSummary, starts, ends, closedAt, at time.Time) error {
	if g.PeriodID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE group_usage_periods SET closed_at=COALESCE(closed_at,$2) WHERE id=$1`, g.PeriodID, closedAt); err != nil {
			return mapDBError("close group period", err)
		}
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO group_usage_periods(id,group_id,starts_at,ends_at,limit_usd,created_at)
		VALUES($1,$2,$3,$4,$5::numeric,$6)`, id, g.ID, starts, ends, g.LimitUSD, at); err != nil {
		return mapDBError("create group period", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE user_groups SET current_period_id=$2,updated_at=$3 WHERE id=$1`, g.ID, id, at)
	return mapDBError("advance group period", err)
}

func groupDetailTx(ctx context.Context, tx *sql.Tx, id string) (Group, error) {
	summary, err := readGroupSummaryTx(ctx, tx, id)
	result := Group{GroupSummary: summary, Members: make([]GroupMember, 0)}
	if err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT u.id,u.username,u.display_name,
		COALESCE((SELECT sum(l.actual_cost_usd) FROM billing_ledger_entries l
		WHERE l.group_period_id=$2 AND l.user_id=u.id AND l.entry_type='usage_charge'),0)::numeric(30,12)::text
		FROM billing_accounts a JOIN users u ON u.id=a.user_id WHERE a.group_id=$1 ORDER BY u.username,u.id`, id, summary.PeriodID)
	if err != nil {
		return result, mapDBError("list group members", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.UserID, &m.Username, &m.DisplayName, &m.UsedUSD); err != nil {
			return result, fmt.Errorf("scan group member: %w", err)
		}
		result.Members = append(result.Members, m)
	}
	return result, rows.Err()
}

func (s *Store) GetGroup(ctx context.Context, id string) (Group, error) {
	var g Group
	var err error
	id, err = canonicalGroupID(id)
	if err != nil {
		return g, err
	}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockGroupTx(ctx, tx, id); err != nil {
			return err
		}
		if _, err := rollGroupTx(ctx, tx, id, s.now().UTC()); err != nil {
			return err
		}
		var err error
		g, err = groupDetailTx(ctx, tx, id)
		return err
	})
	return g, err
}

func (s *Store) ListGroups(ctx context.Context) ([]GroupSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM user_groups ORDER BY created_at,id`)
	if err != nil {
		return nil, mapDBError("list groups", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]GroupSummary, 0, len(ids))
	for _, id := range ids {
		var g GroupSummary
		err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
			if err := lockGroupTx(ctx, tx, id); err != nil {
				return err
			}
			var err error
			g, err = rollGroupTx(ctx, tx, id, s.now().UTC())
			return err
		})
		if err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	return result, nil
}

// groupWrite claims an idempotency key before any account/group locks. Replays
// return the original response even after membership and period changes.
func (s *Store) groupWrite(ctx context.Context, params BillingWriteParams, action string, values []string,
	apply func(*sql.Tx) (Group, error)) (Group, error) {
	var result Group
	if err := validateBillingWrite(params); err != nil {
		return result, err
	}
	params.At = normalizedBillingTime(params.At, s.now)
	fingerprint := billingOperationFingerprint(append(values, strings.TrimSpace(params.Reason))...)
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		insert, err := tx.ExecContext(ctx, `INSERT INTO group_operations(operation_id,actor_user_id,action,request_fingerprint,created_at)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, params.OperationID, params.ActorUserID, action, fingerprint, params.At)
		if err != nil {
			return mapDBError("claim group operation", err)
		}
		n, err := insert.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var actor, storedAction string
			var stored, encoded []byte
			if err := tx.QueryRowContext(ctx, `SELECT actor_user_id,action,request_fingerprint,response FROM group_operations WHERE operation_id=$1 FOR UPDATE`, params.OperationID).Scan(&actor, &storedAction, &stored, &encoded); err != nil {
				return mapDBError("read group operation", err)
			}
			if actor != params.ActorUserID || storedAction != action || !bytes.Equal(stored, fingerprint) || len(encoded) == 0 {
				return fmt.Errorf("%w: group operation replay mismatch", ErrConflict)
			}
			return json.Unmarshal(encoded, &result)
		}
		result, err = apply(tx)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE group_operations SET response=$2::jsonb WHERE operation_id=$1`, params.OperationID, string(encoded)); err != nil {
			return mapDBError("finish group operation", err)
		}
		return appendBillingAuditTx(ctx, tx, params, "group."+action, "group", result.ID, map[string]any{"reason": strings.TrimSpace(params.Reason), "operation_id": params.OperationID, "member_count": result.MemberCount})
	})
	return result, err
}

func (s *Store) PutGroup(ctx context.Context, params PutGroupParams) (Group, error) {
	if params.GroupID != "" {
		var err error
		params.GroupID, err = canonicalGroupID(params.GroupID)
		if err != nil {
			return Group{}, err
		}
	}
	params.Name = strings.TrimSpace(params.Name)
	if params.Name == "" || len([]rune(params.Name)) > 120 {
		return Group{}, fmt.Errorf("%w: group name is required (maximum 120 characters)", ErrInvalid)
	}
	amount, err := decimal.ParseInput(params.LimitUSD, false, true)
	if err != nil {
		return Group{}, fmt.Errorf("%w: invalid group limit", ErrInvalid)
	}
	params.LimitUSD = amount
	duration, err := groupPeriodDuration(params.Period, params.CustomDays)
	if err != nil {
		return Group{}, err
	}
	params.At = normalizedBillingTime(params.At, s.now)
	startFingerprint := ""
	if params.StartsAt != nil {
		startFingerprint = params.StartsAt.UTC().Format(time.RFC3339Nano)
	}
	return s.groupWrite(ctx, params.BillingWriteParams, "put", []string{params.GroupID, params.Name, amount, params.Period, fmt.Sprint(params.CustomDays), startFingerprint}, func(tx *sql.Tx) (Group, error) {
		var g GroupSummary
		create := params.GroupID == ""
		if create {
			id, err := newUUID()
			if err != nil {
				return Group{}, err
			}
			start := params.At
			if params.StartsAt != nil {
				start = params.StartsAt.UTC()
			}
			g = GroupSummary{ID: id, Name: params.Name, LimitUSD: amount, Period: params.Period, CustomDays: params.CustomDays, StartsAt: start}
			if _, err := tx.ExecContext(ctx, `INSERT INTO user_groups(id,name,limit_usd,period,custom_days,starts_at,created_at,updated_at)
				VALUES($1,$2,$3::numeric,$4,$5,$6,$7,$7)`, id, params.Name, amount, params.Period, params.CustomDays, start, params.At); err != nil {
				return Group{}, mapDBError("create group", err)
			}
		} else {
			if err := lockGroupTx(ctx, tx, params.GroupID); err != nil {
				return Group{}, err
			}
			var err error
			g, err = rollGroupTx(ctx, tx, params.GroupID, params.At)
			if err != nil {
				return Group{}, err
			}
			if g.ArchivedAt != nil {
				return Group{}, fmt.Errorf("%w: group is archived", ErrConflict)
			}
		}
		reset := create || g.Period != params.Period || g.CustomDays != params.CustomDays || (params.StartsAt != nil && !params.StartsAt.Equal(g.StartsAt))
		start := g.StartsAt
		if reset {
			if params.StartsAt != nil {
				start = params.StartsAt.UTC()
			} else {
				start = params.At
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE user_groups SET name=$2,limit_usd=$3::numeric,period=$4,custom_days=$5,starts_at=$6,updated_at=$7 WHERE id=$1`, g.ID, params.Name, amount, params.Period, params.CustomDays, start, params.At); err != nil {
			return Group{}, mapDBError("update group", err)
		}
		g.LimitUSD = amount
		if reset {
			// A past anchor selects its current cycle; edits always create a fresh
			// period row, so no existing usage is silently reused or removed.
			periodStart := start
			if !params.At.Before(start) {
				periodStart = groupPeriodStart(start, params.At, duration)
			}
			if err := replaceGroupPeriodTx(ctx, tx, g, periodStart, periodStart.Add(duration), params.At, params.At); err != nil {
				return Group{}, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE group_usage_periods SET limit_usd=$2::numeric WHERE id=$1`, g.PeriodID, amount); err != nil {
				return Group{}, mapDBError("update group period limit", err)
			}
		}
		return groupDetailTx(ctx, tx, g.ID)
	})
}

func (s *Store) SetGroupMembers(ctx context.Context, params SetGroupMembersParams) (Group, error) {
	if params.GroupID == "" || (params.Action != "add" && params.Action != "remove") || len(params.UserIDs) == 0 || len(params.UserIDs) > 5000 {
		return Group{}, fmt.Errorf("%w: invalid group members operation", ErrInvalid)
	}
	ids := append([]string(nil), params.UserIDs...)
	var err error
	params.GroupID, err = canonicalGroupID(params.GroupID)
	if err != nil {
		return Group{}, err
	}
	for i, id := range ids {
		ids[i], err = canonicalGroupID(id)
		if err != nil {
			return Group{}, err
		}
	}
	sort.Strings(ids)
	for i, id := range ids {
		if id == "" || (i > 0 && ids[i-1] == id) {
			return Group{}, fmt.Errorf("%w: user IDs must be distinct and nonempty", ErrInvalid)
		}
	}
	params.At = normalizedBillingTime(params.At, s.now)
	return s.groupWrite(ctx, params.BillingWriteParams, "members."+params.Action, append([]string{params.GroupID}, ids...), func(tx *sql.Tx) (Group, error) {
		// Lock every account in stable order before the group, matching admission
		// and settlement. A transfer must be explicit remove then add.
		for _, id := range ids {
			var current sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT group_id FROM billing_accounts WHERE user_id=$1 FOR UPDATE`, id).Scan(&current); err != nil {
				return Group{}, mapDBError("lock group member", err)
			}
			if current.Valid && current.String != params.GroupID {
				return Group{}, fmt.Errorf("%w: user already belongs to another group", ErrConflict)
			}
		}
		if err := lockGroupTx(ctx, tx, params.GroupID); err != nil {
			return Group{}, err
		}
		g, err := rollGroupTx(ctx, tx, params.GroupID, params.At)
		if err != nil {
			return Group{}, err
		}
		if g.ArchivedAt != nil {
			return Group{}, fmt.Errorf("%w: group is archived", ErrConflict)
		}
		for _, id := range ids {
			var groupID any
			if params.Action == "add" {
				groupID = params.GroupID
			}
			if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET group_id=$2,updated_at=$3 WHERE user_id=$1`, id, groupID, params.At); err != nil {
				return Group{}, mapDBError("set group member", err)
			}
		}
		return groupDetailTx(ctx, tx, params.GroupID)
	})
}

func (s *Store) ArchiveGroup(ctx context.Context, params ArchiveGroupParams) (Group, error) {
	var err error
	params.GroupID, err = canonicalGroupID(params.GroupID)
	if err != nil {
		return Group{}, err
	}
	params.At = normalizedBillingTime(params.At, s.now)
	return s.groupWrite(ctx, params.BillingWriteParams, "archive", []string{params.GroupID}, func(tx *sql.Tx) (Group, error) {
		if err := lockGroupTx(ctx, tx, params.GroupID); err != nil {
			return Group{}, err
		}
		g, err := rollGroupTx(ctx, tx, params.GroupID, params.At)
		if err != nil {
			return Group{}, err
		}
		if g.MemberCount != 0 {
			return Group{}, fmt.Errorf("%w: only empty groups can be archived", ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE user_groups SET archived_at=COALESCE(archived_at,$2),updated_at=$2 WHERE id=$1`, params.GroupID, params.At); err != nil {
			return Group{}, mapDBError("archive group", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE group_usage_periods SET closed_at=COALESCE(closed_at,$2) WHERE id=$1`, g.PeriodID, params.At); err != nil {
			return Group{}, mapDBError("close archived group period", err)
		}
		return groupDetailTx(ctx, tx, params.GroupID)
	})
}

// userGroupTx requires the user's billing-account lock, then takes the group
// lock. This serializes membership changes with the admission snapshot.
func userGroupTx(ctx context.Context, tx *sql.Tx, userID string, at time.Time) (*GroupSummary, error) {
	var id sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT group_id FROM billing_accounts WHERE user_id=$1`, userID).Scan(&id); err != nil {
		return nil, mapDBError("read user group", err)
	}
	if !id.Valid {
		return nil, nil
	}
	if err := lockGroupTx(ctx, tx, id.String); err != nil {
		return nil, err
	}
	g, err := rollGroupTx(ctx, tx, id.String, at)
	if err != nil {
		return nil, err
	}
	if g.ArchivedAt != nil {
		return nil, fmt.Errorf("%w: user belongs to archived group", ErrConflict)
	}
	return &g, nil
}

func requireGroupQuota(g *GroupSummary, at time.Time) error {
	if g == nil {
		return nil
	}
	if at.Before(g.PeriodStartsAt) {
		return &GroupQuotaExceededError{GroupID: g.ID, RetryAfter: g.PeriodStartsAt.Sub(at), NotStarted: true}
	}
	if !billingPositive(g.RemainingUSD) {
		return &GroupQuotaExceededError{GroupID: g.ID, RetryAfter: g.PeriodEndsAt.Sub(at)}
	}
	return nil
}

// lockBoundGroupPeriodTx uses the original group (not current membership).
func lockBoundGroupPeriodTx(ctx context.Context, tx *sql.Tx, groupID, periodID *string) error {
	if groupID == nil && periodID == nil {
		return nil
	}
	if groupID == nil || periodID == nil {
		return fmt.Errorf("%w: incomplete group binding", ErrInvalid)
	}
	if err := lockGroupTx(ctx, tx, *groupID); err != nil {
		return err
	}
	var locked string
	err := tx.QueryRowContext(ctx, `SELECT id FROM group_usage_periods WHERE id=$1 AND group_id=$2 FOR UPDATE`, *periodID, *groupID).Scan(&locked)
	return mapDBError("lock bound group period", err)
}
