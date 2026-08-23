package securefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRequiresRegular0600File(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("value\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := Read(path)
	if err != nil || value != "value" {
		t.Fatalf("Read() = %q, %v", value, err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("accepted group-readable secret")
	}
}

func TestReadRejectsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(real, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link); err == nil {
		t.Fatal("accepted symlink secret")
	}
}
