package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestUpstreamAllocationLargestDeficit(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		candidates []upstreamAllocationCandidate
		want       string
	}{
		{"weighted deficit", []upstreamAllocationCandidate{{"a", 5, "5"}, {"b", 20, "5"}}, "b"},
		{"smaller share under target", []upstreamAllocationCandidate{{"a", 5, "1"}, {"b", 20, "9"}}, "a"},
		{"exclude drained cost", []upstreamAllocationCandidate{{"a", 5, "1"}, {"b", 20, "9"}, {"drained", 0, "1000000"}}, "a"},
		{"candidate subset recomputes target", []upstreamAllocationCandidate{{"b", 20, "9"}}, "b"},
		{"exact tiny deficit with large sums", []upstreamAllocationCandidate{{"a", 1, "999999999999999999.000000000001"}, {"b", 1, "999999999999999999.000000000000"}}, "b"},
		{"exact fractional cost", []upstreamAllocationCandidate{{"a", 5, "0.000000000002"}, {"b", 20, "0.000000000009"}}, "a"},
		{"weight integer upper bound", []upstreamAllocationCandidate{{"a", MaxUpstreamAllocationWeight, "0"}, {"b", 1, "1"}}, "a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			selected, err := selectUpstreamAllocation(test.candidates, func(int64) (int64, error) {
				t.Fatal("unique maximum must not draw a random number")
				return 0, nil
			})
			if err != nil || selected != test.want {
				t.Fatalf("selection = %q, %v; want %q", selected, err, test.want)
			}
		})
	}
}

func TestUpstreamAllocationWeightedTies(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		candidates []upstreamAllocationCandidate
	}{
		{"zero history", []upstreamAllocationCandidate{{"a", 5, "0"}, {"b", 20, "0"}}},
		{"balanced cost", []upstreamAllocationCandidate{{"a", 5, "0.000000000005"}, {"b", 20, "0.000000000020"}}},
		{"only maximum ties", []upstreamAllocationCandidate{{"a", 5, "0.005"}, {"b", 20, "0.035"}, {"c", 100, "0.21"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			counts := map[string]int{}
			for draw := int64(0); draw < 25; draw++ {
				id, err := selectUpstreamAllocation(test.candidates, func(limit int64) (int64, error) {
					if limit != 25 {
						t.Fatalf("random limit = %d, want 25", limit)
					}
					return draw, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				counts[id]++
			}
			if counts["a"] != 5 || counts["b"] != 20 || counts["c"] != 0 {
				t.Fatalf("weighted tie counts = %v, want 5:20", counts)
			}
		})
	}
}

func TestUpstreamAllocationEqualChargesConvergeToWeights(t *testing.T) {
	t.Parallel()
	candidates := []upstreamAllocationCandidate{{"a", 5, "0"}, {"b", 20, "0"}}
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		id, err := selectUpstreamAllocation(candidates, func(int64) (int64, error) { return 0, nil })
		if err != nil {
			t.Fatal(err)
		}
		counts[id]++
		for index := range candidates {
			if candidates[index].id == id {
				candidates[index].cost = fmt.Sprint(counts[id])
			}
		}
	}
	if counts["a"] != 200 || counts["b"] != 800 {
		t.Fatalf("settled allocation proportions = %v, want 200:800", counts)
	}
}

func TestUpstreamAllocationFailsClosed(t *testing.T) {
	t.Parallel()
	for _, candidates := range [][]upstreamAllocationCandidate{
		nil, {{"a", 0, "0"}}, {{"a", 0, "1"}, {"b", 0, "2"}},
	} {
		if _, err := selectUpstreamAllocation(candidates, nil); !errors.Is(err, ErrNoUpstreamAccount) {
			t.Fatalf("no eligible candidates error = %v", err)
		}
	}
	for _, candidates := range [][]upstreamAllocationCandidate{
		{{"a", -1, "0"}}, {{"a", 1, "bad"}}, {{"a", 1, "-0.01"}},
		make([]upstreamAllocationCandidate, MaxUpstreamAllocationCandidates+1),
	} {
		if _, err := selectUpstreamAllocation(candidates, nil); err == nil {
			t.Fatalf("invalid candidates %v succeeded", candidates)
		}
	}
	randErr := errors.New("random source unavailable")
	if _, err := selectUpstreamAllocation([]upstreamAllocationCandidate{{"a", 1, "0"}, {"b", 1, "0"}},
		func(int64) (int64, error) { return 0, randErr }); !errors.Is(err, randErr) {
		t.Fatalf("random error = %v", err)
	}
}

func TestUpstreamAllocationRejectsNoncanonicalCandidatesBeforeDatabase(t *testing.T) {
	t.Parallel()
	repository := New(nil)
	for _, ids := range [][]string{
		nil, {"0123456789abcdeF"}, {" 0123456789abcdef"}, {""},
		{"0123456789abcdef", "0123456789abcdef"}, make([]string, MaxUpstreamAllocationCandidates+1),
	} {
		if _, err := repository.SelectUpstreamAccount(context.Background(), "00000000-0000-0000-0000-000000000001", ids, time.Now()); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid candidates %v error = %v", ids, err)
		}
	}
}

func TestUpstreamAllocationRejectsInvalidWeightWritesBeforeDatabase(t *testing.T) {
	t.Parallel()
	repository := New(nil)
	for _, params := range []SetUpstreamAccountAllocationWeightParams{
		{AccountID: "0123456789abcdef", Weight: -1, ActorUserID: "actor"},
		{AccountID: "0123456789abcdeF", Weight: 1, ActorUserID: "actor"},
		{AccountID: "0123456789abcdef", Weight: 1},
	} {
		if _, err := repository.SetUpstreamAccountAllocationWeight(context.Background(), params); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid write error = %v", err)
		}
	}
}

func TestUpstreamAllocationMigration(t *testing.T) {
	t.Parallel()
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range migrations {
		if migration.Name == "0009_upstream_allocation.sql" {
			sql = migration.SQL
		}
	}
	for _, required := range []string{"allocation_weight INTEGER NOT NULL DEFAULT 1", "CHECK (allocation_weight >= 0)",
		"(upstream_account_id, (COALESCE(usage_requested_at, created_at)))", "WHERE entry_type = 'usage_charge'"} {
		if !strings.Contains(sql, required) {
			t.Errorf("allocation migration missing %q", required)
		}
	}
}

func equalAllocationDecimal(left, right string) bool {
	l, lok := new(big.Rat).SetString(left)
	r, rok := new(big.Rat).SetString(right)
	return lok && rok && l.Cmp(r) == 0
}
