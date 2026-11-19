package e2e_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// Run the shell block from the same offline release as its images.
func (h *e2eHarness) runExample(directory, document, marker string, replacements ...string) {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.releaseRoot, "docs", document))
	if err != nil {
		h.t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "<!-- "+marker+" -->\n```sh\n")
	script, _, closed := strings.Cut(section, "\n```")
	if !found || !closed {
		h.t.Fatalf("missing shell example %s in %s", marker, document)
	}
	env := append(append([]string{}, h.dockerEnv...), "DOCKER_HOST=unix://"+*dockerSocket)
	output, err := runCommand(h.ctx, directory, nil, env, "bash", "-c", strings.NewReplacer(replacements...).Replace(script))
	if err != nil {
		h.t.Fatalf("%s: %v: %s", marker, err, harnessDiagnostics(output))
	}
}

func (h *e2eHarness) checkDatabaseCopies() {
	h.t.Helper()
	root := filepath.Join(h.testRoot, "handbook")
	deployment := filepath.Join(root, "deployment")
	for _, path := range []string{"previous/data", "deployment/data", "deployment/cache", "deployment/credentials", "deployment/state", "deployment/config"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
			h.t.Fatal(err)
		}
	}
	source := filepath.Join(root, "previous/data")
	// Abrupt exit leaves the committed edit in the WAL, as in a cold filesystem copy.
	h.mustDocker("run", "--rm", "--pull=never", "--mount", "type=bind,source="+source+",target=/source",
		"--entrypoint", "/bin/python3", *motoImage, "-c", `import os, sqlite3
conn = sqlite3.connect('/source/db.sqlite3')
conn.execute('PRAGMA journal_mode=WAL')
conn.execute('CREATE TABLE vault(item)')
conn.execute("INSERT INTO vault VALUES ('saved edit')")
conn.commit()
os._exit(0)`)
	walPath := filepath.Join(source, "db.sqlite3-wal")
	wal, err := os.ReadFile(walPath)
	if err != nil || len(wal) <= 32 {
		h.t.Fatal("fixture has no committed WAL")
	}
	h.runExample(deployment, "deployment.md", "sqlite-import")
	if after, err := os.ReadFile(walPath); err != nil || !bytes.Equal(wal, after) {
		h.t.Fatal("import changed source WAL")
	}

	h.runExample(deployment, "maintenance.md", "upgrade-copy")
	query := "SELECT item FROM vault"
	next := filepath.Join(root, "vaultwarden-next/data")
	if got := h.sqlite(next, query); got != "saved edit" {
		h.t.Fatalf("upgrade copy has %q", got)
	}
	h.sqlite(next, "UPDATE vault SET item='upgraded'")
	if got := h.sqlite(filepath.Join(deployment, "data"), query); got != "saved edit" {
		h.t.Fatalf("rollback copy has %q", got)
	}
}
