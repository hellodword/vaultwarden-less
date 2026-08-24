package restore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateOutputRequiresPrivateParentAndNeverOverlapsData(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	restoreRoot := filepath.Join(root, "restore")
	for _, path := range []string{data, restoreRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateOutput(data, filepath.Join(restoreRoot, "prepared")); err != nil {
		t.Fatalf("safe output rejected: %v", err)
	}
	for _, output := range []string{data, filepath.Join(data, "prepared"), root} {
		if err := validateOutput(data, output); err == nil {
			t.Fatalf("unsafe overlapping output accepted: %s", output)
		}
	}
	if err := os.Chmod(restoreRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := validateOutput(data, filepath.Join(restoreRoot, "prepared")); err == nil {
		t.Fatal("non-private restore root was accepted")
	}
}

func TestValidateOutputRejectsSymlinkAndNonEmptyDestination(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	restoreRoot := filepath.Join(root, "restore")
	for _, path := range []string{data, restoreRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	nonEmpty := filepath.Join(restoreRoot, "non-empty")
	if err := os.Mkdir(nonEmpty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "keep"), []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateOutput(data, nonEmpty); err == nil {
		t.Fatal("non-empty output was accepted")
	}
	link := filepath.Join(restoreRoot, "link")
	if err := os.Symlink(restoreRoot, link); err != nil {
		t.Fatal(err)
	}
	if err := validateOutput(data, filepath.Join(link, "prepared")); err == nil {
		t.Fatal("symlink parent was accepted")
	}
}
