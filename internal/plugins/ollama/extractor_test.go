package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "llama3.1" || req.Stream {
			t.Errorf("bad request payload: %+v", req)
		}
		if req.Format == nil {
			t.Errorf("expected a structured-output format schema on the request")
		}
		if len(req.Messages) != 2 || req.Messages[1].Content != "A depends on B" {
			t.Errorf("bad messages: %+v", req.Messages)
		}
		// The model returns its answer as a JSON string in message.content.
		content := `{"entities":[{"name":"A","type":"service","aliases":["a-svc"]},{"name":"B"}],` +
			`"relations":[{"from":"A","type":"depends on","to":"B","why":"startup order"}]}`
		_ = json.NewEncoder(w).Encode(chatResponse{Message: chatMessage{Role: "assistant", Content: content}})
	}))
	defer srv.Close()

	e := NewExtractor(srv.URL, "llama3.1")
	ext, err := e.Extract(context.Background(), "A depends on B")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(ext.Entities) != 2 {
		t.Fatalf("entities = %d, want 2", len(ext.Entities))
	}
	if ext.Entities[0].Name != "A" || ext.Entities[0].Type != "service" || len(ext.Entities[0].Aliases) != 1 {
		t.Errorf("bad entity[0]: %+v", ext.Entities[0])
	}
	if len(ext.Relations) != 1 {
		t.Fatalf("relations = %d, want 1", len(ext.Relations))
	}
	r := ext.Relations[0]
	if r.From != "A" || r.To != "B" || r.Type != "depends on" || r.Why != "startup order" {
		t.Errorf("bad relation: %+v", r)
	}
}

func TestExtractDropsIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// An unnamed entity and a relation missing its "to" endpoint must be dropped.
		content := `{"entities":[{"name":""},{"name":"Kept"}],` +
			`"relations":[{"from":"A","type":"uses","to":""},{"from":"A","type":"uses","to":"B"}]}`
		_ = json.NewEncoder(w).Encode(chatResponse{Message: chatMessage{Content: content}})
	}))
	defer srv.Close()

	e := NewExtractor(srv.URL, "llama3.1")
	ext, err := e.Extract(context.Background(), "x")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(ext.Entities) != 1 || ext.Entities[0].Name != "Kept" {
		t.Errorf("expected only the named entity, got %+v", ext.Entities)
	}
	if len(ext.Relations) != 1 || ext.Relations[0].To != "B" {
		t.Errorf("expected only the complete relation, got %+v", ext.Relations)
	}
}

func TestExtractErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := NewExtractor(srv.URL, "llama3.1", WithExtractorRetries(1))
	if _, err := e.Extract(context.Background(), "x"); err == nil {
		t.Fatal("expected an error on non-200 status")
	}
}

func TestExtractBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(chatResponse{Message: chatMessage{Content: "not json"}})
	}))
	defer srv.Close()

	e := NewExtractor(srv.URL, "llama3.1", WithExtractorRetries(1))
	if _, err := e.Extract(context.Background(), "x"); err == nil {
		t.Fatal("expected an error when the model returns non-JSON content")
	}
}
