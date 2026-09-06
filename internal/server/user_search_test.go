package server

import (
	"os/exec"
	"testing"
)

func TestBillingUserSelectionSafety(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable; skipping dashboard billing selection regression tests")
	}
	command := exec.Command(node, "--test", "testdata/billing_user_selection.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("billing selection regression tests: %v\n%s", err, output)
	}
}
