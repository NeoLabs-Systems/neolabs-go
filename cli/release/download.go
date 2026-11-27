package release

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/cli"
)

// Client downloads release metadata and assets over HTTPS only.
type Client struct {
	HTTP      *http.Client
	UserAgent string
	// APIBase is the GitHub API root; tests point it at a local server.
	APIBase string
	// AllowHTTP permits plain http URLs (tests only).
	AllowHTTP bool
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

func (c *Client) checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && !(c.AllowHTTP && u.Scheme == "http")) {
		return nil, cli.Errorf("SETUP_DOWNLOAD_URL_INVALID", "Runtime downloads require HTTPS.")
	}
	return u, nil
}

func (c *Client) open(ctx context.Context, raw string) (*http.Response, error) {
	u, err := c.checkURL(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, cli.Errorf("SETUP_DOWNLOAD_FAILED", "Download failed: %s", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, cli.Errorf("SETUP_DOWNLOAD_FAILED", "Download failed with HTTP %d.", resp.StatusCode)
	}
	if _, err := c.checkURL(resp.Request.URL.String()); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// Fetch downloads a small document, refusing anything over maxBytes.
func (c *Client) Fetch(ctx context.Context, raw string, maxBytes int64) ([]byte, error) {
	resp, err := c.open(ctx, raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, cli.Errorf("SETUP_DOWNLOAD_TOO_LARGE", "Setup metadata exceeded its safe size limit.")
	}
	return data, nil
}

// Download streams an asset to target (created exclusively, 0600) and
// verifies its size and SHA-256; a failed download leaves no file behind.
func (c *Client) Download(ctx context.Context, raw, target string, size int64, sha string) (err error) {
	resp, err := c.open(ctx, raw)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(target)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, size+1))
	if err != nil {
		return err
	}
	if n > size {
		return cli.Errorf("SETUP_DOWNLOAD_INCOMPLETE", "The runtime download exceeded its manifest size.")
	}
	if n != size {
		return cli.Errorf("SETUP_DOWNLOAD_INCOMPLETE", "The runtime download was incomplete.")
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		return cli.Errorf("SETUP_RUNTIME_HASH_MISMATCH", "The runtime did not pass checksum verification.")
	}
	return f.Sync()
}

