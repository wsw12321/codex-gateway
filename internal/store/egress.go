package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/egress"
)

type EgressServer struct {
	ID                 string
	Name               string
	Server             string
	Port               int
	Cipher             string
	PasswordCiphertext []byte
	UDP                bool
	Revision           int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type EgressBinding struct {
	AccountID string
	Provider  string
	NodeID    string
	Revision  int64
}

type EgressRegistration struct {
	ID                      string
	Provider                string
	NodeID                  string
	Preserve                bool
	AccountID               string
	State                   string
	Revision                int64
	ActorUserID             string
	ActorSessionID          string
	CreatedAt               time.Time
	BaselineBindingRevision int64
}

type EgressState struct {
	Version       int64
	Nodes         []EgressServer
	Bindings      []EgressBinding
	Registrations []EgressRegistration
}

type EgressActor struct {
	UserID    string
	SessionID string
	RequestID string
	SourceIP  string
}

// ValidateEgressServer permits only a host, never a URL, path or credentials.
func ValidateEgressServer(n EgressServer) error {
	if n.Name == "" || len(n.Name) > 80 || strings.TrimSpace(n.Name) != n.Name || strings.IndexFunc(n.Name, unicode.IsControl) >= 0 || n.Port < 1 || n.Port > 65535 {
		return fmt.Errorf("%w: invalid egress node", ErrInvalid)
	}
	if !egress.SupportedCipher(n.Cipher) {
		return fmt.Errorf("%w: invalid SS cipher", ErrInvalid)
	}
	if n.Server == "" || len(n.Server) > 253 || strings.TrimSpace(n.Server) != n.Server {
		return fmt.Errorf("%w: invalid SS host", ErrInvalid)
	}
	if addr, err := netip.ParseAddr(n.Server); err == nil {
		if !addr.Is4() || addr.String() != n.Server {
			return fmt.Errorf("%w: invalid SS host", ErrInvalid)
		}
		return nil
	}
	if strings.ToLower(n.Server) != n.Server || len(strings.Split(n.Server, ".")) < 2 {
		return fmt.Errorf("%w: invalid SS host", ErrInvalid)
	}
	for _, label := range strings.Split(n.Server, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("%w: invalid SS host", ErrInvalid)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("%w: invalid SS host", ErrInvalid)
			}
		}
	}
	return nil
}

const egressServerColumns = `id::text,name,server,port,cipher,password_ciphertext,udp,revision,created_at,updated_at`

func scanEgressServer(row rowScanner) (EgressServer, error) {
	var n EgressServer
	err := row.Scan(&n.ID, &n.Name, &n.Server, &n.Port, &n.Cipher, &n.PasswordCiphertext, &n.UDP, &n.Revision, &n.CreatedAt, &n.UpdatedAt)
	return n, err
}

