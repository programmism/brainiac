// Package mcpserver is the MCP client — a thin adapter that exposes the core
// operations as MCP tools for Claude. It holds no business logic: each tool
// marshals arguments, forwards to internal/core, and renders the result
// (SYSTEM.md §2, §6.1).
package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/model"
)

// parseAsOf accepts an RFC3339 timestamp or a bare YYYY-MM-DD date (interpreted at
// UTC midnight) for the get_node as-of lens (#200).
func parseAsOf(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("as_of %q must be RFC3339 or YYYY-MM-DD", s)
}

// ImportFunc runs an ingest for a source ("notion"|"slack"|"github"|"gdrive"|"linear"|"markdown"), optional target
// (a Notion page URL/id or a path), and optional project (scopes the imported
// chunks). It is supplied by the app wiring so the core/mcp layers stay
// plugin-agnostic; nil disables the ingest tool.
type ImportFunc func(ctx context.Context, source, target, project string) (core.IngestStats, error)

// New builds an MCP server exposing search/remember/link/recall/supersede (and
// ingest, if importFn is set) over the given core.
// principal, when non-nil, is the single hard-isolation identity this stdio
// process runs as (#120): a receiving middleware binds it to every tool call's
// context so core walls reads / pins writes. Nil = Layer 1.
func New(c *core.Core, importFn ImportFunc, principal *core.Principal) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "brainiac", Version: core.Version}, nil)

	if principal != nil {
		s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				return next(core.WithPrincipal(ctx, principal), method, req)
			}
		})
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search",
		Description: "Semantic search over the memory. Returns the most relevant chunks with their source for citation.",
	}, searchTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "remember",
		Description: "Upsert an entity (node). Returns duplicate candidates to review; never auto-merges. Use before link when saving new entities.",
	}, rememberTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "link",
		Description: "Record a relationship (edge) between two entities by name, with the rationale (why), provenance, and author. Missing entities are created.",
	}, linkTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "recall",
		Description: "Answer 'why/how' questions: returns relevant chunks, entities, relationships (with rationale) and the raw evidence behind them. Cite every claim by source.",
	}, recallTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_node",
		Description: "Look up one entity you already know — by name (optionally scoped to a project) or by id — and return its full record (aliases, type, discriminators) plus its relationships. Use after recall surfaces an entity and you need its details, or to check what it links to.",
	}, getNodeTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "rollup",
		Description: "Record a 'current state of X' summary on a hub entity — a synthesis over its detailed edge history. Use on entities with many relationships to give a quick current-state answer without replaying every edge. Look up the node id first (recall/get_node).",
	}, rollupTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "supersede",
		Description: "Record that a new entity replaces an old one: adds a supersedes link and marks the old one historical (kept, not deleted).",
	}, supersedeTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "disambiguate",
		Description: "Re-scope an existing entity by adding identity axes (e.g. env=prod) when you notice it actually conflates two things. The entity keeps its facts; a later save of the other variant becomes a distinct entity. Errors if the target identity is already taken (merge instead).",
	}, disambiguateTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "add_document",
		Description: "Store a document's text into the searchable memory (chunked + embedded). Use when you've read content elsewhere — e.g. a Notion page via your own integration, or a web page — and want it searchable/recall-able later. Give a stable source_uri (the page URL) for citation; re-adding the same source_uri updates it.",
	}, addDocumentTool(c))

	if importFn != nil {
		mcp.AddTool(s, &mcp.Tool{
			Name:        "ingest",
			Description: "Import documents into the memory. source is 'notion', 'slack', 'github', 'gdrive', 'linear', or 'markdown'; target is a Notion page URL/id or a path (empty = the whole source). Use this when the user shares a doc link or asks to import.",
		}, ingestTool(importFn))
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "proposals",
		Description: "List pending extraction proposals (nodes/edges the local-LLM extractor suggested during ingest) awaiting review. Empty unless the local extractor is enabled. Review these, then approve/reject with review_proposal.",
	}, proposalsTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "review_proposal",
		Description: "Approve or reject a pending extraction proposal. kind is 'node' or 'edge'; approve promotes it into the live memory, reject retires it. Approving an edge also promotes its endpoints.",
	}, reviewProposalTool(c))

	return s
}

// --- DTOs ---

