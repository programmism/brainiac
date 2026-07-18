package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatchExtract(t *testing.T) {
	var created batchRequest
	var polls int32

	mux := http.NewServeMux()
	var baseURL string

	inner := `{"entities":[{"name":"OrderService","type":"service","aliases":["orders"]}],` +
		`"relations":[{"from":"OrderService","type":"writes-to","to":"Kafka","why":"durability"}]}`

	mux.HandleFunc("/v1/messages/batches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("create: method %s", r.Method)
		}
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("create: missing api key")
		}
		if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
			t.Errorf("create decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(batchStatus{ID: "batch_1", ProcessingStatus: "in_progress"})
	})
	mux.HandleFunc("/v1/messages/batches/batch_1", func(w http.ResponseWriter, _ *http.Request) {
		// in_progress on the first poll, ended (with results_url) on the second.
		if atomic.AddInt32(&polls, 1) < 2 {
			_ = json.NewEncoder(w).Encode(batchStatus{ID: "batch_1", ProcessingStatus: "in_progress"})
			return
		}
		_ = json.NewEncoder(w).Encode(batchStatus{ID: "batch_1", ProcessingStatus: "ended", ResultsURL: baseURL + "/results"})
	})
	mux.HandleFunc("/results", func(w http.ResponseWriter, _ *http.Request) {
		// JSONL: c1 succeeded, c2 errored (absent from the returned map).
		ok := fmt.Sprintf(`{"custom_id":"c1","result":{"type":"succeeded","message":{"stop_reason":"end_turn","content":[{"type":"text","text":%q}]}}}`, inner)
		bad := `{"custom_id":"c2","result":{"type":"errored"}}`
		fmt.Fprint(w, ok+"\n"+bad+"\n")
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	baseURL = srv.URL

	e := NewExtractor("sk-test", "", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()), WithBatchPollInterval(time.Millisecond))
	got, err := e.BatchExtract(context.Background(), []BatchItem{
		{CustomID: "c1", Text: "OrderService writes to Kafka for durability."},
		{CustomID: "c2", Text: "some chunk that errors"},
	})
	if err != nil {
		t.Fatalf("batch extract: %v", err)
	}

	// The batch carried both requests, each with the structured-output format.
	if len(created.Requests) != 2 || created.Requests[0].CustomID != "c1" {
		t.Fatalf("batch requests = %+v", created.Requests)
	}
	if created.Requests[0].Params.OutputConfig.Format.Type != "json_schema" {
		t.Fatalf("batch request lost structured-output format: %+v", created.Requests[0].Params.OutputConfig)
	}
	// Polling actually waited for "ended".
	if atomic.LoadInt32(&polls) < 2 {
		t.Fatalf("expected >=2 polls (in_progress then ended), got %d", polls)
	}
	// c1 succeeded → parsed; c2 errored → absent.
	if len(got) != 1 {
		t.Fatalf("results = %v, want just c1", keysMap(got))
	}
	ext, ok := got["c1"]
	if !ok || len(ext.Entities) != 1 || ext.Entities[0].Name != "OrderService" {
		t.Fatalf("c1 extraction = %+v", ext)
	}
	if len(ext.Relations) != 1 || ext.Relations[0].To != "Kafka" || ext.Relations[0].Why != "durability" {
		t.Fatalf("c1 relations = %+v", ext.Relations)
	}
	if _, present := got["c2"]; present {
		t.Fatal("errored item c2 should be absent from the results")
	}
}

// TestCreateAndFetchBatch covers the async split (#383): CreateBatch returns the
// provider id without waiting, and FetchBatchResults is non-blocking — (nil,false)
// while processing, then (results,true) once ended.
func TestCreateAndFetchBatch(t *testing.T) {
	var polls int32
	inner := `{"entities":[{"name":"Cache","type":"component","aliases":[]}],"relations":[]}`

	mux := http.NewServeMux()
	var baseURL string
	mux.HandleFunc("/v1/messages/batches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("create: method %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(batchStatus{ID: "batch_9", ProcessingStatus: "in_progress"})
	})
	mux.HandleFunc("/v1/messages/batches/batch_9", func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&polls, 1) < 2 {
			_ = json.NewEncoder(w).Encode(batchStatus{ID: "batch_9", ProcessingStatus: "in_progress"})
			return
		}
		_ = json.NewEncoder(w).Encode(batchStatus{ID: "batch_9", ProcessingStatus: "ended", ResultsURL: baseURL + "/r"})
	})
	mux.HandleFunc("/r", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"custom_id":"c1","result":{"type":"succeeded","message":{"stop_reason":"end_turn","content":[{"type":"text","text":%q}]}}}`+"\n", inner)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	baseURL = srv.URL

	e := NewExtractor("sk-test", "", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	id, err := e.CreateBatch(context.Background(), []BatchItem{{CustomID: "c1", Text: "the cache warms on boot"}})
	if err != nil || id != "batch_9" {
		t.Fatalf("create batch = (%q, %v)", id, err)
	}
	// First check: still processing → non-blocking, no results.
	res, ended, err := e.FetchBatchResults(context.Background(), id)
	if err != nil || ended || res != nil {
		t.Fatalf("first fetch = (%v, %v, %v), want (nil,false,nil)", res, ended, err)
	}
	// Second check: ended → results returned.
	res, ended, err = e.FetchBatchResults(context.Background(), id)
	if err != nil || !ended {
		t.Fatalf("second fetch = (ended %v, %v)", ended, err)
	}
	if ext, ok := res["c1"]; !ok || len(ext.Entities) != 1 || ext.Entities[0].Name != "Cache" {
		t.Fatalf("c1 result = %+v", res["c1"])
	}
}

func TestBatchExtractEmpty(t *testing.T) {
	e := NewExtractor("sk-test", "")
	got, err := e.BatchExtract(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty batch = (%v, %v), want ({}, nil)", got, err)
	}
}

func keysMap[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
