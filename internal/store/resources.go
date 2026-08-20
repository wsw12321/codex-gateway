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
	result := make([]Device, 0)
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
	result := make([]Project, 0)
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
	SecretCiphertext  []byte
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
	created_at, expires_at, last_used_at, secret_ciphertext IS NOT NULL, rotated_from_id`

func scanAPIKeyCore(row rowScanner) (APIKey, error) {
	var key APIKey
	var allowlist []byte
	err := row.Scan(
		&key.ID, &key.PublicID, &key.KeyPrefix, &key.KeyHash, &key.UserID, &key.DeviceID,
		&key.DefaultProjectID, &key.Name, &key.Status, &allowlist, &key.RPMLimit,
		&key.ConcurrentLimit, &key.DailyRequestLimit, &key.DailyTokenLimit,
		&key.CreatedAt, &key.ExpiresAt, &key.LastUsedAt, &key.SecretAvailable,
		&key.RotatedFromID,
	)
	if err == nil {
		err = unmarshalStringArray(allowlist, &key.ModelAllowlist)
	}
	return key, err
}

func scanAPIKeyHistory(row rowScanner) (APIKeyHistory, error) {
	var history APIKeyHistory
	err := row.Scan(
		&history.ID, &history.UserID, &history.DeviceID,
		&history.KeyPrefix, &history.CreatedAt,
	)
	return history, err
}

func (s *Store) CreateAPIKey(ctx context.Context, params CreateAPIKeyParams) (APIKey, error) {
	params.Name = strings.TrimSpace(params.Name)
	if len(params.KeyHash) != 32 || len(params.SecretCiphertext) == 0 ||
		params.PublicID == "" || params.KeyPrefix == "" ||
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
	var key APIKey
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO api_key_history (id, user_id, device_id, key_prefix, created_at)
			VALUES ($1, $2, $3, $4, $5)`,
			params.ID, params.UserID, params.DeviceID, params.KeyPrefix, params.CreatedAt,
		); err != nil {
			return mapDBError("create API key history", err)
		}
		var insertErr error
		key, insertErr = scanAPIKeyCore(tx.QueryRowContext(ctx, `
			INSERT INTO api_keys
				(id, public_id, key_prefix, key_hash, secret_ciphertext, user_id, device_id,
				 default_project_id, name, model_allowlist, rpm_limit, concurrent_limit,
				 daily_request_limit, daily_token_limit, created_at, expires_at, rotated_from_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
				ARRAY(SELECT jsonb_array_elements_text($10::jsonb)), $11, $12, $13, $14, $15, $16, $17)
			RETURNING `+apiKeyCoreColumns,
			params.ID, params.PublicID, params.KeyPrefix, params.KeyHash,
			params.SecretCiphertext, params.UserID, params.DeviceID,
			valueOrNil(params.DefaultProjectID), params.Name, allowlist,
			params.RPMLimit, params.ConcurrentLimit, params.DailyRequestLimit,
			params.DailyTokenLimit, params.CreatedAt, params.ExpiresAt,
			valueOrNil(params.RotatedFromID),
		))
		return mapDBError("create API key", insertErr)
	})
	return key, err
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
			k.created_at, k.expires_at, k.last_used_at,
			k.secret_ciphertext IS NOT NULL, k.rotated_from_id, u.status, d.status, p.slug
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		JOIN devices d ON d.id = k.device_id AND d.user_id = k.user_id
		LEFT JOIN projects p ON p.id = k.default_project_id AND p.user_id = k.user_id
		WHERE k.public_id = $1`, publicID,
	).Scan(
		&key.ID, &key.PublicID, &key.KeyPrefix, &key.KeyHash, &key.UserID, &key.DeviceID,
		&key.DefaultProjectID, &key.Name, &key.Status, &allowlist, &key.RPMLimit,
		&key.ConcurrentLimit, &key.DailyRequestLimit, &key.DailyTokenLimit,
		&key.CreatedAt, &key.ExpiresAt, &key.LastUsedAt, &key.SecretAvailable,
		&key.RotatedFromID, &key.UserStatus, &key.DeviceStatus,
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
	result := make([]APIKey, 0)
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

// GetAPIKeySecret is the only store method that loads encrypted API key
// material. Callers must decrypt and independently verify the canonical key,
// public ID and stored HMAC before returning plaintext.
func (s *Store) GetAPIKeySecret(ctx context.Context, userID, keyID string) (APIKeySecret, error) {
	if userID == "" || keyID == "" {
		return APIKeySecret{}, fmt.Errorf("%w: API key owner and id are required", ErrInvalid)
	}
	var secret APIKeySecret
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, public_id, key_prefix, key_hash, secret_ciphertext
		FROM api_keys WHERE id = $1 AND user_id = $2`, keyID, userID,
	).Scan(
		&secret.ID, &secret.UserID, &secret.PublicID, &secret.KeyPrefix,
		&secret.KeyHash, &secret.SecretCiphertext,
	)
	return secret, mapDBError("get API key secret", err)
}

