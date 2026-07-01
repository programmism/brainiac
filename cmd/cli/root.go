package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/plugins/density"
	"github.com/programmism/brainiac/internal/plugins/ollama"
	"github.com/programmism/brainiac/internal/store"
)

// newRootCmd assembles the full command tree.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "kb",
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
		rememberCmd(),
		linkCmd(),
		supersedeCmd(),
		importCmd(),
		consolidateCmd(),
		mergeCmd(),
		evalCmd(),
		stubCmd("refresh", "#18 (connector actualization)"),
		stubCmd("reembed", "#30 (re-embed from stored raw text)"),
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
// density selector.
func buildCore(cfg *config.Config, pool *pgxpool.Pool) *core.Core {
	embedder := ollama.New(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.Dims)
	return core.New(pool, embedder, density.New())
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
