package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/security"
	"github.com/wsw/codex-gateway/internal/store"
)

func addBillingSourceTestSession(t *testing.T, r *http.Request) {
	t.Helper()
	token, err := security.GenerateOpaqueToken(security.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token.Token})
}

type billingSourceTestWrite struct {
	query string
	args  []driver.NamedValue
}

type billingSourceTestConn struct {
	*statusTestConn
	writes     []billingSourceTestWrite
	writeError error
}

func (c *billingSourceTestConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.writes = append(c.writes, billingSourceTestWrite{query, append([]driver.NamedValue(nil), args...)})
	if c.writeError != nil {
		return nil, c.writeError
	}
	return driver.RowsAffected(1), nil
}

type billingSourceTestConnector struct{ conn *billingSourceTestConn }

func (c billingSourceTestConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (billingSourceTestConnector) Driver() driver.Driver                          { return upstreamAuditDriver{} }

func newBillingSourceTestServer(t *testing.T, role string, verified *time.Time) (*Server, *billingSourceTestConn) {
	t.Helper()
	now := time.Now().UTC()
	var verification driver.Value
	if verified != nil {
		verification = *verified
	}
	conn := &billingSourceTestConn{statusTestConn: &statusTestConn{query: func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
		switch {
		case strings.Contains(query, "FROM sessions"):
			return &upstreamAuditRows{columns: make([]string, 14), values: []driver.Value{"session-1", "self-1", []byte("hash"), []byte("csrf"), nil, nil, now, now, now.Add(time.Hour), now.Add(time.Hour), verification, nil, "", nil}}, nil
		case strings.Contains(query, "SELECT status FROM users"):
			if len(args) != 1 || args[0].Value != "self-1" {
				return nil, errors.New("only session user may be targeted")
			}
			return &upstreamAuditRows{columns: []string{"status"}, values: []driver.Value{store.StatusActive}}, nil
		case strings.Contains(query, "FROM users"):
			return &upstreamAuditRows{columns: make([]string, 10), values: []driver.Value{"self-1", "self", "Self", []byte("webauthn-id"), role, store.StatusActive, now, now, nil, nil}}, nil
		case strings.Contains(query, "FROM billing_accounts"):
			if len(args) != 1 || args[0].Value != "self-1" {
				return nil, errors.New("only session user may be targeted")
			}
			return &upstreamAuditRows{columns: []string{"disabled"}, values: []driver.Value{false}}, nil
		default:
			return nil, errors.New("unexpected billing preference query")
		}
	}}}
	db := sql.OpenDB(billingSourceTestConnector{conn: conn})
	t.Cleanup(func() { _ = db.Close() })
	s := &Server{
		store: store.New(db), mux: http.NewServeMux(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: config.Config{UsagePricing: config.UsagePricing{Models: map[string]config.ModelPricing{"gpt-test": {}}}, RPOrigins: []string{"https://gateway.example"}, ReauthMaxAge: 5 * time.Minute, TokenPepper: []byte(strings.Repeat("p", 32))},
	}
	s.routes()
	return s, conn
}
