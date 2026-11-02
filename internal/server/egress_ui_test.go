package server

import (
	"os/exec"
	"testing"
)

func TestEgressDashboardBehavior(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is required for dashboard behavior tests")
	}
	output, err := exec.Command("node", "testdata/egress_ui_test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("egress dashboard tests: %v\n%s", err, output)
	}
}