// Every write locks this singleton before any other rows. Read snapshots use
// REPEATABLE READ, so their version, routes and encrypted nodes are consistent.
func nextEgressVersion(ctx context.Context, tx *sql.Tx) (int64, error) {
	var v int64
	err := tx.QueryRowContext(ctx, `UPDATE egress_state SET version=version+1 WHERE singleton RETURNING version`).Scan(&v)
	return v, mapDBError("advance egress version", err)
}
func egressAudit(ctx context.Context, tx *sql.Tx, a EgressActor, event, subject string, metadata map[string]any) error {
	data, err := marshalSafeMetadata(metadata)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(occurred_at,actor_user_id,actor_session_id,event_type,severity,success,source_ip,subject_type,subject_id,request_id,metadata) VALUES(now(),$1::uuid,$2::uuid,$3,'info',true,$4::inet,'egress_server',$5,$6,$7::jsonb)`, valueOrNil(a.UserID), valueOrNil(a.SessionID), event, valueOrNil(a.SourceIP), subject, valueOrNil(a.RequestID), data)
	return mapDBError("audit egress operation", err)
}

// An empty ciphertext on update retains the existing encrypted password. A
// nonzero Revision on such an update is an optimistic guard: the row changes
// only if it still has the revision whose password the caller validated.
func (s *Store) SaveEgressServer(ctx context.Context, n EgressServer, create bool, a EgressActor) (EgressServer, error) {
	if a.UserID == "" {
		return EgressServer{}, ErrInvalid
	}
	if err := ValidateEgressServer(n); err != nil {
		return EgressServer{}, err
	}
	if parsed, err := uuid.Parse(n.ID); err != nil || parsed.String() != n.ID {
		return EgressServer{}, fmt.Errorf("%w: invalid node id", ErrInvalid)
	}
	if create && len(n.PasswordCiphertext) < 29 {
		return EgressServer{}, fmt.Errorf("%w: missing SS credential", ErrInvalid)
	}
	var result EgressServer
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		v, err := nextEgressVersion(ctx, tx)
		if err != nil {
			return err
		}
		if create {
			result, err = scanEgressServer(tx.QueryRowContext(ctx, `INSERT INTO egress_servers(id,name,server,port,cipher,password_ciphertext,udp,revision,created_at,updated_at) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,now(),now()) RETURNING `+egressServerColumns, n.ID, n.Name, n.Server, n.Port, n.Cipher, n.PasswordCiphertext, n.UDP, v))
		} else {
			expected := int64(0)
			if len(n.PasswordCiphertext) == 0 {
				expected = n.Revision
			}
			result, err = scanEgressServer(tx.QueryRowContext(ctx, `UPDATE egress_servers SET name=$2,server=$3,port=$4,cipher=$5,password_ciphertext=CASE WHEN octet_length($6::bytea)=0 THEN password_ciphertext ELSE $6 END,udp=$7,revision=$8,updated_at=now() WHERE id=$1::uuid AND ($9::bigint=0 OR revision=$9) RETURNING `+egressServerColumns, n.ID, n.Name, n.Server, n.Port, n.Cipher, nonNilBytes(n.PasswordCiphertext), n.UDP, v, expected))
			if errors.Is(err, sql.ErrNoRows) && expected > 0 {
				var exists bool
				if existsErr := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM egress_servers WHERE id=$1::uuid)`, n.ID).Scan(&exists); existsErr != nil {
					return mapDBError("check egress node", existsErr)
				}
				if exists {
					return fmt.Errorf("%w: egress node changed", ErrConflict)
				}
			}
		}
		if err != nil {
			return mapDBError("save egress node", err)
		}
		return egressAudit(ctx, tx, a, "egress_server.saved", n.ID, map[string]any{"revision": v, "created": create})
	})
	return result, err
}
func nonNilBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// EgressServer returns one node, including its encrypted credential.
func (s *Store) EgressServer(ctx context.Context, id string) (EgressServer, error) {
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		return EgressServer{}, fmt.Errorf("%w: invalid node id", ErrInvalid)
	}
	n, err := scanEgressServer(s.db.QueryRowContext(ctx, `SELECT `+egressServerColumns+` FROM egress_servers WHERE id=$1::uuid`, id))
	return n, mapDBError("find egress node", err)
}

func (s *Store) DeleteEgressServer(ctx context.Context, id string, a EgressActor) error {
	if a.UserID == "" {
		return ErrInvalid
	}
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("%w: invalid node id", ErrInvalid)
	}
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := nextEgressVersion(ctx, tx); err != nil {
			return err
		}
		var referenced bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM upstream_account_egress WHERE node_id=$1::uuid UNION ALL SELECT 1 FROM egress_registrations WHERE node_id=$1::uuid AND state IN ('pending','prepared') AND created_at>now()-interval '24 hours')`, id).Scan(&referenced); err != nil {
			return mapDBError("check node references", err)
		}
		if referenced {
			return fmt.Errorf("%w: node is bound", ErrConflict)
		}
		// Completed intents no longer need their original node reference; replay
		// reads the account's current binding and never reapplies a completed edit.
		if _, err := tx.ExecContext(ctx, `UPDATE egress_registrations SET node_id=NULL WHERE node_id=$1::uuid AND state='committed'`, id); err != nil {
			return mapDBError("release completed registration reference", err)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM egress_servers WHERE id=$1::uuid`, id)
		if err != nil {
			return mapDBError("delete egress node", err)
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			return ErrNotFound
		}
		return egressAudit(ctx, tx, a, "egress_server.deleted", id, map[string]any{})
	})
}

func setEgressBinding(ctx context.Context, tx *sql.Tx, provider, accountID, nodeID string, revision int64) error {
	var actualProvider string
	if err := tx.QueryRowContext(ctx, `SELECT provider FROM upstream_accounts WHERE id=$1`, accountID).Scan(&actualProvider); err != nil {
		return mapDBError("find egress account", err)
	}
	if actualProvider != provider {
		return fmt.Errorf("%w: wrong account provider", ErrConflict)
	}
	if nodeID != "" {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM egress_servers WHERE id=$1::uuid)`, nodeID).Scan(&exists); err != nil {
			return mapDBError("find egress node", err)
		}
		if !exists {
			return ErrNotFound
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO upstream_account_egress(account_id,node_id,revision) VALUES($1,$2::uuid,$3) ON CONFLICT(account_id) DO UPDATE SET node_id=EXCLUDED.node_id,revision=EXCLUDED.revision`, accountID, valueOrNil(nodeID), revision)
	return mapDBError("bind egress node", err)
}

