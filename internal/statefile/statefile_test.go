package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

type document struct {
	Name string `json:"name"`
}

func privateDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestReadWriteAndReplaceJSON(t *testing.T) {
	directory := privateDirectory(t)
	path := filepath.Join(directory, "state.json")
	if err := WriteNewJSON(path, 1024, document{Name: "first"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{\"name\":\"first\"}\n" {
		t.Fatalf("unexpected state: %q %v", data, err)
	}
	var loaded document
	if err := ReadJSON(path, 1024, &loaded); err != nil || loaded.Name != "first" {
		t.Fatalf("unexpected read: %+v %v", loaded, err)
	}
	if err := ReplaceJSON(path, 1024, document{Name: "second"}); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(path, 1024, &loaded); err != nil || loaded.Name != "second" {
		t.Fatalf("unexpected replacement: %+v %v", loaded, err)
	}
	if err := WriteNewJSON(path, 1024, document{Name: "third"}); err == nil {
		t.Fatal("expected no-replace refusal")
	}
}

func TestReadJSONRejectsNoncanonicalAndUnsafeFiles(t *testing.T) {
	directory := privateDirectory(t)
	tests := map[string]string{
		"unknown":   "{\"name\":\"ok\",\"extra\":true}\n",
		"duplicate": "{\"name\":\"a\",\"name\":\"b\"}\n",
		"trailing":  "{\"name\":\"a\"} {}\n",
		"spacing":   "{ \"name\": \"a\" }\n",
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, name+".json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			var loaded document
			if err := ReadJSON(path, 1024, &loaded); err == nil {
				t.Fatal("expected invalid state rejection")
			}
		})
	}
	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte("{\"name\":\"a\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var loaded document
	if err := ReadJSON(link, 1024, &loaded); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestOwnedTemporaryNames(t *testing.T) {
	if !IsOwnedTemporary("prepare.json", ".vaultwarden-less-state-prepare.json-123.tmp") {
		t.Fatal("expected owned temporary")
	}
	for _, name := range []string{
		"prepare.json", ".vaultwarden-less-state-other.json-123.tmp",
		".vaultwarden-less-state-prepare.json-.tmp", "../prepare.tmp",
	} {
		if IsOwnedTemporary("prepare.json", name) {
			t.Fatalf("unexpected owned temporary %q", name)
		}
	}
}

func TestOwnedTemporaryValidationAndCleanup(t *testing.T) {
	directory := privateDirectory(t)
	owned := ".vaultwarden-less-state-prepare.json-interrupted.tmp"
	ownedPath := filepath.Join(directory, owned)
	if err := os.WriteFile(ownedPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	operator := ".vaultwarden-less-state-prepare.json-operator"
	operatorPath := filepath.Join(directory, operator)
	if err := os.WriteFile(operatorPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	matched, err := ValidateOwnedTemporary(directory, owned, 1024, "prepare.json")
	if err != nil || !matched {
		t.Fatalf("owned temporary validation = %t/%v", matched, err)
	}
	matched, err = ValidateOwnedTemporary(directory, operator, 1024, "prepare.json")
	if err != nil || matched {
		t.Fatalf("operator entry validation = %t/%v", matched, err)
	}
	if err := CleanupOwnedTemporaries(directory, 1024, "prepare.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(ownedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned temporary survived cleanup: %v", err)
	}
	if data, err := os.ReadFile(operatorPath); err != nil || string(data) != "keep" {
		t.Fatalf("operator entry changed: %q/%v", data, err)
	}

	unsafe := ".vaultwarden-less-state-decision.json-interrupted.tmp"
	unsafePath := filepath.Join(directory, unsafe)
	if err := os.WriteFile(unsafePath, []byte("partial"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := CleanupOwnedTemporaries(directory, 1024, "decision.json"); err == nil {
		t.Fatal("unsafe owned temporary was removed")
	}
	if _, err := os.Lstat(unsafePath); err != nil {
		t.Fatalf("unsafe owned temporary did not survive refusal: %v", err)
	}
}

func TestWriteFaultMatrixPreservesAbsentOldOrCompleteNewBoundary(t *testing.T) {
	originalCreate := createTemp
	originalChmod := chmodFile
	originalWrite := writeFile
	originalFileSync := syncFile
	originalClose := closeFile
	originalRenameNew := renameNoReplace
	originalRenameReplace := renameReplace
	originalDirectorySync := syncDirectory
	reset := func() {
		createTemp = originalCreate
		chmodFile = originalChmod
		writeFile = originalWrite
		syncFile = originalFileSync
		closeFile = originalClose
		renameNoReplace = originalRenameNew
		renameReplace = originalRenameReplace
		syncDirectory = originalDirectorySync
	}
	t.Cleanup(reset)

	for _, operation := range []string{"write-new", "replace"} {
		for _, stage := range []string{"create", "chmod", "write", "file-sync", "close", "rename", "directory-sync"} {
			operation, stage := operation, stage
			t.Run(operation+"/"+stage, func(t *testing.T) {
				reset()
				defer reset()
				directory := privateDirectory(t)
				path := filepath.Join(directory, "state.json")
				if operation == "replace" {
					if err := WriteNewJSON(path, 1024, document{Name: "old"}); err != nil {
						t.Fatal(err)
					}
				}
				injected := errors.New("injected " + stage + " failure")
				switch stage {
				case "create":
					createTemp = func(string, string) (*os.File, error) { return nil, injected }
				case "chmod":
					chmodFile = func(*os.File, os.FileMode) error { return injected }
				case "write":
					writeFile = func(*os.File, []byte) (int, error) { return 0, injected }
				case "file-sync":
					syncFile = func(*os.File) error { return injected }
				case "close":
					closeFile = func(file *os.File) error {
						if err := file.Close(); err != nil {
							return err
						}
						return injected
					}
				case "rename":
					if operation == "write-new" {
						renameNoReplace = func(string, string) error { return injected }
					} else {
						renameReplace = func(string, string) error { return injected }
					}
				case "directory-sync":
					syncDirectory = func(string) error { return injected }
				}

				var err error
				if operation == "write-new" {
					err = WriteNewJSON(path, 1024, document{Name: "new"})
				} else {
					err = ReplaceJSON(path, 1024, document{Name: "new"})
				}
				if err == nil {
					t.Fatal("expected injected failure")
				}

				var loaded document
				readErr := ReadJSON(path, 1024, &loaded)
				if stage == "directory-sync" {
					if readErr != nil || loaded.Name != "new" {
						t.Fatalf("complete new file not preserved: %+v %v", loaded, readErr)
					}
				} else if operation == "write-new" {
					if !errors.Is(readErr, os.ErrNotExist) {
						t.Fatalf("absent target not preserved: %+v %v", loaded, readErr)
					}
				} else if readErr != nil || loaded.Name != "old" {
					t.Fatalf("complete old file not preserved: %+v %v", loaded, readErr)
				}

				entries, readDirectoryErr := os.ReadDir(directory)
				if readDirectoryErr != nil {
					t.Fatal(readDirectoryErr)
				}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), temporaryPrefix) {
						t.Fatalf("temporary file leaked: %s", entry.Name())
					}
				}
			})
		}
	}
}

func TestWriteRejectsShortWriteAndReplaceRejectsUnsafeTarget(t *testing.T) {
	directory := privateDirectory(t)
	path := filepath.Join(directory, "state.json")
	originalWrite := writeFile
	writeFile = func(*os.File, []byte) (int, error) { return 1, nil }
	if err := WriteNewJSON(path, 1024, document{Name: "new"}); err == nil {
		t.Fatal("short write was accepted")
	}
	writeFile = originalWrite
	t.Cleanup(func() { writeFile = originalWrite })

	if err := os.WriteFile(path, []byte("{\"name\":\"old\"}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceJSON(path, 1024, document{Name: "new"}); err == nil {
		t.Fatal("unsafe replacement target was accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{\"name\":\"old\"}\n" {
		t.Fatalf("unsafe target changed: %q %v", data, err)
	}
}

func TestReplaceRejectsChangedTarget(t *testing.T) {
	directory := privateDirectory(t)
	path := filepath.Join(directory, "state.json")
	if err := WriteNewJSON(path, 1024, document{Name: "old"}); err != nil {
		t.Fatal(err)
	}
	originalCreate := createTemp
	t.Cleanup(func() { createTemp = originalCreate })
	createTemp = func(parent, pattern string) (*os.File, error) {
		file, err := originalCreate(parent, pattern)
		if err == nil {
			if writeErr := os.WriteFile(path, []byte("{\"name\":\"changed\"}\n"), 0o600); writeErr != nil {
				_ = file.Close()
				return nil, writeErr
			}
		}
		return file, err
	}
	if err := ReplaceJSON(path, 1024, document{Name: "new"}); err == nil {
		t.Fatal("expected changed-target refusal")
	}
	var loaded document
	if err := ReadJSON(path, 1024, &loaded); err != nil || loaded.Name != "changed" {
		t.Fatalf("unexpected target after refusal: %+v %v", loaded, err)
	}
}

func TestTemporaryFileContractUsesPrivateMode(t *testing.T) {
	directory := privateDirectory(t)
	path := filepath.Join(directory, "state.json")
	if err := WriteNewJSON(path, 1024, document{Name: "value"}); err != nil {
		t.Fatal(err)
	}
	if _, err := privatefs.ReadBytes(path, 1024); err != nil {
		t.Fatal(err)
	}
}
