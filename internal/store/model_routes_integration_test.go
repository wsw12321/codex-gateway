//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestUserModelRoutesPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	user := usageSummaryUser(t, ctx, repository, "routes-"+suffix)
	other := usageSummaryUser(t, ctx, repository, "routes-other-"+suffix)
	first := time.Now().UTC().Truncate(time.Microsecond)

	routes, err := repository.ReplaceUserModelRoutes(ctx, ReplaceUserModelRoutesParams{
		UserID: user.ID, ActorUserID: user.ID, SourceIP: "192.0.2.20", At: first,
		Routes: []ModelRoute{{SourceModel: " gpt-test ", TargetModel: "claude-test"}, {SourceModel: "gpt-mini", TargetModel: "claude-test"}},
	})
	if err != nil || len(routes) != 2 || routes[0].SourceModel != "gpt-mini" || routes[1].SourceModel != "gpt-test" || routes[1].TargetModel != "claude-test" {
		t.Fatalf("initial routes=%+v err=%v", routes, err)
	}
	if target, ok, err := repository.ResolveUserModelRoute(ctx, user.ID, "gpt-test"); err != nil || !ok || target != "claude-test" {
		t.Fatalf("resolve=%q %t %v", target, ok, err)
	}
	if _, ok, err := repository.ResolveUserModelRoute(ctx, other.ID, "gpt-test"); err != nil || ok {
		t.Fatalf("other user resolved a route: %t %v", ok, err)
	}
	if _, ok, err := repository.ResolveUserModelRoute(ctx, user.ID, "claude-test"); err != nil || ok {
		t.Fatalf("target resolved as a source: %t %v", ok, err)
	}

	// Replacing keeps unchanged rules' timestamps, updates changed ones and
	// removes rules that are no longer listed.
	second := first.Add(time.Minute)
	routes, err = repository.ReplaceUserModelRoutes(ctx, ReplaceUserModelRoutesParams{
		UserID: user.ID, ActorUserID: user.ID, At: second,
		Routes: []ModelRoute{{SourceModel: "gpt-test", TargetModel: "claude-test"}, {SourceModel: "gpt-new", TargetModel: "claude-other"}},
	})
	if err != nil || len(routes) != 2 || routes[0].SourceModel != "gpt-new" || routes[1].SourceModel != "gpt-test" ||
		!routes[1].CreatedAt.Equal(first) || !routes[1].UpdatedAt.Equal(first) || !routes[0].CreatedAt.Equal(second) {
		t.Fatalf("replaced routes=%+v err=%v", routes, err)
	}
	if _, ok, err := repository.ResolveUserModelRoute(ctx, user.ID, "gpt-mini"); err != nil || ok {
		t.Fatalf("removed route still resolves: %t %v", ok, err)
	}

	for name, invalid := range map[string][]ModelRoute{
		"identity":  {{SourceModel: "gpt-test", TargetModel: "gpt-test"}},
		"duplicate": {{SourceModel: "gpt-test", TargetModel: "a"}, {SourceModel: "gpt-test", TargetModel: "b"}},
		"chain":     {{SourceModel: "a", TargetModel: "b"}, {SourceModel: "b", TargetModel: "c"}},
		"syntax":    {{SourceModel: "white space", TargetModel: "b"}},
	} {
		if _, err := repository.ReplaceUserModelRoutes(ctx, ReplaceUserModelRoutesParams{UserID: user.ID, ActorUserID: user.ID, Routes: invalid}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	tooMany := make([]ModelRoute, MaxUserModelRoutes+1)
	for i := range tooMany {
		tooMany[i] = ModelRoute{SourceModel: "source-" + strconv.Itoa(i), TargetModel: "target"}
	}
	if _, err := repository.ReplaceUserModelRoutes(ctx, ReplaceUserModelRoutesParams{UserID: user.ID, ActorUserID: user.ID, Routes: tooMany}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many routes accepted: %v", err)
	}
	if listed, err := repository.ListUserModelRoutes(ctx, user.ID); err != nil || len(listed) != 2 {
		t.Fatalf("rejected writes changed routes: %+v %v", listed, err)
	}

	var audits int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events
		WHERE event_type='user.model_routes_changed' AND subject_id=$1 AND actor_user_id=$1::uuid`, user.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audit events=%d err=%v", audits, err)
	}

	routes, err = repository.ReplaceUserModelRoutes(ctx, ReplaceUserModelRoutesParams{UserID: user.ID, ActorUserID: user.ID, Routes: nil})
	if err != nil || len(routes) != 0 {
		t.Fatalf("clear routes=%+v err=%v", routes, err)
	}

	// The table constraint backs the store validation.
	if _, err := repository.db.ExecContext(ctx, `INSERT INTO user_model_routes(user_id,source_model,target_model) VALUES($1,'same','same')`, user.ID); err == nil {
		t.Fatal("database accepted an identity route")
	}
}
