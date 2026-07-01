package core

import (
	"context"
	"testing"
)

func TestNew(t *testing.T) {
	if New(nil, nil, nil) == nil {
		t.Fatal("New() returned nil")
	}
}

func TestVersion(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
}

// TestEmptyQueryShortCircuits verifies a blank query returns nothing without
// touching the embedder/DB (covers the MCP path too, #82).
func TestEmptyQueryShortCircuits(t *testing.T) {
	c := New(nil, nil, nil) // nil deps are never reached for a blank query
	if hits, err := c.Search(context.Background(), "   ", 5); err != nil || hits != nil {
		t.Fatalf("empty search = %v, %v; want nil, nil", hits, err)
	}
	res, err := c.Recall(context.Background(), "\t\n")
	if err != nil || len(res.Chunks) != 0 || len(res.Nodes) != 0 {
		t.Fatalf("empty recall = %+v, %v; want empty", res, err)
	}
}
