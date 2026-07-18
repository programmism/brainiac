// Package config loads Brainiac's single YAML configuration file and applies
// environment overrides for secrets. All domain specificity lives here so the
// core and plugins stay general (SYSTEM.md §3, PRD §19).
package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/programmism/brainiac/internal/core"
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
	Logging       LoggingConfig       `yaml:"logging"`
	Retrieval     RetrievalConfig     `yaml:"retrieval"`
	Index         IndexConfig         `yaml:"index"`
	Tiering       TieringConfig       `yaml:"tiering"`
	Retention     RetentionConfig     `yaml:"retention"`
	// Principals is the opt-in Layer 2 hard-isolation roster (#120). Empty =
	// Layer 1 (open reads, single AUTH_TOKEN gates writes) — unchanged. When set,
	// every /api call requires a principal's bearer token, reads are walled to its
	// namespaces, and writes are pinned to its single target.
	Principals []PrincipalConfig `yaml:"principals,omitempty"`
}

// PrincipalConfig maps one bearer token to a hard-isolation identity (#120): the
// project namespaces it may read and the single namespace its writes are pinned
// to. Token is a secret — prefer PRINCIPAL_TOKEN_<NAME> in the environment.
type PrincipalConfig struct {
	Name  string   `yaml:"name"`
	Read  []string `yaml:"read"`  // project namespaces; "" or "global" = shared/global
	Write string   `yaml:"write"` // single write target namespace
	Token string   `yaml:"token,omitempty"`
	// TokenSHA256 is the hash-at-rest alternative to Token (#269): store the
	// SHA-256 (hex) of the bearer token instead of the secret itself, so a leaked
	// config.yaml never contains a live credential. Exactly one of Token /
	// TokenSHA256 must be set. Generate with `brainiac token hash`.
	TokenSHA256 string `yaml:"token_sha256,omitempty"`
	// Expires is an optional RFC3339 timestamp after which the token stops
	// authenticating (#269); empty = never. Enforced per request against the wall
	// clock, so expiry is "hot" — no restart needed.
	Expires string `yaml:"expires,omitempty"`
	// Revoked, when true, hard-disables the token (#269). Combined with SIGHUP
	// reload (HTTP) this is hot revocation without a restart.
	Revoked bool `yaml:"revoked,omitempty"`
	// MaxNodes / MaxChunks cap the namespace's row counts (#186). 0 = unlimited.
	MaxNodes  int `yaml:"max_nodes,omitempty"`
	MaxChunks int `yaml:"max_chunks,omitempty"`
}

// resolvedTokenHash returns the SHA-256 the presented bearer token is compared
// against: the decoded token_sha256, or SHA-256(token). ok is false when neither
// is set or token_sha256 is not 32 bytes of hex.
func (p PrincipalConfig) resolvedTokenHash() (sum [32]byte, ok bool) {
	switch {
	case p.TokenSHA256 != "":
		b, err := hex.DecodeString(p.TokenSHA256)
		if err != nil || len(b) != len(sum) {
			return sum, false
		}
		copy(sum[:], b)
		return sum, true
	case p.Token != "":
		return sha256.Sum256([]byte(p.Token)), true
	default:
		return sum, false
	}
}

// expiresAt parses the optional Expires timestamp; a zero time means "never".
func (p PrincipalConfig) expiresAt() (time.Time, error) {
	if p.Expires == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, p.Expires)
}

// PrincipalsEnabled reports whether hard isolation (Layer 2) is configured.
func (c *Config) PrincipalsEnabled() bool { return len(c.Principals) > 0 }

func (p PrincipalConfig) corePrincipal() *core.Principal {
	return &core.Principal{Name: p.Name, Read: p.ReadNamespaces(), Write: p.Write, MaxNodes: p.MaxNodes, MaxChunks: p.MaxChunks}
}

// PrincipalAuthenticator resolves a presented bearer token to its principal,
// honoring hash-at-rest, expiry and revocation (#269). It is immutable after
// BuildAuthenticator and safe for concurrent use; hot reload swaps the whole
// value (see the HTTP server's SIGHUP handler).
type PrincipalAuthenticator struct {
	entries []principalEntry
}