type chunkDTO struct {
	Text      string  `json:"text"`
	SourceURI string  `json:"source_uri"`
	Distance  float64 `json:"distance,omitempty"`
	Scope     string  `json:"scope,omitempty"` // "global" or "project:NAME" (#143)
}

type searchIn struct {
	Query   string `json:"query" jsonschema:"the search query"`
	K       int    `json:"k,omitempty" jsonschema:"maximum number of results (default 10)"`
	Project string `json:"project,omitempty" jsonschema:"scope results to this project + global; omit to search across all projects"`
}
type searchOut struct {
	Chunks []chunkDTO `json:"chunks"`
}

type dupDTO struct {
	CanonicalName string  `json:"canonical_name"`
	Reason        string  `json:"reason"`
	Distance      float64 `json:"distance,omitempty"`
}
type rememberIn struct {
	CanonicalName  string            `json:"canonical_name" jsonschema:"the entity's canonical name"`
	Type           string            `json:"type,omitempty" jsonschema:"node type (snake_case; reuse an existing one over a synonym): service, datastore, decision, constraint, team, person, ..."`
	Aliases        []string          `json:"aliases,omitempty" jsonschema:"alternative surface forms"`
	Summary        string            `json:"summary,omitempty" jsonschema:"short description of the entity; stored and returned on recall/get_node, and embedded for semantic dedup"`
	Project        string            `json:"project,omitempty" jsonschema:"the project this entity belongs to (scopes identity so same-named entities in different projects stay distinct); omit for universal/global entities like a vendor or standard"`
	Discriminators map[string]string `json:"discriminators,omitempty" jsonschema:"extra identity axes beyond project (e.g. env, client, version) to keep same-named entities distinct; keys/values must not contain ';' or '='"`
}
type rememberOut struct {
	NodeID        string   `json:"node_id"`
	CanonicalName string   `json:"canonical_name"`
	Created       bool     `json:"created"`
	Duplicates    []dupDTO `json:"duplicates,omitempty"`
}

type linkIn struct {
	From           string            `json:"from" jsonschema:"source entity canonical name"`
	Type           string            `json:"type" jsonschema:"relationship type (snake_case; reuse an existing one over a synonym — case/separators are normalized): writes_to, depends_on, rejected, supersedes, ..."`
	To             string            `json:"to" jsonschema:"target entity canonical name"`
	Why            string            `json:"why" jsonschema:"the rationale — why it is this way"`
	SourceURI      string            `json:"source_uri,omitempty" jsonschema:"provenance: file, PR, page, thread"`
	Author         string            `json:"author,omitempty"`
	Project        string            `json:"project,omitempty" jsonschema:"the project both endpoints belong to (scopes their identity); omit for universal/global entities"`
	Discriminators map[string]string `json:"discriminators,omitempty" jsonschema:"extra identity axes beyond project (e.g. env, client) applied to both endpoints; keys/values must not contain ';' or '='"`
}
type linkOut struct {
	EdgeID string `json:"edge_id"`
}

type edgeDTO struct {
	From      string `json:"from"`
	Type      string `json:"type"`
	To        string `json:"to"`
	Why       string `json:"why,omitempty"`
	SourceURI string `json:"source_uri,omitempty"`
	Status    string `json:"status,omitempty"`
}

// nodeDTO carries a recalled entity's full identity, not just its name — so a
// caller that recalls an entity can read its aliases/type without a second
// lookup (the core already has these on model.Node; earlier only CanonicalName
// was surfaced).
type nodeDTO struct {
	ID             string            `json:"id"`
	CanonicalName  string            `json:"canonical_name"`
	Aliases        []string          `json:"aliases,omitempty"`
	Type           string            `json:"type,omitempty"`
	Summary        string            `json:"summary,omitempty"`
	Rollup         string            `json:"rollup,omitempty"`
	Discriminators map[string]string `json:"discriminators,omitempty"`
	Status         string            `json:"status,omitempty"`
}

// toNodeDTO flattens a model.Node into the client-facing DTO (identity + summary +
// rollup, without the embedding).
func toNodeDTO(n model.Node) nodeDTO {
	return nodeDTO{
		ID: n.ID, CanonicalName: n.CanonicalName, Aliases: n.Aliases, Type: n.Type,
		Summary: n.Summary, Rollup: n.Rollup, Discriminators: n.Discriminators, Status: string(n.Status),
	}
}

