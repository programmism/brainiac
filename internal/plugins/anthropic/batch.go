package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/programmism/brainiac/internal/plugins"
)

// The Message Batches API cost path (#326): for a large backfill, submit all
// chunks as one async batch (~50% cheaper than one Messages request each), poll
// until it ends, then retrieve each result by custom_id. This is a distinct,
// opt-in capability from the synchronous Extract; wiring it into an async ingest
// job (and cross-doc entity resolution) is a follow-up — BatchExtract itself is
// pure request/response and fully httptest-covered.

// maxBatchPolls bounds the status polling so a stuck batch can't loop forever; at
// the default 10s interval that's ~1h, well under the API's 24h batch window (the
// caller's context deadline is the real bound).
const maxBatchPolls = 360

// BatchItem pairs a stable custom_id with the chunk text to extract.
type BatchItem struct {
	CustomID string
	Text     string
}

type batchRequest struct {
	Requests []batchRequestItem `json:"requests"`
}

type batchRequestItem struct {
	CustomID string         `json:"custom_id"`
	Params   messageRequest `json:"params"`
}

type batchStatus struct {
	ID               string `json:"id"`
	ProcessingStatus string `json:"processing_status"` // "in_progress" | "canceling" | "ended"
	ResultsURL       string `json:"results_url"`
}

type batchResultLine struct {
	CustomID string `json:"custom_id"`
	Result   struct {
		Type    string          `json:"type"` // "succeeded" | "errored" | "canceled" | "expired"
		Message messageResponse `json:"message"`
	} `json:"result"`
}

// BatchExtract submits all items as one Message Batch (#326), polls until it ends,
// and returns each item's Extraction keyed by its custom_id. Items whose result
// wasn't "succeeded" (or failed to parse) are absent from the map — the caller
// treats a missing custom_id as a failed extraction (graceful degradation, §11),
// exactly like Extract returning an error for a single chunk.
func (e *Extractor) BatchExtract(ctx context.Context, items []BatchItem) (map[string]plugins.Extraction, error) {
	out := make(map[string]plugins.Extraction, len(items))
	if len(items) == 0 {
		return out, nil
	}
	id, err := e.createBatch(ctx, items)
	if err != nil {
		return nil, err
	}
	resultsURL, err := e.pollBatch(ctx, id)
	if err != nil {
		return nil, err
	}
	return e.fetchResults(ctx, resultsURL)
}

func (e *Extractor) createBatch(ctx context.Context, items []BatchItem) (string, error) {
	reqs := make([]batchRequestItem, len(items))
	for i, it := range items {
		reqs[i] = batchRequestItem{CustomID: it.CustomID, Params: e.buildRequest(it.Text)}
	}
	payload, err := json.Marshal(batchRequest{Requests: reqs})
	if err != nil {
		return "", err
	}
	var st batchStatus
	if err := e.doJSON(ctx, http.MethodPost, e.baseURL+"/v1/messages/batches", payload, &st); err != nil {
		return "", fmt.Errorf("create batch: %w", err)
	}
	if st.ID == "" {
		return "", fmt.Errorf("create batch: empty batch id in response")
	}
	return st.ID, nil
}

func (e *Extractor) pollBatch(ctx context.Context, id string) (string, error) {
	url := fmt.Sprintf("%s/v1/messages/batches/%s", e.baseURL, id)
	for i := 0; i < maxBatchPolls; i++ {
		var st batchStatus
		if err := e.doJSON(ctx, http.MethodGet, url, nil, &st); err != nil {
			return "", fmt.Errorf("poll batch %s: %w", id, err)
		}
		if st.ProcessingStatus == "ended" {
			if st.ResultsURL == "" {
				return "", fmt.Errorf("batch %s ended with no results_url", id)
			}
			return st.ResultsURL, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(e.batchPollIvl):
		}
	}
	return "", fmt.Errorf("batch %s did not end after %d polls", id, maxBatchPolls)
}

func (e *Extractor) fetchResults(ctx context.Context, resultsURL string) (map[string]plugins.Extraction, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resultsURL, nil)
	if err != nil {
		return nil, err
	}
	e.setHeaders(req)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch batch results: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("batch results: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	out := map[string]plugins.Extraction{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // results lines can be large
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var res batchResultLine
		if err := json.Unmarshal(line, &res); err != nil {
			continue // skip a malformed line rather than fail the whole batch
		}
		if res.Result.Type != "succeeded" {
			continue // errored/canceled/expired → caller treats as failed extraction
		}
		ext, err := e.parseMessage(res.Result.Message)
		if err != nil {
			continue
		}
		out[res.CustomID] = ext
	}
	return out, sc.Err()
}

// doJSON performs an authenticated request with a JSON body (nil for GET) and
// decodes the JSON response into out.
func (e *Extractor) doJSON(ctx context.Context, method, url string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	e.setHeaders(req)
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("anthropic request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (e *Extractor) setHeaders(req *http.Request) {
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", e.apiKey)
	req.Header.Set("anthropic-version", apiVersion)
}
