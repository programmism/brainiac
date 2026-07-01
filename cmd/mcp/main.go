// Command brainiac-mcp is the MCP server exposing core operations as tools to
// Claude over stdio — a thin adapter over internal/core.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/mcpserver"
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
	pool, err := store.Connect(ctx, cfg.Storage.DSN)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	embedder := ollama.New(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.Dims)
	c := core.New(pool, embedder)
	srv := mcpserver.New(c)

	// stdio: logs must go to stderr so they don't corrupt the protocol stream.
	log.SetOutput(os.Stderr)
	log.Printf("brainiac-mcp %s: serving over stdio", core.Version)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

func configPath() string {
	if p := os.Getenv("BRAINIAC_CONFIG"); p != "" {
		return p
	}
	return "config.yaml"
}
