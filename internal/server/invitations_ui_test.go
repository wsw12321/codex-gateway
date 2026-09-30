package server

import (
	"os/exec"
	"testing"
)

func TestInvitationsUI(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable; skipping invitation dashboard regression tests")
	}
	command := exec.Command(node, "--test", "testdata/invitations_ui_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("invitation dashboard regression tests: %v\n%s", err, output)
	}
}