type recallIn struct {
	Query   string `json:"query" jsonschema:"the question to answer"`
	Project string `json:"project,omitempty" jsonschema:"scope recall to this project + global; omit to recall across all projects"`
}
type recallOut struct {
	Chunks   []chunkDTO `json:"chunks"`
	Nodes    []nodeDTO  `json:"nodes"`
	Edges    []edgeDTO  `json:"edges"`
	Evidence []chunkDTO `json:"evidence"`
	// Scope is the requested retrieval scope; ScopeFallback is true when a scoped
	// query found nothing in its project and every result is global (#143).
	Scope         string `json:"scope"`
	ScopeFallback bool   `json:"scope_fallback,omitempty"`
}

type addDocumentIn struct {
	SourceURI string `json:"source_uri" jsonschema:"stable identifier for citation, e.g. the page URL"`
	Text      string `json:"text" jsonschema:"the document's text content to store"`
	Project   string `json:"project,omitempty" jsonschema:"the project this document belongs to (scopes it for the retrieval lens); omit for universal/global content"`
}

type ingestIn struct {
	Source  string `json:"source" jsonschema:"where to import from: notion, slack, github, gdrive, linear, or markdown"`
	Target  string `json:"target,omitempty" jsonschema:"a Notion page URL/id, or a path; empty imports the whole source"`
	Project string `json:"project,omitempty" jsonschema:"the project these documents belong to (scopes them for the retrieval lens); omit for universal/global content"`
}
type ingestOut struct {
	Docs    int `json:"docs"`
	Chunks  int `json:"chunks"`
	Kept    int `json:"kept"`
	Queued  int `json:"queued"`
	Dropped int `json:"dropped"`
	Skipped int `json:"skipped"`
	Deleted int `json:"deleted"`
	Failed  int `json:"failed"`
	// Extraction totals, present only when the local-LLM extractor is enabled.
	ExtractedNodes int `json:"extracted_nodes,omitempty"`
	ExtractedEdges int `json:"extracted_edges,omitempty"`
	ExtractFailed  int `json:"extract_failed,omitempty"`
}

type proposalsIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum proposals of each kind to return (default 100)"`
}
type proposalNodeDTO struct {
	ID            string `json:"id"`
	CanonicalName string `json:"canonical_name"`
	Type          string `json:"type,omitempty"`
	Scope         string `json:"scope,omitempty"`
}
type proposalEdgeDTO struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Type string `json:"type"`
	To   string `json:"to"`
	Why  string `json:"why,omitempty"`
}
type proposalsOut struct {
	Nodes []proposalNodeDTO `json:"nodes"`
	Edges []proposalEdgeDTO `json:"edges"`
}

type reviewProposalIn struct {
	Kind    string `json:"kind" jsonschema:"'node' or 'edge'"`
	ID      string `json:"id" jsonschema:"the proposal's id"`
	Approve bool   `json:"approve" jsonschema:"true to approve (promote to live), false to reject (retire)"`
}
type reviewProposalOut struct {
	OK bool `json:"ok"`
}

type supersedeIn struct {
	OldID  string `json:"old_id" jsonschema:"id of the node being replaced"`
	NewID  string `json:"new_id" jsonschema:"id of the replacement node"`
	Why    string `json:"why" jsonschema:"why the change was made"`
	Author string `json:"author,omitempty"`
}

type disambiguateIn struct {
	NodeID         string            `json:"node_id" jsonschema:"id of the entity to re-scope"`
	Project        string            `json:"project,omitempty" jsonschema:"project axis to add"`
	Discriminators map[string]string `json:"discriminators,omitempty" jsonschema:"identity axes to add, e.g. {\"env\":\"prod\"}; keys/values must not contain ';' or '='"`
}
type disambiguateOut struct {
	NodeID   string `json:"node_id"`
	ScopeKey string `json:"scope_key"`
}
type supersedeOut struct {
	OK bool `json:"ok"`
}

// --- handlers ---

