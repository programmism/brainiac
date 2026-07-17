package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractParsesStructuredOutput(t *testing.T) {
	var gotReq messageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("missing/wrong api key: %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != apiVersion {
			t.Errorf("missing anthropic-version header")
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// The model returns the schema-shaped JSON as a single text block.
		inner := `{"entities":[{"name":"OrderService","type":"service","aliases":["orders"]},{"name":"","type":"x","aliases":[]}],` +
			`"relations":[{"from":"OrderService","type":"writes-to","to":"Kafka","why":"durability"},{"from":"","type":"","to":"","why":""}]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stop_reason": "end_turn",
			"content":     []map[string]any{{"type": "text", "text": inner}},
		})
	}))
	defer srv.Close()

	e := NewExtractor("sk-test", "", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	ext, err := e.Extract(context.Background(), "OrderService writes to Kafka for durability.")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Empty-name entity and empty-endpoint relation are dropped.
	if len(ext.Entities) != 1 || ext.Entities[0].Name != "OrderService" {
		t.Fatalf("entities = %+v, want just OrderService", ext.Entities)
	}
	if len(ext.Relations) != 1 || ext.Relations[0].To != "Kafka" || ext.Relations[0].Why != "durability" {
		t.Fatalf("relations = %+v", ext.Relations)
	}
	// Request carried the default model + structured-output format.
	if gotReq.Model != DefaultModel {
		t.Errorf("model = %q, want %q", gotReq.Model, DefaultModel)
	}
	if gotReq.OutputConfig.Format.Type != "json_schema" {
		t.Errorf("output format = %+v", gotReq.OutputConfig.Format)
	}
}

func TestExtractRefusalIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"stop_reason": "refusal", "content": []any{}})
	}))
	defer srv.Close()

	e := NewExtractor("sk-test", "", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()), WithRetries(1))
	if _, err := e.Extract(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "refusal") {
		t.Fatalf("expected refusal error, got %v", err)
	}
}

func TestExtractErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	e := NewExtractor("sk-test", "custom-model", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()), WithRetries(1))
	if _, err := e.Extract(context.Background(), "x"); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestModelOverride(t *testing.T) {
	if e := NewExtractor("k", ""); e.model != DefaultModel {
		t.Errorf("empty model should default to %q, got %q", DefaultModel, e.model)
	}
	if e := NewExtractor("k", "claude-haiku-4-5"); e.model != "claude-haiku-4-5" {
		t.Errorf("explicit model not honored: %q", e.model)
	}
}
