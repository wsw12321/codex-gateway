package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/billing"
)

// MarketplaceSettings is the Owner's switch and limits for the aggregation
// gateway: tenant upstream accounts, seller plans and the plan marketplace.
// CommissionRate is an exact decimal string in [0, 1] taken from each sale.
type MarketplaceSettings struct {
	Enabled              bool   `json:"enabled"`
	CommissionRate       string `json:"commission_rate"`
	MaxAccountsPerUser   int    `json:"max_accounts_per_user"`
	MaxPlansPerUser      int    `json:"max_plans_per_user"`
	AllowMemberSuppliers bool   `json:"allow_member_suppliers"`
	// MaxEgressServersPerUser caps the egress nodes one tenant may run.
	MaxEgressServersPerUser int       `json:"max_egress_servers_per_user"`
	Revision                int64     `json:"revision"`
	UpdatedAt               time.Time `json:"updated_at"`
}

const (
	MaxMarketplaceAccountsPerUser      = 256
	MaxMarketplacePlansPerUser         = 100
	MaxMarketplaceEgressServersPerUser = 32
)

type SetMarketplaceSettingsParams struct {
	Enabled              bool
	CommissionRate       string
	MaxAccountsPerUser   int
	MaxPlansPerUser      int
	AllowMemberSuppliers bool
	// MaxEgressServersPerUser is 0–32.
	MaxEgressServersPerUser int
	// ExpectedRevision must match the stored revision so a stale page cannot
	// silently undo a newer change.
	ExpectedRevision int64
	ActorUserID      string
	ActorSessionID   string
	RequestID        string
	SourceIP         string
	At               time.Time
}

// NormalizeCommissionRate returns the canonical form of a commission rate:
// a decimal in [0, 1] with at most six fractional digits.
func NormalizeCommissionRate(value string) (string, error) {
	canonical, err := billing.ParseInput(value, false, true)
	if err != nil {
		return "", fmt.Errorf("%w: invalid commission rate", ErrInvalid)
	}
	rate, ok := new(big.Rat).SetString(canonical)
	if !ok || rate.Cmp(big.NewRat(1, 1)) > 0 {
		return "", fmt.Errorf("%w: commission rate must be between 0 and 1", ErrInvalid)
	}
	return canonical, nil
}

const marketplaceSettingsColumns = `enabled, trim_scale(commission_rate)::text, max_accounts_per_user,
	max_plans_per_user, allow_member_suppliers, max_egress_servers_per_user, revision, updated_at`

func scanMarketplaceSettings(row rowScanner) (MarketplaceSettings, error) {
	var settings MarketplaceSettings
	err := row.Scan(&settings.Enabled, &settings.CommissionRate, &settings.MaxAccountsPerUser,
		&settings.MaxPlansPerUser, &settings.AllowMemberSuppliers, &settings.MaxEgressServersPerUser,
		&settings.Revision, &settings.UpdatedAt)
	return settings, err
}

// MarketplaceSettings reads the switch. Callers treat an error as "disabled
// for tenant features" (fail closed); platform paths never depend on it.
func (s *Store) MarketplaceSettings(ctx context.Context) (MarketplaceSettings, error) {
	settings, err := scanMarketplaceSettings(s.db.QueryRowContext(ctx,
		`SELECT `+marketplaceSettingsColumns+` FROM marketplace_settings WHERE singleton`))
	return settings, mapDBError("read marketplace settings", err)
}

func (s *Store) SetMarketplaceSettings(ctx context.Context, params SetMarketplaceSettingsParams) (MarketplaceSettings, error) {
	rate, err := NormalizeCommissionRate(params.CommissionRate)
	if err != nil {
		return MarketplaceSettings{}, err
	}
	if params.ExpectedRevision < 1 || strings.TrimSpace(params.ActorUserID) == "" ||
		params.MaxAccountsPerUser < 0 || params.MaxAccountsPerUser > MaxMarketplaceAccountsPerUser ||
		params.MaxPlansPerUser < 0 || params.MaxPlansPerUser > MaxMarketplacePlansPerUser ||
		params.MaxEgressServersPerUser < 0 || params.MaxEgressServersPerUser > MaxMarketplaceEgressServersPerUser {
		return MarketplaceSettings{}, fmt.Errorf("%w: invalid marketplace settings", ErrInvalid)
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	var settings MarketplaceSettings
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		previous, err := scanMarketplaceSettings(tx.QueryRowContext(ctx,
			`SELECT `+marketplaceSettingsColumns+` FROM marketplace_settings WHERE singleton FOR UPDATE`))
		if err != nil {
			return mapDBError("lock marketplace settings", err)
		}
		if previous.Revision != params.ExpectedRevision {
			return fmt.Errorf("%w: marketplace settings revision changed", ErrConflict)
		}
		settings, err = scanMarketplaceSettings(tx.QueryRowContext(ctx, `UPDATE marketplace_settings
			SET enabled=$1, commission_rate=$2::numeric, max_accounts_per_user=$3, max_plans_per_user=$4,
				allow_member_suppliers=$5, max_egress_servers_per_user=$8, revision=revision+1,
				updated_by_user_id=$6::uuid, updated_at=$7
			WHERE singleton RETURNING `+marketplaceSettingsColumns,
			params.Enabled, rate, params.MaxAccountsPerUser, params.MaxPlansPerUser,
			params.AllowMemberSuppliers, params.ActorUserID, params.At, params.MaxEgressServersPerUser))
		if err != nil {
			return mapDBError("update marketplace settings", err)
		}
		metadata, err := marshalSafeMetadata(map[string]any{
			"previous_enabled":         previous.Enabled,
			"enabled":                  settings.Enabled,
			"previous_commission_rate": previous.CommissionRate,
			"commission_rate":          settings.CommissionRate,
			"max_accounts_per_user":    settings.MaxAccountsPerUser,
			"max_plans_per_user":       settings.MaxPlansPerUser,
			"allow_member_suppliers":   settings.AllowMemberSuppliers,
			"max_egress_servers":       settings.MaxEgressServersPerUser,
			"revision":                 settings.Revision,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity,
				success, source_ip, subject_type, subject_id, request_id, metadata)
			VALUES ($1,$2,$3,'gateway.marketplace_settings_changed','warning',true,
				$4::inet,'gateway_setting','marketplace',$5,$6::jsonb)`,
			params.At, params.ActorUserID, valueOrNil(params.ActorSessionID),
			valueOrNil(params.SourceIP), valueOrNil(params.RequestID), metadata)
		return mapDBError("audit marketplace settings", err)
	})
	if err != nil {
		return MarketplaceSettings{}, err
	}
	return settings, nil
}
