// Package systemone talks to SystemOne decision models: they answer typed
// questions about a state with probabilities instead of generating text. Every
// host (TypeSafe, OpenRouter, Ollama, NeoRouter) takes TypeSafe's request shape
// at its systemone endpoint and answers with { model, answers, usage }.
package systemone

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// Post asks one decision model and returns {model, answers, usage}.
func Post(ctx context.Context, hc *http.Client, provider, url string, headers map[string]string, req llm.DecisionRequest) (map[string]any, error) {
	body, err := transport.Marshal(transport.NewObj().Set("model", req.Model).Set("state", req.State).Set("questions", req.Questions))
	if err != nil {
		return nil, err
	}
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	res, err := transport.FetchText(ctx, hc, transport.FetchOptions{
		Method: http.MethodPost, URL: url, Header: h, Body: body,
		MaxBytes: 1 << 20, Service: "SystemOne decision", Timeout: req.Timeout, Provider: provider,
	})
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if json.Unmarshal([]byte(res.Text), &payload) != nil {
		payload = nil
	}
	if !res.OK() {
		detail := ""
		switch e := payload["error"].(type) {
		case string:
			detail = e
		case map[string]any:
			detail, _ = e["message"].(string)
		}
		msg := fmt.Sprintf("%s returned HTTP %d", req.Model, res.Status)
		if detail != "" {
			msg += ": " + detail
		}
		return nil, &llm.Error{Provider: provider, Status: res.Status, Message: msg, Retryable: llm.RetryableStatus(res.Status), RetryAfter: llm.RetryAfterFrom(res.Header, time.Now())}
	}
	answers, ok := payload["answers"].(map[string]any)
	if !ok {
		return nil, &llm.Error{Provider: provider, Message: req.Model + " returned no answers."}
	}
	model, _ := payload["model"].(string)
	if model == "" {
		model = req.Model
	}
	usage, _ := payload["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
	}
	return map[string]any{"model": model, "answers": answers, "usage": usage}, nil
}
