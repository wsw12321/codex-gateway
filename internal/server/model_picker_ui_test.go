package server

import (
	"os/exec"
	"testing"
)

func TestSharedModelPickerUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for dashboard regressions")
	}
	command := exec.Command(node, "--test", "testdata/model_picker.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("model picker regressions: %v\n%s", err, output)
	}
}
