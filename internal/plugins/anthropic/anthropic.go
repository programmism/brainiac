// Package anthropic implements the plugins.Extractor seam against the Claude
// Messages API (#235): it turns a text chunk into knowledge-graph nodes/edges by
// prompting Claude with a JSON-schema structured output. It is the strong
// server-side extractor for bulk ingest — higher quality than the local-LLM path
// (internal/plugins/ollama) — for a deployment that has an ANTHROPIC_API_KEY and
// wants automated extraction without a human in the loop for every chunk.
//
// Kept to raw HTTP (no SDK dependency) in keeping with the project's
// minimal-dependency stance (SYSTEM.md §3), mirroring the hand-rolled Ollama
// client. Batch API cost optimization and cross-document entity resolution are
// tracked as follow-ups.
package anthropic

import (
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

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	// DefaultModel is the extraction model when none is configured. Opus is the
	// most capable tier; an operator may point Extraction.Model at a cheaper model
	// (e.g. claude-haiku-4-5) for bulk cost.
	DefaultModel = "claude-opus-4-8"
	// DefaultRetries bounds transient-failure attempts per chunk.
	DefaultRetries = 3
	maxTokens      = 4096
)

// Extractor turns a text chunk into graph nodes/edges by prompting the Claude
// Messages API with a JSON-schema structured output.
type Extractor struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
	retries int
}

// Option customizes an Extractor.
type Option func(*Extractor)

// WithHTTPClient overrides the default HTTP client (useful in tests).
func WithHTTPClient(c *http.Client) Option { return func(e *Extractor) { e.client = c } }

// WithBaseURL overrides the API base URL (used in tests).
func WithBaseURL(u string) Option {
	return func(e *Extractor) { e.baseURL = strings.TrimRight(u, "/") }
}

// WithRetries sets how many attempts Extract makes on transient failures (<=0
// keeps the default).
func WithRetries(n int) Option {
	return func(e *Extractor) {
		if n > 0 {
			e.retries = n
		}
	}
}

// NewExtractor builds a Claude-backed Extractor. An empty model uses DefaultModel.
func NewExtractor(apiKey, model string, opts ...Option) *Extractor {
	if model == "" {
		model = DefaultModel
	}
	e := &Extractor{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		model:   model,
		client:  &http.Client{Timeout: 2 * time.Minute},
		retries: DefaultRetries,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

var _ plugins.Extractor = (*Extractor)(nil)

// Extract prompts Claude and returns the structured Extraction. A persistent
// failure is returned so ingest can count the chunk as failed extraction and keep
// going (the chunk itself is still stored) — graceful degradation (§11).
func (e *Extractor) Extract(ctx context.Context, chunk string) (plugins.Extraction, error) {
	var ext plugins.Extraction
	err := e.withRetries(ctx, func() error {
		v, err := e.extractOnce(ctx, chunk)
		if err != nil {
			return err
		}
		ext = v
		return nil
	})
	return ext, err
}

func (e *Extractor) withRetries(ctx context.Context, fn func() error) error {
	attempts := e.retries
	if attempts < 1 {
		attempts = 1
	}
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		if err := fn(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

type messageRequest struct {
	Model        string       `json:"model"`
	MaxTokens    int          `json:"max_tokens"`
	System       string       `json:"system"`
	Messages     []message    `json:"messages"`
	OutputConfig outputConfig `json:"output_config"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type outputConfig struct {
	Format outputFormat `json:"format"`
}

type outputFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

type messageResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

func (e *Extractor) extractOnce(ctx context.Context, chunk string) (plugins.Extraction, error) {
	payload, err := json.Marshal(messageRequest{
		Model:        e.model,
		MaxTokens:    maxTokens,
		System:       extractSystemPrompt,
		Messages:     []message{{Role: "user", Content: chunk}},
		OutputConfig: outputConfig{Format: outputFormat{Type: "json_schema", Schema: extractionSchema}},
	})
	if err != nil {
		return plugins.Extraction{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return plugins.Extraction{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", e.apiKey)
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := e.client.Do(req)
	if err != nil {
		return plugins.Extraction{}, fmt.Errorf("anthropic messages request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return plugins.Extraction{}, fmt.Errorf("anthropic messages: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out messageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return plugins.Extraction{}, fmt.Errorf("decode anthropic response: %w", err)
	}
	if out.StopReason == "refusal" {
		return plugins.Extraction{}, fmt.Errorf("anthropic declined to extract this chunk (stop_reason refusal)")
	}
	text := firstText(out)
	if text == "" {
		return plugins.Extraction{}, fmt.Errorf("anthropic returned no text content")
	}
	var raw rawExtraction
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return plugins.Extraction{}, fmt.Errorf("parse extraction JSON from model %q: %w", e.model, err)
	}
	return toExtraction(raw), nil
}

// firstText returns the first text content block's text, joining any additional
// text blocks (structured output normally yields one).
func firstText(out messageResponse) string {
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// extractionSchema constrains Claude's output to the shape plugins.Extraction
// needs. additionalProperties:false is required for Anthropic structured outputs.
var extractionSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"entities": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"name":    map[string]any{"type": "string"},
					"type":    map[string]any{"type": "string"},
					"aliases": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"name", "type", "aliases"},
			},
		},
		"relations": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"from": map[string]any{"type": "string"},
					"type": map[string]any{"type": "string"},
					"to":   map[string]any{"type": "string"},
					"why":  map[string]any{"type": "string"},
				},
				"required": []string{"from", "type", "to", "why"},
			},
		},
	},
	"required": []string{"entities", "relations"},
}

const extractSystemPrompt = `You extract a knowledge graph from a text chunk for an engineering-team memory.
Return entities (people, services, systems, decisions, concepts) and the relations between them.
For every relation, put the rationale ("why") in the why field when the text gives one — this memory
records decisions, not just facts. Use names exactly as they appear. Do not invent facts not supported
by the text. If the chunk carries no durable knowledge, return empty arrays.`

// rawExtraction matches extractionSchema; decoded from the model's JSON text.
type rawExtraction struct {
	Entities []struct {
		Name    string   `json:"name"`
		Type    string   `json:"type"`
		Aliases []string `json:"aliases"`
	} `json:"entities"`
	Relations []struct {
		From string `json:"from"`
		Type string `json:"type"`
		To   string `json:"to"`
		Why  string `json:"why"`
	} `json:"relations"`
}

// toExtraction maps the schema-shaped result to plugins types, dropping entities
// and relations that lack the fields the graph needs (no name; missing endpoint
// or type) rather than storing garbage — same discipline as the Ollama extractor.
func toExtraction(raw rawExtraction) plugins.Extraction {
	var ext plugins.Extraction
	for _, en := range raw.Entities {
		name := strings.TrimSpace(en.Name)
		if name == "" {
			continue
		}
		ext.Entities = append(ext.Entities, plugins.Entity{
			Name:    name,
			Type:    strings.TrimSpace(en.Type),
			Aliases: en.Aliases,
		})
	}
	for _, r := range raw.Relations {
		from, to, typ := strings.TrimSpace(r.From), strings.TrimSpace(r.To), strings.TrimSpace(r.Type)
		if from == "" || to == "" || typ == "" {
			continue
		}
		ext.Relations = append(ext.Relations, plugins.Relation{From: from, Type: typ, To: to, Why: strings.TrimSpace(r.Why)})
	}
	return ext
}