func searchTool(c *core.Core) mcp.ToolHandlerFor[searchIn, searchOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
		hits, err := c.Search(ctx, in.Query, in.K, in.Project)
		if err != nil {
			return nil, searchOut{}, err
		}
		out := searchOut{Chunks: make([]chunkDTO, 0, len(hits))}
		for _, h := range hits {
			out.Chunks = append(out.Chunks, chunkDTO{Text: h.Text, SourceURI: h.SourceURI, Distance: h.Distance, Scope: h.Scope})
		}
		return text(fmt.Sprintf("found %d chunk(s)", len(out.Chunks))), out, nil
	}
}

func rememberTool(c *core.Core) mcp.ToolHandlerFor[rememberIn, rememberOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in rememberIn) (*mcp.CallToolResult, rememberOut, error) {
		r, err := c.Remember(ctx, core.RememberInput{
			CanonicalName: in.CanonicalName, Type: in.Type, Aliases: in.Aliases, Summary: in.Summary,
			Discriminators: core.Discriminators(in.Project, in.Discriminators),
		})
		if err != nil {
			return nil, rememberOut{}, err
		}
		out := rememberOut{NodeID: r.Node.ID, CanonicalName: r.Node.CanonicalName, Created: r.Created}
		for _, d := range r.Duplicates {
			out.Duplicates = append(out.Duplicates, dupDTO{CanonicalName: d.Node.CanonicalName, Reason: d.Reason, Distance: d.Distance})
		}
		verb := "matched existing"
		if r.Created {
			verb = "created"
		}
		return text(fmt.Sprintf("%s node %q (%d duplicate candidate(s))", verb, r.Node.CanonicalName, len(out.Duplicates))), out, nil
	}
}

func linkTool(c *core.Core) mcp.ToolHandlerFor[linkIn, linkOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in linkIn) (*mcp.CallToolResult, linkOut, error) {
		edge, err := c.Link(ctx, core.LinkInput{
			From: in.From, Type: in.Type, To: in.To, Why: in.Why, SourceURI: in.SourceURI, Author: in.Author,
			Discriminators: core.Discriminators(in.Project, in.Discriminators),
		})
		if err != nil {
			return nil, linkOut{}, err
		}
		return text(fmt.Sprintf("linked %s -%s-> %s", in.From, in.Type, in.To)), linkOut{EdgeID: edge.ID}, nil
	}
}

func recallTool(c *core.Core) mcp.ToolHandlerFor[recallIn, recallOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in recallIn) (*mcp.CallToolResult, recallOut, error) {
		res, err := c.Recall(ctx, in.Query, in.Project)
		if err != nil {
			return nil, recallOut{}, err
		}
		out := recallOut{
			Chunks:        make([]chunkDTO, 0, len(res.Chunks)),
			Nodes:         make([]nodeDTO, 0, len(res.Nodes)),
			Edges:         make([]edgeDTO, 0, len(res.Edges)),
			Evidence:      make([]chunkDTO, 0, len(res.EvidenceChunks)),
			Scope:         res.Scope,
			ScopeFallback: res.ScopeFallback,
		}
		for _, h := range res.Chunks {
			out.Chunks = append(out.Chunks, chunkDTO{Text: h.Text, SourceURI: h.SourceURI, Distance: h.Distance, Scope: h.Scope})
		}
		for _, n := range res.Nodes {
			out.Nodes = append(out.Nodes, toNodeDTO(n))
		}
		for _, e := range res.Edges {
			out.Edges = append(out.Edges, edgeDTO{
				From: e.FromName, Type: e.Edge.Type, To: e.ToName,
				Why: e.Edge.Why, SourceURI: e.Edge.SourceURI, Status: string(e.Edge.Status),
			})
		}
		for _, ch := range res.EvidenceChunks {
			out.Evidence = append(out.Evidence, chunkDTO{Text: ch.Text, SourceURI: ch.SourceURI})
		}
		summary := fmt.Sprintf("recall: %d chunk(s), %d node(s), %d edge(s)", len(out.Chunks), len(out.Nodes), len(out.Edges))
		if out.ScopeFallback {
			summary += fmt.Sprintf(" — no results in %s; showing global memory", out.Scope)
		}
		return text(summary), out, nil
	}
}

