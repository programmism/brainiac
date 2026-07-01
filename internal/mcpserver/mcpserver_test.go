package mcpserver

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/programmism/brainiac/internal/core"
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

	server := New(core.New(pool, fakeEmbedder{}, nil))
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
	})
	if !rem.Created || rem.NodeID == "" {
		t.Fatalf("remember: %+v", rem)
	}

	callTool[linkOut](ctx, t, cs, "link", map[string]any{
		"from": "OrderService", "type": "writes_to", "to": "Postgres",
		"why": "orders persisted", "source_uri": "doc://orders", "author": "claude",
	})

	rec := callTool[recallOut](ctx, t, cs, "recall", map[string]any{"query": "orders"})
	if !contains(rec.Nodes, "OrderService") {
		t.Fatalf("recall nodes missing OrderService: %+v", rec.Nodes)
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
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
