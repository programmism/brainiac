// Package config loads Brainiac's single YAML configuration file and applies
// environment overrides for secrets. All domain specificity lives here so the
// core and plugins stay general (SYSTEM.md §3, PRD §19).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the whole configuration tree. Same engine, different domain =
// different YAML.
type Config struct {
	HTTP          HTTPConfig          `yaml:"http"`
	Storage       StorageConfig       `yaml:"storage"`
	Embedding     EmbeddingConfig     `yaml:"embedding"`
	Extraction    ExtractionConfig    `yaml:"extraction"`
	Consolidation ConsolidationConfig `yaml:"consolidation"`
	Sources       []SourceConfig      `yaml:"sources"`
	Clients       ClientsConfig       `yaml:"clients"`
}

// HTTPConfig configures the REST/WebUI server.
type HTTPConfig struct {
	Addr string `yaml:"addr"`
}

// StorageConfig points at Postgres. DSN is a secret — set it via DATABASE_URL.
type StorageConfig struct {
	DSN string `yaml:"dsn"`
}

// EmbeddingConfig selects the embedder plugin and its model.
type EmbeddingConfig struct {
	Provider string `yaml:"provider"`
	BaseURL  string `yaml:"base_url"`
	Model    string `yaml:"model"`
	Dims     int    `yaml:"dims"`
}

// ExtractionConfig selects how text becomes nodes/edges.
type ExtractionConfig struct {
	Default  string `yaml:"default"`
	Fallback string `yaml:"fallback"`
}

// ConsolidationConfig configures the librarian pass.
type ConsolidationConfig struct {
	Schedule string `yaml:"schedule"`
	Merge    string `yaml:"merge"`
}

// SourceConfig declares one connector and its selection strategy. Token is a
// secret — prefer setting it via the environment (e.g. NOTION_TOKEN).
type SourceConfig struct {
	Type      string `yaml:"type"`
	Selection string `yaml:"selection"`
	Token     string `yaml:"token,omitempty"`
	Path      string `yaml:"path,omitempty"` // for file-based connectors (markdown)
}

// Source returns the first configured source of the given type, or nil.
func (c *Config) Source(typ string) *SourceConfig {
	for i := range c.Sources {
		if c.Sources[i].Type == typ {
			return &c.Sources[i]
		}
	}
	return nil
}

// ClientsConfig toggles the surfaces.
type ClientsConfig struct {
	MCP   bool   `yaml:"mcp"`
	WebUI string `yaml:"webui"`
	CLI   bool   `yaml:"cli"`
}

// Default returns the built-in defaults. A missing config file is fine — these
// plus environment overrides are enough to boot the prototype.
func Default() *Config {
	c := &Config{}
	c.HTTP.Addr = ":8080"
	c.Embedding.Provider = "ollama"
	c.Embedding.BaseURL = "http://localhost:11434"
	c.Embedding.Model = "nomic-embed-text"
	c.Embedding.Dims = 768
	c.Extraction.Default = "chat-driven"
	c.Consolidation.Schedule = "weekly"
	c.Consolidation.Merge = "human-approved"
	c.Clients.MCP = true
	c.Clients.WebUI = "read-only"
	c.Clients.CLI = true
	return c
}

// Load reads config from path (if it exists), layers environment overrides on
// top, and validates the result. A non-existent path is not an error — it
// falls back to Default() + env.
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path) //nolint:gosec // path is operator-controlled
	switch {
	case err == nil:
		if uerr := yaml.Unmarshal(data, cfg); uerr != nil {
			return nil, fmt.Errorf("parse %s: %w", path, uerr)
		}
	case errors.Is(err, fs.ErrNotExist):
		// no file — defaults + env only
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	cfg.applyEnvOverrides()

	if verr := cfg.Validate(); verr != nil {
		return nil, verr
	}
	return cfg, nil
}

// applyEnvOverrides layers secrets/deployment values from the environment on
// top of the file. Environment always wins.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		c.Storage.DSN = v
	}
	if v := os.Getenv("OLLAMA_URL"); v != "" {
		c.Embedding.BaseURL = v
	}
	if v := os.Getenv("HTTP_ADDR"); v != "" {
		c.HTTP.Addr = v
	}
	if v := os.Getenv("NOTION_TOKEN"); v != "" {
		for i := range c.Sources {
			if c.Sources[i].Type == "notion" {
				c.Sources[i].Token = v
			}
		}
	}
}

// Validate checks the invariants the app depends on to boot.
func (c *Config) Validate() error {
	if c.Storage.DSN == "" {
		return errors.New("storage.dsn is empty (set it in config.yaml or via DATABASE_URL)")
	}
	if c.HTTP.Addr == "" {
		return errors.New("http.addr is empty")
	}
	if c.Embedding.Dims <= 0 {
		return fmt.Errorf("embedding.dims must be positive, got %d", c.Embedding.Dims)
	}
	return nil
}
