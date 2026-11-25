package webextract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMatchesNode replays results recorded from the NeoAgent Node extractor
// (testdata/gen_vectors.js) on the pages in testdata/pages.
func TestMatchesNode(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors map[string]Result
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for name, want := range vectors {
		t.Run(name, func(t *testing.T) {
			page, err := os.ReadFile(filepath.Join("testdata", "pages", name))
			if err != nil {
				t.Fatal(err)
			}
			got := Extract(string(page), "https://example.com/base/page")
			if want.Strategies == nil {
				want.Strategies = []string{}
			}
			if got.Strategies == nil {
				got.Strategies = []string{}
			}
			if got.Markdown != want.Markdown {
				gl, wl := strings.Split(got.Markdown, "\n"), strings.Split(want.Markdown, "\n")
				for i := 0; i < len(gl) || i < len(wl); i++ {
					var g, w string
					if i < len(gl) {
						g = gl[i]
					}
					if i < len(wl) {
						w = wl[i]
					}
					if g != w {
						t.Fatalf("markdown differs at line %d:\n got %q\nwant %q", i+1, g, w)
					}
				}
			}
			if got.Metadata != want.Metadata || got.WordCount != want.WordCount || strings.Join(got.Strategies, ",") != strings.Join(want.Strategies, ",") {
				t.Fatalf("got %+v %d %v want %+v %d %v", got.Metadata, got.WordCount, got.Strategies, want.Metadata, want.WordCount, want.Strategies)
			}
		})
	}
}

func TestJSNumber(t *testing.T) {
	for in, want := range map[float64]string{9.99: "9.99", 1e21: "1e+21", 1e-7: "1e-7", 0.000001: "0.000001", -3: "-3", 123456789012: "123456789012"} {
		if got := jsNumber(in); got != want {
			t.Errorf("%v: %s want %s", in, got, want)
		}
	}
}
