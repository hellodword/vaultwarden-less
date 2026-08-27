// Package buildinfo contains values stamped into product binaries at build time.
package buildinfo

import (
	"encoding/hex"
	"regexp"
	"strings"
)

var releasePattern = regexp.MustCompile(`^RELEASE\.[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}Z-g([0-9a-f]{12})$`)

var (
	// Version remains the text-version compatibility value until the schema 1
	// CLI lands. Release builds stamp it with Release.
	Version = "development"

	// ProjectCommit is a full clean Git commit or the empty string for a dirty
	// development build.
	ProjectCommit = ""

	// Release is the immutable release tag or "development".
	Release = "development"

	// Clean is stamped to "true" only for a clean Git flake source.
	Clean = "false"
)

// Ready reports whether the binary has a complete clean Git release identity.
func Ready() bool {
	if Clean != "true" || len(ProjectCommit) != 40 || ProjectCommit != strings.ToLower(ProjectCommit) || Release == "development" {
		return false
	}
	if _, err := hex.DecodeString(ProjectCommit); err != nil {
		return false
	}
	match := releasePattern.FindStringSubmatch(Release)
	return len(match) == 2 && match[1] == ProjectCommit[:12]
}
