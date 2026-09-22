package server

import (
	"os/exec"
	"strings"
	"testing"
)

func TestInformationDashboardBehavior(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for dashboard behavior tests")
	}
	command := exec.Command(node, "--test", "testdata/information_ui_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("information dashboard behavior failed: %v\n%s", err, output)
	}
}

func TestInformationDashboardOwnerNavigationAndConfirmation(t *testing.T) {
	t.Parallel()
	for _, required := range []string{
		`href="#information" data-view="information" class="owner-only hidden"`,
		`class="view owner-only hidden" data-section="information"`,
		`id="information-preview-form"`, `name="retention_days"`, `value="90"`,
		`id="information-create-job"`, `id="information-user-rows"`, `id="information-delete-users"`,
		`id="usage-cleaned-history"`, `id="upstream-concurrency-sampled"`,
	} {
		if !strings.Contains(string(indexHTML), required) {
			t.Errorf("information dashboard is missing %q", required)
		}
	}
}
