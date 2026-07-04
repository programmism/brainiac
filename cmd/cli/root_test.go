package main

import "testing"

func TestCommandTree(t *testing.T) {
	root := newRootCmd()
	want := []string{
		"migrate", "health", "search", "recall", "remember", "link", "disambiguate", "supersede",
		"import", "refresh", "consolidate", "merge", "split", "reembed",
	}
	have := make(map[string]bool)
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

func TestStubReturnsError(t *testing.T) {
	cmd := stubCmd("consolidate", "#24")
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("stub should return a not-implemented error")
	}
}
