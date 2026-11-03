package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeUpstreamAccountAlias(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		input, want string
		valid       bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"  主力 Plus  ", "主力 Plus", true},
		{"\u3000备用-02\u3000", "备用-02", true},
		{"ok (team-a) / 1", "ok (team-a) / 1", true},
		{"👨\u200d💻 dev", "👨\u200d💻 dev", true},
		{strings.Repeat("号", 40), strings.Repeat("号", 40), true},
		{" " + strings.Repeat("a", 40) + " ", strings.Repeat("a", 40), true},
		{strings.Repeat("号", 41), "", false},
		{"user@example.com", "", false},
		{"u***@example.com", "", false},
		{"tab\tinside", "", false},
		{"line\nbreak", "", false},
		{"del\x7f", "", false},
		{"zero\x00", "", false},
		{"na\u0085me", "", false},
		{"a\u2028b", "", false},
		{"a\u2029b", "", false},
		{"\u202eevil", "", false},
		{"isolate\u2066x\u2069", "", false},
		{"\xff", "", false},
	} {
		got, err := NormalizeUpstreamAccountAlias(test.input)
		if test.valid && (err != nil || got != test.want) {
			t.Errorf("NormalizeUpstreamAccountAlias(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
		if !test.valid && !errors.Is(err, ErrInvalid) {
			t.Errorf("NormalizeUpstreamAccountAlias(%q) = %q, %v; want ErrInvalid", test.input, got, err)
		}
	}
}

func TestUpstreamAccountAliasRejectsInvalidWritesBeforeDatabase(t *testing.T) {
	t.Parallel()
	repository := New(nil)
	for _, params := range []SetUpstreamAccountAliasParams{
		{AccountID: "0123456789abcdef", Alias: "a@b", ActorUserID: "actor"},
		{AccountID: "0123456789abcdef", Alias: strings.Repeat("x", 41), ActorUserID: "actor"},
		{AccountID: "0123456789abcdeF", Alias: "ok", ActorUserID: "actor"},
		{AccountID: "0123456789abcdef", Alias: "ok"},
	} {
		if _, err := repository.SetUpstreamAccountAlias(context.Background(), params); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid write %+v error = %v, want ErrInvalid", params, err)
		}
	}
}

func TestUpstreamAccountAliasMigration(t *testing.T) {
	t.Parallel()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range migrations {
		if migration.Name == "0041_upstream_account_alias.sql" {
			sql = migration.SQL
		}
	}
	if sql == "" {
		t.Fatal("0041_upstream_account_alias.sql is missing")
	}
	for _, required := range []string{
		"alias TEXT NOT NULL DEFAULT ''",
		"char_length(alias) <= 40",
		"strpos(alias, '@') = 0",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("alias migration missing %q", required)
		}
	}
}