type principalEntry struct {
	hash    [32]byte // SHA-256 the presented token is compared against
	p       *core.Principal
	expires time.Time // zero = never
	revoked bool
}

// BuildAuthenticator compiles the roster into a matcher for per-request auth, or
// nil when hard isolation is off. It assumes the config already passed Validate,
// so token hashes resolve and timestamps parse; anything malformed is skipped
// defensively (it can then never authenticate).
func (c *Config) BuildAuthenticator() *PrincipalAuthenticator {
	if !c.PrincipalsEnabled() {
		return nil
	}
	a := &PrincipalAuthenticator{entries: make([]principalEntry, 0, len(c.Principals))}
	for _, p := range c.Principals {
		sum, ok := p.resolvedTokenHash()
		if !ok {
			continue
		}
		exp, err := p.expiresAt()
		if err != nil {
			continue
		}
		a.entries = append(a.entries, principalEntry{hash: sum, p: p.corePrincipal(), expires: exp, revoked: p.Revoked})
	}
	return a
}

// Match returns the principal for a presented bearer token at time now, or nil
// if no token matches or the match is revoked/expired. Comparison is
// constant-time over the whole roster so timing never reveals which token, if
// any, was close.
func (a *PrincipalAuthenticator) Match(token string, now time.Time) *core.Principal {
	if a == nil || token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	var match *core.Principal
	for i := range a.entries {
		e := &a.entries[i]
		hit := subtle.ConstantTimeCompare(e.hash[:], sum[:]) == 1
		if hit && !e.revoked && (e.expires.IsZero() || now.Before(e.expires)) {
			match = e.p
		}
	}
	return match
}

// Len reports how many principals the matcher holds (0 after a reload emptied the
// roster).
func (a *PrincipalAuthenticator) Len() int {
	if a == nil {
		return 0
	}
	return len(a.entries)
}

// PrincipalByName returns the configured principal with the given name as a core
// principal, or nil.
func (c *Config) PrincipalByName(name string) *core.Principal {
	for _, p := range c.Principals {
		if p.Name == name {
			return &core.Principal{Name: p.Name, Read: p.ReadNamespaces(), Write: p.Write, MaxNodes: p.MaxNodes, MaxChunks: p.MaxChunks}
		}
	}
	return nil
}

// PrincipalByToken returns the configured principal whose bearer token matches, or
// nil. Used by MCP to bind its process-wide principal from a secret token — so
// knowing a principal's NAME is not enough to assume its identity (#266). Honors
// hash-at-rest, expiry and revocation (#269), evaluated once at process start.
func (c *Config) PrincipalByToken(token string) *core.Principal {
	return c.BuildAuthenticator().Match(token, time.Now())
}

