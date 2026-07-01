package server

import (
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
