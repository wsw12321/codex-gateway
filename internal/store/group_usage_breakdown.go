package store

import (
	"context"
	"fmt"
	"math/big"
	"time"
)

// GroupUsageMember is one user's group-funded spend in a period. Users who
// left the group keep their row (CurrentMember=false), so the parts still add
// up to the period's used amount.
type GroupUsageMember struct {
	UserID        string `json:"user_id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name"`
	CurrentMember bool   `json:"current_member"`
	GroupUsedUSD  string `json:"group_used_usd"`
}

// GroupUsageAccountUser is one member's cost on an account in the period.
type GroupUsageAccountUser struct {
	UserID  string `json:"user_id"`
	CostUSD string `json:"cost_usd"`
}

// GroupUsageAccount splits an account's cost in the period window between
// the group's members (current or with group usage in the period) and
// everyone else. Costs are settled actual_cost_usd, as on the account page.
type GroupUsageAccount struct {
	AccountID        string                  `json:"account_id"`
	Provider         string                  `json:"provider"`
	DisplayName      string                  `json:"display_name,omitempty"`
	Alias            string                  `json:"alias,omitempty"`
	EmailMasked      string                  `json:"email_masked,omitempty"`
	InPool           bool                    `json:"in_pool"`
	TotalCostUSD     string                  `json:"total_cost_usd"`
	MemberCostUSD    string                  `json:"member_cost_usd"`
	NonMemberCostUSD string                  `json:"non_member_cost_usd"`
	GroupChargedUSD  string                  `json:"group_charged_usd"`
	Members          []GroupUsageAccountUser `json:"members"`
}

type GroupUsagePeriodRef struct {
	ID       string    `json:"id"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

type GroupUsageBreakdown struct {
	GroupID        string                `json:"group_id"`
	Period         GroupUsagePeriodRef   `json:"period"`
	CurrentPeriod  bool                  `json:"current_period"`
	LimitUSD       string                `json:"limit_usd"`
	MemberLimitUSD *string               `json:"member_limit_usd"`
	UsedUSD        string                `json:"used_usd"`
	RemainingUSD   string                `json:"remaining_usd"`
	Members        []GroupUsageMember    `json:"members"`
	Accounts       []GroupUsageAccount   `json:"accounts"`
	Periods        []GroupUsagePeriodRef `json:"periods"`
	DataComplete   bool                  `json:"data_complete"`
}

// maxGroupUsageBreakdownPeriods bounds the period picker offered with a breakdown.
const maxGroupUsageBreakdownPeriods = 24

// GroupUsageBreakdown describes one group period: each user's share of the
// group quota, and for every account in the group's selected pools or paid
// for by the group in that period, the cost of each member versus everyone
// else. An empty periodID selects the current period (after rolling it).
func (s *Store) GroupUsageBreakdown(ctx context.Context, groupID, periodID string) (GroupUsageBreakdown, error) {
	var result GroupUsageBreakdown
	// GetGroup rolls the period forward and lists the current members.
	group, err := s.GetGroup(ctx, groupID)
	if err != nil {
		return result, err
	}
	if periodID == "" {
		periodID = group.PeriodID
	}
	if periodID, err = canonicalGroupID(periodID); err != nil {
		return result, err
	}
	result = GroupUsageBreakdown{GroupID: group.ID, CurrentPeriod: periodID == group.PeriodID,
		Members: []GroupUsageMember{}, Accounts: []GroupUsageAccount{}, Periods: []GroupUsagePeriodRef{}}
	err = s.db.QueryRowContext(ctx, `SELECT id,starts_at,ends_at,limit_usd::text,member_limit_usd::text,used_usd::text,
		GREATEST(limit_usd-used_usd,0)::numeric(30,12)::text
		FROM group_usage_periods WHERE group_id=$1 AND id=$2`, group.ID, periodID).Scan(
		&result.Period.ID, &result.Period.StartsAt, &result.Period.EndsAt, &result.LimitUSD, &result.MemberLimitUSD,
		&result.UsedUSD, &result.RemainingUSD)
	if err != nil {
		return result, mapDBError("read group usage period", err)
	}
	if result.CurrentPeriod {
		// The live group row is authoritative for the period being spent and
		// matches the group metrics shown beside this breakdown.
		result.LimitUSD, result.MemberLimitUSD = group.LimitUSD, group.MemberLimitUSD
		result.UsedUSD, result.RemainingUSD = group.UsedUSD, group.RemainingUSD
	}

	current := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		current = append(current, member.UserID)
	}
	encodedCurrent, err := marshalStringArray(current)
	if err != nil {
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH current_members AS (SELECT value::uuid id FROM jsonb_array_elements_text($2::jsonb)),
		ids AS (SELECT id FROM current_members UNION SELECT user_id FROM group_member_usage WHERE period_id=$1)
		SELECT u.id,u.username,u.display_name,u.id IN (SELECT id FROM current_members),
			COALESCE(m.used_usd,0)::numeric(30,12)::text
		FROM ids JOIN users u ON u.id=ids.id
		LEFT JOIN group_member_usage m ON m.period_id=$1 AND m.user_id=u.id
		ORDER BY COALESCE(m.used_usd,0) DESC, lower(u.username), u.id`, result.Period.ID, encodedCurrent)
	if err != nil {
		return result, mapDBError("list group usage members", err)
	}
	members := map[string]bool{}
	for rows.Next() {
		var member GroupUsageMember
		if err := rows.Scan(&member.UserID, &member.Username, &member.DisplayName, &member.CurrentMember, &member.GroupUsedUSD); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("scan group usage member: %w", err)
		}
		result.Members = append(result.Members, member)
		members[member.UserID] = true
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := rows.Err(); err != nil {
		return result, mapDBError("list group usage members", err)
	}

	if err := s.groupUsageAccounts(ctx, &result, members); err != nil {
		return result, err
	}

	var incomplete bool
	if err := s.db.QueryRowContext(ctx, `SELECT p.used_usd>0 AND NOT EXISTS (SELECT 1 FROM billing_ledger_entries l WHERE l.group_period_id=p.id)
		FROM group_usage_periods p WHERE p.id=$1`, result.Period.ID).Scan(&incomplete); err != nil {
		return result, mapDBError("check group usage completeness", err)
	}
	result.DataComplete = !incomplete

	periods, err := s.db.QueryContext(ctx, `SELECT id,starts_at,ends_at FROM group_usage_periods WHERE group_id=$1
		ORDER BY starts_at DESC,id DESC LIMIT $2`, group.ID, maxGroupUsageBreakdownPeriods)
	if err != nil {
		return result, mapDBError("list group usage periods", err)
	}
	defer periods.Close()
	for periods.Next() {
		var period GroupUsagePeriodRef
		if err := periods.Scan(&period.ID, &period.StartsAt, &period.EndsAt); err != nil {
			return result, fmt.Errorf("scan group usage period: %w", err)
		}
		result.Periods = append(result.Periods, period)
	}
	return result, mapDBError("list group usage periods", periods.Err())
}

