// Package buildinfo displays build provenance without gating runtime commands.
package buildinfo

import "runtime"

var ProjectCommit = ""
var Release = "development"

type Info struct {
	ProjectCommit string `json:"project_commit,omitempty"`
	Release       string `json:"release"`
	Go            string `json:"go"`
}

func Current() Info {
	return Info{ProjectCommit: ProjectCommit, Release: Release, Go: runtime.Version()}
}