type getNodeIn struct {
	Name    string `json:"name,omitempty" jsonschema:"the entity's canonical name (optionally scoped by project); or pass id instead"`
	ID      string `json:"id,omitempty" jsonschema:"the entity's node id (alternative to name)"`
	Project string `json:"project,omitempty" jsonschema:"scope a name lookup to this project, then fall back to global"`
	AsOf    string `json:"as_of,omitempty" jsonschema:"answer as of a past instant (RFC3339 or YYYY-MM-DD): only relationships live at that time — 'what did we think about X on date Y'"`
}
type getNodeOut struct {
	Found bool      `json:"found"`
	Node  *nodeDTO  `json:"node,omitempty"`
	Edges []edgeDTO `json:"edges"`
}

func getNodeTool(c *core.Core) mcp.ToolHandlerFor[getNodeIn, getNodeOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in getNodeIn) (*mcp.CallToolResult, getNodeOut, error) {
		var det *core.NodeDetail
		var err error
		if in.AsOf != "" {
			asOf, perr := parseAsOf(in.AsOf)
			if perr != nil {
				return nil, getNodeOut{}, perr
			}
			det, err = c.GetNodeAsOf(ctx, in.ID, in.Name, in.Project, asOf)
		} else {
			det, err = c.GetNode(ctx, in.ID, in.Name, in.Project)
		}
		if err != nil {
			return nil, getNodeOut{}, err
		}
		if det == nil {
			ref := in.Name
			if ref == "" {
				ref = in.ID
			}
			return text(fmt.Sprintf("no entity found for %q", ref)), getNodeOut{Found: false, Edges: []edgeDTO{}}, nil
		}
		n := det.Node
		dto := toNodeDTO(n)
		out := getNodeOut{
			Found: true,
			Node:  &dto,
			Edges: make([]edgeDTO, 0, len(det.Edges)),
		}
		for _, e := range det.Edges {
			out.Edges = append(out.Edges, edgeDTO{
				From: e.FromName, Type: e.Edge.Type, To: e.ToName,
				Why: e.Edge.Why, SourceURI: e.Edge.SourceURI, Status: string(e.Edge.Status),
			})
		}
		return text(fmt.Sprintf("%s [%s]: %d alias(es), %d edge(s)", n.CanonicalName, n.Type, len(n.Aliases), len(out.Edges))), out, nil
	}
}

type rollupIn struct {
	NodeID string `json:"node_id" jsonschema:"the hub entity's node id (from recall/get_node)"`
	Text   string `json:"text" jsonschema:"the 'current state of X' synthesis to record"`
}
type rollupOut struct {
	NodeID        string `json:"node_id"`
	CanonicalName string `json:"canonical_name"`
	Rollup        string `json:"rollup"`
}

func rollupTool(c *core.Core) mcp.ToolHandlerFor[rollupIn, rollupOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in rollupIn) (*mcp.CallToolResult, rollupOut, error) {
		node, err := c.Rollup(ctx, in.NodeID, in.Text)
		if err != nil {
			return nil, rollupOut{}, err
		}
		return text(fmt.Sprintf("rolled up %s", node.CanonicalName)),
			rollupOut{NodeID: node.ID, CanonicalName: node.CanonicalName, Rollup: node.Rollup}, nil
	}
}

func supersedeTool(c *core.Core) mcp.ToolHandlerFor[supersedeIn, supersedeOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in supersedeIn) (*mcp.CallToolResult, supersedeOut, error) {
		if err := c.Supersede(ctx, in.OldID, in.NewID, in.Why, in.Author); err != nil {
			return nil, supersedeOut{}, err
		}
		return text("superseded"), supersedeOut{OK: true}, nil
	}
}

func disambiguateTool(c *core.Core) mcp.ToolHandlerFor[disambiguateIn, disambiguateOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in disambiguateIn) (*mcp.CallToolResult, disambiguateOut, error) {
		node, err := c.Disambiguate(ctx, in.NodeID, core.Discriminators(in.Project, in.Discriminators))
		if err != nil {
			return nil, disambiguateOut{}, err
		}
		scope := model.ScopeKey(node.Discriminators)
		return text(fmt.Sprintf("re-scoped %q to %q", node.CanonicalName, scope)), disambiguateOut{NodeID: node.ID, ScopeKey: scope}, nil
	}
}