// ReadNamespaces returns the principal's read set with the "global" alias
// normalized to "" (the empty-discriminator scope the core matches on).
func (p PrincipalConfig) ReadNamespaces() []string {
	out := make([]string, 0, len(p.Read))
	for _, ns := range p.Read {
		if ns == "global" {
			ns = ""
		}
		out = append(out, ns)
	}
	return out
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

// RetentionConfig controls the historical-row retention sweep (#363). MaxAge is a
// Go duration ("8760h" = 1y); empty/zero disables it (keep everything). When set,
// `kb sweep-retention` hard-deletes historical (superseded) nodes/edges older than
// it — current rows are never affected.
type RetentionConfig struct {
	MaxAge string `yaml:"max_age,omitempty"`
}

// MaxAgeDuration parses Retention.MaxAge; 0 (disabled) on empty or unparseable.
func (c *Config) MaxAgeDuration() time.Duration {
	if c.Retention.MaxAge == "" {
		return 0
	}
	d, err := time.ParseDuration(c.Retention.MaxAge)
	if err != nil {
		return 0
	}
	return d
}

// TieringConfig controls the automated hot→cold demotion policy (#231). MaxHotAge
// is a Go duration ("4320h" = 180d); empty/zero disables demotion (tiering stays
// promote-only). When set, `kb sweep-tiers` archives hot chunks older than it to
// keep the hot vector index within RAM.
type TieringConfig struct {
	MaxHotAge string `yaml:"max_hot_age,omitempty"`
}

// MaxHotAgeDuration parses MaxHotAge; 0 (disabled) on empty or unparseable.
func (c *Config) MaxHotAgeDuration() time.Duration {
	if c.Tiering.MaxHotAge == "" {
		return 0
	}
	d, err := time.ParseDuration(c.Tiering.MaxHotAge)
	if err != nil {
		return 0
	}
	return d
}

// IndexConfig tunes the HNSW vector-index build parameters (#233) applied by the
// `reindex` command. Defaults match pgvector's (m=16, ef_construction=64); raise
// them ahead of a large (10M+) tier for better recall at the cost of build time
// and index size.
type IndexConfig struct {
	HNSWM              int `yaml:"hnsw_m,omitempty"`
	HNSWEfConstruction int `yaml:"hnsw_ef_construction,omitempty"`
}

// RetrievalConfig tunes the cosine-distance gates for retrieval (#332) so a
// deployment can calibrate for its embedding model/domain without a rebuild. Zero
// means "use the built-in default"; cosine distance runs 0..2, gaps are >= 0.
type RetrievalConfig struct {
	MaxChunkDistance float64 `yaml:"max_chunk_distance,omitempty"` // Search absolute cutoff
	ChunkDistanceGap float64 `yaml:"chunk_distance_gap,omitempty"` // Search relative gap
	MaxNodeDistance  float64 `yaml:"max_node_distance,omitempty"`  // Recall absolute cutoff
	NodeDistanceGap  float64 `yaml:"node_distance_gap,omitempty"`  // Recall relative gap
}

// LoggingConfig controls the application logger (#258). The access log is always
// JSON; these tune the app logger's rendering and verbosity.
type LoggingConfig struct {
	// Format is "json" (default — Docker's json-file driver rotates it, so the
	// durable log survives crashes) or "text" for human-readable local runs.
	Format string `yaml:"format,omitempty"`
	// Level is debug|info|warn|error (default info). Bridged stdlib log.Printf
	// lines emit at info, so a level above info also drops those.
	Level string `yaml:"level,omitempty"`
}

// HTTPConfig configures the REST/WebUI server.
type HTTPConfig struct {
	Addr string `yaml:"addr"`
	// AuthToken, if set, is the bearer token required for write endpoints.
	// Prefer setting it via AUTH_TOKEN in the environment.
	AuthToken string `yaml:"auth_token,omitempty"`
	// RateLimitRPS caps sustained /api requests per second per client (#270);
	// 0 disables. A "client" is the principal (Layer 2), else the bearer token,
	// else the source IP. Each /api/search triggers an Ollama embed, so this is the
	// first line of defense against one caller exhausting shared Ollama/DB.
	RateLimitRPS float64 `yaml:"rate_limit_rps,omitempty"`
	// RateLimitBurst is the token-bucket depth — the largest instantaneous burst
	// allowed above the sustained rate. Defaults to ceil(RateLimitRPS) (min 1) when
	// rate limiting is on and this is unset.
	RateLimitBurst int `yaml:"rate_limit_burst,omitempty"`
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
	// MaxConcurrency caps in-flight embed round-trips to Ollama (#270); 0 =
	// unlimited. A bulk ingest and many concurrent /api/search calls otherwise pile
	// onto one Ollama box; this bounds the load independent of request rate.
	MaxConcurrency int `yaml:"max_concurrency,omitempty"`
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
	// APIKey authenticates the Claude Messages API extractor (default "claude",
	// #235). Secret — prefer ANTHROPIC_API_KEY in the environment.
	APIKey string `yaml:"api_key,omitempty"`
}

// LocalExtractionEnabled reports whether the optional local-LLM extractor is
// turned on.
func (c *Config) LocalExtractionEnabled() bool {
	return c.Extraction.Default == "local-llm"
}

// ClaudeExtractionEnabled reports whether the server-side Claude extractor is the
// configured extraction path (#235).
func (c *Config) ClaudeExtractionEnabled() bool {
	return c.Extraction.Default == "claude"
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
	Type      string   `yaml:"type"`
	Selection string   `yaml:"selection"`
	Token     string   `yaml:"token,omitempty"`
	Path      string   `yaml:"path,omitempty"`  // for file-based connectors (markdown)
	Repos     []string `yaml:"repos,omitempty"` // "owner/repo" list for the github connector (#238)
	// BaseURL and Email configure the Atlassian connectors (jira, confluence, #343):
	// the site base URL and the account email for Basic email:token auth.
	BaseURL string `yaml:"base_url,omitempty"`
	Email   string `yaml:"email,omitempty"`
	// Trust marks this source's ingested content trusted (#361): "trusted" or
	// "untrusted". Empty defaults to untrusted (fail-closed, #273) — set it to
	// "trusted" only for a source you vouch for, which then skips the forced
	// extraction review and isn't flagged untrusted in recall.
	Trust string `yaml:"trust,omitempty"`
}

// SourceTrust returns the configured trust level for a source type ("" when no
// such source or it's unset, which the core treats as untrusted).
func (c *Config) SourceTrust(typ string) string {
	if sc := c.Source(typ); sc != nil {
		return sc.Trust
	}
	return ""
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
	c.Logging.Format = "json"
	c.Logging.Level = "info"
	c.Index.HNSWM = 16              // pgvector default
	c.Index.HNSWEfConstruction = 64 // pgvector default
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
	if v := os.Getenv("HTTP_RATE_LIMIT_RPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.HTTP.RateLimitRPS = f
		}
	}
	if v := os.Getenv("HTTP_RATE_LIMIT_BURST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.HTTP.RateLimitBurst = n
		}
	}
	if v := os.Getenv("EMBED_MAX_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Embedding.MaxConcurrency = n
		}
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
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		c.Logging.Format = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		c.Logging.Level = v
	}
	setFloatEnv("RETRIEVAL_MAX_CHUNK_DISTANCE", &c.Retrieval.MaxChunkDistance)
	setFloatEnv("RETRIEVAL_CHUNK_DISTANCE_GAP", &c.Retrieval.ChunkDistanceGap)
	setFloatEnv("RETRIEVAL_MAX_NODE_DISTANCE", &c.Retrieval.MaxNodeDistance)
	setFloatEnv("RETRIEVAL_NODE_DISTANCE_GAP", &c.Retrieval.NodeDistanceGap)
	setIntEnv("HNSW_M", &c.Index.HNSWM)
	setIntEnv("HNSW_EF_CONSTRUCTION", &c.Index.HNSWEfConstruction)
	if v := os.Getenv("TIERING_MAX_HOT_AGE"); v != "" {
		c.Tiering.MaxHotAge = v
	}
	if v := os.Getenv("RETENTION_MAX_AGE"); v != "" {
		c.Retention.MaxAge = v
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
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		c.Extraction.APIKey = v
	}
	// Per-principal bearer tokens (#120), secret — env wins over file. Keyed
	// PRINCIPAL_TOKEN_<NAME> with NAME uppercased and non-alphanumerics → '_'.
	for i := range c.Principals {
		if v := os.Getenv("PRINCIPAL_TOKEN_" + envKey(c.Principals[i].Name)); v != "" {
			c.Principals[i].Token = v
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
	if v := os.Getenv("SLACK_TOKEN"); v != "" {
		found := false
		for i := range c.Sources {
			if c.Sources[i].Type == "slack" {
				c.Sources[i].Token = v
				found = true
			}
		}
		if !found {
			// Auto-create a slack source so the bot token alone is enough to import (#237).
			c.Sources = append(c.Sources, SourceConfig{Type: "slack", Selection: "density-filter", Token: v})
		}
	}
	if v := os.Getenv("GITHUB_TOKEN"); v != "" {
		found := false
		for i := range c.Sources {
			if c.Sources[i].Type == "github" {
				c.Sources[i].Token = v
				found = true
			}
		}
		if !found {
			// Auto-create a github source; repos come from config or the import target (#238).
			c.Sources = append(c.Sources, SourceConfig{Type: "github", Selection: "density-filter", Token: v})
		}
	}
	if v := os.Getenv("GITHUB_REPOS"); v != "" {
		for i := range c.Sources {
			if c.Sources[i].Type == "github" {
				c.Sources[i].Repos = splitCSV(v)
			}
		}
	}
	if v := os.Getenv("GITLAB_TOKEN"); v != "" {
		found := false
		for i := range c.Sources {
			if c.Sources[i].Type == "gitlab" {
				c.Sources[i].Token = v
				found = true
			}
		}
		if !found {
			// Auto-create a gitlab source; projects/host come from config or env (#340).
			c.Sources = append(c.Sources, SourceConfig{Type: "gitlab", Selection: "density-filter", Token: v})
		}
	}
	if v := os.Getenv("GITLAB_PROJECTS"); v != "" {
		for i := range c.Sources {
			if c.Sources[i].Type == "gitlab" {
				c.Sources[i].Repos = splitCSV(v)
			}
		}
	}
	if v := os.Getenv("GITLAB_BASE_URL"); v != "" {
		for i := range c.Sources {
			if c.Sources[i].Type == "gitlab" {
				c.Sources[i].BaseURL = v
			}
		}
	}
	if v := os.Getenv("GDRIVE_TOKEN"); v != "" {
		found := false
		for i := range c.Sources {
			if c.Sources[i].Type == "gdrive" {
				c.Sources[i].Token = v
				found = true
			}
		}
		if !found {
			// Auto-create a gdrive source from the OAuth access token (#239).
			c.Sources = append(c.Sources, SourceConfig{Type: "gdrive", Selection: "density-filter", Token: v})
		}
	}
	if v := os.Getenv("LINEAR_TOKEN"); v != "" {
		found := false
		for i := range c.Sources {
			if c.Sources[i].Type == "linear" {
				c.Sources[i].Token = v
				found = true
			}
		}
		if !found {
			// Auto-create a linear source from the API key (#240).
			c.Sources = append(c.Sources, SourceConfig{Type: "linear", Selection: "density-filter", Token: v})
		}
	}
	// Atlassian connectors (#343) need a site base URL + email + token together.
	c.atlassianFromEnv("jira", "JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_TOKEN")
	c.atlassianFromEnv("confluence", "CONFLUENCE_BASE_URL", "CONFLUENCE_EMAIL", "CONFLUENCE_TOKEN")

	// Per-source trust (#361): <TYPE>_TRUST marks a source trusted/untrusted, e.g.
	// GITHUB_TRUST=trusted. Applied after sources are resolved/auto-created above.
	for i := range c.Sources {
		if v := os.Getenv(strings.ToUpper(c.Sources[i].Type) + "_TRUST"); v != "" {
			c.Sources[i].Trust = v
		}
	}
}

// atlassianFromEnv fills or auto-creates a Basic-auth Atlassian source (jira /
// confluence) when its base-url/email/token env trio is present. Any subset
// updates an existing source; a full trio with no existing source creates one.
func (c *Config) atlassianFromEnv(typ, baseEnv, emailEnv, tokenEnv string) {
	base, email, token := os.Getenv(baseEnv), os.Getenv(emailEnv), os.Getenv(tokenEnv)
	if base == "" && email == "" && token == "" {
		return
	}
	for i := range c.Sources {
		if c.Sources[i].Type == typ {
			if base != "" {
				c.Sources[i].BaseURL = base
			}
			if email != "" {
				c.Sources[i].Email = email
			}
			if token != "" {
				c.Sources[i].Token = token
			}
			return
		}
	}
	if base != "" && email != "" && token != "" {
		c.Sources = append(c.Sources, SourceConfig{
			Type: typ, Selection: "density-filter", BaseURL: base, Email: email, Token: token,
		})
	}
}

// setFloatEnv overwrites *dst with the float value of env var key when it is set
// and parses cleanly; a malformed value is ignored (leaving the default).
func setFloatEnv(key string, dst *float64) {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			*dst = f
		}
	}
}

