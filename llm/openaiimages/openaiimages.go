// Package openaiimages calls an OpenAI-compatible /images/generations
// endpoint (OpenAI, xAI, ...) and returns base64 images.
package openaiimages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MissingKey is what the OpenAI SDK says when a client has no API key.
const MissingKey = "The OPENAI_API_KEY environment variable is missing or empty; either provide it, or instantiate the OpenAI client with an apiKey option, like new OpenAI({ apiKey: 'My API Key' })."

// Client is one endpoint and key.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// Generate asks for n images of prompt from model, as base64 PNG data.
func (c Client) Generate(ctx context.Context, model, prompt string, n int) ([]string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, errors.New(MissingKey)
	}
	body, _ := json.Marshal(map[string]any{"model": model, "prompt": prompt, "n": n, "response_format": "b64_json"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/images/generations", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("%d %s", resp.StatusCode, e.Error.Message)
		}
		return nil, fmt.Errorf("%d status code (no body)", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	out := make([]string, len(parsed.Data))
	for i, d := range parsed.Data {
		out[i] = d.B64JSON
	}
	return out, nil
}
