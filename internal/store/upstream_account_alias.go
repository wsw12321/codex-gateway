package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxUpstreamAccountAliasLength counts characters, matching the
// upstream_accounts_alias_safe constraint.
const MaxUpstreamAccountAliasLength = 40

type SetUpstreamAccountAliasParams struct {
	AccountID      string
	Alias          string
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

// NormalizeUpstreamAccountAlias trims an Owner-chosen alias; empty clears it.
// "@" is rejected so an alias cannot carry a full email address into a table
// that otherwise holds only masked addresses. Control, line-separator and
// bidirectional-override characters could disguise one account as another.
func NormalizeUpstreamAccountAlias(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > MaxUpstreamAccountAliasLength ||
		strings.ContainsRune(value, '@') || strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Bidi_Control)
	}) {
		return "", fmt.Errorf("%w: invalid upstream account alias", ErrInvalid)
	}
	return value, nil
}

// SetUpstreamAccountAlias updates a local display preference. Metadata
// synchronization intentionally does not overwrite this value.
func (s *Store) SetUpstreamAccountAlias(ctx context.Context, params SetUpstreamAccountAliasParams) (UpstreamAccount, error) {
	var result UpstreamAccount
	alias, err := NormalizeUpstreamAccountAlias(params.Alias)
	if err != nil || !upstreamAccountIDPattern.MatchString(params.AccountID) || strings.TrimSpace(params.ActorUserID) == "" {
		return result, fmt.Errorf("%w: invalid alias, account, or actor", ErrInvalid)
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// Synchronization takes a table lock before its row writes. Use the same
		// lock ordering as the other local upstream preferences.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock upstream account alias edit", err)
		}
		var previousAlias string
		if err := tx.QueryRowContext(ctx, `SELECT alias FROM upstream_accounts
			WHERE id = $1 AND provider = $2 FOR UPDATE`, params.AccountID, s.upstreamProviderName()).Scan(&previousAlias); err != nil {
			return mapDBError("lock upstream account alias", err)
		}
		var err error
		result, err = scanUpstreamAccount(tx.QueryRowContext(ctx, `UPDATE upstream_accounts
			SET alias = $2 WHERE id = $1 AND provider = $3 RETURNING `+upstreamAccountColumns,
			params.AccountID, alias, s.upstreamProviderName()))
		if err != nil {
			return mapDBError("set upstream account alias", err)
		}
		metadata, err := marshalSafeMetadata(map[string]any{
			"previous_alias": previousAlias, "alias": alias,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity,
				success, source_ip, subject_type, subject_id, request_id, metadata)
			VALUES ($1,$2,$3,'upstream_account.alias_changed','info',true,
				$4::inet,'upstream_account',$5,$6,$7::jsonb)`,
			params.At, params.ActorUserID, valueOrNil(params.ActorSessionID),
			valueOrNil(params.SourceIP), params.AccountID, valueOrNil(params.RequestID), metadata)
		return mapDBError("audit upstream account alias", err)
	})
	if err != nil {
		return UpstreamAccount{}, err
	}
	return result, nil
}