// setIntEnv is setFloatEnv for ints.
func setIntEnv(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// splitCSV splits a comma-separated env value into trimmed, non-empty items.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
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
	if c.ClaudeExtractionEnabled() && c.Extraction.APIKey == "" {
		return errors.New("extraction.api_key must be set when extraction.default is 'claude' (set it or ANTHROPIC_API_KEY)")
	}
	if c.HTTP.RateLimitRPS < 0 {
		return fmt.Errorf("http.rate_limit_rps must be >= 0, got %g", c.HTTP.RateLimitRPS)
	}
	if c.HTTP.RateLimitBurst < 0 {
		return fmt.Errorf("http.rate_limit_burst must be >= 0, got %d", c.HTTP.RateLimitBurst)
	}
	if c.Embedding.MaxConcurrency < 0 {
		return fmt.Errorf("embedding.max_concurrency must be >= 0, got %d", c.Embedding.MaxConcurrency)
	}
	if f := strings.ToLower(strings.TrimSpace(c.Logging.Format)); f != "" && f != "json" && f != "text" {
		return fmt.Errorf("logging.format must be 'json' or 'text', got %q", c.Logging.Format)
	}
	// Retrieval thresholds (#332): cosine distance runs 0..2; a gap is >= 0. Zero
	// means "use the built-in default", so only reject out-of-range non-zero values.
	for _, d := range []struct {
		name string
		val  float64
	}{
		{"retrieval.max_chunk_distance", c.Retrieval.MaxChunkDistance},
		{"retrieval.max_node_distance", c.Retrieval.MaxNodeDistance},
	} {
		if d.val < 0 || d.val > 2 {
			return fmt.Errorf("%s must be within [0, 2] (cosine distance), got %g", d.name, d.val)
		}
	}
	if c.Retrieval.ChunkDistanceGap < 0 {
		return fmt.Errorf("retrieval.chunk_distance_gap must be >= 0, got %g", c.Retrieval.ChunkDistanceGap)
	}
	if c.Retrieval.NodeDistanceGap < 0 {
		return fmt.Errorf("retrieval.node_distance_gap must be >= 0, got %g", c.Retrieval.NodeDistanceGap)
	}
	// HNSW build params (#233) — pgvector's valid ranges.
	if c.Index.HNSWM < 2 || c.Index.HNSWM > 100 {
		return fmt.Errorf("index.hnsw_m must be within [2, 100], got %d", c.Index.HNSWM)
	}
	if c.Index.HNSWEfConstruction < 4 || c.Index.HNSWEfConstruction > 1000 {
		return fmt.Errorf("index.hnsw_ef_construction must be within [4, 1000], got %d", c.Index.HNSWEfConstruction)
	}
	if c.Index.HNSWEfConstruction < 2*c.Index.HNSWM {
		return fmt.Errorf("index.hnsw_ef_construction (%d) must be >= 2*hnsw_m (%d)", c.Index.HNSWEfConstruction, 2*c.Index.HNSWM)
	}
	// Per-source trust (#361): if set, must be trusted or untrusted.
	for _, s := range c.Sources {
		if t := strings.ToLower(strings.TrimSpace(s.Trust)); t != "" && t != "trusted" && t != "untrusted" {
			return fmt.Errorf("sources[%q].trust must be 'trusted' or 'untrusted', got %q", s.Type, s.Trust)
		}
	}
	// Tiering (#231): if set, max_hot_age must be a positive Go duration.
	if c.Tiering.MaxHotAge != "" {
		d, err := time.ParseDuration(c.Tiering.MaxHotAge)
		if err != nil || d <= 0 {
			return fmt.Errorf("tiering.max_hot_age must be a positive duration (e.g. \"4320h\"), got %q", c.Tiering.MaxHotAge)
		}
	}
	// Retention (#363): if set, max_age must be a positive Go duration.
	if c.Retention.MaxAge != "" {
		d, err := time.ParseDuration(c.Retention.MaxAge)
		if err != nil || d <= 0 {
			return fmt.Errorf("retention.max_age must be a positive duration (e.g. \"8760h\"), got %q", c.Retention.MaxAge)
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.Logging.Level)) {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("logging.level must be one of debug|info|warn|error, got %q", c.Logging.Level)
	}
	if err := c.validatePrincipals(); err != nil {
		return err
	}
	return nil
}

