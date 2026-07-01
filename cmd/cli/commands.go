package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/plugins/notion"
	"github.com/programmism/brainiac/internal/store"
)

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			_, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()
			if err := store.Migrate(ctx, pool); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "migrations applied")
			return nil
		},
	}
}

func healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Check database and embedder connectivity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			out := cmd.OutOrStdout()
			if err := pool.Ping(ctx); err != nil {
				fmt.Fprintf(out, "db:       ERROR (%v)\n", err)
			} else {
				fmt.Fprintln(out, "db:       ok")
			}
			if err := pingOllama(ctx, cfg.Embedding.BaseURL); err != nil {
				fmt.Fprintf(out, "embedder: unreachable (%v)\n", err)
			} else {
				fmt.Fprintln(out, "embedder: ok")
			}

			m, err := buildCore(cfg, pool).Health(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "chunks:   %d hot, %d cold\n", m.ChunksHot, m.ChunksCold)
			fmt.Fprintf(out, "nodes:    %d current, %d historical (%.1f%% historical)\n", m.Nodes, m.NodesHistorical, m.PercentNodesHistory)
			fmt.Fprintf(out, "edges:    %d current, %d historical (%.2f per node)\n", m.Edges, m.EdgesHistorical, m.EdgesPerNode)
			return nil
		},
	}
}

func searchCmd() *cobra.Command {
	var k int
	cmd := &cobra.Command{
		Use:   "search [query...]",
		Short: "Semantic search over the memory",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			hits, err := buildCore(cfg, pool).Search(ctx, strings.Join(args, " "), k)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(hits) == 0 {
				fmt.Fprintln(out, "no results")
				return nil
			}
			for _, h := range hits {
				fmt.Fprintf(out, "[%.3f] %s\n        %s\n", h.Distance, h.SourceURI, oneline(h.Text))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&k, "k", core.DefaultSearchK, "maximum number of results")
	return cmd
}

func recallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recall [query...]",
		Short: "Recall the why/how behind a topic (chunks + graph, cited)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			res, err := buildCore(cfg, pool).Recall(ctx, strings.Join(args, " "))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "nodes: %s\n", strings.Join(nodeNames(res), ", "))
			fmt.Fprintln(out, "edges:")
			for _, e := range res.Edges {
				fmt.Fprintf(out, "  %s -%s-> %s", e.FromName, e.Edge.Type, e.ToName)
				if e.Edge.Why != "" {
					fmt.Fprintf(out, " (why: %s)", e.Edge.Why)
				}
				if e.Edge.SourceURI != "" {
					fmt.Fprintf(out, " [%s]", e.Edge.SourceURI)
				}
				fmt.Fprintln(out)
			}
			fmt.Fprintln(out, "chunks:")
			for _, h := range res.Chunks {
				fmt.Fprintf(out, "  [%.3f] %s — %s\n", h.Distance, h.SourceURI, oneline(h.Text))
			}
			return nil
		},
	}
}

func rememberCmd() *cobra.Command {
	var typ, summary string
	var aliases []string
	cmd := &cobra.Command{
		Use:   "remember [canonical-name]",
		Short: "Upsert an entity (node) with dedup check",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			r, err := buildCore(cfg, pool).Remember(ctx, core.RememberInput{
				CanonicalName: args[0], Type: typ, Aliases: aliases, Summary: summary,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			verb := "matched existing"
			if r.Created {
				verb = "created"
			}
			fmt.Fprintf(out, "%s %s (id=%s)\n", verb, r.Node.CanonicalName, r.Node.ID)
			for _, d := range r.Duplicates {
				fmt.Fprintf(out, "  duplicate? %s (%s", d.Node.CanonicalName, d.Reason)
				if d.Reason == "semantic" {
					fmt.Fprintf(out, ", dist=%.3f", d.Distance)
				}
				fmt.Fprintln(out, ")")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&typ, "type", "", "node type (service, datastore, decision, ...)")
	cmd.Flags().StringVar(&summary, "summary", "", "short description; embedded for semantic dedup")
	cmd.Flags().StringArrayVar(&aliases, "alias", nil, "alternative surface form (repeatable)")
	return cmd
}

func linkCmd() *cobra.Command {
	var from, typ, to, why, source, author string
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Record a relationship (edge) with rationale and provenance",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			edge, err := buildCore(cfg, pool).Link(ctx, core.LinkInput{
				From: from, Type: typ, To: to, Why: why, SourceURI: source, Author: author,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "linked %s -%s-> %s (edge=%s)\n", from, typ, to, edge.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "source entity canonical name")
	cmd.Flags().StringVar(&typ, "type", "", "relationship type (writes_to, depends_on, ...)")
	cmd.Flags().StringVar(&to, "to", "", "target entity canonical name")
	cmd.Flags().StringVar(&why, "why", "", "the rationale")
	cmd.Flags().StringVar(&source, "source", "", "provenance URI")
	cmd.Flags().StringVar(&author, "author", "", "who recorded this")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("type")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func importCmd() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Ingest documents from a configured source (default: notion)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			conn, err := buildConnector(cfg, source)
			if err != nil {
				return err
			}
			stats, err := buildCore(cfg, pool).Ingest(ctx, conn, core.IngestOptions{})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"ingested: %d docs, %d chunks (%d kept, %d queued, %d dropped, %d skipped)\n",
				stats.Docs, stats.Chunks, stats.Kept, stats.Queued, stats.Dropped, stats.Skipped)
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "notion", "source type to import from")
	return cmd
}

