// Package config loads Brainiac's single YAML configuration file and applies
// environment overrides for secrets. All domain specificity lives here so the
// core and plugins stay general (SYSTEM.md §3, PRD §19).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/programmism/brainiac/internal/model"

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
	Ingest        IngestConfig        `yaml:"ingest"`
}

// IngestConfig controls optional background auto-import.
type IngestConfig struct {
	// Interval as a Go duration string (e.g. "60s"); empty disables auto-import.
	Interval string `yaml:"interval"`
}

// AutoImportInterval returns the parsed interval, or 0 if unset/invalid.
func (c *Config) AutoImportInterval() time.Duration {
	d, err := time.ParseDuration(c.Ingest.Interval)
	if err != nil {
		return 0
	}
	return d
}

// HTTPConfig configures the REST/WebUI server.
type HTTPConfig struct {
	Addr string `yaml:"addr"`
	// AuthToken, if set, is the bearer token required for write endpoints.
	// Prefer setting it via AUTH_TOKEN in the environment.
	AuthToken string `yaml:"auth_token,omitempty"`
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
	// BatchSize is how many chunks bulk ingest sends per embed request (#140).
	// 0 = the embedder's default. Tune against the Ollama box's memory.
	BatchSize int `yaml:"batch_size"`
}

// ExtractionConfig selects how text becomes nodes/edges. The default
// "chat-driven" bypasses any server-side extractor (Claude supplies structure
// via remember/link). Setting Default to "local-llm" turns on the optional
// Ollama extractor during ingest — for a box beefy enough to run a chat model —
// while a weak box keeps the free chat-driven path (SYSTEM.md §7).
type ExtractionConfig struct {
	Default string `yaml:"default"`
	// BaseURL is the Ollama endpoint for the extraction model; empty falls back
	// to the embedding base_url (they usually share one Ollama).
	BaseURL string `yaml:"base_url"`
	// Model is the chat model used for local extraction (e.g. "llama3.1").
	// Required when Default == "local-llm".
	Model string `yaml:"model"`
	// Retries bounds transient-failure attempts per chunk (<=0 uses the default).
	Retries int `yaml:"retries"`
	// Review routes extracted nodes/edges to the review queue (status 'proposed')
	// instead of writing them live. Default true — a local model is weaker than
	// Claude, so its output is gated by human approval unless explicitly trusted.
	Review bool `yaml:"review"`
}

// LocalExtractionEnabled reports whether the optional local-LLM extractor is
// turned on.
func (c *Config) LocalExtractionEnabled() bool {
	return c.Extraction.Default == "local-llm"
}

// ExtractorBaseURL is the Ollama endpoint for extraction: the explicit
// extraction base_url, or the embedding one when unset (they usually share one
// Ollama).
func (c *Config) ExtractorBaseURL() string {
	if c.Extraction.BaseURL != "" {
		return c.Extraction.BaseURL
	}
	return c.Embedding.BaseURL
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
	c.Extraction.Review = true // gate local-LLM output on human approval by default
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
	if v := os.Getenv("AUTH_TOKEN"); v != "" {
		c.HTTP.AuthToken = v
	}
	// WEBUI_MODE ("read-only"|"interactive") — the only way to enable WebUI write
	// actions in the shipped image, which carries no config.yaml. Writes also need
	// AUTH_TOKEN (secure by default).
	if v := os.Getenv("WEBUI_MODE"); v != "" {
		c.Clients.WebUI = v
	}
	if v := os.Getenv("INGEST_INTERVAL"); v != "" {
		c.Ingest.Interval = v
	}
	// Local-LLM extractor (opt-in). EXTRACTOR=local-llm turns it on; the model is
	// required in that case, the URL defaults to the embedding Ollama.
	if v := os.Getenv("EXTRACTOR"); v != "" {
		c.Extraction.Default = v
	}
	if v := os.Getenv("EXTRACTION_MODEL"); v != "" {
		c.Extraction.Model = v
	}
	if v := os.Getenv("EXTRACTION_URL"); v != "" {
		c.Extraction.BaseURL = v
	}
	if v := os.Getenv("EXTRACTION_REVIEW"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Extraction.Review = b
		}
	}
	if v := os.Getenv("NOTION_TOKEN"); v != "" {
		found := false
		for i := range c.Sources {
			if c.Sources[i].Type == "notion" {
				c.Sources[i].Token = v
				found = true
			}
		}
		if !found {
			// Auto-create a notion source so the token alone is enough to import.
			c.Sources = append(c.Sources, SourceConfig{Type: "notion", Selection: "density-filter", Token: v})
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
	if c.Embedding.Dims != model.SchemaEmbeddingDims {
		return fmt.Errorf("embedding.dims must be %d to match the schema, got %d", model.SchemaEmbeddingDims, c.Embedding.Dims)
	}
	if c.Embedding.Provider == "" || c.Embedding.Model == "" || c.Embedding.BaseURL == "" {
		return errors.New("embedding.provider, embedding.model and embedding.base_url must all be set")
	}
	if c.LocalExtractionEnabled() && c.Extraction.Model == "" {
		return errors.New("extraction.model must be set when extraction.default is 'local-llm' (set it or EXTRACTION_MODEL)")
	}
	return nil
}

// RedactedDSN masks the password in a DSN for safe logging.
func RedactedDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	if _, hasPw := u.User.Password(); hasPw {
		u.User = url.UserPassword(u.User.Username(), "****")
	}
	return u.String()
}
