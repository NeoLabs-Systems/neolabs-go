// Package embedding calls the OpenAI and Google embedding APIs.
package embedding

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
	"github.com/NeoLabs-Systems/neolabs-go/ssrf"
)

const (
	OpenAIModel      = "text-embedding-3-small"
	OpenAIDimensions = 1536
	GoogleModel      = "gemini-embedding-2"
	GoogleDimensions = 768
)

type Result struct {
	Vector          []float32
	Provider, Model string
	Dimensions      int
}
type Client struct {
	HTTP                 *http.Client
	OpenAIURL, GoogleURL string
}

func New() *Client {
	return &Client{HTTP: ssrf.NewClient(ssrf.Options{Timeout: 15 * time.Second}), OpenAIURL: "https://api.openai.com/v1/embeddings", GoogleURL: "https://generativelanguage.googleapis.com/v1beta/models/" + GoogleModel + ":embedContent"}
}
func Truncate(text string) string {
	units := utf16.Encode([]rune(text))
	if len(units) > 25000 {
		units = units[:25000]
	}
	return string(utf16.Decode(units))
}
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case string:
		if strings.TrimSpace(x) == "" {
			return 0, true
		}
		n, e := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return n, e == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
	case nil:
		return 0, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
func vector(values []any, expected int) []float32 {
	if len(values) != expected {
		return nil
	}
	out := make([]float32, len(values))
	for i, v := range values {
		n, ok := number(v)
		if !ok {
			return nil
		}
		out[i] = float32(n)
	}
	return out
}
func (c *Client) call(ctx context.Context, target string, headers map[string]string, body any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	headers["Content-Type"] = "application/json"
	res, err := transport.FetchText(ctx, c.HTTP, transport.FetchOptions{Method: "POST", URL: target, Header: headers, Body: raw, MaxBytes: 2 << 20, Service: "Embedding", Timeout: 15 * time.Second})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	if !res.OK() {
		return nil, nil
	}
	var data map[string]any
	if json.Unmarshal([]byte(res.Text), &data) != nil {
		return nil, nil
	}
	return data, nil
}
func (c *Client) OpenAI(ctx context.Context, key, text string) (*Result, error) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	data, err := c.call(ctx, c.OpenAIURL, map[string]string{"Authorization": "Bearer " + key}, map[string]any{"model": OpenAIModel, "input": Truncate(text), "encoding_format": "float"})
	if err != nil || data == nil {
		return nil, err
	}
	rows, _ := data["data"].([]any)
	if len(rows) == 0 {
		return nil, nil
	}
	first, _ := rows[0].(map[string]any)
	values, _ := first["embedding"].([]any)
	vec := vector(values, OpenAIDimensions)
	if vec == nil {
		return nil, nil
	}
	return &Result{Vector: vec, Provider: "openai", Model: OpenAIModel, Dimensions: len(vec)}, nil
}
func (c *Client) Google(ctx context.Context, key, text, inputType string) (*Result, error) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	text = Truncate(text)
	switch inputType {
	case "query":
		text = "task: search result | query: " + text
	case "document":
		text = "title: none | text: " + text
	default:
		text = "task: sentence similarity | query: " + text
	}
	data, err := c.call(ctx, c.GoogleURL, map[string]string{"x-goog-api-key": key}, map[string]any{"model": "models/" + GoogleModel, "content": map[string]any{"parts": []any{map[string]any{"text": text}}}, "output_dimensionality": GoogleDimensions})
	if err != nil || data == nil {
		return nil, err
	}
	entry, _ := data["embedding"].(map[string]any)
	values, _ := entry["values"].([]any)
	vec := vector(values, GoogleDimensions)
	if vec == nil {
		return nil, nil
	}
	return &Result{Vector: vec, Provider: "google", Model: GoogleModel, Dimensions: len(vec)}, nil
}
