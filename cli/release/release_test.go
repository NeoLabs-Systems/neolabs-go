package release

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NeoLabs-Systems/neolabs-go/cli"
)

// Vectors from the Node implementation (runtime/release_channel.js).
func TestCompareVersionsMatchesNode(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.4", -1}, {"1.2.3-beta.2", "1.2.3-beta.10", -1}, {"1.2.3", "1.2.3-beta.1", 1},
		{"v2.0.0", "1.9.9", 1}, {"1.0.0-alpha", "1.0.0-1", 1}, {"x", "1.0.0", -1},
		{"1.0.0-beta", "1.0.0-beta.1", -1}, {"3.8.1-beta.50", "3.8.1", -1}, {"1.0.0", "1.0.0", 0},
	} {
		got := CompareVersions(c.a, c.b)
		if (got > 0) != (c.want > 0) || (got < 0) != (c.want < 0) {
			t.Errorf("CompareVersions(%q, %q) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseChannel(t *testing.T) {
	for in, want := range map[string]string{"main": Stable, " Latest ": Stable, "pre-release": Beta, "beta": Beta, "nightly": ""} {
		if got := ParseChannel(in); got != want {
			t.Errorf("ParseChannel(%q) = %q, want %q", in, got, want)
		}
	}
	if NormalizeChannel("x") != Stable || ChannelLabel("beta") != "Beta" {
		t.Fatal("defaults")
	}
}

// A key pair, signature and normalized key produced by Node's crypto module.
const (
	nodePriv = "MC4CAQAwBQYDK2VwBCIEIBdc+aNMSM/oaYxZGIAe4Jdd9j6Qj0g4tP39vs6Dwnxx"
	nodeSPKI = "MCowBQYDK2VwAyEATnX13CO2lvN1XukoM4VzRFEttAE4aNzCK29L5wJl0WQ="
	nodeRaw  = "TnX13CO2lvN1XukoM4VzRFEttAE4aNzCK29L5wJl0WQ="
	nodeSig  = "T8TZV/xoXrJqjqH6AGgDUIuZ+z5j9WqCaU75H21nK+nO/gbopXGbE1j/HU3gflReLWUEa8M9NOIk3lwzcGyHCQ=="
)

func TestSigningMatchesNode(t *testing.T) {
	sig, err := Sign([]byte("hello\n"), nodePriv)
	if err != nil || sig != nodeSig {
		t.Fatalf("Sign = %q, %v", sig, err)
	}
	if raw, err := NormalizePublicKey(nodeSPKI); err != nil || raw != nodeRaw {
		t.Fatalf("NormalizePublicKey = %q, %v", raw, err)
	}
	for _, key := range []string{nodeSPKI, nodeRaw} {
		if ok, err := Verify([]byte("hello\n"), nodeSig+"\n", key); err != nil || !ok {
			t.Fatalf("Verify(%s) = %v, %v", key, ok, err)
		}
	}
	if ok, _ := Verify([]byte("hello"), nodeSig, nodeRaw); ok {
		t.Fatal("tampered data verified")
	}
	if err := CheckKeypair(nodePriv, nodeRaw); err != nil {
		t.Fatal(err)
	}
	if err := CheckKeypair(nodePriv, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); err == nil {
		t.Fatal("mismatched key pair accepted")
	}
	if _, err := ParsePublicKey("AAAA"); err == nil {
		t.Fatal("short key accepted")
	}
}

var targets = Targets{"macos": {"x64", "arm64"}, "linux": {"x64"}}

// The serialized manifest is byte-identical to Node's serializeRuntimeManifest.
func TestRuntimeManifestMatchesNode(t *testing.T) {
	m, err := NewRuntimeManifest(1, "v1.2.3", []Artifact{
		{Platform: "macos", Architecture: "x64", AssetName: "b.zip", SHA256: strings.Repeat("A", 64), SizeBytes: 5},
		{Platform: "linux", Architecture: "x64", AssetName: "a.zip", SHA256: strings.Repeat("b", 64), SizeBytes: 7},
	}, targets)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := Serialize(m)
	want := "{\n  \"schemaVersion\": 1,\n  \"version\": \"1.2.3\",\n  \"artifacts\": [\n    {\n      \"platform\": \"linux\",\n      \"architecture\": \"x64\",\n      \"assetName\": \"a.zip\",\n      \"sha256\": \"" + strings.Repeat("b", 64) + "\",\n      \"sizeBytes\": 7\n    },\n    {\n      \"platform\": \"macos\",\n      \"architecture\": \"x64\",\n      \"assetName\": \"b.zip\",\n      \"sha256\": \"" + strings.Repeat("a", 64) + "\",\n      \"sizeBytes\": 5\n    }\n  ]\n}\n"
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	for _, bad := range [][]Artifact{
		nil,
		{{Platform: "linux", Architecture: "arm64", AssetName: "a.zip", SHA256: strings.Repeat("b", 64), SizeBytes: 7}},
		{{Platform: "linux", Architecture: "x64", AssetName: "../a.zip", SHA256: strings.Repeat("b", 64), SizeBytes: 7}},
		{{Platform: "linux", Architecture: "x64", AssetName: "a.zip", SHA256: strings.Repeat("b", 64), SizeBytes: 7}, {Platform: "linux", Architecture: "x64", AssetName: "c.zip", SHA256: strings.Repeat("b", 64), SizeBytes: 7}},
	} {
		if _, err := NewRuntimeManifest(1, "1.0.0", bad, targets); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestReleaseManifestSkipsSignaturesAndMetadata(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"b.zip": "bb", "a.zip": "a", "a.zip.sig": "x", "x-metadata-linux.json": "{}", "app-release-manifest-1.json": "{}"} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	m, err := NewReleaseManifest(1, "v1.0.0", dir, "app-release-manifest-")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Assets) != 2 || m.Assets[0].Name != "a.zip" || m.Assets[1].SizeBytes != 2 || m.Version != "1.0.0" {
		t.Fatalf("%+v", m)
	}
	out := filepath.Join(dir, "app-release-manifest-1.json")
	data, _ := Serialize(m)
	if err := WriteSigned(out, data, nodePriv, nodeRaw); err != nil {
		t.Fatal(err)
	}
	sig, _ := os.ReadFile(out + ".sig")
	if ok, _ := Verify(data, string(sig), nodeRaw); !ok {
		t.Fatal("written signature does not verify")
	}
}

func TestZipRoundTripAndUnsafeEntries(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "app", "bin"), 0o755)
	os.WriteFile(filepath.Join(src, "app", "bin", "tool"), []byte("#!/bin/sh\n"), 0o755)
	archive := filepath.Join(t.TempDir(), "a.zip")
	if err := CreateZip(src, archive); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := ExtractZip(archive, dest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dest, "app", "bin", "tool"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("mode lost: %v %v", info, err)
	}

	evil := filepath.Join(t.TempDir(), "evil.zip")
	f, _ := os.Create(evil)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../escape.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	err = ExtractZip(evil, t.TempDir())
	if cli.CodeOf(err, "") != "SETUP_RUNTIME_ARCHIVE_INVALID" {
		t.Fatalf("unsafe archive: %v", err)
	}
}

