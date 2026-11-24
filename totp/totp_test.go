package totp

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

type vectors struct {
	Totp []struct {
		Secret  string
		Epoch   int64
		Current string
		Tokens  map[string]string
		Verdict map[string]bool
	}
	Uris []struct{ Issuer, Label, Secret, URI string }
}

func load(t *testing.T) vectors {
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

// Codes and accept/reject decisions come from otplib 13.4.1 with the options
// the Node server used (6 digits, 30 s, epochTolerance 30).
func TestMatchesOtplib(t *testing.T) {
	v := load(t)
	checks := 0
	for _, c := range v.Totp {
		now := time.Unix(c.Epoch, 0)
		got, err := Code(c.Secret, now)
		if err != nil || got != c.Current {
			t.Fatalf("code at %d: got %q err %v want %q", c.Epoch, got, err, c.Current)
		}
		for off, token := range c.Tokens {
			if want, got := c.Verdict[off], Verify(c.Secret, token, now); got != want {
				n, _ := strconv.Atoi(off)
				t.Errorf("secret %.6s epoch %d: token from offset %d: got valid=%v want %v", c.Secret, c.Epoch, n, got, want)
			}
			checks++
		}
	}
	if checks < 150 {
		t.Fatalf("only %d checks ran", checks)
	}
}

func TestRFC6238AndInputHandling(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if got, _ := Code(secret, time.Unix(59, 0)); got != "287082" {
		t.Fatalf("RFC 6238 vector: %s", got)
	}
	now := time.Unix(1_700_000_000, 0)
	code, _ := Code(secret, now)
	spaced := code[:3] + " " + code[3:]
	if !Verify(secret, " "+spaced+"\n", now) {
		t.Fatal("pasted code with spaces must verify")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "12 34 5x"} {
		if Verify(secret, bad, now) {
			t.Fatalf("%q must not verify", bad)
		}
	}
	if Verify("not base32!!", code, now) || Verify("AAAA", code, now) {
		t.Fatal("bad secrets never verify")
	}
}

func TestURIMatchesOtplib(t *testing.T) {
	for _, u := range load(t).Uris {
		if got := URI(u.Issuer, u.Label, u.Secret); got != u.URI {
			t.Errorf("\n got: %s\nwant: %s", got, u.URI)
		}
	}
}

func TestSecretsAndRecoveryCodes(t *testing.T) {
	s, err := GenerateSecret()
	if err != nil || len(s) != 32 || !regexp.MustCompile(`^[A-Z2-7]+$`).MatchString(s) {
		t.Fatalf("secret %q %v", s, err)
	}
	if _, err := Code(s, time.Now()); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := NewRecoveryCode()
		if err != nil || !regexp.MustCompile(`^[A-HJ-NP-Z2-9]{5}-[A-HJ-NP-Z2-9]{5}$`).MatchString(c) {
			t.Fatalf("recovery code %q %v", c, err)
		}
		if seen[c] {
			t.Fatalf("duplicate %s", c)
		}
		seen[c] = true
	}
	if NormalizeRecoveryCode(" abcde-fgh23 ") != "ABCDEFGH23" || NormalizeCode("ab cd") != "ABCD" {
		t.Fatal("normalization")
	}
	if strings.Contains(recoveryAlphabet, "O") || strings.Contains(recoveryAlphabet, "0") || strings.Contains(recoveryAlphabet, "I") {
		t.Fatal("alphabet must avoid look-alikes")
	}
}
