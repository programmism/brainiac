package connectors

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/config"
)

// TestBuildConstructsConfiguredSources checks the shared builder (#428) returns a
// connector for configured sources and a clear error otherwise. Non-OAuth sources
// don't touch the core, so a nil *core.Core is fine here (OAuth resolution for
// gdrive/gmail is exercised by the core's own tests).
func TestBuildConstructsConfiguredSources(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	cfg := &config.Config{Sources: []config.SourceConfig{
		{Type: "notion", Token: "ntn_x"},
		{Type: "github", Token: "ghp_x", Repos: []string{"o/r"}},
		{Type: "markdown", Path: dir},
	}}

	for _, tc := range []struct {
		name    string
		source  string
		path    string
		wantErr bool
	}{
		{"notion configured", "notion", "", false},
		{"github configured", "github", "", false},
		{"github via path", "github", "owner/repo", false},
		{"markdown configured", "markdown", "", false},
		{"markdown via path", "markdown", dir, false},
		{"slack missing token", "slack", "", true},
		{"markdown no dir", "markdown", "", false}, // has sources[].path
		{"unknown source", "banana", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := Build(ctx, nil, cfg, tc.source, tc.path, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Build(%q) = nil error, want error", tc.source)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build(%q) = %v, want nil", tc.source, err)
			}
			if conn == nil {
				t.Fatalf("Build(%q) = nil connector", tc.source)
			}
		})
	}
}

// TestBuildMarkdownNeedsDir verifies markdown with neither a path nor a configured
// directory is a configuration error (not a silent no-op).
func TestBuildMarkdownNeedsDir(t *testing.T) {
	cfg := &config.Config{}
	if _, err := Build(context.Background(), nil, cfg, "markdown", "", nil); err == nil {
		t.Fatal("markdown with no dir: want error, got nil")
	}
}
