// Package buildinfo contains values stamped into product binaries at build time.
package buildinfo

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
