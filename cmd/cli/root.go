package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/doctext"
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
		reindexCmd(),
		compactCmd(),
		reencryptCmd(),
		sweepTiersCmd(),
		sweepRetentionCmd(),
		eraseCmd(),
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
		auditCmd(),
		tokenCmd(),
		rollupCmd(),
		exportCmd(),
		namespaceCmd(),
		mergeCmd(),
		splitCmd(),
		retireEdgeCmd(),
		confirmCmd(),
		flagStaleCmd(),
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
	// Optional app-level encryption (#377/#403); no-op when unset. Retired keys stay
	// readable for rotation until `kb reencrypt` migrates them.
	encKey, err := cfg.ChunkEncryptionKey()
	if err != nil {
		return nil, nil, err
	}
	retiredKeys, err := cfg.RetiredEncryptionKeys()
	if err != nil {
		return nil, nil, err
	}
	if err := store.SetChunkCiphers(encKey, retiredKeys...); err != nil {
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
	embedder := ollama.New(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.Dims, ollama.WithBatchSize(cfg.Embedding.BatchSize), ollama.WithMaxConcurrency(cfg.Embedding.MaxConcurrency))
	var opts []core.Option
	if cfg.LocalExtractionEnabled() {
		ext := ollama.NewExtractor(cfg.ExtractorBaseURL(), cfg.Extraction.Model, ollama.WithExtractorRetries(cfg.Extraction.Retries))
		opts = append(opts, core.WithExtractor(ext, cfg.Extraction.Review))
	}
	opts = append(opts, retrievalOption(cfg))
	return core.New(pool, embedder, density.New(), opts...)
}

// ocrFunc builds the opt-in OCR fallback for scanned PDFs (#356) from config, or
// nil when disabled. It shells out to the configured command with the PDF written
// to a temp file — exec with explicit args (never a shell string), so a
// filename/command can't inject. The command's stdout is the recognized text
// (tesseract's `<cmd> <file> stdout` CLI).
func ocrFunc(cfg *config.Config) doctext.OCRFunc {
	if !cfg.OCR.Enabled || cfg.OCR.Command == "" {
		return nil
	}
	command := cfg.OCR.Command
	return func(pdf []byte) (string, error) {
		f, err := os.CreateTemp("", "brainiac-ocr-*.pdf")
		if err != nil {
			return "", err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err := f.Write(pdf); err != nil {
			_ = f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		out, err := exec.Command(command, f.Name(), "stdout").Output() //nolint:gosec // operator-configured command, explicit args
		if err != nil {
			return "", fmt.Errorf("ocr %q: %w", command, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
}

// retrievalOption maps the config retrieval thresholds (#332) onto the core
// option; zero fields fall back to core's built-in defaults.
func retrievalOption(cfg *config.Config) core.Option {
	return core.WithRetrievalThresholds(core.RetrievalThresholds{
		MaxChunkDistance: cfg.Retrieval.MaxChunkDistance,
		ChunkDistanceGap: cfg.Retrieval.ChunkDistanceGap,
		MaxNodeDistance:  cfg.Retrieval.MaxNodeDistance,
		NodeDistanceGap:  cfg.Retrieval.NodeDistanceGap,
	})
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
