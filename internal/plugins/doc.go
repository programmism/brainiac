// Package plugins defines the four swappable seams of Brainiac — source
// connectors, extractors, selectors, and embedders — plus the registry that
// lets configuration select a variant by name.
//
// The interfaces are drawn from the start (issue #7) but each has exactly one
// implementation for v1 (Notion connector, chat-driven extractor,
// density-filter selector, Ollama embedder). See SYSTEM.md §7.
package plugins
