package store

import "testing"

func TestCloneUpstreamAllocationGroupsIsImmutable(t *testing.T) {
	original := []UpstreamAllocationGroup{{
		GroupID:    "00000000-0000-0000-0000-000000000001",
		AccountIDs: []string{"0123456789abcdef", "fedcba9876543210"},
	}}
	snapshot := cloneUpstreamAllocationGroups(original)
	if len(snapshot) != 1 || len(snapshot[0].AccountIDs) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}

	// Mutating the source after admission must not change the capability that
	// will be forwarded to CPA.
	original[0].AccountIDs[0] = "aaaaaaaaaaaaaaaa"
	original[0].AccountIDs = append(original[0].AccountIDs, "bbbbbbbbbbbbbbbb")
	if snapshot[0].AccountIDs[0] != "0123456789abcdef" || len(snapshot[0].AccountIDs) != 2 {
		t.Fatalf("source mutation changed snapshot: %#v", snapshot)
	}

	// Conversely, conversion of a request's snapshot must not mutate the
	// admission-owned value if a caller edits its forwarding copy.
	forwarded := cloneUpstreamAllocationGroups(snapshot)
	forwarded[0].AccountIDs[0] = "cccccccccccccccc"
	if snapshot[0].AccountIDs[0] != "0123456789abcdef" {
		t.Fatalf("forwarding mutation changed admission snapshot: %#v", snapshot)
	}
}

func TestCloneUpstreamAllocationGroupsPreservesEmptyVsNil(t *testing.T) {
	if got := cloneUpstreamAllocationGroups(nil); got != nil {
		t.Fatalf("nil snapshot became non-nil: %#v", got)
	}
	empty := []UpstreamAllocationGroup{}
	got := cloneUpstreamAllocationGroups(empty)
	if got == nil || len(got) != 0 {
		t.Fatalf("empty snapshot changed shape: %#v", got)
	}
}
