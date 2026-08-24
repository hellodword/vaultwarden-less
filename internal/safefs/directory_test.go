package safefs

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
	if err := os.Mkdir(path, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateDirectory(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Fatalf("validation changed mode to %04o", info.Mode().Perm())
	}
	if err := os.Chmod(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateDirectory(path); err == nil {
		t.Fatal("accepted group-accessible directory")
	}
}
