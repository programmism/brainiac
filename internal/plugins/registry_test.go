package plugins

import (
	"context"
	"testing"
)

// fakeEmbedder verifies the Embedder interface is implementable and lets us
// exercise the registry with a real interface type.
type fakeEmbedder struct{ dims int }

func (f fakeEmbedder) Embed(context.Context, string) ([]float32, error) {
	return make([]float32, f.dims), nil
}
func (f fakeEmbedder) Dims() int { return f.dims }

var _ Embedder = fakeEmbedder{}

func TestRegistryBuild(t *testing.T) {
	r := NewRegistry[Embedder]("embedder")
	r.Register("fake", func() (Embedder, error) { return fakeEmbedder{dims: 768}, nil })

	e, err := r.Build("fake")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if e.Dims() != 768 {
		t.Errorf("dims = %d", e.Dims())
	}
}

func TestRegistryUnknown(t *testing.T) {
	r := NewRegistry[Embedder]("embedder")
	if _, err := r.Build("nope"); err == nil {
		t.Fatal("expected error for unknown variant")
	}
}

func TestRegistryDuplicatePanics(t *testing.T) {
	r := NewRegistry[Selector]("selector")
	r.Register("x", func() (Selector, error) { return nil, nil })

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	r.Register("x", func() (Selector, error) { return nil, nil })
}
