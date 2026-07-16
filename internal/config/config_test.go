package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
