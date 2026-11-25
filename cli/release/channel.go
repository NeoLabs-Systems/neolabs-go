// Package release is the update side shared by the NeoLabs CLIs: release
// channels and version ordering, Ed25519-signed manifests, and verified
// downloads of signed release assets from GitHub.
package release

import (
	"regexp"
	"strconv"
	"strings"
)

// Channels: stable follows main releases, beta takes prereleases too.
const (
	Stable = "stable"
	Beta   = "beta"
)

// ParseChannel accepts the channel names and aliases the CLIs always took;
// "" means not a channel.
func ParseChannel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "stable", "normal", "default", "latest", "main":
		return Stable
	case "beta", "preview", "prerelease", "pre-release":
		return Beta
	}
	return ""
}

// NormalizeChannel is ParseChannel with stable as the default.
func NormalizeChannel(value string) string {
	if c := ParseChannel(value); c != "" {
		return c
	}
	return Stable
}

// ChannelLabel is "Stable" or "Beta".
func ChannelLabel(channel string) string {
	if NormalizeChannel(channel) == Beta {
		return "Beta"
	}
	return "Stable"
}

var semverPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

type semver struct {
	core [3]int
	pre  []string
}

func parseSemver(v string) (semver, bool) {
	m := semverPattern.FindStringSubmatch(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	if m == nil {
		return semver{}, false
	}
	var s semver
	for i := range 3 {
		s.core[i], _ = strconv.Atoi(m[i+1])
	}
	if m[4] != "" {
		s.pre = strings.Split(m[4], ".")
	}
	return s, true
}

func numeric(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// CompareVersions orders semantic versions (a leading "v" is ignored);
// unparseable versions sort first. It returns <0, 0 or >0.
func CompareVersions(a, b string) int {
	l, lok := parseSemver(a)
	r, rok := parseSemver(b)
	switch {
	case !lok && !rok:
		return 0
	case !lok:
		return -1
	case !rok:
		return 1
	}
	for i := range 3 {
		if l.core[i] != r.core[i] {
			return l.core[i] - r.core[i]
		}
	}
	switch {
	case len(l.pre) == 0 && len(r.pre) == 0:
		return 0
	case len(l.pre) == 0:
		return 1
	case len(r.pre) == 0:
		return -1
	}
	for i := 0; i < max(len(l.pre), len(r.pre)); i++ {
		if i >= len(l.pre) {
			return -1
		}
		if i >= len(r.pre) {
			return 1
		}
		ln, lnum := numeric(l.pre[i])
		rn, rnum := numeric(r.pre[i])
		switch {
		case lnum && rnum:
			if ln != rn {
				return ln - rn
			}
		case lnum:
			return -1
		case rnum:
			return 1
		default:
			if c := strings.Compare(l.pre[i], r.pre[i]); c != 0 {
				return c
			}
		}
	}
	return 0
}
