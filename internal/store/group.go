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

// GroupQuotaExceededError is retained for legacy quota error responses. New
// admission falls back to personal funds when group funding is unavailable.
type GroupQuotaExceededError struct {
	GroupID    string
	RetryAfter time.Duration
	NotStarted bool
}

func (e *GroupQuotaExceededError) Error() string { return "group quota unavailable" }
func (e *GroupQuotaExceededError) Unwrap() error { return ErrQuotaExceeded }

type GroupSummary struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	LimitUSD            string     `json:"limit_usd"`
	MemberLimitUSD      *string    `json:"member_limit_usd"`
	UsedUSD             string     `json:"used_usd"`
	RemainingUSD        string     `json:"remaining_usd"`
	Period              string     `json:"period"`
	CustomDays          int        `json:"custom_days"`
	StartsAt            time.Time  `json:"starts_at"`
	PeriodID            string     `json:"period_id"`
	PeriodStartsAt      time.Time  `json:"period_starts_at"`
	PeriodEndsAt        time.Time  `json:"period_ends_at"`
	PeriodCount         int        `json:"period_count"`
	CurrentPeriodNumber int        `json:"current_period_number"`
	ExpiresAt           *time.Time `json:"expires_at"`
	MemberCount         int        `json:"member_count"`
	ArchivedAt          *time.Time `json:"archived_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type GroupMember struct {
	UserID       string  `json:"user_id"`
	Username     string  `json:"username"`
	DisplayName  string  `json:"display_name"`
	UsedUSD      string  `json:"used_usd"`
	RemainingUSD *string `json:"remaining_usd"`
}

type Group struct {
	GroupSummary
	Members []GroupMember `json:"members"`
}

type PutGroupParams struct {
	BillingWriteParams
	GroupID        string
	Name           string
	LimitUSD       string
	MemberLimitUSD *string
	MemberLimitSet bool
	Period         string
	CustomDays     int
	StartsAt       *time.Time
	PeriodCount    *int
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

const groupSummaryColumns = `g.id, g.name, g.limit_usd::text, g.member_limit_usd::text, p.used_usd::text,
	GREATEST(g.limit_usd-p.used_usd,0)::numeric(30,12)::text, g.period, g.custom_days, g.starts_at,
	p.id, p.starts_at, p.ends_at, g.period_count, g.current_period_number, g.expires_at,
	(SELECT count(*) FROM billing_accounts a WHERE a.group_id=g.id),
	g.archived_at, g.created_at, g.updated_at`

func scanGroupSummary(row rowScanner) (GroupSummary, error) {
	var g GroupSummary
	err := row.Scan(&g.ID, &g.Name, &g.LimitUSD, &g.MemberLimitUSD, &g.UsedUSD, &g.RemainingUSD,
		&g.Period, &g.CustomDays, &g.StartsAt, &g.PeriodID, &g.PeriodStartsAt,
		&g.PeriodEndsAt, &g.PeriodCount, &g.CurrentPeriodNumber, &g.ExpiresAt,
		&g.MemberCount, &g.ArchivedAt, &g.CreatedAt, &g.UpdatedAt)
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
	if err != nil || g.ArchivedAt != nil || at.Before(g.PeriodEndsAt) || (g.ExpiresAt != nil && !at.Before(*g.ExpiresAt)) {
		return g, err
	}
	duration, err := groupPeriodDuration(g.Period, g.CustomDays)
	if err != nil {
		return g, err
	}
	start := groupPeriodStart(g.PeriodStartsAt, at, duration)
	end, err := addGroupPeriods(start, duration, 1)
	if err != nil {
		// At the supported calendar boundary there is no representable next
		// cycle. Keep the ending snapshot so admission can use personal funds.
		return g, nil
	}
	g.CurrentPeriodNumber += int((start.Unix() - g.PeriodStartsAt.Unix()) / int64(duration/time.Second))
	if g.PeriodCount > 0 && g.CurrentPeriodNumber > g.PeriodCount {
		return g, fmt.Errorf("%w: group period exceeds its configured count", ErrInvalid)
	}
	if err := replaceGroupPeriodTx(ctx, tx, g, start, end, g.PeriodEndsAt, at); err != nil {
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

func validateGroupTime(at time.Time) error {
	if year := at.UTC().Year(); year < 1 || year > 9999 {
		return fmt.Errorf("%w: group dates must be between years 1 and 9999", ErrInvalid)
	}
	return nil
}

// Calendar timestamps may span far more than time.Duration. Multiply seconds
// only after checking the supported timestamp range, including the final expiry.
func addGroupPeriods(anchor time.Time, duration time.Duration, count int) (time.Time, error) {
	if err := validateGroupTime(anchor); err != nil {
		return time.Time{}, err
	}
	seconds := int64(duration / time.Second)
	lastSecond := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC).Unix()
	if count < 0 || seconds <= 0 || int64(count) > (lastSecond-anchor.Unix())/seconds {
		return time.Time{}, fmt.Errorf("%w: group period or expiry exceeds the supported date range", ErrInvalid)
	}
	return time.Unix(anchor.Unix()+int64(count)*seconds, int64(anchor.Nanosecond())).UTC(), nil
}

func groupExpiration(end time.Time, duration time.Duration, count, current int) (*time.Time, error) {
	if count == 0 {
		return nil, nil
	}
	if count < current {
		return nil, fmt.Errorf("%w: group period count cannot be below the current period number", ErrInvalid)
	}
	expires, err := addGroupPeriods(end, duration, count-current)
	if err != nil {
		return nil, err
	}
	return &expires, nil
}

func groupPeriodActive(g *GroupSummary, at time.Time) bool {
	return g != nil && g.ArchivedAt == nil && !at.Before(g.PeriodStartsAt) && at.Before(g.PeriodEndsAt) &&
		(g.ExpiresAt == nil || at.Before(*g.ExpiresAt))
}

func groupHasNextPeriod(g *GroupSummary) bool {
	if g == nil || g.ArchivedAt != nil || (g.PeriodCount != 0 && g.CurrentPeriodNumber >= g.PeriodCount) ||
		(g.ExpiresAt != nil && !g.PeriodEndsAt.Before(*g.ExpiresAt)) {
		return false
	}
	duration, err := groupPeriodDuration(g.Period, g.CustomDays)
	if err != nil {
		return false
	}
	_, err = addGroupPeriods(g.PeriodEndsAt, duration, 1)
	return err == nil
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO group_usage_periods(id,group_id,starts_at,ends_at,limit_usd,member_limit_usd,created_at)
		VALUES($1,$2,$3,$4,$5::numeric,$6::numeric,$7)`, id, g.ID, starts, ends, g.LimitUSD, g.MemberLimitUSD, at); err != nil {
		return mapDBError("create group period", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE user_groups SET current_period_id=$2,updated_at=$3,current_period_number=$4 WHERE id=$1`, g.ID, id, at, g.CurrentPeriodNumber)
	return mapDBError("advance group period", err)
}

func groupDetailTx(ctx context.Context, tx *sql.Tx, id string) (Group, error) {
	summary, err := readGroupSummaryTx(ctx, tx, id)
	result := Group{GroupSummary: summary, Members: make([]GroupMember, 0)}
	if err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT u.id,u.username,u.display_name,
		COALESCE(m.used_usd,0)::numeric(30,12)::text,
		CASE WHEN $3::numeric IS NULL THEN NULL ELSE GREATEST($3::numeric-COALESCE(m.used_usd,0),0)::numeric(30,12)::text END
		FROM billing_accounts a JOIN users u ON u.id=a.user_id
		LEFT JOIN group_member_usage m ON m.period_id=$2 AND m.user_id=u.id
		WHERE a.group_id=$1 ORDER BY u.username,u.id`, id, summary.PeriodID, summary.MemberLimitUSD)
	if err != nil {
		return result, mapDBError("list group members", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.UserID, &m.Username, &m.DisplayName, &m.UsedUSD, &m.RemainingUSD); err != nil {
			return result, fmt.Errorf("scan group member: %w", err)
		}
		result.Members = append(result.Members, m)
	}
	return result, rows.Err()
}

