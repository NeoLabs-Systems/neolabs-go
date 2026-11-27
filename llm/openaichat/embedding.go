package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// Embed serves OpenAI-compatible vector endpoints. OAuth-only providers are
// excluded because their auth and model APIs do not promise embeddings.
func (p *Provider) Embed(ctx context.Context, texts []string, opt llm.Options) (*llm.EmbeddingResult, error) {
	model := strings.TrimSpace(opt.Model)
	if model == "" {
		return nil, &llm.Error{Provider: p.name, Code: llm.CodeModelSelectionRequired, Message: "An embedding model must be selected."}
	}
	switch p.name {
	case "openai", "openai-compatible", "openrouter", "neorouter":
	default:
		return nil, &llm.Error{Provider: p.name, Code: "EMBEDDINGS_UNAVAILABLE", Message: "This provider does not expose OpenAI-compatible embeddings."}
	}
	if len(texts) == 0 {
		return nil, errors.New("embedding input is empty")
	}
	return withAuthRetry(ctx, p, func() (*llm.EmbeddingResult, error) {
		body, err := json.Marshal(map[string]any{"model": model, "input": texts, "encoding_format": "float"})
		if err != nil {
			return nil, err
		}
		resp, err := transport.Do(ctx, p.hc, p.name, "Connection error.", transport.Request{
			Method: http.MethodPost, URL: transport.JoinURL(p.baseURL, "/embeddings"), Header: p.authHeaders("application/json"), Body: body,
		})
		if err != nil {
			return nil, p.wrap(err, p.requestPrefix)
		}
		if err = transport.CheckStatus(p.name, resp, false); err != nil {
			return nil, p.wrap(err, p.requestPrefix)
		}
		defer resp.Body.Close()
		raw, err := transport.ReadBody(resp.Body, 8<<20, "Embedding")
		if err != nil {
			return nil, p.wrap(err, p.requestPrefix)
		}
		var payload struct {
			Data []struct {
				Index     *int      `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
			Model string         `json:"model"`
			Usage map[string]any `json:"usage"`
		}
		if err = json.Unmarshal(raw, &payload); err != nil {
			return nil, &llm.Error{Provider: p.name, Code: "INVALID_EMBEDDING_RESPONSE", Message: "The provider returned invalid embedding data."}
		}
		if len(payload.Data) != len(texts) {
			return nil, &llm.Error{Provider: p.name, Code: "INVALID_EMBEDDING_RESPONSE", Message: "The provider returned the wrong number of embeddings."}
		}
		vectors := make([][]float32, len(texts))
		for i, row := range payload.Data {
			index := i
			if row.Index != nil {
				index = *row.Index
			}
			if index < 0 || index >= len(texts) || len(row.Embedding) == 0 || vectors[index] != nil {
				return nil, &llm.Error{Provider: p.name, Code: "INVALID_EMBEDDING_RESPONSE", Message: "The provider returned invalid embedding vectors."}
			}
			vectors[index] = row.Embedding
		}
		dimensions := len(vectors[0])
		for _, vector := range vectors {
			if len(vector) != dimensions {
				return nil, &llm.Error{Provider: p.name, Code: "INVALID_EMBEDDING_RESPONSE", Message: "The provider returned vectors with inconsistent dimensions."}
			}
		}
		selected := payload.Model
		if selected == "" {
			selected = model
		}
		return &llm.EmbeddingResult{Vectors: vectors, Model: selected, Usage: usageFrom(payload.Usage)}, nil
	})
}

// EmbeddingModels returns only models explicitly marked as embedding models by
// an OpenAI-compatible /models catalog. Standard catalogs that do not expose
// such metadata remain unknown instead of being classified by model ID.
func (p *Provider) EmbeddingModels(ctx context.Context) ([]llm.CatalogModel, error) {
	switch p.name {
	case "openai", "openai-compatible", "openrouter":
		// Chat catalogs intentionally filter non-chat IDs (including OpenAI
		// embedding IDs). Read the raw response here and classify only models
		// whose metadata explicitly declares embedding capability.
		doc, err := p.get(ctx, "/models")
		if err != nil {
			return nil, p.wrap(err, p.requestPrefix)
		}
		items, _ := doc["data"].([]any)
		result := make([]llm.CatalogModel, 0, len(items))
		for _, item := range items {
			model, ok := item.(map[string]any)
			if !ok || llm.ExplicitModelKind(model) != llm.KindEmbedding {
				continue
			}
			id, _ := model["id"].(string)
			if strings.TrimSpace(id) == "" {
				continue
			}
			result = append(result, llm.CatalogModel{Model: llm.Model{ID: id, Name: nameOr(model, id)}, Kind: llm.KindEmbedding, Raw: model})
		}
		return result, nil
	case "neorouter":
		return p.neoRouterEmbeddingModels(ctx)
	default:
		return nil, &llm.Error{Provider: p.name, Code: "EMBEDDINGS_UNAVAILABLE", Message: "This provider does not expose OpenAI-compatible embedding catalogs."}
	}
}

func (p *Provider) neoRouterEmbeddingModels(ctx context.Context) ([]llm.CatalogModel, error) {
	data, err := p.get(ctx, "/models?type=embedding")
	if err != nil {
		return nil, err
	}
	items, _ := data["data"].([]any)
	result := make([]llm.CatalogModel, 0, len(items))
	for _, item := range items {
		model, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := model["id"].(string)
		if strings.TrimSpace(id) == "" {
			continue
		}
		result = append(result, llm.CatalogModel{Model: llm.Model{ID: id, Name: nameOr(model, id)}, Kind: llm.KindEmbedding, Raw: model})
	}
	return result, nil
}
