package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// The same bounds apply at the internal HTTP boundary and database boundary.
const MaxUpstreamAllocationCandidates = 256

const MaxUpstreamAllocationWeight = 1<<31 - 1

var ErrNoUpstreamAccount = errors.New("store: no upstream account accepts new assignments")

type SetUpstreamAccountAllocationWeightParams struct {
	AccountID      string
	Weight         int
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

// UpstreamAccountAllocation contains rolling usage independently of the
// historical dashboard interval. Shares are decimal fractions, not percents.
// CostShare includes spending on unavailable and zero-weight accounts;
// TargetShare is a reference over currently available positive-weight accounts.
type UpstreamAccountAllocation struct {
	AccountID        string
	AllocationWeight int
	CostUSD          string
	CostShare        string
	TargetShare      string
}

func (s *Store) SetUpstreamAccountAllocationWeight(ctx context.Context, params SetUpstreamAccountAllocationWeightParams) (UpstreamAccount, error) {
	var result UpstreamAccount
	if !upstreamAccountIDPattern.MatchString(params.AccountID) || params.Weight < 0 ||
		params.Weight > MaxUpstreamAllocationWeight || strings.TrimSpace(params.ActorUserID) == "" {
		return result, fmt.Errorf("%w: invalid allocation weight, account, or actor", ErrInvalid)
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// Acquire the UPDATE table lock before a row lock: synchronization takes
		// SHARE ROW EXCLUSIVE first, so taking these in the opposite order could
		// deadlock an allocation edit with a metadata snapshot.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock upstream account allocation edit", err)
		}
		var previousWeight int
		if err := tx.QueryRowContext(ctx, `SELECT allocation_weight FROM upstream_accounts
			WHERE id = $1 FOR UPDATE`, params.AccountID).Scan(&previousWeight); err != nil {
			return mapDBError("lock upstream account allocation weight", err)
		}
		var err error
		result, err = scanUpstreamAccount(tx.QueryRowContext(ctx, `UPDATE upstream_accounts
			SET allocation_weight = $2 WHERE id = $1 RETURNING `+upstreamAccountColumns,
			params.AccountID, params.Weight))
		if err != nil {
			return mapDBError("set upstream account allocation weight", err)
		}
		// updated_at belongs to authoritative metadata synchronization. A local
		// preference edit must not suppress an otherwise-current sidecar snapshot.
		metadata, err := marshalSafeMetadata(map[string]any{
			"previous_weight": previousWeight, "weight": params.Weight,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity,
			 success, source_ip, subject_type, subject_id, request_id, metadata)
			VALUES ($1,$2,$3,'upstream_account.allocation_weight_changed','info',true,
			 $4::inet,'upstream_account',$5,$6,$7::jsonb)`,
			params.At, params.ActorUserID, valueOrNil(params.ActorSessionID),
			valueOrNil(params.SourceIP), params.AccountID, valueOrNil(params.RequestID), metadata)
		return mapDBError("audit upstream account allocation weight", err)
	})
	if err != nil {
		return UpstreamAccount{}, err
	}
	return result, nil
}

// SelectUpstreamAccount uses one PostgreSQL statement/snapshot for preferences
// and settled costs. The sidecar owns model support and live availability; a
// never-synchronized candidate therefore has weight one and no historical cost.
// Selection does not create placeholders, attribution, or reservations.
func (s *Store) SelectUpstreamAccount(ctx context.Context, userID string, candidateIDs []string, at time.Time) (string, error) {
	if _, _, err := upstreamCandidateArguments(userID, candidateIDs); err != nil {
		return "", err
	}
	if len(candidateIDs) == 0 || len(candidateIDs) > MaxUpstreamAllocationCandidates {
		return "", fmt.Errorf("%w: invalid upstream candidate count", ErrInvalid)
	}
	if at.IsZero() {
		at = s.now().UTC()
	} else {
		at = at.UTC()
	}
	args := []any{at.Add(-24 * time.Hour), at, userID}
	values := make([]string, len(candidateIDs))
	seen := make(map[string]struct{}, len(candidateIDs))
	for i, id := range candidateIDs {
		if !upstreamAccountIDPattern.MatchString(id) {
			return "", fmt.Errorf("%w: noncanonical upstream candidate", ErrInvalid)
		}
		if _, exists := seen[id]; exists {
			return "", fmt.Errorf("%w: duplicate upstream candidate", ErrInvalid)
		}
		seen[id] = struct{}{}
		args = append(args, id)
		values[i] = fmt.Sprintf("($%d::text)", i+4)
	}
	rows, err := s.db.QueryContext(ctx, `WITH candidates(id) AS (VALUES `+strings.Join(values, ",")+`)
		SELECT c.id, COALESCE(a.allocation_weight, 1), COALESCE(l.cost, '0')
		FROM candidates c
		LEFT JOIN upstream_accounts a ON a.id = c.id
		LEFT JOIN LATERAL (
			SELECT COALESCE(sum(actual_cost_usd), 0::numeric)::text cost
			FROM billing_ledger_entries
			WHERE entry_type = 'usage_charge' AND upstream_account_id = a.id
			  AND COALESCE(usage_requested_at, created_at) >= $1
			  AND COALESCE(usage_requested_at, created_at) < $2
		) l ON true
		WHERE EXISTS(SELECT 1 FROM users WHERE id=$3::uuid AND status='active')
		  AND (a.id IS NULL OR a.access_mode='shared' OR EXISTS(SELECT 1 FROM upstream_account_users u WHERE u.upstream_account_id=a.id AND u.user_id=$3::uuid))`, args...)
	if err != nil {
		return "", mapDBError("read upstream allocation snapshot", err)
	}
	defer rows.Close()
	candidates := make([]upstreamAllocationCandidate, 0, len(candidateIDs))
	for rows.Next() {
		var candidate upstreamAllocationCandidate
		if err := rows.Scan(&candidate.id, &candidate.weight, &candidate.cost); err != nil {
			return "", fmt.Errorf("scan upstream allocation snapshot: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate upstream allocation snapshot: %w", err)
	}
	return selectUpstreamAllocation(candidates, func(limit int64) (int64, error) {
		value, err := rand.Int(rand.Reader, big.NewInt(limit))
		if err != nil {
			return 0, err
		}
		return value.Int64(), nil
	})
}

type upstreamAllocationCandidate struct {
	id     string
	weight int
	cost   string
}

// Comparing weight*totalCost - cost*totalWeight is equivalent to comparing
// deficits, without division or rounding. Decimal ledger values are parsed
// into exact rationals, and all-zero/tied history uses weighted randomness.
func selectUpstreamAllocation(candidates []upstreamAllocationCandidate, draw func(int64) (int64, error)) (string, error) {
	if len(candidates) > MaxUpstreamAllocationCandidates {
		return "", fmt.Errorf("%w: too many upstream candidates", ErrInvalid)
	}
	totalCost := new(big.Rat)
	var totalWeight int64
	costs := make([]*big.Rat, len(candidates))
	for i, candidate := range candidates {
		if candidate.weight < 0 || candidate.weight > MaxUpstreamAllocationWeight {
			return "", fmt.Errorf("%w: invalid upstream allocation weight", ErrInvalid)
		}
		if candidate.weight == 0 {
			continue
		}
		cost, ok := new(big.Rat).SetString(candidate.cost)
		if !ok || cost.Sign() < 0 {
			return "", fmt.Errorf("invalid upstream allocation cost")
		}
		costs[i] = cost
		totalCost.Add(totalCost, cost)
		totalWeight += int64(candidate.weight)
	}
	if totalWeight == 0 {
		return "", ErrNoUpstreamAccount
	}
	var largest *big.Rat
	ties := make([]upstreamAllocationCandidate, 0, len(candidates))
	var tiedWeight int64
	for i, candidate := range candidates {
		if candidate.weight == 0 {
			continue
		}
		deficit := new(big.Rat).Mul(totalCost, new(big.Rat).SetInt64(int64(candidate.weight)))
		deficit.Sub(deficit, new(big.Rat).Mul(costs[i], new(big.Rat).SetInt64(totalWeight)))
		comparison := 1
		if largest != nil {
			comparison = deficit.Cmp(largest)
		}
		if comparison < 0 {
			continue
		}
		if comparison > 0 {
			largest = deficit
			ties = ties[:0]
			tiedWeight = 0
		}
		ties = append(ties, candidate)
		tiedWeight += int64(candidate.weight)
	}
	if len(ties) == 1 {
		return ties[0].id, nil
	}
	chosen, err := draw(tiedWeight)
	if err != nil {
		return "", fmt.Errorf("randomize tied upstream allocations: %w", err)
	}
	if chosen < 0 || chosen >= tiedWeight {
		return "", fmt.Errorf("invalid upstream allocation random result")
	}
	for _, candidate := range ties {
		if chosen < int64(candidate.weight) {
			return candidate.id, nil
		}
		chosen -= int64(candidate.weight)
	}
	return "", ErrNoUpstreamAccount
}

func (s *Store) ListUpstreamAccountAllocations(ctx context.Context, at time.Time) ([]UpstreamAccountAllocation, error) {
	if at.IsZero() {
		at = s.now().UTC()
	} else {
		at = at.UTC()
	}
	rows, err := s.db.QueryContext(ctx, `WITH account_costs AS (
		SELECT a.id, a.allocation_weight, a.status,
			COALESCE(sum(l.actual_cost_usd), 0::numeric) cost
		FROM upstream_accounts a
		LEFT JOIN billing_ledger_entries l ON l.upstream_account_id = a.id
			AND l.entry_type = 'usage_charge'
			AND COALESCE(l.usage_requested_at, l.created_at) >= $1
			AND COALESCE(l.usage_requested_at, l.created_at) < $2
		GROUP BY a.id
	), totals AS (
		SELECT COALESCE(sum(cost), 0::numeric) cost,
			COALESCE(sum(allocation_weight) FILTER (WHERE status = 'available'), 0)::numeric weight
		FROM account_costs
	)
	SELECT a.id, a.allocation_weight, a.cost::text,
		CASE WHEN t.cost > 0 THEN (a.cost / t.cost)::text ELSE '0' END,
		CASE WHEN a.status = 'available' AND t.weight > 0
			THEN (a.allocation_weight::numeric / t.weight)::text ELSE '0' END
	FROM account_costs a CROSS JOIN totals t ORDER BY a.id`, at.Add(-24*time.Hour), at)
	if err != nil {
		return nil, mapDBError("list upstream allocation statistics", err)
	}
	defer rows.Close()
	result := make([]UpstreamAccountAllocation, 0)
	for rows.Next() {
		var value UpstreamAccountAllocation
		if err := rows.Scan(&value.AccountID, &value.AllocationWeight, &value.CostUSD,
			&value.CostShare, &value.TargetShare); err != nil {
			return nil, fmt.Errorf("scan upstream allocation statistics: %w", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate upstream allocation statistics: %w", err)
	}
	return result, nil
}