// Release is a published release with its asset download URLs.
type Release struct {
	Tag          string
	Prerelease   bool
	Assets       map[string]string
	ManifestName string
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func releaseWithManifest(r githubRelease, channel, manifestPrefix string) (Release, bool) {
	if r.Draft || (NormalizeChannel(channel) != Beta && r.Prerelease) {
		return Release{}, false
	}
	rel := Release{Tag: r.TagName, Prerelease: r.Prerelease, Assets: map[string]string{}}
	for _, asset := range r.Assets {
		rel.Assets[asset.Name] = asset.URL
	}
	for _, asset := range r.Assets {
		if strings.HasPrefix(asset.Name, manifestPrefix) && strings.HasSuffix(asset.Name, ".json") && rel.Assets[asset.Name+".sig"] != "" {
			rel.ManifestName = asset.Name
			return rel, true
		}
	}
	return Release{}, false
}

// ResolveRelease returns the newest release on the channel (stable skips
// prereleases) that carries a `<prefix>*.json` manifest and its `.sig`.
func (c *Client) ResolveRelease(ctx context.Context, repository, channel, manifestPrefix string) (Release, error) {
	base := c.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	data, err := c.Fetch(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=20", base, repository), 4<<20)
	if err != nil {
		return Release{}, err
	}
	var releases []githubRelease
	if err := json.Unmarshal(data, &releases); err != nil {
		return Release{}, cli.Errorf("SETUP_MANIFEST_INVALID", "The release list could not be read.")
	}
	// GitHub's releases collection can briefly lag a newly published beta.
	// Tags are indexed independently, so resolve the newest version tag
	// directly before falling back to the collection response.
	tagData, tagErr := c.Fetch(ctx, fmt.Sprintf("%s/repos/%s/tags?per_page=100", base, repository), 2<<20)
	if tagErr == nil {
		var tags []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(tagData, &tags) == nil {
			byTag := make(map[string]githubRelease, len(releases))
			for _, r := range releases {
				byTag[r.TagName] = r
			}
			for _, tag := range tags {
				version, err := NormalizeVersion(tag.Name)
				if err != nil || (NormalizeChannel(channel) != Beta && strings.Contains(version, "-")) {
					continue
				}
				r, ok := byTag[tag.Name]
				if !ok {
					tagURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", base, repository, url.PathEscape(tag.Name))
					body, err := c.Fetch(ctx, tagURL, 4<<20)
					if err != nil || json.Unmarshal(body, &r) != nil {
						continue
					}
				}
				if rel, ok := releaseWithManifest(r, channel, manifestPrefix); ok {
					return rel, nil
				}
			}
		}
	}
	for _, r := range releases {
		if rel, ok := releaseWithManifest(r, channel, manifestPrefix); ok {
			return rel, nil
		}
	}
	return Release{}, cli.Errorf("SETUP_RUNTIME_NOT_PUBLISHED", "No verified %s runtime is published yet.", NormalizeChannel(channel))
}

// FetchVerifiedManifest downloads a release's manifest and signature and
// checks the signature against publicKey before returning the bytes.
func (c *Client) FetchVerifiedManifest(ctx context.Context, rel Release, publicKey string) ([]byte, error) {
	manifest, err := c.Fetch(ctx, rel.Assets[rel.ManifestName], 2<<20)
	if err != nil {
		return nil, err
	}
	sig, err := c.Fetch(ctx, rel.Assets[rel.ManifestName+".sig"], 8192)
	if err != nil {
		return nil, err
	}
	ok, err := Verify(manifest, string(sig), publicKey)
	if err != nil {
		return nil, cli.Errorf("SETUP_TRUST_NOT_CONFIGURED", "The embedded runtime verification key is invalid.")
	}
	if !ok {
		return nil, cli.Errorf("SETUP_MANIFEST_SIGNATURE_INVALID", "The runtime manifest did not pass signature verification.")
	}
	return manifest, nil
}

// ExtractZip unpacks archive into dest, refusing entries that would land
// outside it and keeping the stored permission bits (never setuid).
func ExtractZip(archive, dest string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return cli.Errorf("SETUP_RUNTIME_ARCHIVE_INVALID", "The runtime archive could not be opened: %s", err)
	}
	defer r.Close()
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for _, f := range r.File {
		target := filepath.Join(root, filepath.FromSlash(f.Name))
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return cli.Errorf("SETUP_RUNTIME_ARCHIVE_INVALID", "The runtime archive contains an unsafe path.")
		}
	}
	for _, f := range r.File {
		target := filepath.Join(root, filepath.FromSlash(f.Name))
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			return cli.Errorf("SETUP_RUNTIME_ARCHIVE_INVALID", "The runtime archive contains a symbolic link.")
		default:
			if err := extractFile(f, target, mode.Perm()&0o755|0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractFile(f *zip.File, target string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// CreateZip archives every file under dir (paths relative to it, forward
// slashes, permissions kept) into archive.
func CreateZip(dir, archive string) error {
	out, err := os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to archive symbolic link %s", rel)
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
			_, err = zw.CreateHeader(hdr)
			return err
		}
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err := zw.Close(); walkErr == nil {
		walkErr = err
	}
	if err := out.Close(); walkErr == nil {
		walkErr = err
	}
	return walkErr
}

// ReleaseByTag returns the release published under tag, which must carry a
// `<prefix>*.json` manifest and its `.sig`.
func (c *Client) ReleaseByTag(ctx context.Context, repository, tag, manifestPrefix string) (Release, error) {
	version, err := NormalizeVersion(tag)
	if err != nil {
		return Release{}, err
	}
	base := c.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	data, err := c.Fetch(ctx, fmt.Sprintf("%s/repos/%s/releases/tags/%s", base, repository, url.PathEscape("v"+version)), 4<<20)
	if err != nil {
		return Release{}, err
	}
	var found githubRelease
	if err := json.Unmarshal(data, &found); err != nil {
		return Release{}, cli.Errorf("SETUP_MANIFEST_INVALID", "The release could not be read.")
	}
	rel, ok := releaseWithManifest(found, Beta, manifestPrefix)
	if !ok {
		return Release{}, cli.Errorf("SETUP_RUNTIME_NOT_PUBLISHED", "Release v%s has no verified runtime.", version)
	}
	return rel, nil
}
