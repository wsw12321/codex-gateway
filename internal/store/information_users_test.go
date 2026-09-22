package store

import (
	"errors"
	"testing"
)

func TestInformationDeletionUserSelection(t *testing.T) {
	const a = "01234567-89ab-4cde-8fab-012345678901"
	const b = "11234567-89ab-4cde-8fab-012345678901"
	for _, ids := range [][]string{nil, {}, {a, a}, {"not-a-uuid"}, {"00000000-0000-0000-0000-000000000000"}, make([]string, 101)} {
		if _, err := normalizeInformationUserIDs(ids); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid selection accepted: %v", ids)
		}
	}
	ids, err := normalizeInformationUserIDs([]string{b, a})
	if err != nil || len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Fatalf("canonical selection: %v %v", ids, err)
	}
}
