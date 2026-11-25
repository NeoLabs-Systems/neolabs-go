package llm

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// vectors.json is produced by the Node implementation (testdata/gen/vectors.js).
type vectors struct {
	Sanitize []struct {
		Input  *string `json:"input"`
		Output string  `json:"output"`
	} `json:"sanitize"`
	Guard []struct {
		Parts    []json.RawMessage `json:"parts"`
		Verdicts []*struct {
			Reason string `json:"reason"`
			Bytes  int    `json:"bytes"`
			Period int    `json:"period"`
		} `json:"verdicts"`
	} `json:"guard"`
	Tracker []struct {
		Chunks  []string `json:"chunks"`
		Keys    []string `json:"keys"`
		Results []bool   `json:"results"`
		Reason  *string  `json:"reason"`
		Key     string   `json:"key"`
	} `json:"tracker"`
	Transient []struct {
		Status    *int   `json:"status"`
		Message   string `json:"message"`
		Transient bool   `json:"transient"`
	} `json:"transient"`
	RetryAfter []struct {
		Headers map[string]string `json:"headers"`
		MS      *float64          `json:"ms"`
	} `json:"retryAfter"`
	Identity struct {
		Models []struct {
			ID, ModelID, Provider string
		} `json:"models"`
		SelectionIDs []string `json:"selectionIds"`
		Parse        []struct {
			V string `json:"v"`
			R *struct {
				Provider string `json:"provider"`
				ModelID  string `json:"modelId"`
			} `json:"r"`
		} `json:"parse"`
		Resolve []struct {
			V string `json:"v"`
			P string `json:"p"`
			R *struct {
				ID string `json:"id"`
			} `json:"r"`
		} `json:"resolve"`
		Normalize  []string `json:"normalize"`
		Matches    [][]bool `json:"matches"`
		Selectable struct {
			ID, ModelID, Provider string
		} `json:"selectable"`
	} `json:"identity"`
	Reference struct {
		Catalog []map[string]any `json:"catalog"`
		Facts   []struct {
			CreatedAt     *float64 `json:"createdAt"`
			InputCostPerM *float64 `json:"inputCostPerM"`
			SupportsTools *bool    `json:"supportsTools"`
		} `json:"facts"`
		Lookups []struct {
			ID    string `json:"id"`
			Facts *struct {
				CreatedAt     *float64 `json:"createdAt"`
				InputCostPerM *float64 `json:"inputCostPerM"`
				SupportsTools *bool    `json:"supportsTools"`
			} `json:"facts"`
			Tier *string  `json:"tier"`
			Cost *float64 `json:"cost"`
		} `json:"lookups"`
		Tiers []struct {
			C    *float64 `json:"c"`
			Tier *string  `json:"tier"`
		} `json:"tiers"`
	} `json:"reference"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSanitizeDetailMatchesNode(t *testing.T) {
	for _, c := range loadVectors(t).Sanitize {
		input := ""
		if c.Input != nil {
			input = *c.Input
		}
		if got := SanitizeDetail(input); got != c.Output {
			t.Errorf("SanitizeDetail(%.60q)\n got: %.120q\nwant: %.120q", input, got, c.Output)
		}
	}
}

func TestStreamGuardMatchesNode(t *testing.T) {
	for i, c := range loadVectors(t).Guard {
		g := NewStreamGuard()
		for j, raw := range c.Parts {
			var delta string
			var rep struct {
				Text   string `json:"text"`
				Repeat int    `json:"repeat"`
			}
			if json.Unmarshal(raw, &delta) != nil {
				if err := json.Unmarshal(raw, &rep); err != nil {
					t.Fatal(err)
				}
				delta = strings.Repeat(rep.Text, rep.Repeat)
			}
			got := g.Feed(delta)
			want := c.Verdicts[j]
			switch {
			case got == nil && want == nil:
			case got == nil || want == nil:
				t.Errorf("case %d part %d: got %+v, want %+v", i, j, got, want)
			case got.Reason != want.Reason || got.Bytes != want.Bytes || got.Period != want.Period:
				t.Errorf("case %d part %d: got %+v, want %+v", i, j, *got, *want)
			}
		}
	}
}

func TestJSONPrefixTrackerMatchesNode(t *testing.T) {
	for i, c := range loadVectors(t).Tracker {
		var known func(string) bool
		if c.Keys != nil {
			set := map[string]bool{}
			for _, k := range c.Keys {
				set[k] = true
			}
			known = func(k string) bool { return set[k] }
		}
		tr := NewJSONPrefixTracker(known)
		for j, chunk := range c.Chunks {
			if got := tr.Feed(chunk); got != c.Results[j] {
				t.Errorf("case %d (%q) chunk %d: Feed = %v, want %v", i, c.Chunks, j, got, c.Results[j])
			}
		}
		wantReason := ""
		if c.Reason != nil {
			wantReason = *c.Reason
		}
		if tr.Reason() != wantReason || tr.Key() != c.Key {
			t.Errorf("case %d (%q): reason/key = %q/%q, want %q/%q", i, c.Chunks, tr.Reason(), tr.Key(), wantReason, c.Key)
		}
	}
}

func TestIsTransientMatchesNode(t *testing.T) {
	for _, c := range loadVectors(t).Transient {
		var err error = errors.New(c.Message)
		if c.Status != nil {
			err = &Error{Status: *c.Status, Message: c.Message, Retryable: RetryableStatus(*c.Status)}
		}
		if got := IsTransient(err); got != c.Transient {
			t.Errorf("IsTransient(status=%v, %q) = %v, Node says %v", c.Status, c.Message, got, c.Transient)
		}
	}
}

func TestRetryAfterMatchesNode(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	for _, c := range loadVectors(t).RetryAfter {
		h := http.Header{}
		for k, v := range c.Headers {
			h.Set(k, v)
		}
		want := time.Duration(0)
		if c.MS != nil {
			want = time.Duration(*c.MS * float64(time.Millisecond))
		}
		if got := RetryAfterFrom(h, now); got != want {
			t.Errorf("RetryAfterFrom(%v) = %v, want %v", c.Headers, got, want)
		}
	}
}

func TestModelIdentityMatchesNode(t *testing.T) {
	v := loadVectors(t).Identity
	var models []ModelRef
	for _, m := range v.Models {
		models = append(models, ModelRef{ID: m.ID, ModelID: m.ModelID, Provider: m.Provider})
	}
	if got := []string{SelectionID("OpenAI ", " gpt-5 "), SelectionID("x", "y")}; !reflect.DeepEqual(got, v.SelectionIDs) {
		t.Errorf("SelectionID = %v, want %v", got, v.SelectionIDs)
	}
	for _, c := range v.Parse {
		p, m, ok := ParseSelectionID(c.V)
		if ok != (c.R != nil) || (ok && (p != c.R.Provider || m != c.R.ModelID)) {
			t.Errorf("ParseSelectionID(%q) = %q %q %v, want %+v", c.V, p, m, ok, c.R)
		}
	}
	for _, c := range v.Resolve {
		m, ok := ResolveSelection(models, c.V, c.P)
		if ok != (c.R != nil) || (ok && m.ID != c.R.ID) {
			t.Errorf("ResolveSelection(%q, %q) = %v %v, want %+v", c.V, c.P, m.ID, ok, c.R)
		}
	}
	if got := NormalizeSelections(models, []string{"gpt-5", "openai::gpt-5", "claude-x", "missing", "plain-id"}); !reflect.DeepEqual(got, v.Normalize) {
		t.Errorf("NormalizeSelections = %v, want %v", got, v.Normalize)
	}
	sets := []map[string]bool{{"gpt-5": true}, {"openai::gpt-5": true}, {"other": true}, nil}
	for i, set := range sets {
		for j, m := range models {
			if got := MatchesConfiguredID(m, set); got != v.Matches[i][j] {
				t.Errorf("MatchesConfiguredID(set %d, model %d) = %v", i, j, got)
			}
		}
	}
	if s := Selectable(ModelRef{Provider: " OpenAI ", ID: "gpt-5"}); s.ID != v.Selectable.ID || s.ModelID != v.Selectable.ModelID || s.Provider != v.Selectable.Provider {
		t.Errorf("Selectable = %+v, want %+v", s, v.Selectable)
	}
}

func TestReferenceFactsMatchNode(t *testing.T) {
	v := loadVectors(t).Reference
	ptrEq := func(got *float64, want *float64) bool {
		return (got == nil) == (want == nil) && (got == nil || math.Abs(*got-*want) < 1e-9)
	}
	for i, m := range v.Catalog {
		got, want := ReadCatalogFacts(m), v.Facts[i]
		var created *float64
		if got.CreatedAtMs != nil {
			f := float64(*got.CreatedAtMs)
			created = &f
		}
		if !ptrEq(created, want.CreatedAt) || !ptrEq(got.InputCostPerM, want.InputCostPerM) || (got.SupportsTools == nil) != (want.SupportsTools == nil) || (got.SupportsTools != nil && *got.SupportsTools != *want.SupportsTools) {
			t.Errorf("ReadCatalogFacts(%v) = %+v, want %+v", m["id"], got, want)
		}
	}
	ref := NewReference()
	var data []any
	for _, m := range v.Catalog {
		data = append(data, m)
	}
	ref.Record(data)
	for _, l := range v.Lookups {
		f, ok := ref.Lookup(l.ID)
		if ok != (l.Facts != nil) {
			t.Errorf("Lookup(%q) found=%v, want %v", l.ID, ok, l.Facts != nil)
		}
		if !ptrEq(ref.InputCostPerM(l.ID), l.Cost) {
			t.Errorf("InputCostPerM(%q) = %v, want %v", l.ID, ref.InputCostPerM(l.ID), l.Cost)
		}
		wantTier := ""
		if l.Tier != nil {
			wantTier = *l.Tier
		}
		if got := ref.PriceTier(l.ID); got != wantTier {
			t.Errorf("PriceTier(%q) = %q, want %q", l.ID, got, wantTier)
		}
		_ = f
	}
	for _, c := range v.Tiers {
		want := ""
		if c.Tier != nil {
			want = *c.Tier
		}
		if got := PriceTierForCost(c.C); got != want {
			t.Errorf("PriceTierForCost(%v) = %q, want %q", c.C, got, want)
		}
	}
}

func TestProviderDefinitionsMatchNode(t *testing.T) {
	raw, err := os.ReadFile("testdata/definitions.json")
	if err != nil {
		t.Fatal(err)
	}
	var node []map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	if len(node) != len(ProviderDefinitions) {
		t.Fatalf("%d definitions, Node has %d", len(ProviderDefinitions), len(node))
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	flag := func(m map[string]any, k string) bool { b, _ := m[k].(bool); return b }
	for i, d := range ProviderDefinitions {
		n := node[i]
		same := d.ID == str(n, "id") && d.Label == str(n, "label") && d.Description == str(n, "description") && d.EnvKey == str(n, "envKey") &&
			d.BaseURLEnvKey == str(n, "baseUrlEnvKey") && d.Authentication == str(n, "authentication") && d.DefaultBaseURL == str(n, "defaultBaseUrl") &&
			d.SupportsAPIKey == flag(n, "supportsApiKey") && d.SupportsBaseURL == flag(n, "supportsBaseUrl") && d.RequiresBaseURL == flag(n, "requiresBaseUrl") &&
			d.Aggregator == flag(n, "aggregator") && d.DefaultEnabled == flag(n, "defaultEnabled")
		if !same {
			t.Errorf("definition %s differs from Node", d.ID)
		}
	}
}
