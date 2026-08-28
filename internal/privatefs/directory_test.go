package privatefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsurePrivateDirectoryCreates0700AndRejectsSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	private := filepath.Join(root, "private", "nested")
	if err := EnsurePrivateDirectory(private); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(private)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory mode = %v", info.Mode())
	}
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDirectory(filepath.Join(link, "child")); err == nil {
		t.Fatal("private directory accepted a symlink component")
	}
	if _, err := os.Lstat(filepath.Join(real, "child")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was modified before rejection: %v", err)
	}
	if err := EnsurePrivateDirectory(string(filepath.Separator)); err == nil {
		t.Fatal("filesystem root was accepted as a private directory")
	}
}

func TestValidatePrivateDirectoryDoesNotMutateMountRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "mounted")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateDirectory(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("validation changed mode to %04o", info.Mode().Perm())
	}
	if err := os.Chmod(path, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateDirectory(path); err == nil {
		t.Fatal("accepted a private directory with the wrong exact mode")
	}
}

func TestValidatePrivateDirectoryRejectsSymlinkAncestor(t *testing.T) {
	t.Parallel()
	realRoot := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(realRoot, "leaf")
	if err := os.Mkdir(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateDirectory(filepath.Join(link, "leaf")); err == nil {
		t.Fatal("symlink ancestor was accepted")
	}
}

func TestAtomicRenamePrimitivesNeverOverwriteByAccident(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	third := filepath.Join(root, "third")
	if err := os.WriteFile(first, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(first, second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(third, []byte("third"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(third, second); err == nil {
		t.Fatal("RenameNoReplace overwrote an existing path")
	}
	if data, err := os.ReadFile(second); err != nil || string(data) != "first" {
		t.Fatalf("existing path changed: %q/%v", data, err)
	}
	if data, err := os.ReadFile(third); err != nil || string(data) != "third" {
		t.Fatalf("unpublished path changed: %q/%v", data, err)
	}
	if err := RenameExchange(second, third); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(second); err != nil || string(data) != "third" {
		t.Fatalf("exchange left wrong second path: %q/%v", data, err)
	}
	if data, err := os.ReadFile(third); err != nil || string(data) != "first" {
		t.Fatalf("exchange left wrong third path: %q/%v", data, err)
	}
	if err := SyncDirectory(root); err != nil {
		t.Fatal(err)
	}
}

func TestRenameExchangeSwapsTwoDisposableSiblingDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	left := filepath.Join(root, "left")
	right := filepath.Join(root, "right")
	for path, value := range map[string]string{left: "left", right: "right"} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "identity"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RenameExchange(left, right); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(left, "identity")); err != nil || string(data) != "right" {
		t.Fatalf("left after exchange = %q/%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(right, "identity")); err != nil || string(data) != "left" {
		t.Fatalf("right after exchange = %q/%v", data, err)
	}
	if err := SyncDirectory(root); err != nil {
		t.Fatal(err)
	}
}
