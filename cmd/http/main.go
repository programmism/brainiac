// Command brainiac-http is the deployable app: on boot it loads config,
// connects to Postgres, applies migrations, and serves the HTTP surface
// (health endpoints now; REST/WebUI in #19/#21). A thin adapter over core.
//
// The `healthcheck` subcommand lets the distroless container health-probe
// itself without a shell.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/programmism/brainiac/internal/applog"
	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/logbuf"
	"github.com/programmism/brainiac/internal/plugins/anthropic"
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
	// The in-memory ring backs the WebUI Logs tab (#166); config.Load doesn't log,
	// so nothing is lost by configuring the logger from cfg immediately after.
	logs := logbuf.New(0)

	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}

	// Structured JSON (or text) to stdout, teed into the ring (#258). Docker's
	// json-file driver rotates stdout, so the durable app log survives a crash;
	// the ring is only a WebUI convenience. Bridges stdlib log.Printf, so every
	// existing call site emits the same structured records.
	applog.Setup(os.Stdout, logs, cfg.Logging.Format, cfg.Logging.Level)

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

	embedder := ollama.New(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.Dims, ollama.WithBatchSize(cfg.Embedding.BatchSize), ollama.WithMaxConcurrency(cfg.Embedding.MaxConcurrency))
	c := core.New(pool, embedder, density.New(), append(extractorOptions(cfg), retrievalOption(cfg))...)

	writable := cfg.Clients.WebUI == "interactive"
	if writable && cfg.HTTP.AuthToken == "" {
		log.Printf("warning: clients.webui=interactive but AUTH_TOKEN is unset — write endpoints stay DISABLED")
	}
	var auth server.PrincipalMatcher
	var reloadable *reloadableAuth
	if a := cfg.BuildAuthenticator(); a != nil {
		reloadable = &reloadableAuth{}
		reloadable.store(a)
		auth = reloadable
		log.Printf("hard isolation ON: %d principal(s) — /api requires a principal token, reads walled per namespace (#120)", a.Len())
	}
	embedderCheck := ollamaChecker(cfg.Embedding.BaseURL, cfg.Embedding.Model)
	// The embedder readiness caveat (still-downloading / unreachable model, #250) is
	// surfaced in the startup banner below, once, so it lands next to the WebUI URL.
	if cfg.RateLimitEnabled() {
		log.Printf("rate limiting ON: %g req/s per client, burst %d (#270)", cfg.HTTP.RateLimitRPS, cfg.EffectiveRateLimitBurst())
	}
	if cfg.Embedding.MaxConcurrency > 0 {
		log.Printf("embed concurrency capped at %d in-flight round-trips to Ollama (#270)", cfg.Embedding.MaxConcurrency)
	}
	handler := server.New(pool, embedderCheck, c, server.Options{
		Writable:       writable,
		AuthToken:      cfg.HTTP.AuthToken,
		Auth:           auth,
		Logs:           logs,
		RateLimitRPS:   cfg.HTTP.RateLimitRPS,
		RateLimitBurst: cfg.EffectiveRateLimitBurst(),
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

	// Hot principal reload on SIGHUP (#269): re-read config and swap the roster
	// atomically, so a revocation, rotation, or expiry edit takes effect without a
	// restart. Only wired under hard isolation. A config that no longer validates —
	// or that dropped principals entirely — is rejected, keeping the live roster.
	if reloadable != nil {
		go watchReload(shutdownCtx, reloadable)
	}

	// Optional background auto-import: drop files in ./data/docs and they appear.
	if d := cfg.AutoImportInterval(); d > 0 {
		go autoImport(shutdownCtx, c, cfg, d)
	}

	logStartupBanner(cfg, writable, embedderCheck)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// logStartupBanner prints a friendly, greppable summary at boot so a first-time
// operator sees the WebUI URL, write mode, and any "still warming up" caveats
// without reading the docs (#254). Everything here is already logged in pieces;
// the banner just puts the actionable bits in one block.
func logStartupBanner(cfg *config.Config, writable bool, embedderCheck server.Checker) {
	url := webURL(cfg.HTTP.Addr)
	mode := "read-only"
	if writable {
		mode = "interactive (writes enabled)"
	} else if cfg.Clients.WebUI == "interactive" {
		mode = "read-only (set AUTH_TOKEN to enable writes)"
	}
	log.Printf("──────────────────────────────────────────────")
	log.Printf(" Brainiac %s is up", core.Version)
	log.Printf("   WebUI:   %s", url)
	log.Printf("   Health:  %s/healthz   Ready: %s/readyz", url, url)
	log.Printf("   WebUI mode: %s", mode)
	if cfg.PrincipalsEnabled() {
		log.Printf("   Auth: hard isolation ON — every /api call needs a principal token (#120)")
	}
	// Surface the common first-run gotcha: the embedder model is still downloading,
	// so search/recall will 503 until the pull finishes.
	if embedderCheck != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := embedderCheck(cctx); errors.Is(err, server.ErrEmbedderModelMissing) {
			log.Printf("   ⏳ embedder model %q still downloading — search/recall 503 until it finishes", cfg.Embedding.Model)
		} else if err != nil {
			log.Printf("   ⚠ embedder not reachable yet at %s — search/recall 503 until it is up", cfg.Embedding.BaseURL)
		} else {
			log.Printf("   ✓ embedder ready (%s)", cfg.Embedding.Model)
		}
		cancel()
	}
	log.Printf("──────────────────────────────────────────────")
}

// webURL turns a listen address (":8080", "0.0.0.0:8080") into a clickable URL,
// defaulting the host to localhost for the common bind-all / bind-local cases.
func webURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://localhost" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

// reloadableAuth is a server.PrincipalMatcher backed by an atomically swappable
// authenticator, so SIGHUP can install a new roster while requests are in flight
// (#269).
type reloadableAuth struct {
	p atomic.Pointer[config.PrincipalAuthenticator]
}

func (r *reloadableAuth) store(a *config.PrincipalAuthenticator) { r.p.Store(a) }

func (r *reloadableAuth) Match(token string, now time.Time) *core.Principal {
	return r.p.Load().Match(token, now)
}

// watchReload swaps the live principal roster on each SIGHUP until ctx is done.
// A config that fails to load/validate, or that removed every principal, is
// rejected with a log line and the previous roster stays in force (isolation can
// never silently turn off mid-flight).
func watchReload(ctx context.Context, r *reloadableAuth) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			ncfg, err := config.Load(configPath())
			if err != nil {
				log.Printf("SIGHUP reload rejected (config invalid, keeping current roster): %v", err)
				continue
			}
			na := ncfg.BuildAuthenticator()
			if na == nil {
				log.Printf("SIGHUP reload rejected: roster is now empty; keeping current one (restart to disable isolation)")
				continue
			}
			r.store(na)
			log.Printf("SIGHUP: reloaded %d principal(s) — revocations/rotations/expiry now live", na.Len())
		}
	}
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
			stats, err := c.Ingest(ctx, markdown.New(dir), core.IngestOptions{OnProgress: onProgress, Incremental: true})
			if err != nil {
				log.Printf("auto-import %s: %v", dir, err)
				continue
			}
			if stats.Kept+stats.Queued+stats.Deleted > 0 {
				log.Printf("auto-import %s: +%d kept, %d deleted", dir, stats.Kept+stats.Queued, stats.Deleted)
			}
			if stats.FetchErrors > 0 {
				log.Printf("auto-import %s: %d fetch error(s) skipped (import continued) (#241)", dir, stats.FetchErrors)
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
// extractorOptions builds the optional local-LLM extractor wiring from config.
// Off by default (chat-driven): returns no options so ingest is unchanged.
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

func ollamaChecker(baseURL, model string) server.Checker {
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
		// Ollama is up — is the required model actually pulled? Distinguish
		// "model-missing" from "unreachable" so a still-downloading model reads
		// clearly (#250).
		var tags struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
			return nil // reachable; if we can't parse tags, don't claim model-missing
		}
		base := strings.SplitN(model, ":", 2)[0]
		for _, m := range tags.Models {
			if strings.SplitN(m.Name, ":", 2)[0] == base {
				return nil
			}
		}
		return server.ErrEmbedderModelMissing
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
