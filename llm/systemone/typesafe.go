package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

const typesafeBaseURL = "https://api.typesafe.ai/v1"

// TypeSafe serves its SystemOne models (Jev) directly. It has no chat models.
type TypeSafe struct {
	hc      *http.Client
	baseURL string
	apiKey  string
}

// NewTypeSafe returns the TypeSafe client. cfg.BaseURL overrides the API root
// (tests); the API key is sent as a bearer token.
func NewTypeSafe(cfg transport.Config) *TypeSafe {
	base := cfg.BaseURL
	if base == "" {
		base = typesafeBaseURL
	}
	cfg.TrustedBaseURL = cfg.TrustedBaseURL || cfg.BaseURL == ""
	return &TypeSafe{hc: cfg.Client(), baseURL: base, apiKey: cfg.APIKey}
}

func (t *TypeSafe) Name() string { return "typesafe" }

// Models is empty: TypeSafe has no chat models.
func (t *TypeSafe) Models(context.Context) ([]llm.Model, error) { return nil, nil }

// DecisionModels lists the SystemOne models.
func (t *TypeSafe) DecisionModels(ctx context.Context) ([]llm.Model, error) {
	res, err := transport.FetchText(ctx, t.hc, transport.FetchOptions{
		URL: t.baseURL + "/models", Header: map[string]string{"Authorization": "Bearer " + t.apiKey},
		MaxBytes: 1 << 20, Service: "TypeSafe model catalog", Provider: "typesafe",
	})
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, &llm.Error{Provider: "typesafe", Status: res.Status, Retryable: llm.RetryableStatus(res.Status), Message: fmt.Sprintf("TypeSafe /models returned HTTP %d", res.Status)}
	}
	var payload struct {
		Models []struct {
			Name any `json:"name"`
		} `json:"models"`
	}
	text := res.Text
	if text == "" {
		text = "{}"
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, &llm.Error{Provider: "typesafe", Message: "TypeSafe /models returned invalid JSON."}
	}
	var out []llm.Model
	for _, m := range payload.Models {
		if name, ok := m.Name.(string); ok {
			out = append(out, llm.Model{ID: name, Name: name})
		}
	}
	return out, nil
}

// Decide asks a decision model.
func (t *TypeSafe) Decide(ctx context.Context, req llm.DecisionRequest) (map[string]any, error) {
	return Post(ctx, t.hc, "typesafe", t.baseURL+"/systemone", map[string]string{"Authorization": "Bearer " + t.apiKey}, req)
}

// Chat is not served: TypeSafe answers decisions only.
func (t *TypeSafe) Chat(context.Context, []llm.Message, []llm.Tool, llm.Options) (*llm.Result, error) {
	return nil, errors.New("chat() not implemented")
}

// Stream is not served: TypeSafe answers decisions only.
func (t *TypeSafe) Stream(context.Context, []llm.Message, []llm.Tool, llm.Options) iter.Seq2[llm.Event, error] {
	return func(yield func(llm.Event, error) bool) {
		yield(llm.Event{}, errors.New("stream() not implemented"))
	}
}

func (t *TypeSafe) ContextWindow(string) int { return 128000 }
func (t *TypeSafe) SupportsVision() bool     { return false }

var (
	_ llm.Provider = (*TypeSafe)(nil)
	_ llm.Decider  = (*TypeSafe)(nil)
)
