package server

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
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

// The dashboard repeats the family table; it must name exactly the families
// the store accepts, in the same order and with the same labels.
func TestDashboardModelFamiliesMatchConfig(t *testing.T) {
	source, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "const modelFamilyScopes = [")
	if start < 0 {
		t.Fatal("modelFamilyScopes table missing from app.js")
	}
	end := strings.Index(text[start:], "];")
	if end < 0 {
		t.Fatal("modelFamilyScopes table is not terminated")
	}
	var want strings.Builder
	want.WriteString("const modelFamilyScopes = [\n")
	for _, family := range config.ModelFamilies() {
		fmt.Fprintf(&want, "  {token: %q, group: %q, label: %q},\n", family.Token, family.Group, family.Label)
	}
	if got := text[start : start+end]; got != want.String() {
		t.Fatalf("dashboard families:\n%s\nwant:\n%s", got, want.String())
	}
}
