package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

type ModelSupplier struct {
	ID         string   `json:"id"`
	Provider   string   `json:"-"`
	BaseURL    string   `json:"-"`
	Ciphertext []byte   `json:"-"`
	Models     []string `json:"models"`
	Enabled    bool     `json:"enabled"`
	Revision   int64    `json:"-"`
}

func (s *Store) ModelSuppliers(ctx context.Context) ([]ModelSupplier, error) {
	// #nosec G202 -- accountScopeSQL is fixed SQL over a placeholder name; the owner and every other value are bound parameters.
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.provider, m.base_url, m.key_ciphertext, m.models, a.status='available', m.revision
		FROM model_suppliers m JOIN upstream_accounts a ON a.id=m.account_id WHERE a.provider=$1 AND `+accountScopeSQL("a", "$2")+` ORDER BY a.id`, s.upstreamProviderName(), s.upstreamOwnerArg())
	if err != nil {
		return nil, mapDBError("list model suppliers", err)
	}
	defer rows.Close()
	var values []ModelSupplier
	for rows.Next() {
		var value ModelSupplier
		var models []byte
		if err := rows.Scan(&value.ID, &value.Provider, &value.BaseURL, &value.Ciphertext, &models, &value.Enabled, &value.Revision); err != nil {
			return nil, mapDBError("read model supplier", err)
		}
		if json.Unmarshal(models, &value.Models) != nil {
			return nil, fmt.Errorf("invalid stored supplier catalog")
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// CreateModelSupplier adds an API key supplier owned by the store's scope: a
// tenant's supplier counts towards its account limit.
func (s *Store) CreateModelSupplier(ctx context.Context, value ModelSupplier, nodeID string, actor EgressActor) error {
	models, err := json.Marshal(value.Models)
	if err != nil {
		return err
	}
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var bindingRevision int64
		if nodeID != "" {
			var err error
			bindingRevision, err = nextEgressVersion(ctx, tx)
			if err != nil {
				return err
			}
		}
		if s.upstreamOwner != "" {
			if err := checkTenantAccountLimitTx(ctx, tx, s.upstreamOwner); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO upstream_accounts(id, provider, display_name, masked_email, plan, status, created_at, updated_at, last_synced_at, credential_kind, owner_user_id)
			VALUES($1,$2,$3,'','unknown','available',now(),now(),now(),'supplier',$4::uuid)`, value.ID, s.upstreamProviderName(), "supplier-"+value.ID, s.upstreamOwnerArg()); err != nil {
			return mapDBError("create model supplier account", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_suppliers(account_id, base_url, key_ciphertext, models) VALUES($1,$2,$3,$4::jsonb)`, value.ID, value.BaseURL, value.Ciphertext, string(models)); err != nil {
			return mapDBError("store model supplier", err)
		}
		if nodeID != "" {
			if err := setEgressBinding(ctx, tx, s.upstreamProviderName(), value.ID, nodeID, bindingRevision); err != nil {
				return err
			}
			return egressAudit(ctx, tx, actor, "upstream_account.egress_changed", value.ID, map[string]any{"provider": s.upstreamProviderName(), "node_id": nodeID, "revision": bindingRevision})
		}
		return nil
	})
}

func (s *Store) ReplaceModelSupplierKey(ctx context.Context, value ModelSupplier) error {
	models, err := json.Marshal(value.Models)
	if err != nil {
		return err
	}
	// #nosec G202 -- accountScopeSQL is fixed SQL over a placeholder name; the owner and every other value are bound parameters.
	result, err := s.db.ExecContext(ctx, `UPDATE model_suppliers m SET key_ciphertext=$1, models=$2::jsonb, revision=revision+1
		FROM upstream_accounts a WHERE m.account_id=a.id AND a.id=$3 AND a.provider=$4 AND `+accountScopeSQL("a", "$6")+` AND m.revision=$5`, value.Ciphertext, string(models), value.ID, s.upstreamProviderName(), value.Revision, s.upstreamOwnerArg())
	return supplierWriteResult(result, err)
}

func (s *Store) SetModelSupplierEnabled(ctx context.Context, id string, enabled bool) error {
	status := UpstreamAccountStatusUnavailable
	if enabled {
		status = UpstreamAccountStatusAvailable
	}
	// #nosec G202 -- accountScopeSQL is fixed SQL over a placeholder name; the owner and every other value are bound parameters.
	result, err := s.db.ExecContext(ctx, `UPDATE upstream_accounts SET status=$1, updated_at=now()
		WHERE id=$2 AND provider=$3 AND credential_kind='supplier' AND `+accountScopeSQL("", "$4")+` AND EXISTS(SELECT 1 FROM model_suppliers WHERE account_id=$2)`, status, id, s.upstreamProviderName(), s.upstreamOwnerArg())
	return supplierWriteResult(result, err)
}

// DeleteModelSupplier removes a disabled supplier and releases its account
// row, which stays for usage history.
func (s *Store) DeleteModelSupplier(ctx context.Context, id string) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := nextEgressVersion(ctx, tx); err != nil {
			return err
		}
		// #nosec G202 -- accountScopeSQL is fixed SQL over a placeholder name; the owner and every other value are bound parameters.
		result, err := tx.ExecContext(ctx, `DELETE FROM model_suppliers m USING upstream_accounts a
			WHERE m.account_id=a.id AND a.id=$1 AND a.provider=$2 AND `+accountScopeSQL("a", "$3")+` AND a.status='unavailable'`, id, s.upstreamProviderName(), s.upstreamOwnerArg())
		if err := supplierWriteResult(result, err); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM upstream_account_egress WHERE account_id=$1`, id); err != nil {
			return mapDBError("release supplier egress", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE upstream_accounts SET credential_removed_at=now() WHERE id=$1`, id)
		return mapDBError("release supplier account", err)
	})
}

func supplierWriteResult(result sql.Result, err error) error {
	if err != nil {
		return mapDBError("write model supplier", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}
