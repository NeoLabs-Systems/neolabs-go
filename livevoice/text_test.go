package livevoice

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// text.json holds the Node server's answers (gen/text.js).
func TestTextHelpersMatchNode(t *testing.T) {
	raw, err := os.ReadFile("testdata/text.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Split []struct {
			Input string   `json:"input"`
			Max   int      `json:"max"`
			Parts []string `json:"parts"`
		} `json:"split"`
		Fragments []struct{ Buffer, Fragment, Result string } `json:"fragments"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Split {
		got := SplitForAppend(c.Input, c.Max)
		if len(got) == 0 && len(c.Parts) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.Parts) {
			t.Errorf("SplitForAppend(%q, %d) = %q; Node %q", c.Input, c.Max, got, c.Parts)
		}
	}
	for _, c := range v.Fragments {
		if got := AppendFragment(c.Buffer, c.Fragment); got != c.Result {
			t.Errorf("AppendFragment(%q, %q) = %q; Node %q", c.Buffer, c.Fragment, got, c.Result)
		}
	}
}
