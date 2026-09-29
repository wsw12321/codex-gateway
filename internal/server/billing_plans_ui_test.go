package server

import (
	"os/exec"
	"testing"
)

func TestBillingPlansUI(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable; skipping dashboard plan regression tests")
	}
	command := exec.Command(node, "--test", "testdata/billing_plans.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("billing plan regression tests: %v\n%s", err, output)
	}
}
