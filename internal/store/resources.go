package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const deviceColumns = `id, user_id, name, status, created_at, updated_at, last_seen_at, disabled_at`

func scanDevice(row rowScanner) (Device, error) {
	var device Device
	err := row.Scan(
		&device.ID, &device.UserID, &device.Name, &device.Status,
		&device.CreatedAt, &device.UpdatedAt, &device.LastSeenAt, &device.DisabledAt,
	)
	return device, err
}

type CreateDeviceParams struct {
	ID     string
	UserID string
	Name   string
}

func (s *Store) CreateDevice(ctx context.Context, params CreateDeviceParams) (Device, error) {
	params.Name = strings.TrimSpace(params.Name)
	if params.UserID == "" || params.Name == "" {
		return Device{}, fmt.Errorf("%w: device owner and name are required", ErrInvalid)
	}
	if params.ID == "" {
		var err error
		params.ID, err = newUUID()
		if err != nil {
			return Device{}, err
		}
	}
	device, err := scanDevice(s.db.QueryRowContext(ctx, `
		INSERT INTO devices (id, user_id, name) VALUES ($1, $2, $3)
		RETURNING `+deviceColumns, params.ID, params.UserID, params.Name,
	))
	return device, mapDBError("create device", err)
}

func (s *Store) GetDevice(ctx context.Context, userID, deviceID string) (Device, error) {
	device, err := scanDevice(s.db.QueryRowContext(ctx,
		`SELECT `+deviceColumns+` FROM devices WHERE id = $1 AND user_id = $2`, deviceID, userID,
	))
	return device, mapDBError("get device", err)
}

func (s *Store) ListDevices(ctx context.Context, userID string) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deviceColumns+` FROM devices WHERE user_id = $1 ORDER BY created_at`, userID,
	)
	if err != nil {
		return nil, mapDBError("list devices", err)
	}
	defer rows.Close()
	var result []Device
	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		result = append(result, device)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate devices: %w", err)
	}
	return result, nil
}

func (s *Store) DisableDevice(ctx context.Context, userID, deviceID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE devices SET status = 'disabled', disabled_at = $3
		WHERE id = $1 AND user_id = $2 AND status = 'active'`, deviceID, userID, at,
	)
	if err != nil {
		return mapDBError("disable device", err)
	}
	return requireAffected("disable device", result)
}

const projectColumns = `id, user_id, slug, name, status, created_at, updated_at, archived_at`

func scanProject(row rowScanner) (Project, error) {
	var project Project
	err := row.Scan(
		&project.ID, &project.UserID, &project.Slug, &project.Name, &project.Status,
		&project.CreatedAt, &project.UpdatedAt, &project.ArchivedAt,
	)
	return project, err
}

type CreateProjectParams struct {
	ID     string
	UserID string
	Slug   string
	Name   string
}

func (s *Store) CreateProject(ctx context.Context, params CreateProjectParams) (Project, error) {
	params.Slug = strings.ToLower(strings.TrimSpace(params.Slug))
	params.Name = strings.TrimSpace(params.Name)
	if params.UserID == "" || params.Slug == "" || params.Name == "" {
		return Project{}, fmt.Errorf("%w: project owner, slug and name are required", ErrInvalid)
	}
	if params.ID == "" {
		var err error
		params.ID, err = newUUID()
		if err != nil {
			return Project{}, err
		}
	}
	project, err := scanProject(s.db.QueryRowContext(ctx, `
		INSERT INTO projects (id, user_id, slug, name) VALUES ($1, $2, $3, $4)
		RETURNING `+projectColumns, params.ID, params.UserID, params.Slug, params.Name,
	))
	return project, mapDBError("create project", err)
}

func (s *Store) GetProject(ctx context.Context, userID, projectID string) (Project, error) {
	project, err := scanProject(s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = $1 AND user_id = $2`, projectID, userID,
	))
	return project, mapDBError("get project", err)
}

// ResolveProject only returns an active project belonging to the current user.
func (s *Store) ResolveProject(ctx context.Context, userID, slug string) (Project, error) {
	project, err := scanProject(s.db.QueryRowContext(ctx, `
		SELECT `+projectColumns+` FROM projects
		WHERE user_id = $1 AND slug = lower($2) AND status = 'active'`, userID, strings.TrimSpace(slug),
	))
	return project, mapDBError("resolve project", err)
}