// RateLimitEnabled reports whether per-client /api rate limiting is configured.
func (c *Config) RateLimitEnabled() bool { return c.HTTP.RateLimitRPS > 0 }

// EffectiveRateLimitBurst returns the token-bucket depth to use: the configured
// burst, or a sensible default of ceil(rps) (at least 1) when unset. Meaningful
// only when RateLimitEnabled.
func (c *Config) EffectiveRateLimitBurst() int {
	if c.HTTP.RateLimitBurst > 0 {
		return c.HTTP.RateLimitBurst
	}
	b := int(math.Ceil(c.HTTP.RateLimitRPS))
	if b < 1 {
		b = 1
	}
	return b
}

// MinTokenLen is the minimum length for a plaintext principal bearer token — a
// weak token is easy to guess and slow to fully retire, so require entropy up
// front. 32 hex chars = 128 bits (#269). Does not apply to token_sha256, whose
// pre-image entropy the operator owns.
const MinTokenLen = 32

// validatePrincipals enforces the Layer 2 roster invariants: every principal
// needs a unique name, a single write target, and a resolved bearer token
// (unique across principals), so a misconfigured token can never silently widen
// or collide access (#120).
func (c *Config) validatePrincipals() error {
	names := map[string]bool{}
	hashes := map[string]bool{}
	for _, p := range c.Principals {
		if p.Name == "" {
			return errors.New("each principal needs a name")
		}
		if names[p.Name] {
			return fmt.Errorf("duplicate principal name %q", p.Name)
		}
		names[p.Name] = true
		if p.Write == "" {
			return fmt.Errorf("principal %q needs a write namespace", p.Name)
		}
		if p.Token == "" && p.TokenSHA256 == "" {
			return fmt.Errorf("principal %q has no token (set PRINCIPAL_TOKEN_%s, or token_sha256 for hash-at-rest)", p.Name, envKey(p.Name))
		}
		if p.Token != "" && p.TokenSHA256 != "" {
			return fmt.Errorf("principal %q sets both token and token_sha256 — use exactly one", p.Name)
		}
		if p.Token != "" && len(p.Token) < MinTokenLen {
			return fmt.Errorf("principal %q token is too short (%d chars, need >= %d) — generate one with `brainiac token gen`", p.Name, len(p.Token), MinTokenLen)
		}
		sum, ok := p.resolvedTokenHash()
		if !ok {
			return fmt.Errorf("principal %q token_sha256 must be 64 hex chars (SHA-256) — produce one with `brainiac token hash`", p.Name)
		}
		if _, err := p.expiresAt(); err != nil {
			return fmt.Errorf("principal %q has invalid expires %q (want RFC3339, e.g. 2026-12-31T00:00:00Z): %w", p.Name, p.Expires, err)
		}
		h := hex.EncodeToString(sum[:])
		if hashes[h] {
			return fmt.Errorf("principal %q reuses another principal's token", p.Name)
		}
		hashes[h] = true
	}
	return nil
}

// envKey maps a principal name to its env-var suffix: uppercased, every
// non-alphanumeric run collapsed to a single '_'.
func envKey(name string) string {
	var b []byte
	prevUnderscore := false
	for i := 0; i < len(name); i++ {
		ch := name[i]
		switch {
		case ch >= 'a' && ch <= 'z':
			b = append(b, ch-32)
			prevUnderscore = false
		case (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9'):
			b = append(b, ch)
			prevUnderscore = false
		case !prevUnderscore:
			b = append(b, '_')
			prevUnderscore = true
		}
	}
	return string(b)
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