func (s *Store) SetEgressBinding(ctx context.Context, accountID, nodeID string, a EgressActor) (EgressBinding, error) {
	if a.UserID == "" {
		return EgressBinding{}, ErrInvalid
	}
	if !s.validUpstreamProvider() || !upstreamAccountIDPattern.MatchString(accountID) {
		return EgressBinding{}, ErrInvalid
	}
	if nodeID != "" {
		if _, err := uuid.Parse(nodeID); err != nil {
			return EgressBinding{}, ErrInvalid
		}
	}
	var result EgressBinding
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		v, err := nextEgressVersion(ctx, tx)
		if err != nil {
			return err
		}
		if err = setEgressBinding(ctx, tx, s.upstreamProviderName(), accountID, nodeID, v); err != nil {
			return err
		}
		result = EgressBinding{AccountID: accountID, Provider: s.upstreamProviderName(), NodeID: nodeID, Revision: v}
		return egressAudit(ctx, tx, a, "upstream_account.egress_changed", accountID, map[string]any{"provider": result.Provider, "node_id": nodeID, "revision": v})
	})
	return result, err
}

const egressRegistrationColumns = `id::text,provider,COALESCE(node_id::text,''),preserve_binding,COALESCE(account_id,''),state,revision,COALESCE(actor_user_id::text,''),COALESCE(actor_session_id::text,''),created_at,baseline_binding_revision`

func scanEgressRegistration(row rowScanner) (EgressRegistration, error) {
	var v EgressRegistration
	err := row.Scan(&v.ID, &v.Provider, &v.NodeID, &v.Preserve, &v.AccountID, &v.State, &v.Revision, &v.ActorUserID, &v.ActorSessionID, &v.CreatedAt, &v.BaselineBindingRevision)
	return v, err
}

