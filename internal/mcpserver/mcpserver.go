// Package mcpserver is the MCP client — a thin adapter that exposes the core
// operations as MCP tools for Claude. It holds no business logic: each tool
// marshals arguments, forwards to internal/core, and renders the result
// (SYSTEM.md §2, §6.1).
package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/programmism/brainiac/internal/core"
)

// ImportFunc runs an ingest for a source ("notion"|"markdown") and optional
// target (a Notion page URL/id or a path). It is supplied by the app wiring so
// the core/mcp layers stay plugin-agnostic; nil disables the ingest tool.
type ImportFunc func(ctx context.Context, source, target string) (core.IngestStats, error)

// New builds an MCP server exposing search/remember/link/recall/supersede (and
// ingest, if importFn is set) over the given core.
func New(c *core.Core, importFn ImportFunc) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "brainiac", Version: core.Version}, nil)

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
		Name:        "supersede",
		Description: "Record that a new entity replaces an old one: adds a supersedes link and marks the old one historical (kept, not deleted).",
	}, supersedeTool(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "add_document",
		Description: "Store a document's text into the searchable memory (chunked + embedded). Use when you've read content elsewhere — e.g. a Notion page via your own integration, or a web page — and want it searchable/recall-able later. Give a stable source_uri (the page URL) for citation; re-adding the same source_uri updates it.",
	}, addDocumentTool(c))

	if importFn != nil {
		mcp.AddTool(s, &mcp.Tool{
			Name:        "ingest",
			Description: "Import documents into the memory. source is 'notion' or 'markdown'; target is a Notion page URL/id or a path (empty = the whole source). Use this when the user shares a doc link or asks to import.",
		}, ingestTool(importFn))
	}

	return s
}

// --- DTOs ---

type chunkDTO struct {
	Text      string  `json:"text"`
	SourceURI string  `json:"source_uri"`
	Distance  float64 `json:"distance,omitempty"`
}

type searchIn struct {
	Query string `json:"query" jsonschema:"the search query"`
	K     int    `json:"k,omitempty" jsonschema:"maximum number of results (default 10)"`
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
	CanonicalName string   `json:"canonical_name" jsonschema:"the entity's canonical name"`
	Type          string   `json:"type,omitempty" jsonschema:"node type: service, datastore, decision, constraint, team, person, ..."`
	Aliases       []string `json:"aliases,omitempty" jsonschema:"alternative surface forms"`
	Summary       string   `json:"summary,omitempty" jsonschema:"short description; embedded for semantic dedup"`
	Project       string   `json:"project,omitempty" jsonschema:"the project this entity belongs to (scopes identity so same-named entities in different projects stay distinct); omit for universal/global entities like a vendor or standard"`
}
type rememberOut struct {
	NodeID        string   `json:"node_id"`
	CanonicalName string   `json:"canonical_name"`
	Created       bool     `json:"created"`
	Duplicates    []dupDTO `json:"duplicates,omitempty"`
}

type linkIn struct {
	From      string `json:"from" jsonschema:"source entity canonical name"`
	Type      string `json:"type" jsonschema:"relationship type: writes_to, depends_on, rejected, supersedes, ..."`
	To        string `json:"to" jsonschema:"target entity canonical name"`
	Why       string `json:"why" jsonschema:"the rationale — why it is this way"`
	SourceURI string `json:"source_uri,omitempty" jsonschema:"provenance: file, PR, page, thread"`
	Author    string `json:"author,omitempty"`
	Project   string `json:"project,omitempty" jsonschema:"the project both endpoints belong to (scopes their identity); omit for universal/global entities"`
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
type recallIn struct {
	Query string `json:"query" jsonschema:"the question to answer"`
}
type recallOut struct {
	Chunks   []chunkDTO `json:"chunks"`
	Nodes    []string   `json:"nodes"`
	Edges    []edgeDTO  `json:"edges"`
	Evidence []chunkDTO `json:"evidence"`
}

type addDocumentIn struct {
	SourceURI string `json:"source_uri" jsonschema:"stable identifier for citation, e.g. the page URL"`
	Text      string `json:"text" jsonschema:"the document's text content to store"`
}

type ingestIn struct {
	Source string `json:"source" jsonschema:"where to import from: notion or markdown"`
	Target string `json:"target,omitempty" jsonschema:"a Notion page URL/id, or a path; empty imports the whole source"`
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
}

type supersedeIn struct {
	OldID  string `json:"old_id" jsonschema:"id of the node being replaced"`
	NewID  string `json:"new_id" jsonschema:"id of the replacement node"`
	Why    string `json:"why" jsonschema:"why the change was made"`
	Author string `json:"author,omitempty"`
}
type supersedeOut struct {
	OK bool `json:"ok"`
}

// --- handlers ---

func searchTool(c *core.Core) mcp.ToolHandlerFor[searchIn, searchOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
		hits, err := c.Search(ctx, in.Query, in.K)
		if err != nil {
			return nil, searchOut{}, err
		}
		out := searchOut{Chunks: make([]chunkDTO, 0, len(hits))}
		for _, h := range hits {
			out.Chunks = append(out.Chunks, chunkDTO{Text: h.Text, SourceURI: h.SourceURI, Distance: h.Distance})
		}
		return text(fmt.Sprintf("found %d chunk(s)", len(out.Chunks))), out, nil
	}
}