func TestResolveAndDownloadVerified(t *testing.T) {
	payload := []byte("runtime archive bytes")
	sum := sha256.Sum256(payload)
	manifest := []byte(`{"schemaVersion":1}`)
	sig, _ := Sign(manifest, nodePriv)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			fmt.Fprintf(w, `[{"tag_name":"v2.0.0-beta.1","prerelease":true,"assets":[{"name":"rt-manifest-2.json","browser_download_url":"%[1]s/m"},{"name":"rt-manifest-2.json.sig","browser_download_url":"%[1]s/s"}]},
			{"tag_name":"v1.0.0","assets":[{"name":"rt-manifest-1.json","browser_download_url":"%[1]s/m"},{"name":"rt-manifest-1.json.sig","browser_download_url":"%[1]s/s"},{"name":"a.zip","browser_download_url":"%[1]s/a"}]}]`, srv.URL)
		case "/m":
			w.Write(manifest)
		case "/s":
			w.Write([]byte(sig + "\n"))
		case "/a":
			w.Write(payload)
		}
	}))
	defer srv.Close()
	c := &Client{APIBase: srv.URL, AllowHTTP: true, UserAgent: "test"}
	ctx := context.Background()
	stable, err := c.ResolveRelease(ctx, "o/r", Stable, "rt-manifest-")
	if err != nil || stable.Tag != "v1.0.0" || stable.ManifestName != "rt-manifest-1.json" {
		t.Fatalf("stable %+v %v", stable, err)
	}
	beta, _ := c.ResolveRelease(ctx, "o/r", Beta, "rt-manifest-")
	if beta.Tag != "v2.0.0-beta.1" {
		t.Fatalf("beta %+v", beta)
	}
	if got, err := c.FetchVerifiedManifest(ctx, stable, nodeRaw); err != nil || string(got) != string(manifest) {
		t.Fatalf("manifest %q %v", got, err)
	}
	if _, err := c.FetchVerifiedManifest(ctx, stable, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); cli.CodeOf(err, "") != "SETUP_MANIFEST_SIGNATURE_INVALID" {
		t.Fatalf("wrong key: %v", err)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.zip")
	if err := c.Download(ctx, stable.Assets["a.zip"], good, int64(len(payload)), hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.zip")
	if err := c.Download(ctx, stable.Assets["a.zip"], bad, int64(len(payload)), strings.Repeat("0", 64)); cli.CodeOf(err, "") != "SETUP_RUNTIME_HASH_MISMATCH" {
		t.Fatalf("hash: %v", err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("failed download left a file")
	}
	if err := c.Download(ctx, stable.Assets["a.zip"], filepath.Join(dir, "short"), 3, hex.EncodeToString(sum[:])); cli.CodeOf(err, "") != "SETUP_DOWNLOAD_INCOMPLETE" {
		t.Fatalf("size: %v", err)
	}
	strict := &Client{APIBase: srv.URL}
	if _, err := strict.ResolveRelease(ctx, "o/r", Stable, "rt-manifest-"); cli.CodeOf(err, "") != "SETUP_DOWNLOAD_URL_INVALID" {
		t.Fatalf("http accepted: %v", err)
	}
}
