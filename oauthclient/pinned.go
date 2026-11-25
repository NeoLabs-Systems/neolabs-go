package oauthclient

import (
	"errors"
	"net/url"
	"strings"
)

// PinnedURL resolves a provider API path without allowing it to change the
// configured scheme or host. The caller must still use an SSRF-safe client.
func PinnedURL(baseURL, path string, query url.Values) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil {
		return "", errors.New("invalid API base URL")
	}
	target, err := url.Parse(strings.TrimSpace(path))
	if err != nil || target.User != nil {
		return "", errors.New("invalid API path")
	}
	if target.IsAbs() && (target.Scheme != base.Scheme || !strings.EqualFold(target.Host, base.Host)) {
		return "", errors.New("API request targets another host")
	}
	if target.Host != "" && !target.IsAbs() {
		return "", errors.New("invalid API path")
	}
	if !target.IsAbs() {
		if !strings.HasPrefix(target.Path, "/") {
			target.Path = "/" + target.Path
		}
		target = base.ResolveReference(target)
	}
	values := target.Query()
	for key, items := range query {
		for _, value := range items {
			if strings.TrimSpace(value) != "" {
				values.Set(key, value)
			}
		}
	}
	target.RawQuery = values.Encode()
	return target.String(), nil
}