// Member counters live independently from the ledger so deleting history or
// temporarily removing a member never restores the member's current allowance.
func groupMemberUsedTx(ctx context.Context, tx *sql.Tx, periodID, userID string) (string, error) {
	var used string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT used_usd FROM group_member_usage WHERE period_id=$1 AND user_id=$2),0)::numeric(30,12)::text`, periodID, userID).Scan(&used)
	return used, mapDBError("read group member usage", err)
}

func groupMemberRemainingTx(ctx context.Context, tx *sql.Tx, periodID, userID string, memberLimit *string) (*string, error) {
	if memberLimit == nil {
		return nil, nil
	}
	var remaining string
	err := tx.QueryRowContext(ctx, `SELECT GREATEST($3::numeric-COALESCE((SELECT used_usd FROM group_member_usage WHERE period_id=$1 AND user_id=$2),0),0)::numeric(30,12)::text`, periodID, userID, *memberLimit).Scan(&remaining)
	if err != nil {
		return nil, mapDBError("read group member remaining", err)
	}
	return &remaining, nil
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
			return decodeGroupOperationResponse(encoded, &result)
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

func decodeGroupOperationResponse(encoded []byte, result *Group) error {
	if err := json.Unmarshal(encoded, result); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return err
	}
	if _, present := fields["period_count"]; !present {
		// Replays describe the original operation, not today's group state.
		// Historical responses predate renewal limits and become their own 1/1.
		result.PeriodCount = 1
		result.CurrentPeriodNumber = 1
		expires := result.PeriodEndsAt
		result.ExpiresAt = &expires
	}
	return nil
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
	if params.MemberLimitUSD != nil {
		memberLimit, err := decimal.ParseInput(*params.MemberLimitUSD, false, true)
		if err != nil {
			return Group{}, fmt.Errorf("%w: invalid group member limit", ErrInvalid)
		}
		params.MemberLimitUSD = &memberLimit
		params.MemberLimitSet = true
	}
	duration, err := groupPeriodDuration(params.Period, params.CustomDays)
	if err != nil {
		return Group{}, err
	}
	params.At = normalizedBillingTime(params.At, s.now)
	if err := validateGroupTime(params.At); err != nil {
		return Group{}, err
	}
	if params.PeriodCount != nil && (*params.PeriodCount < 0 || *params.PeriodCount > 99) {
		return Group{}, fmt.Errorf("%w: group period count must be between 0 and 99", ErrInvalid)
	}
	startFingerprint := ""
	if params.StartsAt != nil {
		if err := validateGroupTime(*params.StartsAt); err != nil {
			return Group{}, err
		}
		startFingerprint = params.StartsAt.UTC().Format(time.RFC3339Nano)
	}
	fingerprintValues := []string{params.GroupID, params.Name, amount, params.Period, fmt.Sprint(params.CustomDays), startFingerprint}
	if params.MemberLimitSet {
		memberLimitFingerprint := "null"
		if params.MemberLimitUSD != nil {
			memberLimitFingerprint = *params.MemberLimitUSD
		}
		fingerprintValues = append(fingerprintValues, "member_limit_usd", memberLimitFingerprint)
	}
	if params.PeriodCount != nil {
		fingerprintValues = append(fingerprintValues, "period_count", fmt.Sprint(*params.PeriodCount))
	}
	return s.groupWrite(ctx, params.BillingWriteParams, "put", fingerprintValues, func(tx *sql.Tx) (Group, error) {
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
			g = GroupSummary{ID: id, Name: params.Name, LimitUSD: amount, MemberLimitUSD: params.MemberLimitUSD, Period: params.Period,
				CustomDays: params.CustomDays, StartsAt: start, PeriodCount: 1, CurrentPeriodNumber: 1}
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
		if params.MemberLimitSet {
			g.MemberLimitUSD = params.MemberLimitUSD
		}
		if params.PeriodCount != nil {
			g.PeriodCount = *params.PeriodCount
		}
		g.LimitUSD = amount
		if reset {
			// A past anchor selects its current cycle; edits always create a fresh
			// period numbered 1, so the chosen cycle receives the entire term.
			g.CurrentPeriodNumber = 1
			g.PeriodStartsAt = groupPeriodStart(start, params.At, duration)
			g.PeriodEndsAt, err = addGroupPeriods(g.PeriodStartsAt, duration, 1)
			if err != nil {
				return Group{}, err
			}
		}
		g.ExpiresAt, err = groupExpiration(g.PeriodEndsAt, duration, g.PeriodCount, g.CurrentPeriodNumber)
		if err != nil {
			return Group{}, err
		}
		if create {
			if _, err := tx.ExecContext(ctx, `INSERT INTO user_groups(id,name,limit_usd,member_limit_usd,period,custom_days,starts_at,
				period_count,current_period_number,expires_at,created_at,updated_at)
				VALUES($1,$2,$3::numeric,$4::numeric,$5,$6,$7,$8,$9,$10,$11,$11)`, g.ID, params.Name, amount, g.MemberLimitUSD,
				params.Period, params.CustomDays, start, g.PeriodCount, g.CurrentPeriodNumber, g.ExpiresAt, params.At); err != nil {
				return Group{}, mapDBError("create group", err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE user_groups SET name=$2,limit_usd=$3::numeric,period=$4,custom_days=$5,starts_at=$6,
			updated_at=$7,member_limit_usd=$8::numeric,period_count=$9,current_period_number=$10,expires_at=$11 WHERE id=$1`,
			g.ID, params.Name, amount, params.Period, params.CustomDays, start, params.At, g.MemberLimitUSD, g.PeriodCount, g.CurrentPeriodNumber, g.ExpiresAt); err != nil {
			return Group{}, mapDBError("update group", err)
		}
		if reset {
			if err := replaceGroupPeriodTx(ctx, tx, g, g.PeriodStartsAt, g.PeriodEndsAt, params.At, params.At); err != nil {
				return Group{}, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE group_usage_periods SET limit_usd=$2::numeric,member_limit_usd=$3::numeric WHERE id=$1`, g.PeriodID, amount, g.MemberLimitUSD); err != nil {
				return Group{}, mapDBError("update group period limit", err)
			}
			// Extending an expired group resumes along its original timeline only
			// when the new expiry covers this instant; idle cycles still count.
			if _, err := rollGroupTx(ctx, tx, g.ID, params.At); err != nil {
				return Group{}, err
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
		for _, id := range ids {
			if err := lockNonPendingUserTx(ctx, tx, id); err != nil {
				return Group{}, err
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
	if g.ArchivedAt != nil || (g.ExpiresAt != nil && !at.Before(*g.ExpiresAt)) || !at.Before(g.PeriodEndsAt) {
		return &GroupQuotaExceededError{GroupID: g.ID}
	}
	if at.Before(g.PeriodStartsAt) {
		return &GroupQuotaExceededError{GroupID: g.ID, RetryAfter: g.PeriodStartsAt.Sub(at), NotStarted: true}
	}
	if !billingPositive(g.RemainingUSD) {
		err := &GroupQuotaExceededError{GroupID: g.ID}
		if groupHasNextPeriod(g) {
			err.RetryAfter = g.PeriodEndsAt.Sub(at)
		}
		return err
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
