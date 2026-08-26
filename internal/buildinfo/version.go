// Package buildinfo contains values stamped into product binaries at build time.
package buildinfo

// Version is replaced through -ldflags for release builds.
var Version = "dev"
