// Package ytguard holds build information and files embedded into the
// ytguard binary.
package ytguard

import (
	"embed"
	"strconv"
	"strings"
)

// Extension is the Chrome extension source. The daemon packs it into a
// signed CRX at startup.
//
//go:embed extension
var Extension embed.FS

// Set at build time with -ldflags "-X ytguard.Version=... -X ytguard.Repo=...".
// The Makefile derives them from git; release builds from the tag and the
// GitHub repository.
var (
	// Version is a git tag like "v1.2.0", or "v1.2.0-3-gabc1234" for a build
	// 3 commits after a tag, or "dev".
	Version = "dev"
	// Repo is the GitHub "owner/name" checked for new releases ("" = none).
	Repo = ""
)

// SemVer parses Version-style strings into [major, minor, patch, commits].
// ok is false for builds without a version tag.
func SemVer(v string) (parts [4]int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v = strings.TrimSuffix(v, "-dirty")
	base, rest, _ := strings.Cut(v, "-")
	nums := strings.Split(base, ".")
	if len(nums) != 3 {
		return parts, false
	}
	for i, n := range nums {
		x, err := strconv.Atoi(n)
		if err != nil || x < 0 {
			return parts, false
		}
		parts[i] = x
	}
	// git describe: "<commits>-g<hash>"
	if c, _, found := strings.Cut(rest, "-g"); found {
		if x, err := strconv.Atoi(c); err == nil {
			parts[3] = x
		}
	}
	return parts, true
}

// Newer reports whether version a is newer than b. Unparseable versions
// are never newer.
func Newer(a, b string) bool {
	pa, okA := SemVer(a)
	pb, okB := SemVer(b)
	if !okA {
		return false
	}
	if !okB {
		return true
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

// ExtensionVersion is the Chrome extension version for this build. Chrome
// only installs an update when this number goes up, so it's derived from
// the release tag (plus commits since the tag for in-between builds).
func ExtensionVersion() string {
	p, ok := SemVer(Version)
	if !ok {
		return "0.0.1" // untagged dev build
	}
	if p[3] > 0 {
		return strconv.Itoa(p[0]) + "." + strconv.Itoa(p[1]) + "." + strconv.Itoa(p[2]) + "." + strconv.Itoa(p[3])
	}
	return strconv.Itoa(p[0]) + "." + strconv.Itoa(p[1]) + "." + strconv.Itoa(p[2])
}
