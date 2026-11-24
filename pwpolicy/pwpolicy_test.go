package pwpolicy

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// Every case was produced by the Node password_policy.js: score, label, advice
// and the rejection message must be identical.
func TestMatchesNodeImplementation(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []struct {
			Password string
			Context  struct{ Username, Email string }
			Result   Strength
			Error    string
		}
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 90 {
		t.Fatalf("only %d vectors", len(v.Cases))
	}
	for _, c := range v.Cases {
		got := Evaluate(c.Password, Context{c.Context.Username, c.Context.Email})
		if !reflect.DeepEqual(got, c.Result) {
			t.Errorf("%q (ctx %+v)\n got: %+v\nwant: %+v", c.Password, c.Context, got, c.Result)
		}
		if msg := Error(got); msg != c.Error {
			t.Errorf("%q error: got %q want %q", c.Password, msg, c.Error)
		}
	}
}

func TestEmptyFeedbackIsAnArrayNotNull(t *testing.T) {
	out, _ := json.Marshal(Evaluate("", Context{}))
	var m map[string]any
	json.Unmarshal(out, &m)
	if _, ok := m["feedback"].([]any); !ok {
		t.Fatalf("feedback must serialize as an array: %s", out)
	}
}
