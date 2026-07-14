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

func TestRootNameFromEnv(t *testing.T) {
	if got := newRootCmd().Name(); got != "kb" {
		t.Errorf("default root name = %q, want kb", got)
	}
	t.Setenv("BRAINIAC_CLI_NAME", "brainiac")
	if got := newRootCmd().Name(); got != "brainiac" {
		t.Errorf("root name with BRAINIAC_CLI_NAME = %q, want brainiac", got)
	}
}

func TestStubReturnsError(t *testing.T) {
	cmd := stubCmd("consolidate", "#24")
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("stub should return a not-implemented error")
	}
}