func addDocumentTool(c *core.Core) mcp.ToolHandlerFor[addDocumentIn, ingestOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in addDocumentIn) (*mcp.CallToolResult, ingestOut, error) {
		st, err := c.IngestText(ctx, in.SourceURI, in.Text, in.Project)
		if err != nil {
			return nil, ingestOut{}, err
		}
		out := ingestOut{
			Docs: st.Docs, Chunks: st.Chunks, Kept: st.Kept, Queued: st.Queued,
			Dropped: st.Dropped, Skipped: st.Skipped, Deleted: st.Deleted, Failed: st.Failed,
			ExtractedNodes: st.ExtractedNodes, ExtractedEdges: st.ExtractedEdges, ExtractFailed: st.ExtractFailed,
		}
		return text(fmt.Sprintf("stored %q: %d new chunk(s), %d skipped, %d dropped", in.SourceURI, st.Kept+st.Queued, st.Skipped, st.Dropped)), out, nil
	}
}

func ingestTool(importFn ImportFunc) mcp.ToolHandlerFor[ingestIn, ingestOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ingestIn) (*mcp.CallToolResult, ingestOut, error) {
		st, err := importFn(ctx, in.Source, in.Target, in.Project)
		if err != nil {
			return nil, ingestOut{}, err
		}
		out := ingestOut{
			Docs: st.Docs, Chunks: st.Chunks, Kept: st.Kept, Queued: st.Queued,
			Dropped: st.Dropped, Skipped: st.Skipped, Deleted: st.Deleted, Failed: st.Failed,
			ExtractedNodes: st.ExtractedNodes, ExtractedEdges: st.ExtractedEdges, ExtractFailed: st.ExtractFailed,
		}
		return text(fmt.Sprintf("ingested %d doc(s): %d new, %d skipped, %d dropped, %d deleted",
			st.Docs, st.Kept+st.Queued, st.Skipped, st.Dropped, st.Deleted)), out, nil
	}
}

func proposalsTool(c *core.Core) mcp.ToolHandlerFor[proposalsIn, proposalsOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in proposalsIn) (*mcp.CallToolResult, proposalsOut, error) {
		q, err := c.Proposals(ctx, in.Limit)
		if err != nil {
			return nil, proposalsOut{}, err
		}
		out := proposalsOut{
			Nodes: make([]proposalNodeDTO, 0, len(q.Nodes)),
			Edges: make([]proposalEdgeDTO, 0, len(q.Edges)),
		}
		for _, n := range q.Nodes {
			out.Nodes = append(out.Nodes, proposalNodeDTO{
				ID: n.ID, CanonicalName: n.CanonicalName, Type: n.Type, Scope: model.ScopeLabel(n.Discriminators),
			})
		}
		for _, e := range q.Edges {
			out.Edges = append(out.Edges, proposalEdgeDTO{
				ID: e.ID, From: e.FromName, Type: e.Type, To: e.ToName, Why: e.Why,
			})
		}
		return text(fmt.Sprintf("%d proposed node(s), %d proposed edge(s)", len(out.Nodes), len(out.Edges))), out, nil
	}
}

func reviewProposalTool(c *core.Core) mcp.ToolHandlerFor[reviewProposalIn, reviewProposalOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in reviewProposalIn) (*mcp.CallToolResult, reviewProposalOut, error) {
		var err error
		switch {
		case in.Kind == "node" && in.Approve:
			err = c.ApproveNode(ctx, in.ID)
		case in.Kind == "node":
			err = c.RejectNode(ctx, in.ID)
		case in.Kind == "edge" && in.Approve:
			err = c.ApproveEdge(ctx, in.ID)
		case in.Kind == "edge":
			err = c.RejectEdge(ctx, in.ID)
		default:
			return nil, reviewProposalOut{}, fmt.Errorf("kind must be 'node' or 'edge', got %q", in.Kind)
		}
		if err != nil {
			return nil, reviewProposalOut{}, err
		}
		verb := "approved"
		if !in.Approve {
			verb = "rejected"
		}
		return text(fmt.Sprintf("%s %s %s", verb, in.Kind, in.ID)), reviewProposalOut{OK: true}, nil
	}
}

// text builds a CallToolResult carrying a human-readable summary; the SDK also
// attaches the typed Out value as structured content.
func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
