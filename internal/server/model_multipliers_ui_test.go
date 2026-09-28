package server

import (
	"os/exec"
	"strings"
	"testing"
)

func TestModelMultipliersUIBehavior(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable; skipping dashboard model multiplier regression tests")
	}
	command := exec.Command(node, "--test", "testdata/model_multipliers_ui_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("model multiplier dashboard regression tests: %v\n%s", err, output)
	}
}

func TestModelMultipliersOwnerNavigationAndDisclosure(t *testing.T) {
	t.Parallel()
	for _, required := range []string{
		`href="#model-multipliers" data-view="model-multipliers" class="owner-only hidden"`,
		`class="view owner-only hidden" data-section="model-multipliers"`,
		`id="model-multipliers-refresh"`, `id="model-multiplier-list"`,
		`仅影响新请求`, `最多 18 位整数、12 位小数`,
		`Gemini 已归一化别名共用倍率`, `codex-auto-review 固定为 1.0`,
	} {
		if !strings.Contains(string(indexHTML), required) {
			t.Errorf("model multiplier dashboard missing %q", required)
		}
	}
}
