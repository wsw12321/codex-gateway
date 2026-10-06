package server

import (
	"os/exec"
	"strings"
	"testing"
)

func TestUserUpstreamAccessUIBehavior(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable; skipping user upstream access dashboard regressions")
	}
	command := exec.Command(node, "--test", "testdata/user_upstream_access_ui_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("user upstream access dashboard regression tests: %v\n%s", err, output)
	}
}

func TestUserUpstreamAccessOwnerNavigation(t *testing.T) {
	t.Parallel()
	for _, required := range []string{
		`href="#user-upstream-access" data-view="user-upstream-access" class="owner-only hidden"`,
		`class="view owner-only hidden" data-section="user-upstream-access"`,
		`id="user-upstream-user-search"`, `id="user-upstream-cards"`,
		`Owner 的普通模型请求同样受限`,
	} {
		if !strings.Contains(string(indexHTML), required) {
			t.Errorf("user upstream access dashboard missing %q", required)
		}
	}
}