func buildConnector(cfg *config.Config, source string) (plugins.SourceConnector, error) {
	switch source {
	case "notion":
		sc := cfg.Source("notion")
		if sc == nil || sc.Token == "" {
			return nil, fmt.Errorf("notion source not configured (set a token via NOTION_TOKEN or config.yaml)")
		}
		return notion.New(sc.Token), nil
	default:
		return nil, fmt.Errorf("unknown source %q", source)
	}
}

func consolidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "consolidate",
		Short: "Run the librarian pass and print review candidates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			rep, err := buildCore(cfg, pool).Consolidate(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "merge candidates (%d groups):\n", len(rep.MergeGroups))
			for _, g := range rep.MergeGroups {
				names := make([]string, 0, len(g))
				for _, n := range g {
					names = append(names, fmt.Sprintf("%s (%s)", n.CanonicalName, n.ID))
				}
				fmt.Fprintf(out, "  - %s\n", strings.Join(names, "  ↔  "))
			}
			fmt.Fprintf(out, "conflicts (%d):\n", len(rep.Conflicts))
			for _, c := range rep.Conflicts {
				fmt.Fprintf(out, "  - %s -%s-> %s  vs  %s\n", c.From, c.Type, c.ToA, c.ToB)
			}
			fmt.Fprintf(out, "stale edges: %d\n", len(rep.Stale))
			fmt.Fprintf(out, "rollup candidates (%d):\n", len(rep.Rollups))
			for _, r := range rep.Rollups {
				fmt.Fprintf(out, "  - %s (%d edges)\n", r.Name, r.EdgeCount)
			}
			return nil
		},
	}
}

func mergeCmd() *cobra.Command {
	var keep, drop string
	cmd := &cobra.Command{
		Use:   "merge",
		Short: "Merge a duplicate node into a keeper (reversible)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			if err := buildCore(cfg, pool).ApplyMerge(ctx, keep, drop); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "merged %s into %s\n", drop, keep)
			return nil
		},
	}
	cmd.Flags().StringVar(&keep, "keep", "", "id of the node to keep")
	cmd.Flags().StringVar(&drop, "drop", "", "id of the duplicate node to fold in")
	_ = cmd.MarkFlagRequired("keep")
	_ = cmd.MarkFlagRequired("drop")
	return cmd
}

func supersedeCmd() *cobra.Command {
	var oldID, newID, why, author string
	cmd := &cobra.Command{
		Use:   "supersede",
		Short: "Mark that a new node replaces an old one (kept as historical)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			if err := buildCore(cfg, pool).Supersede(ctx, oldID, newID, why, author); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "superseded")
			return nil
		},
	}
	cmd.Flags().StringVar(&oldID, "old", "", "id of the node being replaced")
	cmd.Flags().StringVar(&newID, "new", "", "id of the replacement node")
	cmd.Flags().StringVar(&why, "why", "", "why the change was made")
	cmd.Flags().StringVar(&author, "author", "", "who recorded this")
	_ = cmd.MarkFlagRequired("old")
	_ = cmd.MarkFlagRequired("new")
	return cmd
}

// --- helpers ---

func nodeNames(res *core.RecallResult) []string {
	names := make([]string, 0, len(res.Nodes))
	for _, n := range res.Nodes {
		names = append(names, n.CanonicalName)
	}
	return names
}

func oneline(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 100 {
		return s[:97] + "..."
	}
	return s
}

func pingOllama(ctx context.Context, baseURL string) error {
	if baseURL == "" {
		return fmt.Errorf("no embedder base url configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