func (s *Store) CreateEgressRegistration(ctx context.Context, provider string, nodeID *string, a EgressActor) (EgressRegistration, error) {
	if !s.WithUpstreamProvider(provider).validUpstreamProvider() || a.UserID == "" {
		return EgressRegistration{}, ErrInvalid
	}
	selected := ""
	if nodeID != nil {
		selected = *nodeID
	}
	if selected != "" {
		if _, err := uuid.Parse(selected); err != nil {
			return EgressRegistration{}, ErrInvalid
		}
	}
	var result EgressRegistration
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		v, err := nextEgressVersion(ctx, tx)
		if err != nil {
			return err
		}
		if selected != "" {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM egress_servers WHERE id=$1::uuid)`, selected).Scan(&exists); err != nil {
				return mapDBError("find registration node", err)
			}
			if !exists {
				return ErrNotFound
			}
		}
		result, err = scanEgressRegistration(tx.QueryRowContext(ctx, `INSERT INTO egress_registrations(id,provider,node_id,preserve_binding,revision,actor_user_id,actor_session_id,created_at) VALUES($1::uuid,$2,$3::uuid,$4,$5,$6::uuid,$7::uuid,now()) RETURNING `+egressRegistrationColumns, uuid.NewString(), provider, valueOrNil(selected), nodeID == nil, v, a.UserID, valueOrNil(a.SessionID)))
		return mapDBError("create egress registration", err)
	})
	return result, err
}

// The account index is supplied only after CPA verifies upstream identity. A
// replay acknowledges the original commit without changing any later binding.
func (s *Store) CommitEgressRegistration(ctx context.Context, id, provider, accountID string, expectedNodeRevision ...int64) (EgressRegistration, error) {
	expected := int64(0)
	if len(expectedNodeRevision) > 0 {
		expected = expectedNodeRevision[0]
	}
	return s.transitionEgressRegistration(ctx, id, provider, accountID, true, expected)
}

// Prepare makes a future route durable without changing the live binding.
func (s *Store) PrepareEgressRegistration(ctx context.Context, id, provider, accountID string) (EgressRegistration, error) {
	return s.transitionEgressRegistration(ctx, id, provider, accountID, false, 0)
}

func (s *Store) transitionEgressRegistration(ctx context.Context, id, provider, accountID string, commit bool, expectedNodeRevision int64) (EgressRegistration, error) {
	if _, err := uuid.Parse(id); err != nil {
		return EgressRegistration{}, ErrInvalid
	}
	if !s.WithUpstreamProvider(provider).validUpstreamProvider() || !upstreamAccountIDPattern.MatchString(accountID) {
		return EgressRegistration{}, ErrInvalid
	}
	var result EgressRegistration
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var lockedVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT version FROM egress_state WHERE singleton FOR UPDATE`).Scan(&lockedVersion); err != nil {
			return mapDBError("lock egress registration", err)
		}
		registration, err := scanEgressRegistration(tx.QueryRowContext(ctx, `SELECT `+egressRegistrationColumns+` FROM egress_registrations WHERE id=$1::uuid FOR UPDATE`, id))
		if err != nil {
			return mapDBError("find egress registration", err)
		}
		if registration.Provider != provider || registration.AccountID != "" && registration.AccountID != accountID {
			return ErrConflict
		}
		if registration.State == "committed" {
			result = registration
			return nil
		}
		if !commit && registration.State == "prepared" {
			result = registration
			return nil
		}
		if commit && registration.State != "prepared" {
			return ErrConflict
		}
		if _, err := nextEgressVersion(ctx, tx); err != nil {
			return err
		}
		// Lock table before row writes, consistently with metadata synchronization.
		if _, err = tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock registration account", err)
		}
		var insertedID string
		err = tx.QueryRowContext(ctx, `INSERT INTO upstream_accounts(id,masked_email,plan,status,provider,created_at,updated_at) VALUES($1,'','unknown','unavailable',$2,now(),now()) ON CONFLICT(id) DO UPDATE SET id=upstream_accounts.id WHERE upstream_accounts.provider=EXCLUDED.provider RETURNING id`, accountID, provider).Scan(&insertedID)
		if err != nil {
			return mapDBError("ensure registration account", err)
		}
		var currentBindingRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM upstream_account_egress WHERE account_id=$1),0)`, accountID).Scan(&currentBindingRevision); err != nil {
			return mapDBError("read registration binding revision", err)
		}
		if !commit {
			if !registration.Preserve && currentBindingRevision > registration.Revision {
				return ErrConflict
			}
			registration.BaselineBindingRevision = currentBindingRevision
		}
		if commit && !registration.Preserve {
			if registration.BaselineBindingRevision != currentBindingRevision {
				return ErrConflict
			}
			if registration.NodeID != "" && expectedNodeRevision > 0 {
				var currentNodeRevision int64
				if err := tx.QueryRowContext(ctx, `SELECT revision FROM egress_servers WHERE id=$1::uuid`, registration.NodeID).Scan(&currentNodeRevision); err != nil {
					return mapDBError("read prepared node revision", err)
				}
				if currentNodeRevision != expectedNodeRevision {
					return ErrConflict
				}
			}
			if err = setEgressBinding(ctx, tx, provider, accountID, registration.NodeID, registration.Revision); err != nil {
				return err
			}
		}
		phase := "prepared"
		if commit {
			phase = "committed"
		}
		result, err = scanEgressRegistration(tx.QueryRowContext(ctx, `UPDATE egress_registrations SET account_id=$2,state=$3,baseline_binding_revision=$4 WHERE id=$1::uuid RETURNING `+egressRegistrationColumns, id, accountID, phase, registration.BaselineBindingRevision))
		if err != nil {
			return mapDBError("commit egress registration", err)
		}
		if !commit {
			return nil
		}
		return egressAudit(ctx, tx, EgressActor{UserID: registration.ActorUserID, SessionID: registration.ActorSessionID}, "upstream_account.egress_registered", accountID, map[string]any{"provider": provider, "registration_id": id, "preserved": registration.Preserve, "node_id": registration.NodeID})
	})
	return result, err
}

func (s *Store) EgressSnapshot(ctx context.Context) (EgressState, error) {
	result := EgressState{Nodes: []EgressServer{}, Bindings: []EgressBinding{}, Registrations: []EgressRegistration{}}
	err := s.withTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT version FROM egress_state WHERE singleton`).Scan(&result.Version); err != nil {
			return mapDBError("read egress version", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+egressServerColumns+` FROM egress_servers ORDER BY id`)
		if err != nil {
			return mapDBError("list egress nodes", err)
		}
		for rows.Next() {
			n, err := scanEgressServer(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			result.Nodes = append(result.Nodes, n)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT b.account_id,a.provider,COALESCE(b.node_id::text,''),b.revision FROM upstream_account_egress b JOIN upstream_accounts a ON a.id=b.account_id ORDER BY b.account_id`)
		if err != nil {
			return mapDBError("list egress bindings", err)
		}
		for rows.Next() {
			var b EgressBinding
			if err = rows.Scan(&b.AccountID, &b.Provider, &b.NodeID, &b.Revision); err != nil {
				_ = rows.Close()
				return err
			}
			result.Bindings = append(result.Bindings, b)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT `+egressRegistrationColumns+` FROM egress_registrations ORDER BY id`)
		if err != nil {
			return mapDBError("list egress registrations", err)
		}
		for rows.Next() {
			v, err := scanEgressRegistration(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			result.Registrations = append(result.Registrations, v)
		}
		err = rows.Err()
		_ = rows.Close()
		return err
	})
	return result, err
}
