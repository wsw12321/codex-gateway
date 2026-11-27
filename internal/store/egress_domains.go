package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/wsw/codex-gateway/internal/egress"
)

// ErrEgressDomainNotAllowed marks a member node whose host name is not on the
// Owner's allowlist. It is returned wrapped with ErrInvalid.
var ErrEgressDomainNotAllowed = errors.New("store: egress domain not allowed")

// EgressDomainInUseError refuses an allowlist change that would strand member
// nodes. Hosts lists the names those nodes still use.
type EgressDomainInUseError struct{ Hosts []string }

func (e *EgressDomainInUseError) Error() string { return "store: egress domain in use" }
func (e *EgressDomainInUseError) Unwrap() error { return ErrConflict }

// NormalizeEgressAllowedDomains canonicalizes, deduplicates and sorts entries.
func NormalizeEgressAllowedDomains(raw []string) ([]string, error) {
	if len(raw) > egress.MaxAllowedDomains {
		return nil, fmt.Errorf("%w: too many allowed egress domains", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		domain, ok := egress.NormalizeAllowedDomain(value)
		if !ok {
			return nil, fmt.Errorf("%w: invalid allowed egress domain", ErrInvalid)
		}
		if _, dup := seen[domain]; dup {
			continue
		}
		seen[domain] = struct{}{}
		out = append(out, domain)
	}
	sort.Strings(out)
	return out, nil
}

func egressAllowedDomains(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT domain FROM egress_allowed_domains ORDER BY domain`)
	if err != nil {
		return nil, mapDBError("list allowed egress domains", err)
	}
	defer rows.Close()
	domains := []string{}
	for rows.Next() {
		var domain string
		if err := rows.Scan(&domain); err != nil {
			return nil, mapDBError("scan allowed egress domain", err)
		}
		domains = append(domains, domain)
	}
	return domains, mapDBError("list allowed egress domains", rows.Err())
}

// EgressAllowedDomains returns the Owner's allowlist for member node hosts.
func (s *Store) EgressAllowedDomains(ctx context.Context) ([]string, error) {
	return egressAllowedDomains(ctx, s.db)
}

// checkTenantEgressHostTx enforces the allowlist for a member node inside the
// write transaction, after the egress lock, so a concurrent allowlist change
// cannot slip between the check and the write.
func checkTenantEgressHostTx(ctx context.Context, tx *sql.Tx, server string) error {
	if !egress.IsHostName(server) {
		return nil
	}
	domains, err := egressAllowedDomains(ctx, tx)
	if err != nil {
		return err
	}
	if !egress.DomainAllowed(domains, server) {
		return fmt.Errorf("%w: %w", ErrInvalid, ErrEgressDomainNotAllowed)
	}
	return nil
}

// ReplaceEgressAllowedDomains replaces the allowlist. It refuses a change that
// would leave an existing member node on a host name no longer allowed, so a
// node never silently loses its approval.
func (s *Store) ReplaceEgressAllowedDomains(ctx context.Context, raw []string, a EgressActor) ([]string, error) {
	if a.UserID == "" {
		return nil, ErrInvalid
	}
	domains, err := NormalizeEgressAllowedDomains(raw)
	if err != nil {
		return nil, err
	}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// Serialize with node writes, which take this lock first.
		if _, err := tx.ExecContext(ctx, `SELECT version FROM egress_state WHERE singleton FOR UPDATE`); err != nil {
			return mapDBError("lock egress state", err)
		}
		previous, err := egressAllowedDomains(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT server FROM egress_servers WHERE owner_user_id IS NOT NULL AND server <> '' ORDER BY server`)
		if err != nil {
			return mapDBError("list member egress hosts", err)
		}
		var stranded []string
		for rows.Next() {
			var server string
			if err := rows.Scan(&server); err != nil {
				rows.Close()
				return mapDBError("scan member egress host", err)
			}
			if egress.IsHostName(server) && !egress.DomainAllowed(domains, server) {
				stranded = append(stranded, server)
			}
		}
		if err := rows.Close(); err != nil {
			return mapDBError("list member egress hosts", err)
		}
		if len(stranded) > 0 {
			return &EgressDomainInUseError{Hosts: stranded}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM egress_allowed_domains`); err != nil {
			return mapDBError("clear allowed egress domains", err)
		}
		for _, domain := range domains {
			if _, err := tx.ExecContext(ctx, `INSERT INTO egress_allowed_domains(domain) VALUES($1)`, domain); err != nil {
				return mapDBError("save allowed egress domain", err)
			}
		}
		data, err := marshalSafeMetadata(map[string]any{"previous": previous, "current": domains})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(occurred_at,actor_user_id,actor_session_id,event_type,severity,success,source_ip,subject_type,subject_id,request_id,metadata) VALUES(now(),$1::uuid,$2::uuid,'egress_allowed_domains.replaced','info',true,$3::inet,'egress_allowed_domains','singleton',$4,$5::jsonb)`, valueOrNil(a.UserID), valueOrNil(a.SessionID), valueOrNil(a.SourceIP), valueOrNil(a.RequestID), data)
		return mapDBError("audit allowed egress domains", err)
	})
	if err != nil {
		return nil, err
	}
	return domains, nil
}