// groupUsageAccounts fills result.Accounts for the accounts in the group's
// selected pools plus any account the group paid for in the period.
func (s *Store) groupUsageAccounts(ctx context.Context, result *GroupUsageBreakdown, members map[string]bool) error {
	rows, err := s.db.QueryContext(ctx, `WITH pool AS (
			SELECT ga.upstream_account_id id FROM group_upstream_account_policies gp
			JOIN group_upstream_accounts ga ON ga.group_id=gp.group_id AND ga.provider=gp.provider
			WHERE gp.group_id=$1 AND gp.strategy='selected'),
		paid AS (SELECT DISTINCT l.upstream_account_id id FROM billing_ledger_entries l
			WHERE l.group_period_id=$2 AND l.entry_type='usage_charge' AND l.upstream_account_id IS NOT NULL),
		accounts AS (SELECT id FROM pool UNION SELECT id FROM paid),
		costs AS (SELECT l.upstream_account_id account_id,l.user_id,
				COALESCE(sum(l.actual_cost_usd),0) cost,
				COALESCE(sum(l.group_charged_usd) FILTER (WHERE l.group_period_id=$2),0) charged
			FROM billing_ledger_entries l
			WHERE l.entry_type='usage_charge' AND l.upstream_account_id IN (SELECT id FROM accounts)
				AND COALESCE(l.usage_requested_at,l.created_at)>=$3 AND COALESCE(l.usage_requested_at,l.created_at)<$4
			GROUP BY l.upstream_account_id,l.user_id)
		SELECT a.id,a.provider,a.display_name,a.alias,a.masked_email,a.id IN (SELECT id FROM pool),
			c.user_id,COALESCE(c.cost,0)::numeric(30,12)::text,COALESCE(c.charged,0)::numeric(30,12)::text
		FROM accounts ac JOIN upstream_accounts a ON a.id=ac.id
		LEFT JOIN costs c ON c.account_id=a.id
		ORDER BY a.provider,a.id,c.user_id`, result.GroupID, result.Period.ID, result.Period.StartsAt, result.Period.EndsAt)
	if err != nil {
		return mapDBError("calculate group account usage breakdown", err)
	}
	defer rows.Close()
	type totals struct{ total, member, other, charged *big.Rat }
	var sums []totals
	for rows.Next() {
		var account GroupUsageAccount
		var userID *string
		var cost, charged string
		if err := rows.Scan(&account.AccountID, &account.Provider, &account.DisplayName, &account.Alias, &account.EmailMasked,
			&account.InPool, &userID, &cost, &charged); err != nil {
			return fmt.Errorf("scan group account usage: %w", err)
		}
		last := len(result.Accounts) - 1
		if last < 0 || result.Accounts[last].AccountID != account.AccountID {
			account.Members = []GroupUsageAccountUser{}
			result.Accounts = append(result.Accounts, account)
			sums = append(sums, totals{new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat)})
			last++
		}
		if userID == nil {
			continue
		}
		costValue, ok := new(big.Rat).SetString(cost)
		chargedValue, okCharged := new(big.Rat).SetString(charged)
		if !ok || !okCharged {
			return fmt.Errorf("group account usage amount is not a decimal")
		}
		sum := sums[last]
		sum.total.Add(sum.total, costValue)
		sum.charged.Add(sum.charged, chargedValue)
		if members[*userID] {
			sum.member.Add(sum.member, costValue)
			result.Accounts[last].Members = append(result.Accounts[last].Members, GroupUsageAccountUser{UserID: *userID, CostUSD: cost})
		} else {
			sum.other.Add(sum.other, costValue)
		}
	}
	if err := rows.Err(); err != nil {
		return mapDBError("calculate group account usage breakdown", err)
	}
	for index := range result.Accounts {
		account := &result.Accounts[index]
		account.TotalCostUSD = sums[index].total.FloatString(12)
		account.MemberCostUSD = sums[index].member.FloatString(12)
		account.NonMemberCostUSD = sums[index].other.FloatString(12)
		account.GroupChargedUSD = sums[index].charged.FloatString(12)
	}
	return nil
}
