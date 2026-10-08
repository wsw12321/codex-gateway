package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
)

var modelPriceNamePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var modelPriceStructurePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type ModelPrice struct {
	Model           string               `json:"model"`
	SchemaVersion   int                  `json:"schema_version"`
	ConfiguredPrice config.ModelPricing  `json:"configured_price"`
	EffectivePrice  *config.ModelPricing `json:"effective_price"`
	Multiplier      string               `json:"multiplier"`
	Version         int64                `json:"version"`
	StructureID     string               `json:"structure_id"`
	Source          string               `json:"source"`
	Conflict        bool                 `json:"conflict"`
	Editable        bool                 `json:"editable"`
	UpdatedAt       *time.Time           `json:"updated_at"`
}

type SetModelPriceParams struct {
	BillingWriteParams
	Pricing     config.UsagePricing
	Model       string
	Action      string
	Version     int64
	StructureID string
	Price       *config.ModelPricing
}

type storedModelPrice struct {
	price       []byte
	structureID string
	version     int64
	updatedAt   *time.Time
}

func modelPriceValue(pricing config.UsagePricing, model, multiplier string, stored storedModelPrice) (ModelPrice, error) {
	if _, configured := pricing.Models[model]; model == config.InternalGovernanceModel && !configured {
		pricing = config.UsagePricing{SchemaVersion: config.PricingSchemaV1, Models: map[string]config.ModelPricing{
			model: {InputUSDPerMillion: "0", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0"},
		}}
	}
	value := ModelPrice{Model: model, SchemaVersion: pricing.SchemaVersion, Multiplier: multiplier,
		Version: stored.version, UpdatedAt: stored.updatedAt, Source: "config", Editable: model != config.InternalGovernanceModel}
	if value.SchemaVersion == 0 {
		value.SchemaVersion = config.PricingSchemaV1
	}
	var err error
	value.StructureID, err = pricing.ModelPriceStructureID(model)
	if err != nil {
		return value, err
	}
	value.ConfiguredPrice, err = config.NormalizeModelPrice(value.SchemaVersion, model, pricing.Models[model])
	if err != nil {
		return value, fmt.Errorf("validate configured model prices: %w", err)
	}
	effective := value.ConfiguredPrice
	value.EffectivePrice = &effective
	if model == config.InternalGovernanceModel {
		if value.SchemaVersion == config.PricingSchemaV1 {
			effective.InputUSDPerMillion, effective.CachedInputUSDPerMillion, effective.OutputUSDPerMillion = "0", "0", "0"
		} else {
			for _, contexts := range effective.ServiceTiers {
				for _, price := range []*config.TokenPricing{contexts.Short, contexts.Long} {
					if price == nil {
						continue
					}
					price.InputUSDPerMillion, price.CachedInputUSDPerMillion, price.OutputUSDPerMillion = "0", "0", "0"
					for _, cachePrice := range []*string{price.CacheWriteUSDPerMillion, price.CacheWrite5mUSDPerMillion, price.CacheWrite1hUSDPerMillion} {
						if cachePrice != nil {
							*cachePrice = "0"
						}
					}
				}
			}
		}
		value.ConfiguredPrice = effective
		value.Source, value.Multiplier = "internal", "1"
		return value, nil
	}
	if len(stored.price) == 0 {
		return value, nil
	}
	if stored.structureID != value.StructureID {
		value.Source, value.Conflict, value.EffectivePrice = "conflict", true, nil
		return value, nil
	}
	var override config.ModelPricing
	if err := json.Unmarshal(stored.price, &override); err != nil {
		return value, fmt.Errorf("decode stored model price: %w", err)
	}
	override, err = pricing.ValidateModelPriceOverride(model, override)
	if err != nil {
		return value, fmt.Errorf("validate stored model price: %w", err)
	}
	value.Source, value.EffectivePrice = "override", &override
	return value, nil
}

func (s *Store) ListModelPrices(ctx context.Context, pricing config.UsagePricing) ([]ModelPrice, error) {
	models := make([]string, 0, len(pricing.Models))
	for model := range pricing.Models {
		models = append(models, model)
	}
	if _, exists := pricing.Models[config.InternalGovernanceModel]; !exists {
		models = append(models, config.InternalGovernanceModel)
	}
	sort.Strings(models)
	rows, err := s.db.QueryContext(ctx, `SELECT m.model, p.override_price, COALESCE(p.structure_id,''),
		COALESCE(p.version,0),p.updated_at,COALESCE(x.multiplier::text,'1')
		FROM unnest($1::text[]) WITH ORDINALITY m(model,position)
		LEFT JOIN billing_model_prices p ON p.model=m.model
		LEFT JOIN billing_model_multipliers x ON x.model=m.model ORDER BY m.position`, models)
	if err != nil {
		return nil, mapDBError("list model prices", err)
	}
	defer rows.Close()
	values := make([]ModelPrice, 0, len(models))
	for rows.Next() {
		var model, multiplier string
		var stored storedModelPrice
		if err := rows.Scan(&model, &stored.price, &stored.structureID, &stored.version, &stored.updatedAt, &multiplier); err != nil {
			return nil, fmt.Errorf("scan model price: %w", err)
		}
		value, err := modelPriceValue(pricing, model, multiplier, stored)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError("iterate model prices", err)
	}
	return values, nil
}

func readModelPriceTx(ctx context.Context, tx *sql.Tx, model string) (storedModelPrice, error) {
	var value storedModelPrice
	err := tx.QueryRowContext(ctx, `SELECT override_price,structure_id,version,updated_at FROM billing_model_prices WHERE model=$1`, model).
		Scan(&value.price, &value.structureID, &value.version, &value.updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return value, nil
	}
	return value, mapDBError("read model price", err)
}

func (s *Store) SetModelPrice(ctx context.Context, params SetModelPriceParams) (ModelPrice, error) {
	var result ModelPrice
	if err := validateBillingWrite(params.BillingWriteParams); err != nil {
		return result, err
	}
	params.Model, params.Reason = strings.TrimSpace(params.Model), strings.TrimSpace(params.Reason)
	if !modelPriceNamePattern.MatchString(params.Model) || params.Model == config.InternalGovernanceModel || params.Version < 0 || !modelPriceStructurePattern.MatchString(params.StructureID) {
		return result, fmt.Errorf("%w: invalid editable model price", ErrInvalid)
	}
	if params.Action != "save" && params.Action != "restore" {
		return result, fmt.Errorf("%w: invalid model price action", ErrInvalid)
	}
	if (params.Action == "save") != (params.Price != nil) {
		return result, fmt.Errorf("%w: save requires a full matrix; restore must omit it", ErrInvalid)
	}
	var encoded []byte
	if params.Price != nil {
		schema := config.PricingSchemaV1
		if params.Price.ServiceTiers != nil {
			schema = config.PricingSchemaV2
		}
		price, err := config.NormalizeModelPrice(schema, params.Model, *params.Price)
		if err != nil {
			return result, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		params.Price = &price
		encoded, err = json.Marshal(price)
		if err != nil {
			return result, err
		}
	}
	params.At = normalizedBillingTime(params.At, s.now)
	fingerprint := billingOperationFingerprint("model_price", params.ActorUserID, params.Reason, params.Model,
		params.Action, strconv.FormatInt(params.Version, 10), params.StructureID, string(encoded))
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		created, replayID, err := claimBillingOperationTx(ctx, tx, params.BillingWriteParams, "model_price", "", fingerprint)
		if err != nil {
			return err
		}
		if !created {
			entry, err := replayBillingLedgerTx(ctx, tx, *replayID)
			if err != nil {
				return err
			}
			var snapshot struct {
				ModelPrice *ModelPrice `json:"model_price"`
			}
			if err := json.Unmarshal(entry.TransactionSnapshot, &snapshot); err != nil {
				return fmt.Errorf("decode model price replay: %w", err)
			}
			if snapshot.ModelPrice == nil || snapshot.ModelPrice.Model != params.Model {
				return fmt.Errorf("%w: missing model price replay", ErrConflict)
			}
			result = *snapshot.ModelPrice
			return nil
		}
		if _, configured := params.Pricing.Models[params.Model]; !configured {
			return fmt.Errorf("%w: model price is no longer configured", ErrNotFound)
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('billing.model_price.'||$1,0))`, params.Model); err != nil {
			return mapDBError("lock model price setting", err)
		}
		structureID, err := params.Pricing.ModelPriceStructureID(params.Model)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		previous, err := readModelPriceTx(ctx, tx, params.Model)
		if err != nil {
			return err
		}
		if previous.version != params.Version || structureID != params.StructureID {
			return fmt.Errorf("%w: model price changed; reload current configuration", ErrConflict)
		}
		if params.Price != nil {
			if _, err := params.Pricing.ValidateModelPriceOverride(params.Model, *params.Price); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
		}
		var current storedModelPrice
		if err := tx.QueryRowContext(ctx, `INSERT INTO billing_model_prices (model,override_price,structure_id,version,updated_at,updated_by_user_id)
			VALUES ($1,$2::jsonb,$3,1,$4,$5) ON CONFLICT (model) DO UPDATE SET
			override_price=EXCLUDED.override_price,structure_id=EXCLUDED.structure_id,version=billing_model_prices.version+1,
			updated_at=EXCLUDED.updated_at,updated_by_user_id=EXCLUDED.updated_by_user_id
			RETURNING override_price,structure_id,version,updated_at`, params.Model, valueOrNil(string(encoded)), structureID, params.At, params.ActorUserID).
			Scan(&current.price, &current.structureID, &current.version, &current.updatedAt); err != nil {
			return mapDBError("save model price", err)
		}
		multiplier, err := snapshotModelMultiplierTx(ctx, tx, params.Model)
		if err != nil {
			return err
		}
		result, err = modelPriceValue(params.Pricing, params.Model, multiplier, current)
		if err != nil {
			return err
		}
		snapshot, err := json.Marshal(struct {
			ModelPrice    ModelPrice      `json:"model_price"`
			Action        string          `json:"action"`
			PreviousPrice json.RawMessage `json:"previous_price"`
		}{result, params.Action, previous.price})
		if err != nil {
			return err
		}
		var ledgerID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO billing_ledger_entries
			(user_id,operation_id,entry_type,model,pricing_multiplier,reason,actor_user_id,created_at,transaction_snapshot)
			VALUES ($5,$1,'model_price',$2,$3::numeric,$4,$5,$6,$7::jsonb) RETURNING id`,
			params.OperationID, params.Model, multiplier, params.Reason, params.ActorUserID, params.At, snapshot).Scan(&ledgerID); err != nil {
			return mapDBError("record model price ledger", err)
		}
		if err := finishBillingOperationTx(ctx, tx, params.OperationID, ledgerID); err != nil {
			return err
		}
		return appendBillingAuditTx(ctx, tx, params.BillingWriteParams, "billing.model_price_updated", "billing_model_price", params.Model,
			map[string]any{"operation_id": params.OperationID, "model": params.Model, "action": params.Action, "version": result.Version,
				"structure_id": structureID, "previous_version": previous.version, "reason": params.Reason})
	})
	return result, err
}

