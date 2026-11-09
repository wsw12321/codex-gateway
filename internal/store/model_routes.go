package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxUserModelRoutes bounds one user's rule set, which is replaced as a whole.
const MaxUserModelRoutes = 32

// ModelRoute sends requests for SourceModel to TargetModel. Permission,
// allowlist, pricing and funding all apply to TargetModel.
type ModelRoute struct {
	SourceModel string    `json:"source_model"`
	TargetModel string    `json:"target_model"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ReplaceUserModelRoutesParams struct {
	UserID         string
	Routes         []ModelRoute
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

// NormalizeModelRoutes validates a complete rule set: model syntax, unique
// sources, no identity rules and no chains, so every request is routed at most
// once and the result never depends on rule order.
func NormalizeModelRoutes(routes []ModelRoute) ([]ModelRoute, error) {
	if len(routes) > MaxUserModelRoutes {
		return nil, fmt.Errorf("%w: at most %d model routes", ErrInvalid, MaxUserModelRoutes)
	}
	normalized := make([]ModelRoute, 0, len(routes))
	sources := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		source, target := strings.TrimSpace(route.SourceModel), strings.TrimSpace(route.TargetModel)
		if !modelPriceNamePattern.MatchString(source) || !modelPriceNamePattern.MatchString(target) || source == target {
			return nil, fmt.Errorf("%w: invalid model route", ErrInvalid)
		}
		if _, duplicate := sources[source]; duplicate {
			return nil, fmt.Errorf("%w: duplicate model route source", ErrInvalid)
		}
		sources[source] = struct{}{}
		normalized = append(normalized, ModelRoute{SourceModel: source, TargetModel: target})
	}
	for _, route := range normalized {
		if _, chained := sources[route.TargetModel]; chained {
			return nil, fmt.Errorf("%w: model route target is also a source", ErrInvalid)
		}
	}
	return normalized, nil
}

func (s *Store) ListUserModelRoutes(ctx context.Context, userID string) ([]ModelRoute, error) {
	return listUserModelRoutes(ctx, s.db, userID)
}

type modelRouteQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listUserModelRoutes(ctx context.Context, queryer modelRouteQueryer, userID string) ([]ModelRoute, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT source_model, target_model, created_at, updated_at
		FROM user_model_routes WHERE user_id = $1 ORDER BY source_model`, userID)
	if err != nil {
		return nil, mapDBError("list model routes", err)
	}
	defer rows.Close()
	routes := []ModelRoute{}
	for rows.Next() {
		var route ModelRoute
		if err := rows.Scan(&route.SourceModel, &route.TargetModel, &route.CreatedAt, &route.UpdatedAt); err != nil {
			return nil, mapDBError("scan model route", err)
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError("list model routes", err)
	}
	return routes, nil
}

// ResolveUserModelRoute returns the target for one requested model. Callers
// must fail closed on error rather than forwarding the unrouted model.
func (s *Store) ResolveUserModelRoute(ctx context.Context, userID, model string) (string, bool, error) {
	var target string
	err := s.db.QueryRowContext(ctx, `SELECT target_model FROM user_model_routes
		WHERE user_id = $1 AND source_model = $2`, userID, model).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, mapDBError("resolve model route", err)
	}
	return target, true, nil
}

// ReplaceUserModelRoutes atomically replaces a user's rule set and audits the
// change. Unchanged rules keep their creation time.
func (s *Store) ReplaceUserModelRoutes(ctx context.Context, params ReplaceUserModelRoutesParams) ([]ModelRoute, error) {
	routes, err := NormalizeModelRoutes(params.Routes)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.UserID) == "" || strings.TrimSpace(params.ActorUserID) == "" {
		return nil, fmt.Errorf("%w: model routes require a user and actor", ErrInvalid)
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	var result []ModelRoute
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// Serialize replacements per user without blocking foreign-key writes.
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, params.UserID).Scan(&locked); err != nil {
			return mapDBError("lock model route owner", err)
		}
		previous, err := listUserModelRoutes(ctx, tx, params.UserID)
		if err != nil {
			return err
		}
		keep := make([]string, 0, len(routes))
		for _, route := range routes {
			keep = append(keep, route.SourceModel)
			if _, err := tx.ExecContext(ctx, `INSERT INTO user_model_routes
				(user_id, source_model, target_model, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$4)
				ON CONFLICT (user_id, source_model) DO UPDATE
				SET target_model = EXCLUDED.target_model,
					updated_at = CASE WHEN user_model_routes.target_model = EXCLUDED.target_model
						THEN user_model_routes.updated_at ELSE EXCLUDED.updated_at END`,
				params.UserID, route.SourceModel, route.TargetModel, params.At); err != nil {
				return mapDBError("write model route", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_model_routes
			WHERE user_id = $1 AND NOT (source_model = ANY($2::text[]))`, params.UserID, keep); err != nil {
			return mapDBError("delete model routes", err)
		}
		result, err = listUserModelRoutes(ctx, tx, params.UserID)
		if err != nil {
			return err
		}
		metadata, err := marshalSafeMetadata(map[string]any{
			"previous": modelRouteAuditList(previous), "current": modelRouteAuditList(result),
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity,
				success, source_ip, subject_type, subject_id, request_id, metadata)
			VALUES ($1,$2,$3,'user.model_routes_changed','info',true,
				$4::inet,'user',$5,$6,$7::jsonb)`,
			params.At, params.ActorUserID, valueOrNil(params.ActorSessionID),
			valueOrNil(params.SourceIP), params.UserID, valueOrNil(params.RequestID), metadata)
		return mapDBError("audit model routes", err)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func modelRouteAuditList(routes []ModelRoute) []any {
	list := make([]any, 0, len(routes))
	for _, route := range routes {
		list = append(list, map[string]any{"source": route.SourceModel, "target": route.TargetModel})
	}
	return list
}
