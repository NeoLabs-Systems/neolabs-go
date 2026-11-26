package userid

import (
	"encoding/json"
	"testing"
)

func TestBothKindsRoundTrip(t *testing.T) {
	out, err := json.Marshal([]ID{FromInt(7), "b6f1c0de-1111-4222-8333-444455556666", "007"})
	if err != nil || string(out) != `[7,"b6f1c0de-1111-4222-8333-444455556666","007"]` {
		t.Fatal(string(out), err)
	}
	var id ID
	if err := id.Scan(int64(7)); err != nil || id != "7" {
		t.Fatal(id, err)
	}
	if _, err := ID("abc").Int64(); err == nil {
		t.Fatal("a text id is not a number")
	}
}
