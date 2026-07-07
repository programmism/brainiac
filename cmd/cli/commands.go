package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/plugins/markdown"
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
	var project string
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

			hits, err := buildCore(cfg, pool).Search(ctx, strings.Join(args, " "), k, project)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(hits) == 0 {
				fmt.Fprintln(out, "no results")
				return nil
			}
			for _, h := range hits {
				fmt.Fprintf(out, "[%.3f]%s %s\n        %s\n", h.Distance, scopeTag(h.Scope), h.SourceURI, oneline(h.Text))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&k, "k", core.DefaultSearchK, "maximum number of results")
	cmd.Flags().StringVar(&project, "project", "", "scope results to this project + global (omit to search all)")
	return cmd
}

func recallCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
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

			res, err := buildCore(cfg, pool).Recall(ctx, strings.Join(args, " "), project)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if res.ScopeFallback {
				fmt.Fprintf(out, "(no results in %s — showing global memory)\n", res.Scope)
			}
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
				fmt.Fprintf(out, "  [%.3f]%s %s — %s\n", h.Distance, scopeTag(h.Scope), h.SourceURI, oneline(h.Text))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "scope recall to this project + global (omit to recall all)")
	return cmd
}

func rememberCmd() *cobra.Command {
	var typ, summary, project string
	var aliases, discs []string
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

			disc, err := parseDiscs(discs)
			if err != nil {
				return err
			}
			r, err := buildCore(cfg, pool).Remember(ctx, core.RememberInput{
				CanonicalName: args[0], Type: typ, Aliases: aliases, Summary: summary,
				Discriminators: core.Discriminators(project, disc),
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
	cmd.Flags().StringVar(&project, "project", "", "project this entity belongs to (scopes identity; omit for global)")
	cmd.Flags().StringArrayVar(&discs, "disc", nil, "extra identity axis key=value (repeatable, e.g. --disc env=prod)")
	return cmd
}

func linkCmd() *cobra.Command {
	var from, typ, to, why, source, author, project string
	var discs []string
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

			disc, err := parseDiscs(discs)
			if err != nil {
				return err
			}
			edge, err := buildCore(cfg, pool).Link(ctx, core.LinkInput{
				From: from, Type: typ, To: to, Why: why, SourceURI: source, Author: author,
				Discriminators: core.Discriminators(project, disc),
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
	cmd.Flags().StringVar(&project, "project", "", "project both endpoints belong to (scopes identity; omit for global)")
	cmd.Flags().StringArrayVar(&discs, "disc", nil, "extra identity axis key=value (repeatable, e.g. --disc env=prod)")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("type")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func disambiguateCmd() *cobra.Command {
	var project string
	var discs []string
	cmd := &cobra.Command{
		Use:   "disambiguate [node-id]",
		Short: "Re-scope an existing entity by adding identity axes (e.g. --disc env=prod)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			disc, err := parseDiscs(discs)
			if err != nil {
				return err
			}
			node, err := buildCore(cfg, pool).Disambiguate(ctx, args[0], core.Discriminators(project, disc))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "re-scoped %s to %q\n", node.CanonicalName, model.ScopeKey(node.Discriminators))
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project axis to add")
	cmd.Flags().StringArrayVar(&discs, "disc", nil, "identity axis key=value to add (repeatable, e.g. --disc env=prod)")
	return cmd
}

func importCmd() *cobra.Command {
	var source, path, project string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Ingest documents from a configured source (notion | markdown)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			conn, err := buildConnector(cfg, source, path)
			if err != nil {
				return err
			}
			stats, err := buildCore(cfg, pool).Ingest(ctx, conn, core.IngestOptions{Project: project})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"ingested: %d docs, %d chunks (%d kept, %d queued, %d dropped, %d skipped)\n",
				stats.Docs, stats.Chunks, stats.Kept, stats.Queued, stats.Dropped, stats.Skipped)
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "notion", "source type to import from (notion | markdown)")
	cmd.Flags().StringVar(&path, "path", "", "root directory for the markdown source (overrides config)")
	cmd.Flags().StringVar(&project, "project", "", "project to scope imported documents to (omit for global)")
	return cmd
}

func buildConnector(cfg *config.Config, source, path string) (plugins.SourceConnector, error) {
	switch source {
	case "notion":
		sc := cfg.Source("notion")
		if sc == nil || sc.Token == "" {
			return nil, fmt.Errorf("notion source not configured (set a token via NOTION_TOKEN or config.yaml)")
		}
		if path != "" { // --path holds a page URL/id for a targeted import
			return notion.NewForPages(sc.Token, []string{path}), nil
		}
		return notion.New(sc.Token), nil
	case "markdown":
		dir := path
		if dir == "" {
			if sc := cfg.Source("markdown"); sc != nil {
				dir = sc.Path
			}
		}
		if dir == "" {
			return nil, fmt.Errorf("markdown source needs a directory (--path or sources[].path)")
		}
		return markdown.New(dir), nil
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
			fmt.Fprintf(out, "split candidates (%d — contradictory facts, maybe two entities):\n", len(rep.Splits))
			for _, s := range rep.Splits {
				fmt.Fprintf(out, "  - %s (%s)\n", s.Node.CanonicalName, s.Node.ID)
				for _, e := range s.Edges {
					fmt.Fprintf(out, "      edge %s: -%s-> %s\n", e.Edge.ID, e.Edge.Type, e.ToName)
				}
			}
			fmt.Fprintf(out, "conflicts (%d — retire the losing edge with `kb retire-edge <id>`):\n", len(rep.Conflicts))
			for _, c := range rep.Conflicts {
				fmt.Fprintf(out, "  - %s -%s-> %s (%s)  vs  %s (%s)\n", c.From, c.Type, c.ToA, c.EdgeA, c.ToB, c.EdgeB)
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

func retireEdgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retire-edge [edge-id]",
		Short: "Retire an edge (mark historical) to resolve a conflict — reversible via recall history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			if err := buildCore(cfg, pool).RetireEdge(ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "retired edge %s\n", args[0])
			return nil
		},
	}
}

func splitCmd() *cobra.Command {
	var node, axis string
	var routes []string
	cmd := &cobra.Command{
		Use:   "split",
		Short: "Split a conflated node into scoped children, routing its edges (reversible)",
		Long: "Separate one node that conflates two entities into children along a new axis.\n" +
			"Route each edge to a value:  kb split --node <id> --axis env --route <edgeId>=prod --route <edgeId>=staging\n" +
			"Edge ids come from `kb consolidate` split candidates.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			routeMap, err := parseRoutes(routes)
			if err != nil {
				return err
			}
			res, err := buildCore(cfg, pool).Split(ctx, node, axis, routeMap)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, ch := range res.Children {
				fmt.Fprintf(out, "%s{%s=%s} ← %d edge(s) [%s]\n", ch.Node.CanonicalName, axis, ch.Value, ch.Edges, ch.Node.ID)
			}
			if res.ParentRetired {
				fmt.Fprintln(out, "parent retired (no edges left)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "id of the node to split")
	cmd.Flags().StringVar(&axis, "axis", "", "discriminator axis to introduce (e.g. env)")
	cmd.Flags().StringArrayVar(&routes, "route", nil, "edgeId=value (repeatable) — which axis value each edge belongs to")
	_ = cmd.MarkFlagRequired("node")
	_ = cmd.MarkFlagRequired("axis")
	return cmd
}

func reembedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reembed",
		Short: "Re-embed all chunks from stored raw text (after an embedding-model change)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			n, err := buildCore(cfg, pool).Reembed(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "re-embedded %d chunks\n", n)
			return nil
		},
	}
}

func evalCmd() *cobra.Command {
	var goldenPath string
	var k int
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run the golden query set and report recall@k",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			data, err := os.ReadFile(goldenPath) //nolint:gosec // operator-provided path
			if err != nil {
				return err
			}
			var golden []core.GoldenQuery
			if err := json.Unmarshal(data, &golden); err != nil {
				return fmt.Errorf("parse %s: %w", goldenPath, err)
			}
			cfg, pool, err := connect(ctx)
			if err != nil {
				return err
			}
			defer pool.Close()

			res, err := buildCore(cfg, pool).Eval(ctx, golden, k)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "recall@%d: %.1f%%  ·  mean source recall: %.1f%%  (%d queries)\n",
				res.K, res.RecallAtK*100, res.MeanSourceRecall*100, res.Queries)
			for _, q := range res.PerQuery {
				mark := "MISS"
				if q.Hit {
					mark = "ok  "
				}
				fmt.Fprintf(out, "  [%s] %d/%d  %s\n", mark, q.Found, q.Expected, q.Query)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&goldenPath, "golden", "eval/golden.json", "path to the golden query set JSON")
	cmd.Flags().IntVar(&k, "k", core.DefaultEvalK, "recall@k cutoff")
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

// scopeTag renders a result's scope as a space-prefixed tag for CLI output,
// shown only when the result is project-scoped (global is the unmarked default)
// so provenance is visible without cluttering the common case (#143).
func scopeTag(scope string) string {
	if scope == "" || scope == "global" {
		return ""
	}
	return " [" + scope + "]"
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
