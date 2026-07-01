package server

import (
	"bytes"
	"context"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/model"
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

func TestAPISearchHealthAndErrors(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping REST API test")
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

	emb, _ := fakeEmbedder{}.Embed(ctx, "alpha")
	if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: "alpha doc", Embedding: emb, SourceURI: "u/a"}); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}

	c := core.New(pool, fakeEmbedder{}, density.New())
	srv := httptest.NewServer(New(pool, nil, c))
	defer srv.Close()

	// /api/search
	var hits []struct {
		Text      string `json:"text"`
		SourceURI string `json:"source_uri"`
	}
	if code := getJSON(t, srv.URL+"/api/search?q=alpha", &hits); code != http.StatusOK {
		t.Fatalf("search status %d", code)
	}
	if len(hits) == 0 || hits[0].Text != "alpha doc" {
		t.Fatalf("search hits = %+v", hits)
	}

	// missing q → 400
	if code := getJSON(t, srv.URL+"/api/search", nil); code != http.StatusBadRequest {
		t.Fatalf("missing q status = %d, want 400", code)
	}

	// /api/health
	var health map[string]any
	if code := getJSON(t, srv.URL+"/api/health", &health); code != http.StatusOK {
		t.Fatalf("health status %d", code)
	}
	if health["chunks_hot"].(float64) < 1 {
		t.Fatalf("health chunks_hot = %v, want >= 1", health["chunks_hot"])
	}
}

func TestAPIConsolidateAndMerge(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping consolidation API test")
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

	c := core.New(pool, fakeEmbedder{}, density.New())
	if _, err := c.Remember(ctx, core.RememberInput{CanonicalName: "OrderService"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := c.Remember(ctx, core.RememberInput{CanonicalName: "Order Service"}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	srv := httptest.NewServer(New(pool, nil, c))
	defer srv.Close()

	var rep struct {
		MergeGroups [][]struct {
			ID   string `json:"id"`
			Name string `json:"canonical_name"`
		} `json:"merge_groups"`
	}
	if code := getJSON(t, srv.URL+"/api/consolidate", &rep); code != http.StatusOK {
		t.Fatalf("consolidate status %d", code)
	}
	if len(rep.MergeGroups) != 1 || len(rep.MergeGroups[0]) != 2 {
		t.Fatalf("merge groups = %+v", rep.MergeGroups)
	}

	body, _ := json.Marshal(map[string]string{"Keep": rep.MergeGroups[0][0].ID, "Drop": rep.MergeGroups[0][1].ID})
	resp, err := http.Post(srv.URL+"/api/merge", "application/json", bytes.NewReader(body)) //nolint:noctx // test
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("merge: err=%v code=%v", err, resp.StatusCode)
	}
	_ = resp.Body.Close()

	// After the merge, no candidates remain.
	var rep2 struct {
		MergeGroups [][]any `json:"merge_groups"`
	}
	getJSON(t, srv.URL+"/api/consolidate", &rep2)
	if len(rep2.MergeGroups) != 0 {
		t.Fatalf("after merge, groups = %d, want 0", len(rep2.MergeGroups))
	}
}

func TestAPIGraph(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping graph API test")
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

	c := core.New(pool, fakeEmbedder{}, density.New())
	if _, err := c.Link(ctx, core.LinkInput{From: "A", Type: "depends_on", To: "B", Why: "x"}); err != nil {
		t.Fatalf("link: %v", err)
	}

	srv := httptest.NewServer(New(pool, nil, c))
	defer srv.Close()

	var g struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	if code := getJSON(t, srv.URL+"/api/graph", &g); code != http.StatusOK {
		t.Fatalf("graph status %d", code)
	}
	if len(g.Nodes) != 2 || len(g.Edges) != 1 {
		t.Fatalf("graph = %d nodes, %d edges; want 2, 1", len(g.Nodes), len(g.Edges))
	}
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		body, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("decode %s: %v (%s)", url, err, body)
		}
	}
	return resp.StatusCode
}
