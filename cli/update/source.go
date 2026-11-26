// Package update is the one way a NeoLabs app replaces itself: find the signed
// runtime for this computer, download and unpack it, switch to it, and go back
// if it does not come up. Apps keep what is theirs: backups, their service and
// how they check that the server answers.
package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/cli"
	"github.com/NeoLabs-Systems/neolabs-go/cli/release"
)

// Source is where an app's runtimes are published and how they are trusted.
type Source struct {
	Client         *release.Client
	Repository     string // owner/name
	ManifestPrefix string // e.g. "neoagent-runtime-manifest-"
	PublicKey      string // base64 Ed25519
	SchemaVersion  int
	Targets        release.Targets
	// Platform and Architecture default to this computer.
	Platform, Architecture string
}

// Candidate is a verified runtime for this computer.
type Candidate struct {
	Release  release.Release
	Version  string
	Artifact release.Artifact
}

// Host names this computer the way release assets do.
func Host() (platform, architecture string) {
	platform, architecture = runtime.GOOS, runtime.GOARCH
	if platform == "darwin" {
		platform = "macos"
	}
	if architecture == "amd64" {
		architecture = "x64"
	}
	return platform, architecture
}

func invalidManifest(format string, args ...any) error {
	return cli.Errorf("SETUP_MANIFEST_INVALID", format, args...)
}

// Find is the newest runtime of the channel for this computer.
func (s Source) Find(ctx context.Context, channel string) (Candidate, error) {
	rel, err := s.Client.ResolveRelease(ctx, s.Repository, channel, s.ManifestPrefix)
	if err != nil {
		return Candidate{}, err
	}
	return s.Select(ctx, rel)
}

// Select verifies one release's manifest and picks this computer's runtime.
func (s Source) Select(ctx context.Context, rel release.Release) (Candidate, error) {
	if rel.ManifestName == "" || rel.Assets[rel.ManifestName] == "" || rel.Assets[rel.ManifestName+".sig"] == "" {
		return Candidate{}, cli.Errorf("SETUP_RUNTIME_NOT_PUBLISHED", "This release has no signed runtime manifest.")
	}
	raw, err := s.Client.FetchVerifiedManifest(ctx, rel, s.PublicKey)
	if err != nil {
		return Candidate{}, err
	}
	var manifest release.RuntimeManifest
	if json.Unmarshal(raw, &manifest) != nil || manifest.SchemaVersion != s.SchemaVersion {
		return Candidate{}, invalidManifest("The runtime manifest is invalid.")
	}
	version, err := release.NormalizeVersion(manifest.Version)
	if err != nil {
		return Candidate{}, invalidManifest("The runtime manifest is invalid.")
	}
	// A manifest copied from another release must not pass as this one.
	if rel.Tag != "" && version != strings.TrimPrefix(rel.Tag, "v") {
		return Candidate{}, invalidManifest("The runtime manifest does not match its release.")
	}
	platform, architecture := s.Platform, s.Architecture
	if platform == "" {
		platform, architecture = Host()
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.Platform != platform || artifact.Architecture != architecture {
			continue
		}
		chosen, err := release.NormalizeArtifact(artifact, s.Targets)
		if err != nil {
			return Candidate{}, invalidManifest("%s", err)
		}
		if rel.Assets[chosen.AssetName] == "" {
			return Candidate{}, cli.Errorf("SETUP_RUNTIME_ASSET_MISSING", "The selected runtime asset is missing.")
		}
		return Candidate{Release: rel, Version: version, Artifact: chosen}, nil
	}
	return Candidate{}, cli.Errorf("SETUP_PLATFORM_UNSUPPORTED", "No runtime is available for this computer.")
}

// Download fetches and verifies the runtime archive into a staging folder next
// to the store's versions and returns its path.
func (s Source) Download(ctx context.Context, store release.RuntimeStore, c Candidate) (string, error) {
	versions := filepath.Join(store.Root, "versions")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(versions, ".staging-")
	if err != nil {
		return "", err
	}
	archive := filepath.Join(staging, c.Artifact.AssetName)
	if err := s.Client.Download(ctx, c.Release.Assets[c.Artifact.AssetName], archive, c.Artifact.SizeBytes, c.Artifact.SHA256); err != nil {
		os.RemoveAll(staging)
		return "", err
	}
	return archive, nil
}

// Unpack installs a downloaded archive as its version, without activating it,
// and removes the staging folder. A complete installed version is kept unless
// force is set.
func Unpack(store release.RuntimeStore, version, archive string, force bool) error {
	defer os.RemoveAll(filepath.Dir(archive))
	if !force && store.Validate(version) == nil {
		return nil
	}
	return store.InstallArchive(version, archive)
}
