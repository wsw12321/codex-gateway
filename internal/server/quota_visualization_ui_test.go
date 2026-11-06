package server

import (
	"os/exec"
	"testing"
)

func TestQuotaVisualizationUI(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable; skipping quota freshness and visualization regression tests")
	}
	command := exec.Command(node, "--test", "testdata/quota_visualization_ui_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("quota visualization regression tests: %v\n%s", err, output)
	}
}
