package mcpserver

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/plugins/density"
	"github.com/programmism/brainiac/internal/store"
)

type fakeEmbedder struct{}

func (fakeEmbedder) Dims() int { return 768 }
func (fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	v := make([]float32, 768)
	v[h.Sum32()%768] = 1
	return v, nil
}

func callTool[Out any](ctx context.Context, t *testing.T, cs *mcp.ClientSession, name string, args any) Out {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("tool %s returned error: %+v", name, res.Content)
	}
	var out Out
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %s output: %v", name, err)
	}
	return out
}

func TestMCPRoundTrip(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping MCP round-trip test")
	}
	ctx := context.Background()

	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE edges, nodes, chunks"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	server := New(core.New(pool, fakeEmbedder{}, density.New()), nil, nil)
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	// remember → link → recall through the MCP boundary.
	rem := callTool[rememberOut](ctx, t, cs, "remember", map[string]any{
		"canonical_name": "OrderService", "type": "service", "summary": "orders",
		"aliases": []string{"orders-svc"},
	})
	if !rem.Created || rem.NodeID == "" {
		t.Fatalf("remember: %+v", rem)
	}

	// The project arg scopes identity across the MCP boundary (#116): same name,
	// different projects → two distinct nodes.
	pa := callTool[rememberOut](ctx, t, cs, "remember", map[string]any{"canonical_name": "Config", "project": "alpha"})
	pb := callTool[rememberOut](ctx, t, cs, "remember", map[string]any{"canonical_name": "Config", "project": "beta"})
	if !pa.Created || !pb.Created || pa.NodeID == pb.NodeID {
		t.Fatalf("project scoping failed: alpha=%+v beta=%+v", pa, pb)
	}
	paAgain := callTool[rememberOut](ctx, t, cs, "remember", map[string]any{"canonical_name": "Config", "project": "alpha"})
	if paAgain.Created || paAgain.NodeID != pa.NodeID {
		t.Fatalf("same-project re-remember should match: %+v", paAgain)
	}

	callTool[linkOut](ctx, t, cs, "link", map[string]any{
		"from": "OrderService", "type": "writes_to", "to": "Postgres",
		"why": "orders persisted", "source_uri": "doc://orders", "author": "claude",
	})

	rec := callTool[recallOut](ctx, t, cs, "recall", map[string]any{"query": "orders"})
	var os *nodeDTO
	for i := range rec.Nodes {
		if rec.Nodes[i].CanonicalName == "OrderService" {
			os = &rec.Nodes[i]
		}
	}
	if os == nil {
		t.Fatalf("recall nodes missing OrderService: %+v", rec.Nodes)
	}
	// Node detail (type + aliases) must survive the MCP boundary, not just the name.
	if os.Type != "service" || !contains(os.Aliases, "orders-svc") {
		t.Fatalf("recall node OrderService missing type/aliases: %+v", *os)
	}
	found := false
	for _, e := range rec.Edges {
		if e.From == "OrderService" && e.To == "Postgres" && e.Type == "writes_to" {
			found = true
		}
	}
	if !found {
		t.Fatalf("recall edges missing writes_to: %+v", rec.Edges)
	}

	// get_node: direct lookup by name returns the full record + edges.
	gn := callTool[getNodeOut](ctx, t, cs, "get_node", map[string]any{"name": "OrderService"})
	if !gn.Found || gn.Node == nil {
		t.Fatalf("get_node OrderService not found: %+v", gn)
	}
	if gn.Node.Type != "service" || !contains(gn.Node.Aliases, "orders-svc") {
		t.Fatalf("get_node missing type/aliases: %+v", gn.Node)
	}
	gnEdge := false
	for _, e := range gn.Edges {
		if e.From == "OrderService" && e.To == "Postgres" && e.Type == "writes_to" {
			gnEdge = true
		}
	}
	if !gnEdge {
		t.Fatalf("get_node edges missing writes_to: %+v", gn.Edges)
	}
	// A missing entity is not-found, not an error.
	if miss := callTool[getNodeOut](ctx, t, cs, "get_node", map[string]any{"name": "NoSuchEntity"}); miss.Found {
		t.Fatalf("get_node should not find NoSuchEntity: %+v", miss)
	}

	// add_document: store text Claude "read elsewhere", then find it via search.
	const doc = "OrderService streams events to Kafka for durability and audit."
	add := callTool[ingestOut](ctx, t, cs, "add_document", map[string]any{
		"source_uri": "notion://wiki", "text": doc,
	})
	if add.Kept < 1 {
		t.Fatalf("add_document kept=%d, want >=1", add.Kept)
	}
	hits := callTool[searchOut](ctx, t, cs, "search", map[string]any{"query": doc})
	gotDoc := false
	for _, h := range hits.Chunks {
		if h.SourceURI == "notion://wiki" {
			gotDoc = true
		}
	}
	if !gotDoc {
		t.Fatalf("search did not return the added document: %+v", hits.Chunks)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestIngestToolCallsImporter(t *testing.T) {
	ctx := context.Background()
	var gotSource, gotTarget, gotProject string
	importFn := func(_ context.Context, source, target, project string) (core.IngestStats, error) {
		gotSource, gotTarget, gotProject = source, target, project
		return core.IngestStats{Docs: 1, Kept: 3}, nil
	}
	// core is unused by the ingest tool, so nil is fine here.
	server := New(nil, importFn, nil)
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	out := callTool[ingestOut](ctx, t, cs, "ingest", map[string]any{
		"source": "notion", "target": "https://notion.so/Team-Wiki-abc123def4567890abc123def4567890",
		"project": "wiki",
	})
	if out.Docs != 1 || out.Kept != 3 {
		t.Fatalf("ingest out = %+v", out)
	}
	if gotSource != "notion" || gotTarget == "" || gotProject != "wiki" {
		t.Fatalf("importer got source=%q target=%q project=%q", gotSource, gotTarget, gotProject)
	}
}
