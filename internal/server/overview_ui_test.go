package server

import (
	"os/exec"
	"testing"
)

func TestOverviewUI(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable; skipping dashboard overview regression tests")
	}
	command := exec.Command(node, "--test", "testdata/overview_ui_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("overview regression tests: %v\n%s", err, output)
	}
}
