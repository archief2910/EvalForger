// Package model holds the storage-shaped domain types shared by the API,
// worker and queue packages. It deliberately contains no database or HTTP code.
package model

import (
	"encoding/json"
	"time"
)

// SpanEvent is one span as accepted by the native ingest endpoint.
type SpanEvent struct {
	ID               string      `json:"id,omitempty"`
	TraceID          string      `json:"trace_id,omitempty"`
	ParentID         string      `json:"parent_id,omitempty"`
	Name             string      `json:"name"`
	Type             string      `json:"type"`
	Model            string      `json:"model,omitempty"`
	Input            interface{} `json:"input,omitempty"`
	Output           interface{} `json:"output,omitempty"`
	Error            interface{} `json:"error,omitempty"`
	Metadata         interface{} `json:"metadata,omitempty"`
	StartMS          int64       `json:"start_ms,omitempty"`
	EndMS            int64       `json:"end_ms,omitempty"`
	PromptTokens     int         `json:"prompt_tokens,omitempty"`
	CompletionTokens int         `json:"completion_tokens,omitempty"`
	TokenSource      string      `json:"token_source,omitempty"`
}

// IngestInput is the body of POST /projects/{id}/spans:ingest.
type IngestInput struct {
	Events []SpanEvent `json:"events"`
}

// SpanRow is the storage-shaped span shared by both ingest paths.
type SpanRow struct {
	ID, TraceID, ParentID, Name, Type, Model string
	Input, Output, Error, Metadata           interface{}
	StartMS, EndMS, LatencyMS                int64
	PromptTokens, CompletionTokens           int
	CostUSD                                  float64
	Status                                   string
	// TokenSource is "provider" when the caller reported usage, "estimated"
	// when this server estimated it, and "" when the span has no LLM usage.
	TokenSource string
}

// EvalInput is persisted in jobs.payload and accepted by the evaluations API.
type EvalInput struct {
	DatasetID            string   `json:"dataset_id"`
	ExperimentName       string   `json:"experiment_name"`
	ScorerNames          []string `json:"scorer_names"`
	BaselineExperimentID string   `json:"baseline_experiment_id"`

	// DatasetVersionID pins the immutable snapshot the experiment evaluates.
	DatasetVersionID string `json:"dataset_version_id,omitempty"`

	// PromptName/PromptVersion optionally pin the prompt template. The
	// resolved PromptVersionID is what the worker reads, so renaming a prompt
	// later cannot change a running experiment.
	PromptName      string `json:"prompt_name,omitempty"`
	PromptVersion   int    `json:"prompt_version,omitempty"`
	PromptVersionID string `json:"prompt_version_id,omitempty"`
}

// DatasetRecord is one row in a dataset snapshot.
type DatasetRecord struct {
	ID        string      `json:"id"`
	Input     interface{} `json:"input"`
	Expected  interface{} `json:"expected,omitempty"`
	Metadata  interface{} `json:"metadata,omitempty"`
	Tags      []string    `json:"tags"`
	Source    string      `json:"source,omitempty"`
	CreatedAt string      `json:"created_at,omitempty"`
}

// Prompt is one append-only prompt version.
type Prompt struct {
	ID        string      `json:"id"`
	ProjectID string      `json:"project_id"`
	Name      string      `json:"name"`
	Version   int         `json:"version"`
	Model     string      `json:"model"`
	Template  interface{} `json:"template"`
	Params    interface{} `json:"params"`
	CreatedAt string      `json:"created_at"`
}

// Token source values.
const (
	TokenProvider  = "provider"
	TokenEstimated = "estimated"
)

// DefaultPrices is the in-memory fallback table used by the ingest path. The
// ingest transaction must not issue nested queries, so prices are resolved
// from this table rather than from model_prices.
var DefaultPrices = map[string][2]float64{
	"gpt-4o":          {0.0025, 0.01},
	"gpt-4o-mini":     {0.00015, 0.0006},
	"gpt-4.1":         {0.002, 0.008},
	"claude-sonnet-4": {0.003, 0.015},
	"llama-3.1-8b":    {0.0001, 0.0002},
}

// PriceFor returns input/output USD per 1K tokens for a model.
func PriceFor(model string) (float64, float64) {
	if p, ok := DefaultPrices[model]; ok {
		return p[0], p[1]
	}
	return 0.001, 0.002
}

// EstimateTokens is an honest placeholder, not a tokenizer: callers must label
// the result as estimated (see TokenSource).
func EstimateTokens(v interface{}) int {
	if v == nil {
		return 0
	}
	s, ok := v.(string)
	if !ok {
		b, _ := json.Marshal(v)
		s = string(b)
	}
	return len(s) / 4
}

// NowUTC is the canonical timestamp format used across the database.
func NowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// MarshalJSONValue renders a JSON column, falling back to an empty array.
func MarshalJSONValue(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}
