package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicReplacePublishesOnlyAfterAllPreRenameStages(t *testing.T) {
	t.Parallel()
	stages := []string{"create", "chmod", "write", "file-sync", "close", "rename", "open-parent", "parent-sync", "parent-close"}
	for _, stage := range stages {
		stage := stage
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var closeCalls, openDirCalls int
			ops := FileOps{}
			if stage == "create" {
				ops.CreateTemp = func(string, string) (*os.File, error) { return nil, errors.New("create failure") }
			}
			if stage == "chmod" {
				ops.Chmod = func(*os.File, os.FileMode) error { return errors.New("chmod failure") }
			}
			if stage == "write" {
				ops.Write = func(*os.File, []byte) (int, error) { return 0, errors.New("write failure") }
			}
			if stage == "file-sync" {
				ops.SyncFile = func(*os.File) error { return errors.New("file sync failure") }
			}
			if stage == "close" {
				ops.Close = func(file *os.File) error {
					closeCalls++
					if closeCalls == 1 {
						return errors.New("close failure")
					}
					return file.Close()
				}
			}
			if stage == "rename" {
				ops.Rename = func(string, string) error { return errors.New("rename failure") }
			}
			if stage == "open-parent" {
				ops.OpenDir = func(string) (*os.File, error) { return nil, errors.New("open parent failure") }
			}
			if stage == "parent-sync" {
				ops.SyncDir = func(*os.File) error { return errors.New("parent sync failure") }
			}
			if stage == "parent-close" {
				ops.Close = func(file *os.File) error {
					closeCalls++
					if closeCalls == 2 {
						return errors.New("parent close failure")
					}
					return file.Close()
				}
			}
			ops.OpenDir = func(path string) (*os.File, error) {
				openDirCalls++
				if stage == "open-parent" {
					return nil, errors.New("open parent failure")
				}
				return os.Open(path)
			}
			result, err := AtomicReplace(path, []byte("new\n"), ops)
			if err == nil {
				t.Fatal("fault injection did not fail")
			}
			contents, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if stage == "parent-sync" || stage == "parent-close" || stage == "open-parent" {
				if result != PublishedUnconfirmed || string(contents) != "new\n" {
					t.Fatalf("published fault result/content = %q/%s", contents, result)
				}
			} else if result != Unpublished || string(contents) != "old\n" {
				t.Fatalf("pre-publication fault result/content = %q/%s", contents, result)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "state.json" {
					t.Fatalf("temporary file remained after %s: %s", stage, entry.Name())
				}
			}
			_ = openDirCalls
		})
	}
}