func rememberTool(c *core.Core) mcp.ToolHandlerFor[rememberIn, rememberOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in rememberIn) (*mcp.CallToolResult, rememberOut, error) {
		r, err := c.Remember(ctx, core.RememberInput{
			CanonicalName: in.CanonicalName, Type: in.Type, Aliases: in.Aliases, Summary: in.Summary,
			Discriminators: projectScope(in.Project),
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
			Discriminators: projectScope(in.Project),
		})
		if err != nil {
			return nil, linkOut{}, err
		}
		return text(fmt.Sprintf("linked %s -%s-> %s", in.From, in.Type, in.To)), linkOut{EdgeID: edge.ID}, nil
	}
}

func recallTool(c *core.Core) mcp.ToolHandlerFor[recallIn, recallOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in recallIn) (*mcp.CallToolResult, recallOut, error) {
		res, err := c.Recall(ctx, in.Query)
		if err != nil {
			return nil, recallOut{}, err
		}
		out := recallOut{
			Chunks:   make([]chunkDTO, 0, len(res.Chunks)),
			Nodes:    make([]string, 0, len(res.Nodes)),
			Edges:    make([]edgeDTO, 0, len(res.Edges)),
			Evidence: make([]chunkDTO, 0, len(res.EvidenceChunks)),
		}
		for _, h := range res.Chunks {
			out.Chunks = append(out.Chunks, chunkDTO{Text: h.Text, SourceURI: h.SourceURI, Distance: h.Distance})
		}
		for _, n := range res.Nodes {
			out.Nodes = append(out.Nodes, n.CanonicalName)
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
		return text(fmt.Sprintf("recall: %d chunk(s), %d node(s), %d edge(s)", len(out.Chunks), len(out.Nodes), len(out.Edges))), out, nil
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

func addDocumentTool(c *core.Core) mcp.ToolHandlerFor[addDocumentIn, ingestOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in addDocumentIn) (*mcp.CallToolResult, ingestOut, error) {
		st, err := c.IngestText(ctx, in.SourceURI, in.Text)
		if err != nil {
			return nil, ingestOut{}, err
		}
		out := ingestOut{
			Docs: st.Docs, Chunks: st.Chunks, Kept: st.Kept, Queued: st.Queued,
			Dropped: st.Dropped, Skipped: st.Skipped, Deleted: st.Deleted, Failed: st.Failed,
		}
		return text(fmt.Sprintf("stored %q: %d new chunk(s), %d skipped, %d dropped", in.SourceURI, st.Kept+st.Queued, st.Skipped, st.Dropped)), out, nil
	}
}

func ingestTool(importFn ImportFunc) mcp.ToolHandlerFor[ingestIn, ingestOut] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ingestIn) (*mcp.CallToolResult, ingestOut, error) {
		st, err := importFn(ctx, in.Source, in.Target)
		if err != nil {
			return nil, ingestOut{}, err
		}
		out := ingestOut{
			Docs: st.Docs, Chunks: st.Chunks, Kept: st.Kept, Queued: st.Queued,
			Dropped: st.Dropped, Skipped: st.Skipped, Deleted: st.Deleted, Failed: st.Failed,
		}
		return text(fmt.Sprintf("ingested %d doc(s): %d new, %d skipped, %d dropped, %d deleted",
			st.Docs, st.Kept+st.Queued, st.Skipped, st.Dropped, st.Deleted)), out, nil
	}
}

// text builds a CallToolResult carrying a human-readable summary; the SDK also
// attaches the typed Out value as structured content.
func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// projectScope turns an agent-supplied project name into the identity
// discriminator set. Empty project = nil = global/shared identity (#116).
func projectScope(project string) map[string]string {
	if project == "" {
		return nil
	}
	return map[string]string{"project": project}
}
