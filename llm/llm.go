// Package llm is the contract between agent code and language-model providers.
// The types mirror the harness transcript the NeoLabs apps already use (OpenAI
// style messages and tool calls), so an engine written against Provider works
// with any backend: OpenAI and compatible services, Anthropic, Google, Ollama,
// NeoRouter and so on. Concrete providers live in subpackages or in the app.
package llm

import (
	"context"
	"iter"
	"strings"
	"time"
)

// Message is one entry of a transcript. Content is a string or a list of
// content parts (text, images); it is kept as the JSON-shaped value the
// providers already exchange.
type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`

	// ReasoningState and ProviderContentBlocks carry provider-specific reasoning
	// the harness sends back on the next turn. They never leave the process unless
	// the provider knows how to return them.
	ReasoningState        map[string]any `json:"reasoningState,omitempty"`
	ProviderContentBlocks []any          `json:"providerContentBlocks,omitempty"`
}

// ToolCall is a model's request to run a tool. Arguments is the JSON text the
// model produced, which may be malformed; callers parse and repair it.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the tool and carries its raw JSON arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool describes a callable tool. Parameters is a JSON Schema object.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Options select the model and shape the call. Unset fields use provider defaults.
type Options struct {
	Model           string
	MaxTokens       int
	Temperature     *float64
	ReasoningEffort string
	// Extra carries provider-specific settings the engine passes through.
	Extra map[string]any
}

// Usage counts tokens as the provider reported them.
type Usage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	ReasoningTokens  int `json:"reasoningTokens"`
	CachedReadTokens int `json:"cachedReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

// Result is a complete (non-streamed) answer.
type Result struct {
	Content   string
	ToolCalls []ToolCall
	// Model is set when the service answered with another model than asked for
	// (a router, a fallback).
	Model        string
	FinishReason string
	// Truncated means output stopped at the token limit, not because the model
	// was done; callers must not treat it as a finished answer.
	Truncated      bool
	Usage          *Usage
	ReasoningState map[string]any
}

// Event kinds of a stream.
const (
	EventContent   = "content"    // a text delta
	EventToolCalls = "tool_calls" // final event: the model wants tools run
	EventDone      = "done"       // final event: the answer is complete
)

// Event is one step of a streamed answer. Content events carry a delta in
// Content; the final event (tool_calls or done) carries the whole answer.
type Event struct {
	Type           string
	Content        string
	ToolCalls      []ToolCall
	FinishReason   string
	Truncated      bool
	Usage          *Usage
	Model          string
	ReasoningState map[string]any
}

// Model is one entry of a provider's live catalog.
type Model struct {
	ID            string
	Name          string
	ContextWindow int
	Vision        bool
}

// ModelKind is an explicitly reported model capability. Empty means unknown;
// callers must not infer capabilities from model names.
type ModelKind string

const (
	KindChat       ModelKind = "chat"
	KindEmbedding  ModelKind = "embedding"
	KindTranscribe ModelKind = "transcription"
	KindImage      ModelKind = "image"
)

// ExplicitModelKind returns only a capability the provider explicitly put in
// its model metadata. Model names are deliberately not inspected.
func ExplicitModelKind(raw map[string]any) ModelKind {
	for _, key := range []string{"kind", "model_type", "task"} {
		if value, ok := raw[key].(string); ok {
			switch ModelKind(value) {
			case KindChat, KindEmbedding, KindTranscribe, KindImage:
				return ModelKind(value)
			}
		}
	}
	if values, ok := raw["capabilities"].([]any); ok {
		for _, value := range values {
			if text, ok := value.(string); ok && ModelKind(text) == KindEmbedding {
				return KindEmbedding
			}
		}
	}
	// OpenRouter reports embedding models through their structured modality
	// field rather than a kind or task field (for example, "text->embedding").
	if architecture, ok := raw["architecture"].(map[string]any); ok {
		if modality, ok := architecture["modality"].(string); ok {
			parts := strings.Split(modality, "->")
			if len(parts) == 2 && strings.TrimSpace(parts[1]) == string(KindEmbedding) {
				return KindEmbedding
			}
		}
	}
	return ""
}

// EmbeddingResult contains vectors in input order. Model may differ from the
// requested model when a router selects a fallback.
type EmbeddingResult struct {
	Vectors [][]float32
	Model   string
	Usage   *Usage
}

// EmbeddingProvider is an optional capability on providers that serve vectors.
type EmbeddingProvider interface {
	Embed(context.Context, []string, Options) (*EmbeddingResult, error)
}

// EmbeddingCataloger lists models explicitly advertised for vector embedding.
type EmbeddingCataloger interface {
	EmbeddingModels(context.Context) ([]CatalogModel, error)
}

// Provider is a language-model backend.
type Provider interface {
	// Name is the provider key, e.g. "openai" or "anthropic".
	Name() string
	// Models lists the live catalog. A provider requires an explicit model:
	// there are no silent defaults.
	Models(ctx context.Context) ([]Model, error)
	Chat(ctx context.Context, msgs []Message, tools []Tool, opt Options) (*Result, error)
	// Stream yields content deltas and ends with one tool_calls or done event.
	// An error ends the sequence. Cancelling ctx stops the request.
	Stream(ctx context.Context, msgs []Message, tools []Tool, opt Options) iter.Seq2[Event, error]
	ContextWindow(model string) int
	SupportsVision() bool
}

// ImageAnalyzer is implemented by providers that can describe images.
type ImageAnalyzer interface {
	AnalyzeImage(ctx context.Context, req ImageRequest) (string, error)
}

// ImageRequest asks a vision model about an image.
type ImageRequest struct {
	Model    string
	Prompt   string
	ImageB64 string
	MIMEType string
}

// Decider is implemented by providers serving "SystemOne" decision models:
// typed questions about a state, answered with probabilities.
type Decider interface {
	Decide(ctx context.Context, req DecisionRequest) (map[string]any, error)
	DecisionModels(ctx context.Context) ([]Model, error)
}

// DecisionCataloger is implemented by deciders whose decision catalog carries
// more than Model.
type DecisionCataloger interface {
	DecisionCatalog(ctx context.Context) ([]CatalogModel, error)
}

// DecisionRequest is a SystemOne question.
type DecisionRequest struct {
	Model     string
	State     map[string]any
	Questions map[string]any
	Timeout   time.Duration
}

// Error is a provider failure with what a retry policy needs.
type Error struct {
	Provider   string
	Status     int    // HTTP status, 0 if none
	Code       string // provider or stable code, e.g. MODEL_SELECTION_REQUIRED
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Message }

// EstimateTokens is the cheap length-based estimate the harness uses before a
// provider reports real usage (about four characters per token).
func EstimateTokens(text string) int { return (len(text) + 3) / 4 }
