// Command brainiac-http is the deployable app: on boot it loads config,
// connects to Postgres, applies migrations, and serves the HTTP surface
// (health endpoints now; REST/WebUI in #19/#21). A thin adapter over core.
//
// The `healthcheck` subcommand lets the distroless container health-probe
// itself without a shell.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/plugins/density"
	"github.com/programmism/brainiac/internal/plugins/markdown"
	"github.com/programmism/brainiac/internal/plugins/ollama"
	"github.com/programmism/brainiac/internal/server"
	"github.com/programmism/brainiac/internal/store"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(runHealthcheck())
		case "version", "--version":
			fmt.Printf("brainiac-http %s\n", core.Version)
			return
		}
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
	log.Printf("brainiac-http %s: connecting to %s", core.Version, config.RedactedDSN(cfg.Storage.DSN))
	pool, err := store.ConnectWithRetry(ctx, cfg.Storage.DSN, 60*time.Second)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	log.Printf("brainiac-http %s: applying migrations", core.Version)
	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	embedder := ollama.New(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.Dims, ollama.WithBatchSize(cfg.Embedding.BatchSize))
	c := core.New(pool, embedder, density.New())

	writable := cfg.Clients.WebUI == "interactive"
	if writable && cfg.HTTP.AuthToken == "" {
		log.Printf("warning: clients.webui=interactive but AUTH_TOKEN is unset — write endpoints stay DISABLED")
	}
	handler := server.New(pool, ollamaChecker(cfg.Embedding.BaseURL), c, server.Options{
		Writable:  writable,
		AuthToken: cfg.HTTP.AuthToken,
	})
	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	shutdownCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownCtx.Done()
		log.Printf("shutting down")
		toCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(toCtx)
	}()

	// Optional background auto-import: drop files in ./data/docs and they appear.
	if d := cfg.AutoImportInterval(); d > 0 {
		go autoImport(shutdownCtx, c, cfg, d)
	}

	log.Printf("listening on %s", cfg.HTTP.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// autoImport re-ingests the conventional /data/docs folder plus any configured
// markdown sources on a timer. Content-hash reconcile + content-defined chunking
// make repeated runs cheap and idempotent; a missing folder is a no-op.
func autoImport(ctx context.Context, c *core.Core, cfg *config.Config, every time.Duration) {
	dirs := map[string]bool{"/data/docs": true}
	for _, s := range cfg.Sources {
		if s.Type == "markdown" && s.Path != "" {
			dirs[s.Path] = true
		}
	}
	// Log completion of a large document's embedding (#139) — small docs stay
	// quiet (they finish under one progress step).
	onProgress := func(p core.IngestProgress) {
		if p.ToEmbed >= 64 && p.Embedded == p.ToEmbed {
			log.Printf("auto-import: embedded %d chunks of %s", p.Embedded, p.Doc)
		}
	}
	run := func() {
		for dir := range dirs {
			stats, err := c.Ingest(ctx, markdown.New(dir), core.IngestOptions{OnProgress: onProgress})
			if err != nil {
				log.Printf("auto-import %s: %v", dir, err)
				continue
			}
			if stats.Kept+stats.Queued+stats.Deleted > 0 {
				log.Printf("auto-import %s: +%d kept, %d deleted", dir, stats.Kept+stats.Queued, stats.Deleted)
			}
		}
	}
	log.Printf("auto-import enabled every %s (watching /data/docs)", every)
	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// ollamaChecker returns a readiness probe for the embedder backend. It hits the
// Ollama tags endpoint; a failure is reported but never fatal (§11).
func ollamaChecker(baseURL string) server.Checker {
	if baseURL == "" {
		return nil
	}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("ollama status %d", resp.StatusCode)
		}
		return nil
	}
}

// runHealthcheck probes the local /healthz endpoint and maps the result to an
// exit code, for use as the container HEALTHCHECK command.
func runHealthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host := addr
	if strings.HasPrefix(addr, ":") {
		host = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + host + "/healthz") //nolint:noctx // short-lived probe
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func configPath() string {
	if p := os.Getenv("BRAINIAC_CONFIG"); p != "" {
		return p
	}
	return "config.yaml"
}
