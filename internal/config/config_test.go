package config

import (
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
