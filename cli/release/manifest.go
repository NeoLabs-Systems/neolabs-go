package release

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/cli"
)

// Artifact is one platform's runtime archive in a runtime manifest.
type Artifact struct {
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
	AssetName    string `json:"assetName"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"sizeBytes"`
}

// RuntimeManifest lists the runtime archives of one version; it is signed
// and published next to them.
type RuntimeManifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	Version       string     `json:"version"`
	Artifacts     []Artifact `json:"artifacts"`
}

// Asset is one file of a release manifest.
type Asset struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

// ReleaseManifest lists every asset of a release with its digest.
type ReleaseManifest struct {
	SchemaVersion int     `json:"schemaVersion"`
	Version       string  `json:"version"`
	Assets        []Asset `json:"assets"`
}

// Targets maps a platform to its supported architectures.
type Targets map[string][]string

var (
	assetNamePattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]*$`)
	sha256Pattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	versionPattern   = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]*$`)
)

// NormalizeVersion trims a leading "v" and validates the version text.
func NormalizeVersion(v string) (string, error) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if !versionPattern.MatchString(v) {
		return "", cli.Errorf("RUNTIME_MANIFEST_VERSION_REQUIRED", "A valid runtime manifest version is required.")
	}
	return v, nil
}

// NormalizeArtifact validates one artifact against the supported targets.
func NormalizeArtifact(a Artifact, targets Targets) (Artifact, error) {
	a.Platform = strings.TrimSpace(a.Platform)
	a.Architecture = strings.TrimSpace(a.Architecture)
	a.AssetName = strings.TrimSpace(a.AssetName)
	a.SHA256 = strings.ToLower(strings.TrimSpace(a.SHA256))
	if a.Platform == "" || a.Architecture == "" || !slices.Contains(targets[a.Platform], a.Architecture) ||
		!assetNamePattern.MatchString(a.AssetName) || !sha256Pattern.MatchString(a.SHA256) || a.SizeBytes <= 0 {
		return Artifact{}, cli.Errorf("RUNTIME_MANIFEST_INVALID", "Invalid runtime artifact metadata.")
	}
	return a, nil
}

// NewRuntimeManifest validates, sorts and checks the artifacts for duplicates.
func NewRuntimeManifest(schemaVersion int, version string, artifacts []Artifact, targets Targets) (RuntimeManifest, error) {
	v, err := NormalizeVersion(version)
	if err != nil {
		return RuntimeManifest{}, err
	}
	out := make([]Artifact, 0, len(artifacts))
	for _, a := range artifacts {
		n, err := NormalizeArtifact(a, targets)
		if err != nil {
			return RuntimeManifest{}, err
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return RuntimeManifest{}, cli.Errorf("RUNTIME_MANIFEST_EMPTY", "At least one runtime artifact is required.")
	}
	slices.SortStableFunc(out, func(l, r Artifact) int {
		return cmp.Or(strings.Compare(l.Platform, r.Platform), strings.Compare(l.Architecture, r.Architecture))
	})
	seen := map[string]bool{}
	for _, a := range out {
		key := a.Platform + ":" + a.Architecture
		if seen[key] {
			return RuntimeManifest{}, cli.Errorf("RUNTIME_MANIFEST_DUPLICATE", "Duplicate runtime artifact for %s.", key)
		}
		seen[key] = true
	}
	return RuntimeManifest{SchemaVersion: schemaVersion, Version: v, Artifacts: out}, nil
}

// Serialize renders v as two-space indented JSON with a trailing newline,
// the exact bytes that get signed.
func Serialize(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// FileDigest returns the SHA-256 hex digest and size of a file.
func FileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// NewReleaseManifest digests every file in dir except signatures, metadata
// files and earlier release manifests (prefix names them), sorted by name.
func NewReleaseManifest(schemaVersion int, version, dir, manifestPrefix string) (ReleaseManifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ReleaseManifest{}, err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasSuffix(name, ".sig") || strings.Contains(name, "-metadata-") || strings.HasPrefix(name, manifestPrefix) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	m := ReleaseManifest{SchemaVersion: schemaVersion, Version: strings.TrimPrefix(version, "v")}
	for _, name := range names {
		sum, size, err := FileDigest(filepath.Join(dir, name))
		if err != nil {
			return m, err
		}
		m.Assets = append(m.Assets, Asset{Name: name, SizeBytes: size, SHA256: sum})
	}
	if len(m.Assets) == 0 {
		return m, fmt.Errorf("No release assets are available for signing.")
	}
	return m, nil
}

// WriteSigned signs data with privateKey, checks the signature against
// publicKey and writes path and path+".sig".
func WriteSigned(path string, data []byte, privateKey, publicKey string) error {
	if err := CheckKeypair(privateKey, publicKey); err != nil {
		return err
	}
	sig, err := Sign(data, privateKey)
	if err != nil {
		return err
	}
	if ok, err := Verify(data, sig, publicKey); err != nil || !ok {
		return fmt.Errorf("manifest signature verification failed")
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(path+".sig", []byte(sig+"\n"), 0o644)
}
