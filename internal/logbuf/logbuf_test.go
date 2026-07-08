package logbuf

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestWriteSplitsLines(t *testing.T) {
	b := New(10)
	_, _ = b.Write([]byte("one\ntwo\n"))
	_, _ = b.Write([]byte("three\n"))
	got := b.Lines(0)
	want := []string{"one", "two", "three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %v, want %v", got, want)
	}
}

func TestPartialLineBufferedUntilNewline(t *testing.T) {
	b := New(10)
	_, _ = b.Write([]byte("hel"))
	if len(b.Lines(0)) != 0 {
		t.Fatalf("partial line stored before newline: %v", b.Lines(0))
	}
	_, _ = b.Write([]byte("lo\n"))
	got := b.Lines(0)
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("lines = %v, want [hello]", got)
	}
}

func TestRingDropsOldest(t *testing.T) {
	b := New(3)
	for i := 0; i < 5; i++ {
		_, _ = fmt.Fprintf(b, "line%d\n", i)
	}
	got := b.Lines(0)
	want := []string{"line2", "line3", "line4"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %v, want %v (ring should keep the newest)", got, want)
	}
}

func TestLinesLimit(t *testing.T) {
	b := New(10)
	for i := 0; i < 5; i++ {
		_, _ = fmt.Fprintf(b, "l%d\n", i)
	}
	got := b.Lines(2)
	if strings.Join(got, "|") != "l3|l4" {
		t.Fatalf("Lines(2) = %v, want [l3 l4]", got)
	}
}

func TestRedaction(t *testing.T) {
	cases := map[string]string{
		"token github_pat_11ABCDEFGHIJKLMNOPQRSTUV_rest here": "github_pat_",
		"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789":            "ghp_",
		"Authorization: Bearer abcdef1234567890":              "Bearer ",
		"password = hunter2secret":                            "password",
	}
	for in, keepPrefix := range cases {
		out := Redact(in)
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("Redact(%q) = %q, expected a [REDACTED] mask", in, out)
		}
		// The token body itself must be gone.
		if strings.Contains(out, "hunter2secret") || strings.Contains(out, "github_pat_11ABCDEFGHIJKLMNOPQRSTUV_rest") ||
			strings.Contains(out, "abcdef1234567890") {
			t.Errorf("Redact(%q) = %q leaked the secret body", in, out)
		}
		_ = keepPrefix
	}
}

func TestWriteConcurrent(t *testing.T) {
	b := New(1000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_, _ = fmt.Fprintf(b, "g%d-%d\n", g, i)
			}
		}(g)
	}
	wg.Wait()
	if got := len(b.Lines(0)); got != 800 {
		t.Fatalf("got %d lines, want 800", got)
	}
}
