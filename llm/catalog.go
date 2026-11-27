package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CatalogModel is a catalog entry with everything the vendor listed, a superset
// of Model. Raw carries the vendor's own entry for facts a provider does not
// map (architecture, pricing, supported parameters).
type CatalogModel struct {
	Model
	Kind          ModelKind
	Created       int64 // unix seconds, 0 if unknown
	CreatedAt     string
	SupportsTools *bool
	PriceTier     string
	Thinking      bool
	Reasoning     map[string]any
	Protocols     []string
	Raw           map[string]any
}

// Cataloger is implemented by providers whose catalog carries more than Model.
type Cataloger interface {
	Catalog(ctx context.Context) ([]CatalogModel, error)
}

// LiveVoiceLister is implemented by providers that serve live-voice models.
type LiveVoiceLister interface {
	LiveVoiceModels(ctx context.Context) ([]CatalogModel, error)
}

// ModelFacts are what a catalog states about a model; unknowns stay nil.
type ModelFacts struct {
	CreatedAtMs   *int64
	InputCostPerM *float64
	SupportsTools *bool
}

var jsFloat = regexp.MustCompile(`^[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?`)

// ParseFloatPrefix is JavaScript's parseFloat on a string.
func ParseFloatPrefix(s string) (float64, bool) {
	m := jsFloat.FindString(strings.TrimSpace(s))
	if m == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(m, 64)
	return f, err == nil
}

// ReadCatalogFacts reads release time, input price per million tokens and tool
// support from any OpenAI-style catalog entry.
func ReadCatalogFacts(model map[string]any) ModelFacts {
	var facts ModelFacts
	if seconds, ok := model["created"].(float64); ok && seconds > 0 {
		ms := int64(seconds * 1000)
		facts.CreatedAtMs = &ms
	} else if s, ok := model["created_at"].(string); ok {
		if t, ok := parseDate(s); ok {
			ms := t.UnixMilli()
			facts.CreatedAtMs = &ms
		}
	}
	if pricing, ok := model["pricing"].(map[string]any); ok {
		var cost float64
		var have bool
		switch p := pricing["prompt"].(type) {
		case string:
			cost, have = ParseFloatPrefix(p)
		case float64:
			cost, have = p, true
		}
		if have {
			cost *= 1_000_000
			if cost >= 0 {
				facts.InputCostPerM = &cost
			}
		}
	}
	if params, ok := model["supported_parameters"].([]any); ok {
		supports := false
		for _, p := range params {
			if p == "tools" {
				supports = true
			}
		}
		facts.SupportsTools = &supports
	}
	return facts
}

func parseDate(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

var (
	snapshotSuffix = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)
	vendorPrefix   = regexp.MustCompile(`^.*/`)
)

// ReferenceNameKey compares one model across vendors: "gpt-5.5", "gpt-5.5-2026-04-23",
// "claude-sonnet-4-5-20250929" and "anthropic/claude-sonnet-4.5" all reduce to a
// key without vendor prefix, dated snapshot suffix, or dot/dash difference.
func ReferenceNameKey(id string) string {
	key := strings.ToLower(strings.TrimSpace(id))
	key = vendorPrefix.ReplaceAllString(key, "")
	key = snapshotSuffix.ReplaceAllString(key, "")
	return strings.NewReplacer(".", "-", "_", "-").Replace(key)
}

// PriceTierForCost buckets an input price per million tokens.
func PriceTierForCost(costPerM *float64) string {
	switch {
	case costPerM == nil:
		return ""
	case *costPerM == 0:
		return "free"
	case *costPerM < 0.5:
		return "cheap"
	case *costPerM < 5:
		return "medium"
	}
	return "expensive"
}

// Reference holds facts most provider catalogs leave out (release date, price,
// tool support), read from OpenRouter's public catalog. Routing ranks on them,
// so it follows new releases without naming a model in code.
type Reference struct {
	mu      sync.Mutex
	byID    map[string]ModelFacts
	byName  map[string]ModelFacts
	fresh   time.Time
	first   chan struct{} // closed when the first load has finished
	running bool
	lastErr error
}

// NewReference returns an empty reference catalog.
func NewReference() *Reference {
	return &Reference{byID: map[string]ModelFacts{}, byName: map[string]ModelFacts{}}
}

// Record adds catalog entries (as decoded from an OpenAI-style "data" array).
func (r *Reference) Record(models []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range models {
		model, _ := entry.(map[string]any)
		id, _ := model["id"].(string)
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		facts := ReadCatalogFacts(model)
		r.byID[id] = facts
		// A ":free" or ":batch" variant is priced apart from the model itself.
		if !strings.Contains(id, ":") {
			r.byName[ReferenceNameKey(id)] = facts
		}
	}
}

// Lookup returns the facts for a model id, or false.
func (r *Reference) Lookup(modelID string) (ModelFacts, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := strings.TrimSpace(modelID)
	if f, ok := r.byID[id]; ok {
		return f, true
	}
	f, ok := r.byName[ReferenceNameKey(id)]
	return f, ok
}

// InputCostPerM is the model's input price per million tokens, if known.
func (r *Reference) InputCostPerM(modelID string) *float64 {
	if f, ok := r.Lookup(modelID); ok {
		return f.InputCostPerM
	}
	return nil
}

// PriceTier classifies a model by its input price ("" when unknown).
func (r *Reference) PriceTier(modelID string) string {
	return PriceTierForCost(r.InputCostPerM(modelID))
}

// Expired reports whether the catalog should be fetched again.
func (r *Reference) Expired(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !now.Before(r.fresh)
}

// MarkLoaded records a successful (6 h) or failed (5 min) load.
func (r *Reference) MarkLoaded(now time.Time, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		r.fresh = now.Add(6 * time.Hour)
	} else {
		r.fresh = now.Add(5 * time.Minute)
	}
}

// ReferenceURL is OpenRouter's public model catalog. The request carries no key
// and nothing about the user.
const ReferenceURL = "https://openrouter.ai/api/v1/models"

// EnsureFresh loads the reference catalog when it is stale. Only the very first
// load is waited for; afterwards the catalog already held keeps serving while a
// fresh one loads in the background. A failed load is retried after five minutes
// and reported by Err, never as an error here. hc may be nil for the default client.
func (r *Reference) EnsureFresh(ctx context.Context, hc *http.Client) error {
	r.mu.Lock()
	if time.Now().Before(r.fresh) {
		r.mu.Unlock()
		return nil
	}
	if r.first == nil {
		r.first = make(chan struct{})
		r.start(hc, r.first)
	} else if !r.running {
		r.start(hc, nil)
	}
	wait := r.first
	r.mu.Unlock()
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// start runs a load in the background; the caller holds r.mu.
func (r *Reference) start(hc *http.Client, done chan struct{}) {
	r.running = true
	go func() {
		err := r.load(hc)
		r.mu.Lock()
		r.running, r.lastErr = false, err
		r.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()
}

// Err is the error of the latest load, nil after a successful one.
func (r *Reference) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

func (r *Reference) load(hc *http.Client) error {
	if hc == nil {
		hc = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReferenceURL, nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
		if err != nil {
			return err
		}
		models, err := ParseReferenceCatalog(body)
		if err != nil {
			return err
		}
		r.Record(models)
		return nil
	}()
	r.MarkLoaded(time.Now(), err == nil)
	return err
}

// ParseReferenceCatalog decodes an OpenAI-style catalog response body.
func ParseReferenceCatalog(body []byte) ([]any, error) {
	var payload struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}
