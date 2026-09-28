package server

import (
	"os/exec"
	"testing"
)

func TestClientConfigurator(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable; skipping client configurator regression tests")
	}
	command := exec.Command(node, "--test", "testdata/configure_client_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("client configurator regression tests: %v\n%s", err, output)
	}
}
