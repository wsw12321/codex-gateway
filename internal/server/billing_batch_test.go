package server

import (
	"os/exec"
	"testing"
)

func TestBillingBatchSafety(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable; skipping dashboard batch billing regression tests")
	}
	command := exec.Command(node, "--test", "testdata/billing_batch.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("batch billing regression tests: %v\n%s", err, output)
	}
}
