package geoip

import (
	"encoding/json"
	"os"
	"testing"
)

// dataDir is a geoip-lite data directory; the test is skipped without one.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("GEOIP_DATA_DIR")
	if dir == "" {
		t.Skip("GEOIP_DATA_DIR is not set")
	}
	if _, err := os.Stat(dir + "/geoip-city.dat"); err != nil {
		t.Skip("no geoip-lite data at " + dir)
	}
	return dir
}

type nodeRecord struct {
	Country  string     `json:"country"`
	Region   string     `json:"region"`
	City     string     `json:"city"`
	Timezone string     `json:"timezone"`
	EU       string     `json:"eu"`
	Metro    int32      `json:"metro"`
	Area     uint32     `json:"area"`
	LL       []*float64 `json:"ll"`
}

// TestMatchesNode replays lookups recorded from the real geoip-lite
// (testdata/gen_vectors.js).
func TestMatchesNode(t *testing.T) {
	db := Open(dataDir(t))
	defer db.Close()
	raw, err := os.ReadFile("testdata/node_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		IP  string      `json:"ip"`
		Raw *nodeRecord `json:"raw"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	hits := 0
	for _, v := range vectors {
		rec, ok, err := db.Lookup(v.IP)
		if err != nil {
			t.Fatalf("%q: %v", v.IP, err)
		}
		if ok != (v.Raw != nil) {
			if v.Raw == nil && v.IP != "" && len(v.IP) > 0 && containsComma(v.IP) {
				continue // the app layer takes the first address of a list
			}
			t.Errorf("%q: found=%v, node found=%v", v.IP, ok, v.Raw != nil)
			continue
		}
		if !ok {
			continue
		}
		hits++
		n := v.Raw
		if rec.Country != n.Country || rec.Region != n.Region || rec.City != n.City ||
			rec.Timezone != n.Timezone || rec.EU != n.EU || rec.Metro != n.Metro || rec.Area != n.Area {
			t.Errorf("%q: got %+v, node %+v", v.IP, rec, *n)
		}
		for i := 0; i < 2; i++ {
			var want *float64
			if len(n.LL) == 2 {
				want = n.LL[i]
			}
			switch {
			case want == nil && rec.HasLL:
				t.Errorf("%q: ll should be null, got %v", v.IP, rec.LL)
			case want != nil && (!rec.HasLL || rec.LL[i] != *want):
				t.Errorf("%q: ll[%d] = %v (has %v), node %v", v.IP, i, rec.LL[i], rec.HasLL, *want)
			}
		}
	}
	if hits < 100 {
		t.Fatalf("only %d vectors hit; the data does not cover the vectors", hits)
	}
}

func containsComma(s string) bool {
	for _, c := range s {
		if c == ',' {
			return true
		}
	}
	return false
}

func TestMissingData(t *testing.T) {
	db := Open(t.TempDir())
	if _, _, err := db.Lookup("8.8.8.8"); err == nil {
		t.Fatal("empty directory must report an error")
	}
}

func TestAton6MatchesNode(t *testing.T) {
	// Values computed with geoip-lite's utils.aton6, which keeps stale groups
	// when it expands "::".
	cases := map[string][2]uint32{
		"2001:db8::1":          {0x20010db8, 1},
		"2001:4860:4860::8888": {0x20014860, 0x48600000},
		"::1":                  {0, 0x10000},
	}
	for in, want := range cases {
		if got := aton6(in); got != want {
			t.Errorf("aton6(%q) = %x, want %x", in, got, want)
		}
	}
}
