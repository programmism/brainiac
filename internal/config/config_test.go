package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChunkEncryptionKey(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env-dsn")
	none := func() string { return filepath.Join(t.TempDir(), "none.yaml") }

	// Default: OFF (nil key, no error) — plaintext, as before.
	c, err := Load(none())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if k, err := c.ChunkEncryptionKey(); err != nil || k != nil {
		t.Fatalf("default key = (%v, %v), want (nil, nil)", k, err)
	}

	// Valid 32-byte base64 key decodes.
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	c, _ = Load(none())
	if k, err := c.ChunkEncryptionKey(); err != nil || len(k) != 32 {
		t.Fatalf("valid key = (len %d, %v), want (32, nil)", len(k), err)
	}

	// Wrong length is rejected (fail fast).
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	c, _ = Load(none())
	if _, err := c.ChunkEncryptionKey(); err == nil {
		t.Fatal("16-byte key should be rejected")
	}

	// Non-base64 is rejected.
	t.Setenv("ENCRYPTION_KEY", "not valid base64 !!!")
	c, _ = Load(none())
	if _, err := c.ChunkEncryptionKey(); err == nil {
		t.Fatal("non-base64 key should be rejected")
	}
}

func TestDefaultIsSane(t *testing.T) {
	c := Default()
	if c.HTTP.Addr != ":8080" {
		t.Errorf("addr = %q", c.HTTP.Addr)
	}
	if c.Embedding.Dims != 768 {
		t.Errorf("dims = %d", c.Embedding.Dims)
	}
	if c.Extraction.Default != "chat-driven" {
		t.Errorf("extraction default = %q", c.Extraction.Default)
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err == nil {
		t.Fatal("expected error: empty DSN")
	}
	c.Storage.DSN = "postgres://x"
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c.Embedding.Dims = 0
	if err := c.Validate(); err == nil {
		t.Fatal("expected error: non-positive dims")
	}
}

func TestPrincipalValidationAndMapping(t *testing.T) {
	base := Default()
	base.Storage.DSN = "postgres://x"

	// A principal missing its token fails validation.
	c := base
	c.Principals = []PrincipalConfig{{Name: "team-a", Read: []string{"team-a"}, Write: "team-a"}}
	if err := c.Validate(); err == nil {
		t.Fatal("principal with no token should fail validation")
	}

	// A well-formed roster validates and the authenticator resolves each token to
	// its principal, with the "global" read alias normalized to "".
	c.Principals[0].Token = "team-a-token-000000000000000000000000"
	c.Principals = append(c.Principals, PrincipalConfig{Name: "platform", Read: []string{"team-a", "global"}, Write: "platform", Token: "platform-token-00000000000000000000"})
	if err := c.Validate(); err != nil {
		t.Fatalf("valid roster rejected: %v", err)
	}
	a := c.BuildAuthenticator()
	if a.Len() != 2 {
		t.Fatalf("authenticator len = %d, want 2", a.Len())
	}
	if p := a.Match("team-a-token-000000000000000000000000", time.Now()); p == nil || p.Name != "team-a" {
		t.Fatalf("team-a token did not resolve: %+v", p)
	}
	pl := a.Match("platform-token-00000000000000000000", time.Now())
	if pl == nil {
		t.Fatal("platform token did not resolve")
	}
	if got := pl.Read; len(got) != 2 || got[0] != "team-a" || got[1] != "" {
		t.Fatalf("global alias not normalized to \"\": %+v", got)
	}

	// A reused token is rejected (would conflate two identities).
	c.Principals[1].Token = "team-a-token-000000000000000000000000"
	if err := c.Validate(); err == nil {
		t.Fatal("duplicate token should fail validation")
	}
}

func TestPrincipalByToken(t *testing.T) {
	c := Default()
	c.Principals = []PrincipalConfig{
		{Name: "team-a", Read: []string{"team-a"}, Write: "team-a", Token: "tok-a"},
		{Name: "team-b", Read: []string{"team-b"}, Write: "team-b", Token: "tok-b"},
	}
	if p := c.PrincipalByToken("tok-b"); p == nil || p.Name != "team-b" || p.Write != "team-b" {
		t.Fatalf("token should select team-b, got %+v", p)
	}
	if p := c.PrincipalByToken("nope"); p != nil {
		t.Fatalf("unknown token must not match, got %+v", p)
	}
	if p := c.PrincipalByToken(""); p != nil {
		t.Fatalf("empty token must not match, got %+v", p)
	}
}

func TestPrincipalTokenEntropyFloor(t *testing.T) {
	c := Default()
	c.Storage.DSN = "postgres://x"
	c.Principals = []PrincipalConfig{{Name: "a", Read: []string{"a"}, Write: "a", Token: "short"}}
	if err := c.Validate(); err == nil {
		t.Fatal("a too-short principal token must fail validation")
	}
}

// sha256Hex mirrors what `brainiac token hash` prints for a token.
func sha256Hex(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func TestPrincipalHashAtRest(t *testing.T) {
	const secret = "team-a-token-000000000000000000000000"
	c := Default()
	c.Storage.DSN = "postgres://x"
	c.Principals = []PrincipalConfig{{Name: "a", Read: []string{"a"}, Write: "a", TokenSHA256: sha256Hex(secret)}}
	if err := c.Validate(); err != nil {
		t.Fatalf("hash-at-rest roster rejected: %v", err)
	}
	// The presented plaintext token resolves against the stored hash; the hash
	// itself must not authenticate.
	if p := c.PrincipalByToken(secret); p == nil || p.Name != "a" {
		t.Fatalf("plaintext token did not resolve against token_sha256: %+v", p)
	}
	if p := c.PrincipalByToken(c.Principals[0].TokenSHA256); p != nil {
		t.Fatal("the hash must not authenticate as if it were the token")
	}

	// Setting both token and token_sha256 is a misconfiguration.
	c.Principals[0].Token = secret
	if err := c.Validate(); err == nil {
		t.Fatal("token + token_sha256 together must fail validation")
	}

	// A malformed hash (not 64 hex chars) is rejected.
	c.Principals[0] = PrincipalConfig{Name: "a", Read: []string{"a"}, Write: "a", TokenSHA256: "nothex"}
	if err := c.Validate(); err == nil {
		t.Fatal("malformed token_sha256 must fail validation")
	}

	// A plaintext token and another principal's hash of the same secret collide.
	c.Principals = []PrincipalConfig{
		{Name: "a", Read: []string{"a"}, Write: "a", Token: secret},
		{Name: "b", Read: []string{"b"}, Write: "b", TokenSHA256: sha256Hex(secret)},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("a plaintext token colliding with another's hash must fail validation")
	}
}

func TestPrincipalExpiryAndRevocation(t *testing.T) {
	const secret = "team-a-token-000000000000000000000000"
	c := Default()
	c.Storage.DSN = "postgres://x"

	// An invalid timestamp is rejected up front.
	c.Principals = []PrincipalConfig{{Name: "a", Read: []string{"a"}, Write: "a", Token: secret, Expires: "not-a-time"}}
	if err := c.Validate(); err == nil {
		t.Fatal("invalid expires must fail validation")
	}

	// A valid future expiry authenticates now but not after it lapses.
	c.Principals[0].Expires = "2030-01-01T00:00:00Z"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid expires rejected: %v", err)
	}
	a := c.BuildAuthenticator()
	before, _ := time.Parse(time.RFC3339, "2029-01-01T00:00:00Z")
	after, _ := time.Parse(time.RFC3339, "2031-01-01T00:00:00Z")
	if p := a.Match(secret, before); p == nil {
		t.Fatal("token should authenticate before it expires")
	}
	if p := a.Match(secret, after); p != nil {
		t.Fatal("token must not authenticate after it expires")
	}

	// A revoked principal never authenticates, even with the right token.
	c.Principals[0].Revoked = true
	if err := c.Validate(); err != nil {
		t.Fatalf("revoked principal should still validate: %v", err)
	}
	if p := c.BuildAuthenticator().Match(secret, before); p != nil {
		t.Fatal("a revoked token must not authenticate")
	}
}

func TestRateLimitAndConcurrencyConfig(t *testing.T) {
	c := Default()
	c.Storage.DSN = "postgres://x"

	// Off by default.
	if c.RateLimitEnabled() {
		t.Fatal("rate limiting should be off by default")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}

	// Burst defaults to ceil(rps), min 1, when unset.
	c.HTTP.RateLimitRPS = 2.5
	if !c.RateLimitEnabled() {
		t.Fatal("rate limiting should be on when rps > 0")
	}
	if got := c.EffectiveRateLimitBurst(); got != 3 {
		t.Fatalf("effective burst = %d, want 3 (ceil 2.5)", got)
	}
	c.HTTP.RateLimitBurst = 10
	if got := c.EffectiveRateLimitBurst(); got != 10 {
		t.Fatalf("explicit burst = %d, want 10", got)
	}

	// Negatives are rejected.
	for _, bad := range []func(*Config){
		func(c *Config) { c.HTTP.RateLimitRPS = -1 },
		func(c *Config) { c.HTTP.RateLimitBurst = -1 },
		func(c *Config) { c.Embedding.MaxConcurrency = -1 },
	} {
		cc := Default()
		cc.Storage.DSN = "postgres://x"
		bad(cc)
		if err := cc.Validate(); err == nil {
			t.Fatal("negative rate-limit/concurrency value must fail validation")
		}
	}
}

func TestClaudeExtractionRequiresKey(t *testing.T) {
	c := Default()
	c.Storage.DSN = "postgres://x"
	c.Extraction.Default = "claude"
	if !c.ClaudeExtractionEnabled() {
		t.Fatal("default=claude should enable claude extraction")
	}
	if err := c.Validate(); err == nil {
		t.Fatal("claude extraction without an API key must fail validation")
	}
	c.Extraction.APIKey = "sk-test"
	if err := c.Validate(); err != nil {
		t.Fatalf("claude extraction with a key should validate: %v", err)
	}
}

func TestEnvKey(t *testing.T) {
	cases := map[string]string{"team-a": "TEAM_A", "Platform": "PLATFORM", "a.b c": "A_B_C"}
	for in, want := range cases {
		if got := envKey(in); got != want {
			t.Errorf("envKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateEmbeddingFields(t *testing.T) {
	c := Default()
	c.Storage.DSN = "postgres://x"
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
	c.Embedding.BaseURL = ""
	if err := c.Validate(); err == nil {
		t.Error("empty base_url should fail validation")
	}
}

func TestRedactedDSN(t *testing.T) {
	got := RedactedDSN("postgres://user:secret@db:5432/brainiac?sslmode=disable")
	if got == "" || cfgContains(got, "secret") {
		t.Fatalf("password not redacted: %q", got)
	}
	if !cfgContains(got, "user") {
		t.Fatalf("username lost: %q", got)
	}
}

func cfgContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestLoadMissingFileUsesDefaultsPlusEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env-dsn")
	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Storage.DSN != "postgres://env-dsn" {
		t.Errorf("DSN not taken from env: %q", c.Storage.DSN)
	}
	if c.HTTP.Addr != ":8080" {
		t.Errorf("addr default lost: %q", c.HTTP.Addr)
	}
}

func TestGithubDiscussionsOptIn(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env-dsn")
	t.Setenv("GITHUB_TOKEN", "ghp-abc")
	t.Setenv("GITHUB_REPOS", "octo/mem")

	// Default: OFF — issues/PRs only.
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sc := c.Source("github")
	if sc == nil {
		t.Fatal("github source not created from token")
	}
	if sc.Discussions {
		t.Fatal("Discussions must default to false (opt-in)")
	}

	// Env opts in.
	t.Setenv("GITHUB_DISCUSSIONS", "true")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.Source("github").Discussions {
		t.Fatal("GITHUB_DISCUSSIONS=true did not enable discussions")
	}
}

func TestIngestPruneDeletedDefaultsOffAndEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env-dsn")
	// Default: OFF — the retention default keeps deleted content (#107), so the
	// minimal single-user config never propagates deletions.
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Ingest.PruneDeleted {
		t.Fatal("PruneDeleted must default to false (opt-in)")
	}
	// Env opts in.
	t.Setenv("INGEST_PRUNE_DELETED", "true")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.Ingest.PruneDeleted {
		t.Fatal("INGEST_PRUNE_DELETED=true did not enable prune")
	}
}

func TestLoggingConfigDefaultsAndEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env-dsn")
	// Default: JSON at info (#258).
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Logging.Format != "json" || c.Logging.Level != "info" {
		t.Fatalf("default logging = %q/%q, want json/info", c.Logging.Format, c.Logging.Level)
	}
	// Env overrides both.
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("LOG_LEVEL", "debug")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Logging.Format != "text" || c.Logging.Level != "debug" {
		t.Fatalf("env not applied: %q/%q", c.Logging.Format, c.Logging.Level)
	}
	// Invalid values are rejected by Validate (via Load).
	t.Setenv("LOG_FORMAT", "xml")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for logging.format=xml")
	}
	t.Setenv("LOG_FORMAT", "json")
	t.Setenv("LOG_LEVEL", "loud")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for logging.level=loud")
	}
}

func TestRetentionMaxAgeEnvAndValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.MaxAgeDuration() != 0 {
		t.Fatalf("default retention.max_age should be 0 (disabled), got %v", c.MaxAgeDuration())
	}
	t.Setenv("RETENTION_MAX_AGE", "8760h")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.MaxAgeDuration() != 8760*time.Hour {
		t.Fatalf("retention.max_age env not applied: %v", c.MaxAgeDuration())
	}
	t.Setenv("RETENTION_MAX_AGE", "forever")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for unparseable retention.max_age")
	}
}

func TestTieringMaxHotAgeEnvAndValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	// Default: unset → disabled (0).
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.MaxHotAgeDuration() != 0 {
		t.Fatalf("default max_hot_age should be 0 (disabled), got %v", c.MaxHotAgeDuration())
	}
	// Env sets a parseable duration.
	t.Setenv("TIERING_MAX_HOT_AGE", "4320h")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.MaxHotAgeDuration() != 4320*time.Hour {
		t.Fatalf("max_hot_age env not applied: %v", c.MaxHotAgeDuration())
	}
	// A malformed / non-positive duration is rejected.
	t.Setenv("TIERING_MAX_HOT_AGE", "later")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for unparseable max_hot_age")
	}
}

func TestHNSWIndexParamsEnvAndValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	// Defaults match pgvector.
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Index.HNSWM != 16 || c.Index.HNSWEfConstruction != 64 {
		t.Fatalf("default HNSW params = %d/%d, want 16/64", c.Index.HNSWM, c.Index.HNSWEfConstruction)
	}
	// Env overrides.
	t.Setenv("HNSW_M", "32")
	t.Setenv("HNSW_EF_CONSTRUCTION", "128")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Index.HNSWM != 32 || c.Index.HNSWEfConstruction != 128 {
		t.Fatalf("HNSW env not applied: %d/%d", c.Index.HNSWM, c.Index.HNSWEfConstruction)
	}
	// Out-of-range m is rejected.
	t.Setenv("HNSW_M", "200")
	t.Setenv("HNSW_EF_CONSTRUCTION", "128")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for hnsw_m > 100")
	}
	// ef_construction < 2*m is rejected.
	t.Setenv("HNSW_M", "32")
	t.Setenv("HNSW_EF_CONSTRUCTION", "40")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for ef_construction < 2*m")
	}
}

func TestRetrievalThresholdsEnvAndValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	// Default: unset (zero) → core applies its built-in defaults.
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Retrieval != (RetrievalConfig{}) {
		t.Fatalf("retrieval should default to zero (use core defaults), got %+v", c.Retrieval)
	}
	// Env overrides parse into the fields.
	t.Setenv("RETRIEVAL_MAX_CHUNK_DISTANCE", "0.80")
	t.Setenv("RETRIEVAL_NODE_DISTANCE_GAP", "0.05")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Retrieval.MaxChunkDistance != 0.80 || c.Retrieval.NodeDistanceGap != 0.05 {
		t.Fatalf("retrieval env not applied: %+v", c.Retrieval)
	}
	// Out-of-range distance is rejected.
	t.Setenv("RETRIEVAL_MAX_CHUNK_DISTANCE", "2.5")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for max_chunk_distance > 2")
	}
	t.Setenv("RETRIEVAL_MAX_CHUNK_DISTANCE", "0.80")
	t.Setenv("RETRIEVAL_NODE_DISTANCE_GAP", "-0.1")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for negative node_distance_gap")
	}
}

func TestOCRConfigEnvAndValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	// Default: OFF (pluggability — no OCR without explicit opt-in).
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.OCR.Enabled {
		t.Fatal("OCR must be OFF by default")
	}
	// Enabled + command → applied.
	t.Setenv("OCR_ENABLED", "true")
	t.Setenv("OCR_COMMAND", "tesseract")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.OCR.Enabled || c.OCR.Command != "tesseract" {
		t.Fatalf("OCR env not applied: %+v", c.OCR)
	}
	// Enabled without a command is rejected.
	t.Setenv("OCR_COMMAND", "")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for ocr.enabled without ocr.command")
	}
}

func TestLocalMarkdownTrustedByDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	for _, k := range []string{"GITHUB_TOKEN", "NOTION_TOKEN", "SLACK_TOKEN", "LINEAR_TOKEN"} {
		t.Setenv(k, "")
	}
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Local files are the operator's own → trusted by default (single-user path
	// works with zero config), while remote connectors default untrusted ("").
	if c.SourceTrust("markdown") != "trusted" {
		t.Fatalf("markdown should be trusted by default, got %q", c.SourceTrust("markdown"))
	}
	if c.SourceTrust("github") != "" {
		t.Fatalf("remote connector should default untrusted (\"\"), got %q", c.SourceTrust("github"))
	}
	// An explicit override still wins.
	t.Setenv("MARKDOWN_TRUST", "untrusted")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// MARKDOWN_TRUST only applies to an existing markdown source; with none
	// configured the default still stands, so add one via yaml-less path: env
	// creates no markdown source, so the default remains — assert the default holds
	// and that an explicit config value would win via the direct field.
	if c.SourceTrust("markdown") != "trusted" {
		t.Fatalf("no markdown source configured → default trusted stands, got %q", c.SourceTrust("markdown"))
	}
	explicit := &Config{Sources: []SourceConfig{{Type: "markdown", Trust: "untrusted"}}}
	if explicit.SourceTrust("markdown") != "untrusted" {
		t.Fatalf("explicit markdown trust should win, got %q", explicit.SourceTrust("markdown"))
	}
}

func TestPerSourceTrustEnvAndValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	for _, k := range []string{"NOTION_TOKEN", "SLACK_TOKEN", "LINEAR_TOKEN"} {
		t.Setenv(k, "")
	}
	// A github source (auto-created from the token) picks up GITHUB_TRUST.
	t.Setenv("GITHUB_TOKEN", "ghp-abc")
	t.Setenv("GITHUB_REPOS", "octo/mem")
	t.Setenv("GITHUB_TRUST", "trusted")
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.SourceTrust("github") != "trusted" {
		t.Fatalf("GITHUB_TRUST not applied: %q", c.SourceTrust("github"))
	}
	// An unconfigured source type resolves to "" (core treats as untrusted).
	if c.SourceTrust("gdrive") != "" {
		t.Fatalf("unset source trust should be empty, got %q", c.SourceTrust("gdrive"))
	}
	// An invalid trust value is rejected.
	t.Setenv("GITHUB_TRUST", "maybe")
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("expected error for source trust 'maybe'")
	}
}

func TestGitLabEnvAutoCreatesSource(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	for _, k := range []string{"GITHUB_TOKEN", "NOTION_TOKEN", "SLACK_TOKEN", "LINEAR_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("GITLAB_TOKEN", "glpat-abc")
	t.Setenv("GITLAB_PROJECTS", "group/proj, group/other ,")
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")

	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	g := c.Source("gitlab")
	if g == nil || g.Token != "glpat-abc" || g.BaseURL != "https://gitlab.example.com" {
		t.Fatalf("gitlab source not created from env: %+v", g)
	}
	if len(g.Repos) != 2 || g.Repos[0] != "group/proj" || g.Repos[1] != "group/other" {
		t.Fatalf("gitlab projects not parsed (trimmed, non-empty): %v", g.Repos)
	}
}

func TestAtlassianEnvAutoCreatesSources(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	// Clear ambient tokens that would auto-create other sources.
	for _, k := range []string{"GITHUB_TOKEN", "NOTION_TOKEN", "SLACK_TOKEN", "LINEAR_TOKEN"} {
		t.Setenv(k, "")
	}
	// A full Jira trio auto-creates a jira source; a partial Confluence trio does not.
	t.Setenv("JIRA_BASE_URL", "https://site.atlassian.net")
	t.Setenv("JIRA_EMAIL", "me@x.com")
	t.Setenv("JIRA_TOKEN", "jtok")
	t.Setenv("CONFLUENCE_BASE_URL", "https://site.atlassian.net/wiki")
	t.Setenv("CONFLUENCE_EMAIL", "") // missing → no confluence source
	t.Setenv("CONFLUENCE_TOKEN", "ctok")

	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	j := c.Source("jira")
	if j == nil || j.BaseURL != "https://site.atlassian.net" || j.Email != "me@x.com" || j.Token != "jtok" {
		t.Fatalf("jira source not created from env: %+v", j)
	}
	if cf := c.Source("confluence"); cf != nil {
		t.Fatalf("confluence should not be created from a partial trio: %+v", cf)
	}
}

func TestWebUIModeEnvOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env-dsn")
	// Default (no env): read-only.
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Clients.WebUI != "read-only" {
		t.Fatalf("default webui = %q, want read-only", c.Clients.WebUI)
	}
	// WEBUI_MODE flips it — the only switch available in the config-less image.
	t.Setenv("WEBUI_MODE", "interactive")
	c, err = Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Clients.WebUI != "interactive" {
		t.Fatalf("WEBUI_MODE not applied: %q", c.Clients.WebUI)
	}
}

func TestLoadFileThenEnvWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
http:
  addr: ":9000"
storage:
  dsn: "postgres://from-file"
embedding:
  model: "custom-embed"
  dims: 768
sources:
  - type: notion
    selection: density-filter
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	// Isolate from ambient connector-token env vars (CI/Actions sets GITHUB_TOKEN),
	// which would otherwise auto-inject extra sources.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPOS", "")
	t.Setenv("NOTION_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")
	// Env override must beat the file value for the DSN.
	t.Setenv("DATABASE_URL", "postgres://from-env")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Storage.DSN != "postgres://from-env" {
		t.Errorf("env should win: %q", c.Storage.DSN)
	}
	if c.HTTP.Addr != ":9000" {
		t.Errorf("file addr lost: %q", c.HTTP.Addr)
	}
	if c.Embedding.Model != "custom-embed" || c.Embedding.Dims != 768 {
		t.Errorf("embedding not loaded: %+v", c.Embedding)
	}
	if len(c.Sources) != 1 || c.Sources[0].Type != "notion" {
		t.Errorf("sources not loaded: %+v", c.Sources)
	}
}

func TestAutoImportInterval(t *testing.T) {
	c := Default()
	if c.AutoImportInterval() != 0 {
		t.Error("empty interval should be 0 (disabled)")
	}
	c.Ingest.Interval = "45s"
	if c.AutoImportInterval() != 45*time.Second {
		t.Errorf("got %v, want 45s", c.AutoImportInterval())
	}
	c.Ingest.Interval = "garbage"
	if c.AutoImportInterval() != 0 {
		t.Error("invalid interval should be 0")
	}
}

func TestGitHubTokenAndReposAutoCreateSource(t *testing.T) {
	t.Setenv("NOTION_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "ghp-abc")
	t.Setenv("GITHUB_REPOS", "octo/mem, octo/other ,")
	t.Setenv("DATABASE_URL", "postgres://x")
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := c.Source("github")
	if s == nil || s.Token != "ghp-abc" {
		t.Fatalf("github source not created: %+v", s)
	}
	if len(s.Repos) != 2 || s.Repos[0] != "octo/mem" || s.Repos[1] != "octo/other" {
		t.Errorf("GITHUB_REPOS not parsed/trimmed: %v", s.Repos)
	}
}

func TestNotionTokenAutoCreatesSource(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("NOTION_TOKEN", "secret_abc")
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := c.Source("notion")
	if s == nil || s.Token != "secret_abc" {
		t.Fatalf("NOTION_TOKEN should auto-create a notion source with the token, got %+v", s)
	}
}
