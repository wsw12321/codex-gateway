package server

import (
	"os/exec"
	"strings"
	"testing"
)

func TestModelPricesUIBehavior(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable; skipping dashboard model pricing regression tests")
	}
	command := exec.Command(node, "--test", "testdata/model_prices_ui_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("model pricing dashboard regression tests: %v\n%s", err, output)
	}
}

func TestModelPricesOwnerNavigationAndDisclosure(t *testing.T) {
	t.Parallel()
	for _, required := range []string{
		`href="#model-pricing" data-view="model-pricing" class="owner-only hidden"`,
		`class="view owner-only hidden" data-section="model-pricing"`,
		`id="model-prices-refresh"`, `id="model-prices-search"`, `id="model-price-list"`,
		`USD / 百万 tokens`, `跨重启、部署保留`, `最终费用仍按现有模型倍率计算`,
		`单价允许为零，最多 18 位整数、12 位小数`, `codex-auto-review 固定零价`,
	} {
		if !strings.Contains(string(indexHTML), required) {
			t.Errorf("model pricing dashboard missing %q", required)
		}
	}
}
