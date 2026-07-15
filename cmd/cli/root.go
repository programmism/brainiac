package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/plugins/density"
	"github.com/programmism/brainiac/internal/plugins/ollama"
	"github.com/programmism/brainiac/internal/store"
)

// newRootCmd assembles the full command tree. The command name shown in help is
// taken from BRAINIAC_CLI_NAME when set (the `brainiac` wrapper passes
// "brainiac"), so usage text matches how the user actually invoked it; direct
// `/kb` in the container keeps the "kb" name.
func newRootCmd() *cobra.Command {
	name := os.Getenv("BRAINIAC_CLI_NAME")
	if name == "" {
		name = "kb"
	}
	root := &cobra.Command{
		Use:           name,
		Short:         "Brainiac operator CLI",
		Version:       core.Version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(
		migrateCmd(),
		healthCmd(),
		searchCmd(),
		recallCmd(),
		nodeCmd(),
		rememberCmd(),
		linkCmd(),
		disambiguateCmd(),
		supersedeCmd(),
		importCmd(),
		consolidateCmd(),
		rollupCmd(),
		exportCmd(),
		namespaceCmd(),
		mergeCmd(),
		splitCmd(),
		retireEdgeCmd(),
		evalCmd(),
		reembedCmd(),
		stubCmd("refresh", "#18 (connector actualization)"),
	)
	return root
}

// configPath resolves the config file: BRAINIAC_CONFIG or ./config.yaml.
func configPath() string {
	if p := os.Getenv("BRAINIAC_CONFIG"); p != "" {
		return p
	}
	return "config.yaml"
}

// connect loads config and opens the database pool.
func connect(ctx context.Context) (*config.Config, *pgxpool.Pool, error) {
	cfg, err := config.Load(configPath())
	if err != nil {
		return nil, nil, err
	}
	pool, err := store.Connect(ctx, cfg.Storage.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("connect db: %w", err)
	}
	return cfg, pool, nil
}

// buildCore wires a Core over the pool, the configured embedder, and the
// density selector, plus the optional local-LLM extractor when enabled.
func buildCore(cfg *config.Config, pool *pgxpool.Pool) *core.Core {
	embedder := ollama.New(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.Dims, ollama.WithBatchSize(cfg.Embedding.BatchSize))
	var opts []core.Option
	if cfg.LocalExtractionEnabled() {
		ext := ollama.NewExtractor(cfg.ExtractorBaseURL(), cfg.Extraction.Model, ollama.WithExtractorRetries(cfg.Extraction.Retries))
		opts = append(opts, core.WithExtractor(ext, cfg.Extraction.Review))
	}
	return core.New(pool, embedder, density.New(), opts...)
}

// parseDiscs turns repeatable --disc key=value flags into a discriminator map.
func parseDiscs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --disc %q (want key=value)", p)
		}
		m[k] = v
	}
	return m, nil
}

// parseRoutes turns repeatable --route edgeId=value flags into an edge→value map.
func parseRoutes(pairs []string) (map[string]string, error) {
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		edgeID, v, ok := strings.Cut(p, "=")
		if !ok || edgeID == "" || v == "" {
			return nil, fmt.Errorf("invalid --route %q (want edgeId=value)", p)
		}
		m[edgeID] = v
	}
	return m, nil
}

// stubCmd is a placeholder for a command whose implementation lands later.
func stubCmd(name, ref string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: "(not implemented yet)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("%q is not implemented yet — see %s", name, ref)
		},
	}
}