// SetAPIKeyStatus changes an active credential between active and disabled.
// The bool reports whether a database mutation was necessary.
func (s *Store) SetAPIKeyStatus(
	ctx context.Context,
	userID, keyID, status string,
	at time.Time,
) (APIKey, bool, error) {
	status = strings.TrimSpace(status)
	if userID == "" || keyID == "" || (status != StatusActive && status != StatusDisabled) {
		return APIKey{}, false, fmt.Errorf("%w: invalid API key status", ErrInvalid)
	}
	if at.IsZero() {
		at = s.now().UTC()
	} else {
		at = at.UTC()
	}
	var key APIKey
	changed := false
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		current, err := scanAPIKeyCore(tx.QueryRowContext(ctx, `
			SELECT `+apiKeyCoreColumns+` FROM api_keys
			WHERE id = $1 AND user_id = $2 FOR UPDATE`, keyID, userID,
		))
		if err != nil {
			return mapDBError("lock API key status", err)
		}
		if current.Status == status {
			key = current
			return nil
		}
		if status == StatusActive && !current.ExpiresAt.After(at) {
			return fmt.Errorf("enable API key: %w", ErrAPIKeyExpired)
		}
		key, err = scanAPIKeyCore(tx.QueryRowContext(ctx, `
			UPDATE api_keys SET status = $3
			WHERE id = $1 AND user_id = $2
			RETURNING `+apiKeyCoreColumns, keyID, userID, status,
		))
		if err != nil {
			return mapDBError("set API key status", err)
		}
		changed = true
		return nil
	})
	if err != nil {
		return APIKey{}, false, err
	}
	return key, changed, nil
}

// DeleteAPIKey permanently removes authentication and management material but
// leaves its immutable history identity available to existing accounting rows.
func (s *Store) DeleteAPIKey(ctx context.Context, userID, keyID string) (APIKeyHistory, error) {
	if userID == "" || keyID == "" {
		return APIKeyHistory{}, fmt.Errorf("%w: API key owner and id are required", ErrInvalid)
	}
	history, err := scanAPIKeyHistory(s.db.QueryRowContext(ctx, `
		WITH deleted AS (
			DELETE FROM api_keys WHERE id = $1 AND user_id = $2
			RETURNING id
		)
		SELECT h.id, h.user_id, h.device_id, h.key_prefix, h.created_at
		FROM api_key_history h JOIN deleted d ON d.id = h.id`, keyID, userID,
	))
	return history, mapDBError("delete API key", err)
}

func (s *Store) RecordAPIKeyUse(ctx context.Context, keyID, deviceID string, at time.Time) error {
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE api_keys SET last_used_at = $2 WHERE id = $1`, keyID, at,
		)
		if err != nil {
			return mapDBError("record API key use", err)
		}
		deviceResult, err := tx.ExecContext(ctx, `
			UPDATE devices SET last_seen_at = $2
			WHERE id = $1 AND EXISTS (
				SELECT 1 FROM api_key_history h
				WHERE h.id = $3 AND h.device_id = devices.id
				  AND h.user_id = devices.user_id
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