// Resolve while holding the model's shared transaction lock. A missing override
// is the only case that uses deployment prices; storage failures never do.
func snapshotModelPriceTx(ctx context.Context, tx *sql.Tx, params *BillingReservationParams) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('billing.model_price.'||$1,0))`, params.Model); err != nil {
		return mapDBError("lock model price snapshot", err)
	}
	stored, err := readModelPriceTx(ctx, tx, params.Model)
	if err != nil {
		return err
	}
	if len(stored.price) == 0 {
		return nil
	}
	pricing := config.UsagePricing{SchemaVersion: params.PricingRuleVersion, Models: map[string]config.ModelPricing{}}
	if params.PricingRuleVersion == config.PricingSchemaV2 {
		snapshot, err := config.ParsePricingSnapshot(params.PricingSnapshot)
		if err != nil {
			return err
		}
		pricing.FallbackPolicy, pricing.Models[params.Model] = snapshot.FallbackPolicy, snapshot.Rule
	} else {
		pricing.Models[params.Model] = config.ModelPricing{InputUSDPerMillion: params.InputUSDPerMillion, CachedInputUSDPerMillion: params.CachedInputUSDPerMillion, OutputUSDPerMillion: params.OutputUSDPerMillion}
	}
	value, err := modelPriceValue(pricing, params.Model, "1", stored)
	if err != nil {
		return err
	}
	if value.Conflict {
		return fmt.Errorf("%w: configured model price structure conflicts with stored override", ErrConflict)
	}
	if params.PricingRuleVersion == config.PricingSchemaV2 {
		pricing.Models[params.Model] = *value.EffectivePrice
		params.PricingSnapshot, _, _, err = pricing.ModelSnapshot(params.Model)
		return err
	}
	params.InputUSDPerMillion = value.EffectivePrice.InputUSDPerMillion
	params.CachedInputUSDPerMillion = value.EffectivePrice.CachedInputUSDPerMillion
	params.OutputUSDPerMillion = value.EffectivePrice.OutputUSDPerMillion
	return nil
}
