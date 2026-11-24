package passhash

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Hashes produced by the Node bcrypt package must verify in Go.
func TestVerifiesNodeHashes(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []struct{ Plain, Hash string } `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		if !strings.HasPrefix(c.Hash, "$2") {
			t.Fatalf("unexpected hash %q", c.Hash)
		}
		if !Verify(c.Hash, c.Plain) {
			t.Fatalf("node hash did not verify for %.12q", c.Plain)
		}
		if Verify(c.Hash, c.Plain+"x") && len(c.Plain) < 72 {
			t.Fatalf("wrong password verified for %.12q", c.Plain)
		}
	}
}

func TestHashRoundTripAndTruncation(t *testing.T) {
	long := strings.Repeat("a", 100)
	h, err := Hash(long, 4)
	if err != nil {
		t.Fatal(err)
	}
	// Same first 72 bytes means same password, exactly like the Node bcrypt package.
	if !Verify(h, strings.Repeat("a", 72)+"different tail") {
		t.Fatal("bcrypt must ignore bytes past 72")
	}
	if Verify(h, "other") || Verify("not-a-hash", "other") || Verify("", "") {
		t.Fatal("bad credentials must not verify")
	}
	if !NeedsRehash(h, 12) || NeedsRehash(h, 4) {
		t.Fatal("NeedsRehash compares cost")
	}
}

func TestTokens(t *testing.T) {
	a, _ := RandomToken(32)
	b, _ := RandomToken(32)
	if a == b || len(a) != 43 {
		t.Fatalf("tokens must be unique 43-char base64url: %q %q", a, b)
	}
	if DigestToken("x") != "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" {
		t.Fatal("digest mismatch")
	}
	if !Equal("abc", "abc") || Equal("abc", "abd") || Equal("abc", "ab") {
		t.Fatal("Equal")
	}
}
