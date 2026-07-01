package core

import "testing"

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
