package plugins

import "fmt"

// Registry maps a variant name to a factory, so configuration can select a
// plugin implementation by name (e.g. embedding.provider = "ollama"). One
// Registry per seam.
type Registry[T any] struct {
	kind      string
	factories map[string]func() (T, error)
}

// NewRegistry creates an empty registry. kind is used in error messages
// (e.g. "embedder").
func NewRegistry[T any](kind string) *Registry[T] {
	return &Registry[T]{kind: kind, factories: make(map[string]func() (T, error))}
}

// Register adds a named factory. It panics on a duplicate name, since
// registration happens at startup with static names.
func (r *Registry[T]) Register(name string, factory func() (T, error)) {
	if _, exists := r.factories[name]; exists {
		panic(fmt.Sprintf("plugins: %s variant %q already registered", r.kind, name))
	}
	r.factories[name] = factory
}

// Build constructs the named variant, or returns an error listing what is
// available.
func (r *Registry[T]) Build(name string) (T, error) {
	var zero T
	factory, ok := r.factories[name]
	if !ok {
		return zero, fmt.Errorf("unknown %s variant %q (have %v)", r.kind, name, r.Names())
	}
	return factory()
}

// Names lists the registered variant names.
func (r *Registry[T]) Names() []string {
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	return names
}
