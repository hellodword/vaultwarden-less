package e2e_test

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func (h *e2eHarness) singleFileIn(relativeDirectory string) string {
	h.t.Helper()
	relativeDirectory = cleanTestRelative(h.t, relativeDirectory)
	directory := filepath.Join(h.localRoot, filepath.FromSlash(relativeDirectory))
	entries, err := os.ReadDir(directory)
	if err != nil {
		h.t.Fatalf("list file Send data: %v", err)
	}
	if len(entries) != 1 || !lowerHex64Pattern.MatchString(entries[0].Name()) {
		h.t.Fatalf("file Send directory contains unexpected entries: %#v", entries)
	}
	info, err := entries[0].Info()
	if err != nil || !info.Mode().IsRegular() {
		h.t.Fatalf("file Send entry is not a regular file: %v", err)
	}
	return relativeDirectory + "/" + entries[0].Name()
}

func (h *e2eHarness) holdDataFile(relative string) func() {
	h.t.Helper()
	relative = cleanTestRelative(h.t, relative)
	if !strings.HasPrefix(relative, "data/") {
		h.t.Fatalf("refusing to hold a non-data test path %q", relative)
	}
	source := filepath.Join(h.localRoot, filepath.FromSlash(relative))
	held := filepath.Join(h.localRoot, "data", ".e2e-held-"+strings.ReplaceAll(strings.TrimPrefix(relative, "data/"), "/", "-"))
	restored := false
	h.renameDataFile(source, held)
	restore := func() {
		if restored {
			return
		}
		h.renameDataFile(held, source)
		restored = true
	}
	h.t.Cleanup(restore)
	return restore
}

func (h *e2eHarness) renameDataFile(from, to string) {
	h.t.Helper()
	if err := os.Rename(from, to); err != nil {
		h.t.Fatalf("rename E2E data file: %v", err)
	}
}

func cleanTestRelative(t *testing.T, value string) string {
	t.Helper()
	clean := path.Clean(value)
	if value == "" || strings.HasPrefix(value, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		t.Fatalf("unsafe E2E relative path %q", value)
	}
	return value
}
