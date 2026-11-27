package openaichat

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/llm"
	"github.com/NeoLabs-Systems/neolabs-go/llm/transport"
)

// ModelList is GET /models as the endpoint sent it, for callers that apply
// their own rules to what a catalog contains.
type ModelList struct {
	Data []map[string]any
	// NotAllowed counts, by price class, the models a NeoRouter has that the
	// key may not use.
	NotAllowed map[string]int
}

// ModelList reads the unfiltered model list. kind, when set, is NeoRouter's
// ?type= filter.
func (p *Provider) ModelList(ctx context.Context, kind string) (ModelList, error) {
	path := "/models"
	if kind != "" {
		path += "?type=" + url.QueryEscape(kind)
	}
	return withAuthRetry(ctx, p, func() (ModelList, error) {
		res, err := transport.FetchText(ctx, p.hc, transport.FetchOptions{
			URL: transport.JoinURL(p.baseURL, path), Header: p.authHeaders("application/json"),
			MaxBytes: 5 << 20, Service: "Model catalog", Timeout: 30 * time.Second, Provider: p.name,
		})
		if err != nil {
			return ModelList{}, err
		}
		var payload struct {
			Data      []map[string]any `json:"data"`
			NeoRouter struct {
				NotAllowed map[string]int `json:"not_allowed_for_key"`
			} `json:"neorouter"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		decodeErr := json.Unmarshal([]byte(res.Text), &payload)
		if !res.OK() {
			message := payload.Error.Message
			if message == "" {
				message = "HTTP " + strconv.Itoa(res.Status)
			}
			return ModelList{}, &llm.Error{Provider: p.name, Status: res.Status, Retryable: llm.RetryableStatus(res.Status),
				RetryAfter: llm.RetryAfterFrom(res.Header, time.Now()), Message: p.name + ": " + llm.SanitizeDetail(message)}
		}
		if decodeErr != nil {
			return ModelList{}, &llm.Error{Provider: p.name, Message: "Unexpected token in JSON response from " + p.name}
		}
		return ModelList{Data: payload.Data, NotAllowed: payload.NeoRouter.NotAllowed}, nil
	})
}
