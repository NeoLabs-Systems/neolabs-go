package secretbox

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type vectors struct {
	Material string `json:"material"`
	Cases    []struct {
		Plain  string `json:"plain"`
		Sealed string `json:"sealed"`
	} `json:"cases"`
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

// Values sealed by the Node implementation (secrets.js) must open in Go.
func TestOpensValuesSealedByNode(t *testing.T) {
	v := loadVectors(t)
	box, err := New("enc:v1:", v.Material)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		got, err := box.OpenSealed(c.Sealed)
		if err != nil {
			t.Fatalf("open %.20q: %v", c.Plain, err)
		}
		if got != c.Plain {
			t.Fatalf("got %q want %q", got, c.Plain)
		}
	}
}

func TestRoundTripAndIdempotentSeal(t *testing.T) {
	box, _ := New("enc:v1:", "  material  ")
	sealed, err := box.Seal("secret")
	if err != nil || !strings.HasPrefix(sealed, "enc:v1:") {
		t.Fatalf("seal: %q %v", sealed, err)
	}
	again, _ := box.Seal(sealed)
	if again != sealed {
		t.Fatal("sealing a sealed value must not change it")
	}
	if got, _ := box.Open(sealed); got != "secret" {
		t.Fatalf("got %q", got)
	}
	if got, _ := box.Seal(""); got != "" {
		t.Fatal("empty stays empty")
	}
}

func TestTamperAndWrongKeyAreRejected(t *testing.T) {
	box, _ := New("enc:v1:", "k1")
	sealed, _ := box.Seal("secret")
	flipped := sealed[:len(sealed)-3] + "AAA"
	if _, err := box.OpenSealed(flipped); err == nil {
		t.Fatal("tampered value opened")
	}
	other, _ := New("enc:v1:", "k2")
	if _, err := other.OpenSealed(sealed); err == nil {
		t.Fatal("wrong key opened")
	}
	if _, err := box.OpenSealed("plaintext"); err == nil {
		t.Fatal("unsealed value must fail strict open")
	}
	if got, _ := box.Open("legacy-plaintext"); got != "legacy-plaintext" {
		t.Fatal("legacy plaintext passes through Open")
	}
}

func TestEmptyMaterialRejected(t *testing.T) {
	if _, err := New("p:", "   "); err != ErrNoKey {
		t.Fatalf("want ErrNoKey, got %v", err)
	}
}

// Prints a Go-sealed value so a Node script can prove the reverse direction.
func TestEmitGoSealedForNode(t *testing.T) {
	path := os.Getenv("SECRETBOX_EMIT")
	if path == "" {
		t.Skip("set SECRETBOX_EMIT to write a Go-sealed vector for Node")
	}
	v := loadVectors(t)
	box, _ := New("enc:v1:", v.Material)
	out := map[string]string{}
	for _, c := range v.Cases {
		sealed, _ := box.Seal(c.Plain)
		out[c.Plain] = sealed
	}
	raw, _ := json.Marshal(out)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
