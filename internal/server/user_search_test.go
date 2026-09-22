package server

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBillingUserSelectionSafety(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable; skipping dashboard billing selection regression tests")
	}
	command := exec.Command(node, "--test", "testdata/billing_user_selection.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("billing selection regression tests: %v\n%s", err, output)
	}
}

func TestDeriveUserSearchFieldsIndexesTextFullPinyinAndInitials(t *testing.T) {
	tests := []struct {
		username    string
		displayName string
		full        string
		initials    string
	}{
		{username: "Alice.Z", displayName: "张三", full: "zhangsan", initials: "zs"},
		{username: "member", displayName: "爱丽丝", full: "ailisi", initials: "als"},
		{username: "BOB", displayName: "Bob Smith", full: "bobsmith", initials: "bobsmith"},
	}
	for _, test := range tests {
		fields := deriveUserSearchFields(test.username, test.displayName)
		if fields.PinyinFull != test.full || fields.PinyinInitials != test.initials ||
			!strings.Contains(fields.SearchIndex, strings.ToLower(test.username)) ||
			!strings.Contains(fields.SearchIndex, strings.ToLower(test.displayName)) {
			t.Fatalf("deriveUserSearchFields(%q, %q) = %#v", test.username, test.displayName, fields)
		}
	}
	for _, query := range []string{"张三", "zhangsan", "zs", "alice.z", "ZHANG san"} {
		if !matchesDerivedUserSearch("user-1", "Alice.Z", "张三", query) {
			t.Fatalf("query %q did not match the derived user index", query)
		}
	}
	if matchesDerivedUserSearch("user-1", "Alice.Z", "张三", "lisi") {
		t.Fatal("unrelated pinyin matched the derived user index")
	}
}
