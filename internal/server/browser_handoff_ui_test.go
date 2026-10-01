package server

import (
	"os/exec"
	"testing"
)

func TestBrowserHandoffDashboardBehavior(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for executable dashboard behavior tests")
	}
	command := exec.Command(node, "--test", "testdata/browser_handoff_ui_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser handoff dashboard behavior failed: %v\n%s", err, output)
	}
}
