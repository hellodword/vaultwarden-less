package buildinfo

import (
	"strings"
	"testing"
)

func TestReadyRequiresExactCleanGitIdentity(t *testing.T) {
	originalCommit, originalRelease, originalClean := ProjectCommit, Release, Clean
	t.Cleanup(func() {
		ProjectCommit, Release, Clean = originalCommit, originalRelease, originalClean
	})

	commit := strings.Repeat("a", 40)
	for _, test := range []struct {
		name    string
		commit  string
		release string
		clean   string
		want    bool
	}{
		{name: "exact", commit: commit, release: "git-" + commit, clean: "true", want: true},
		{name: "short commit", commit: strings.Repeat("a", 12), release: "git-" + strings.Repeat("a", 12), clean: "true"},
		{name: "legacy release", commit: commit, release: "v0.0.0-" + commit[:12], clean: "true"},
		{name: "short release", commit: commit, release: "git-" + commit[:12], clean: "true"},
		{name: "uppercase", commit: strings.ToUpper(commit), release: "git-" + strings.ToUpper(commit), clean: "true"},
		{name: "mismatch", commit: commit, release: "git-" + strings.Repeat("b", 40), clean: "true"},
		{name: "dirty", commit: commit, release: "git-" + commit, clean: "false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ProjectCommit, Release, Clean = test.commit, test.release, test.clean
			if got := Ready(); got != test.want {
				t.Fatalf("Ready() = %t, want %t", got, test.want)
			}
		})
	}
}
