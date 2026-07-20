// Command brainiac-mcp is the MCP server exposing core operations as tools to
// Claude over stdio — a thin adapter over internal/core.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/programmism/brainiac/internal/applog"
	"github.com/programmism/brainiac/internal/chunk"
	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/connectors"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/doctext"
	"github.com/programmism/brainiac/internal/mcpserver"
	"github.com/programmism/brainiac/internal/plugins/anthropic"
	"github.com/programmism/brainiac/internal/plugins/density"
	"github.com/programmism/brainiac/internal/plugins/ollama"
	"github.com/programmism/brainiac/internal/store"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("brainiac-mcp %s\n", core.Version)
		return
	}
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}

	// stdio transport owns stdout, so app logs MUST go to stderr or they corrupt
	// the protocol stream. Structured JSON to stderr (#258); no ring here.
	applog.Setup(os.Stderr, nil, cfg.Logging.Format, cfg.Logging.Level)

	// Optional app-level encryption (#377/#403); no-op when ENCRYPTION_KEY is unset.
	// Retired keys stay readable for rotation until `kb reencrypt` migrates them.
	encKey, err := cfg.ChunkEncryptionKey()
	if err != nil {
		return err
	}
	retiredKeys, err := cfg.RetiredEncryptionKeys()
	if err != nil {
		return err
	}
	if err := store.SetChunkCiphers(encKey, retiredKeys...); err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := store.ConnectWithRetry(ctx, cfg.Storage.DSN, 60*time.Second)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	embedder := ollama.New(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.Dims, ollama.WithBatchSize(cfg.Embedding.BatchSize), ollama.WithMaxConcurrency(cfg.Embedding.MaxConcurrency))
	c := core.New(pool, embedder, density.New(), append(extractorOptions(cfg), retrievalOption(cfg))...)
	principal, err := selectPrincipal(cfg)
	if err != nil {
		return err
	}
	if principal != nil {
		log.Printf("hard isolation ON: this process runs as principal %q — reads walled, writes pinned to %q (#120)", principal.Name, principal.Write)
	}
	srv := mcpserver.New(c, importFunc(c, cfg), principal)

	log.Printf("brainiac-mcp %s: serving over stdio", core.Version)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// selectPrincipal resolves which hard-isolation identity this stdio process runs
// as (#120). No principals configured → nil (Layer 1). BRAINIAC_PRINCIPAL names
// one explicitly; with exactly one configured it is used by default; with several
// configured and no name set it is an error (the process must not guess which
// namespace it speaks for).
func selectPrincipal(cfg *config.Config) (*core.Principal, error) {
	if !cfg.PrincipalsEnabled() {
		return nil, nil
	}
	// Under isolation, MCP is the write path — bind it to a principal by its SECRET
	// token, not just a name, so exec-ing the binary with a known name isn't enough
	// to assume that namespace's identity (#266).
	tok := os.Getenv("BRAINIAC_PRINCIPAL_TOKEN")
	if tok == "" {
		return nil, fmt.Errorf("BRAINIAC_PRINCIPAL_TOKEN is required when principals are configured")
	}
	p := cfg.PrincipalByToken(tok)
	if p == nil {
		return nil, fmt.Errorf("BRAINIAC_PRINCIPAL_TOKEN does not match any configured principal")
	}
	return p, nil
}

// extractorOptions builds the optional local-LLM extractor wiring from config.
// Off by default (chat-driven): returns no options, so ingest keeps its current
// behavior and only the chat path writes nodes/edges.
func extractorOptions(cfg *config.Config) []core.Option {
	switch {
	case cfg.ClaudeExtractionEnabled():
		ext := anthropic.NewExtractor(cfg.Extraction.APIKey, cfg.Extraction.Model, anthropic.WithRetries(cfg.Extraction.Retries))
		return []core.Option{core.WithExtractor(ext, cfg.Extraction.Review)}
	case cfg.LocalExtractionEnabled():
		ext := ollama.NewExtractor(cfg.ExtractorBaseURL(), cfg.Extraction.Model, ollama.WithExtractorRetries(cfg.Extraction.Retries))
		return []core.Option{core.WithExtractor(ext, cfg.Extraction.Review)}
	default:
		return nil
	}
}

// ocrFunc builds the opt-in OCR fallback for scanned PDFs (#356) from config, or
// nil when disabled. Shells out to the configured command with the PDF in a temp
// file — explicit args, never a shell string; stdout is the recognized text.
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

// importFunc dispatches an MCP ingest request to the right connector via the
// shared internal/connectors builder (#428), keeping the mcp/core layers
// plugin-agnostic. An empty markdown target falls back to the container's
// /data/docs mount.
func importFunc(c *core.Core, cfg *config.Config) mcpserver.ImportFunc {
	return func(ctx context.Context, source, target, project string) (core.IngestStats, error) {
		opts := core.IngestOptions{Project: project, Trust: cfg.SourceTrust(source), ChunkParams: chunk.Preset(cfg.SourceChunkPreset(source))}
		if source == "markdown" && target == "" {
			target = "/data/docs"
		}
		conn, err := connectors.Build(ctx, c, cfg, source, target, ocrFunc(cfg))
		if err != nil {
			return core.IngestStats{}, err
		}
		return c.Ingest(ctx, conn, opts)
	}
}

func configPath() string {
	if p := os.Getenv("BRAINIAC_CONFIG"); p != "" {
		return p
	}
	return "config.yaml"
}