func (s *Store) ListProjects(ctx context.Context, userID string) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE user_id = $1 ORDER BY created_at`, userID,
	)
	if err != nil {
		return nil, mapDBError("list projects", err)
	}
	defer rows.Close()
	var result []Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		result = append(result, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	return result, nil
}

func (s *Store) ArchiveProject(ctx context.Context, userID, projectID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE projects SET status = 'archived', archived_at = $3
		WHERE id = $1 AND user_id = $2 AND status = 'active'`, projectID, userID, at,
	)
	if err != nil {
		return mapDBError("archive project", err)
	}
	return requireAffected("archive project", result)
}

type CreateAPIKeyParams struct {
	ID                string
	PublicID          string
	KeyPrefix         string
	KeyHash           []byte
	UserID            string
	DeviceID          string
	DefaultProjectID  string
	Name              string
	ModelAllowlist    []string
	RPMLimit          *int
	ConcurrentLimit   *int
	DailyRequestLimit *int
	DailyTokenLimit   *int64
	CreatedAt         time.Time
	ExpiresAt         time.Time
	RotatedFromID     string
}

const apiKeyCoreColumns = `id, public_id, key_prefix, key_hash, user_id, device_id,
	default_project_id, name, status, to_json(model_allowlist)::text,
	rpm_limit, concurrent_limit, daily_request_limit, daily_token_limit,
	created_at, expires_at, last_used_at, revoked_at, revoke_reason, rotated_from_id`

func scanAPIKeyCore(row rowScanner) (APIKey, error) {
	var key APIKey
	var allowlist []byte
	err := row.Scan(
		&key.ID, &key.PublicID, &key.KeyPrefix, &key.KeyHash, &key.UserID, &key.DeviceID,
		&key.DefaultProjectID, &key.Name, &key.Status, &allowlist, &key.RPMLimit,
		&key.ConcurrentLimit, &key.DailyRequestLimit, &key.DailyTokenLimit,
		&key.CreatedAt, &key.ExpiresAt, &key.LastUsedAt, &key.RevokedAt,
		&key.RevokeReason, &key.RotatedFromID,
	)
	if err == nil {
		err = unmarshalStringArray(allowlist, &key.ModelAllowlist)
	}
	return key, err
}

func (s *Store) CreateAPIKey(ctx context.Context, params CreateAPIKeyParams) (APIKey, error) {
	params.Name = strings.TrimSpace(params.Name)
	if len(params.KeyHash) != 32 || params.PublicID == "" || params.KeyPrefix == "" ||
		params.UserID == "" || params.DeviceID == "" || params.Name == "" {
		return APIKey{}, fmt.Errorf("%w: invalid API key metadata", ErrInvalid)
	}
	if params.CreatedAt.IsZero() {
		params.CreatedAt = s.now().UTC()
	}
	if params.ExpiresAt.IsZero() {
		params.ExpiresAt = params.CreatedAt.Add(90 * 24 * time.Hour)
	}
	if !params.ExpiresAt.After(params.CreatedAt) ||
		params.ExpiresAt.After(params.CreatedAt.Add(365*24*time.Hour)) {
		return APIKey{}, fmt.Errorf("%w: API key expiry must be within 365 days", ErrInvalid)
	}
	if params.ID == "" {
		var err error
		params.ID, err = newUUID()
		if err != nil {
			return APIKey{}, err
		}
	}
	allowlist, err := marshalStringArray(params.ModelAllowlist)
	if err != nil {
		return APIKey{}, err
	}
	key, err := scanAPIKeyCore(s.db.QueryRowContext(ctx, `
		INSERT INTO api_keys
			(id, public_id, key_prefix, key_hash, user_id, device_id,
			 default_project_id, name, model_allowlist, rpm_limit, concurrent_limit,
			 daily_request_limit, daily_token_limit, created_at, expires_at, rotated_from_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			ARRAY(SELECT jsonb_array_elements_text($9::jsonb)), $10, $11, $12, $13, $14, $15, $16)
		RETURNING `+apiKeyCoreColumns,
		params.ID, params.PublicID, params.KeyPrefix, params.KeyHash, params.UserID,
		params.DeviceID, valueOrNil(params.DefaultProjectID), params.Name, allowlist,
		params.RPMLimit, params.ConcurrentLimit, params.DailyRequestLimit,
		params.DailyTokenLimit, params.CreatedAt, params.ExpiresAt, valueOrNil(params.RotatedFromID),
	))
	return key, mapDBError("create API key", err)
}

