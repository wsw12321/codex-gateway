package server

import (
	"os/exec"
	"testing"
)

func TestBillingSourcePreferencesUI(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable; skipping dashboard billing source regression tests")
	}
	command := exec.Command(node, "--test", "testdata/billing_source_preferences.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("billing source regression tests: %v\n%s", err, output)
	}
}
