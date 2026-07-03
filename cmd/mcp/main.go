// Command brainiac-mcp is the MCP server exposing core operations as tools to
// Claude over stdio — a thin adapter over internal/core.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/mcpserver"
	"github.com/programmism/brainiac/internal/plugins/density"
	"github.com/programmism/brainiac/internal/plugins/markdown"
	"github.com/programmism/brainiac/internal/plugins/notion"
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

	ctx := context.Background()
	pool, err := store.ConnectWithRetry(ctx, cfg.Storage.DSN, 60*time.Second)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	embedder := ollama.New(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.Dims)
	c := core.New(pool, embedder, density.New())
	srv := mcpserver.New(c, importFunc(c, cfg))

	// stdio: logs must go to stderr so they don't corrupt the protocol stream.
	log.SetOutput(os.Stderr)
	log.Printf("brainiac-mcp %s: serving over stdio", core.Version)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// importFunc dispatches an MCP ingest request to the right connector, keeping
// the mcp/core layers plugin-agnostic.
func importFunc(c *core.Core, cfg *config.Config) mcpserver.ImportFunc {
	return func(ctx context.Context, source, target, project string) (core.IngestStats, error) {
		opts := core.IngestOptions{Project: project}
		switch source {
		case "markdown":
			dir := target
			if dir == "" {
				dir = "/data/docs"
			}
			return c.Ingest(ctx, markdown.New(dir), opts)
		case "notion":
			sc := cfg.Source("notion")
			if sc == nil || sc.Token == "" {
				return core.IngestStats{}, fmt.Errorf("notion is not configured (set NOTION_TOKEN)")
			}
			if target == "" {
				return c.Ingest(ctx, notion.New(sc.Token), opts)
			}
			return c.Ingest(ctx, notion.NewForPages(sc.Token, []string{target}), opts)
		default:
			return core.IngestStats{}, fmt.Errorf("unknown source %q (use notion or markdown)", source)
		}
	}
}

func configPath() string {
	if p := os.Getenv("BRAINIAC_CONFIG"); p != "" {
		return p
	}
	return "config.yaml"
}