// LookupAPIKey returns key material plus the owning user/device state in one
// query. Callers must still compare KeyHash in constant time and check expiry,
// key status, user status, device status and model allowlist.
func (s *Store) LookupAPIKey(ctx context.Context, publicID string) (APIKey, error) {
	var key APIKey
	var allowlist []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT k.id, k.public_id, k.key_prefix, k.key_hash, k.user_id, k.device_id,
			k.default_project_id, k.name, k.status, to_json(k.model_allowlist)::text,
			k.rpm_limit, k.concurrent_limit, k.daily_request_limit, k.daily_token_limit,
			k.created_at, k.expires_at, k.last_used_at, k.revoked_at, k.revoke_reason,
			k.rotated_from_id, u.status, d.status, p.slug
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		JOIN devices d ON d.id = k.device_id AND d.user_id = k.user_id
		LEFT JOIN projects p ON p.id = k.default_project_id AND p.user_id = k.user_id
		WHERE k.public_id = $1`, publicID,
	).Scan(
		&key.ID, &key.PublicID, &key.KeyPrefix, &key.KeyHash, &key.UserID, &key.DeviceID,
		&key.DefaultProjectID, &key.Name, &key.Status, &allowlist, &key.RPMLimit,
		&key.ConcurrentLimit, &key.DailyRequestLimit, &key.DailyTokenLimit,
		&key.CreatedAt, &key.ExpiresAt, &key.LastUsedAt, &key.RevokedAt,
		&key.RevokeReason, &key.RotatedFromID, &key.UserStatus, &key.DeviceStatus,
		&key.DefaultProjectSlug,
	)
	if err == nil {
		err = unmarshalStringArray(allowlist, &key.ModelAllowlist)
	}
	return key, mapDBError("lookup API key", err)
}

func (s *Store) GetAPIKey(ctx context.Context, userID, keyID string) (APIKey, error) {
	key, err := scanAPIKeyCore(s.db.QueryRowContext(ctx,
		`SELECT `+apiKeyCoreColumns+` FROM api_keys WHERE id = $1 AND user_id = $2`, keyID, userID,
	))
	return key, mapDBError("get API key", err)
}

func (s *Store) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+apiKeyCoreColumns+` FROM api_keys
		WHERE user_id = $1 ORDER BY created_at DESC`, userID,
	)
	if err != nil {
		return nil, mapDBError("list API keys", err)
	}
	defer rows.Close()
	var result []APIKey
	for rows.Next() {
		key, err := scanAPIKeyCore(rows)
		if err != nil {
			return nil, fmt.Errorf("scan API key: %w", err)
		}
		result = append(result, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate API keys: %w", err)
	}
	return result, nil
}

func (s *Store) RevokeAPIKey(ctx context.Context, userID, keyID, reason string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET status = 'revoked', revoked_at = $4, revoke_reason = $3
		WHERE id = $1 AND user_id = $2 AND status <> 'revoked'`, keyID, userID, reason, at,
	)
	if err != nil {
		return mapDBError("revoke API key", err)
	}
	return requireAffected("revoke API key", result)
}

func (s *Store) RecordAPIKeyUse(ctx context.Context, keyID, deviceID string, at time.Time) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		keyResult, err := tx.ExecContext(ctx,
			`UPDATE api_keys SET last_used_at = $2 WHERE id = $1`, keyID, at,
		)
		if err != nil {
			return mapDBError("record API key use", err)
		}
		if err := requireAffected("record API key use", keyResult); err != nil {
			return err
		}
		deviceResult, err := tx.ExecContext(ctx, `
			UPDATE devices SET last_seen_at = $2
			WHERE id = $1 AND EXISTS (
				SELECT 1 FROM api_keys k
				WHERE k.id = $3 AND k.device_id = devices.id
			)`, deviceID, at, keyID,
		)
		if err != nil {
			return mapDBError("record device use", err)
		}
		if err := requireAffected("record device use", deviceResult); err != nil {
			return err
		}
		return nil
	})
}
