package drillcopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

func TestCopyManifestFilesCopiesOnlyListedStableFiles(t *testing.T) {
	source := privateDirectory(t, "source")
	destination := privateDirectory(t, "destination")
	uid, gid := testRuntimeIdentity()
	data := []byte("database")
	writeFile(t, filepath.Join(source, "db.sqlite3"), data)
	digest := sha256.Sum256(data)
	files := []snapshot.File{{
		Path: "db.sqlite3", Size: int64(len(data)), SHA256: "sha256:" + hex.EncodeToString(digest[:]),
	}}
	if err := copyManifestFiles(context.Background(), source, destination, files, uid, gid); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(destination, "db.sqlite3"))
	if err != nil || string(copied) != string(data) {
		t.Fatalf("copied data = %q/%v", copied, err)
	}
	if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("destination mode = %v/%v", info, err)
	}
	assertOwnership(t, destination, uid, gid)
	assertOwnership(t, filepath.Join(destination, "db.sqlite3"), uid, gid)
	if _, err := os.Lstat(filepath.Join(destination, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("manifest was copied into drill data")
	}
}

func TestCopyManifestFilesRejectsUnsafeDestinationAndSource(t *testing.T) {
	t.Run("nonempty destination", func(t *testing.T) {
		source := privateDirectory(t, "source")
		destination := privateDirectory(t, "destination")
		writeFile(t, filepath.Join(destination, "existing"), []byte("data"))
		uid, gid := testRuntimeIdentity()
		if err := copyManifestFiles(context.Background(), source, destination, []snapshot.File{}, uid, gid); err == nil {
			t.Fatal("nonempty destination was accepted")
		}
	})
	t.Run("symlink source", func(t *testing.T) {
		source := privateDirectory(t, "source")
		destination := privateDirectory(t, "destination")
		target := filepath.Join(t.TempDir(), "target")
		writeFile(t, target, []byte("data"))
		if err := os.Symlink(target, filepath.Join(source, "db.sqlite3")); err != nil {
			t.Fatal(err)
		}
		files := []snapshot.File{{Path: "db.sqlite3", Size: 4, SHA256: "sha256:" + strings.Repeat("0", 64)}}
		uid, gid := testRuntimeIdentity()
		if err := copyManifestFiles(context.Background(), source, destination, files, uid, gid); err == nil {
			t.Fatal("symlink source was accepted")
		}
	})
	t.Run("path escape", func(t *testing.T) {
		source := privateDirectory(t, "source")
		destination := privateDirectory(t, "destination")
		files := []snapshot.File{{Path: "../escape", Size: 0, SHA256: "sha256:" + strings.Repeat("0", 64)}}
		uid, gid := testRuntimeIdentity()
		if err := copyManifestFiles(context.Background(), source, destination, files, uid, gid); err == nil {
			t.Fatal("path escape was accepted")
		}
	})
}

func TestParseRuntimeIdentity(t *testing.T) {
	uid, gid, err := ParseRuntimeIdentity("1000", "2000")
	if err != nil || uid != 1000 || gid != 2000 {
		t.Fatalf("valid runtime identity = %d:%d/%v", uid, gid, err)
	}
	for _, invalid := range []string{"", "0", "01", "+1", "-1", " 1", "1 ", "4294967295", "4294967296"} {
		if _, _, err := ParseRuntimeIdentity(invalid, "1000"); err == nil {
			t.Fatalf("invalid UID %q was accepted", invalid)
		}
		if _, _, err := ParseRuntimeIdentity("1000", invalid); err == nil {
			t.Fatalf("invalid GID %q was accepted", invalid)
		}
	}
	uid, gid, err = ParseRuntimeIdentity("4294967294", "4294967294")
	if err != nil || uid != math.MaxUint32-1 || gid != math.MaxUint32-1 {
		t.Fatalf("maximum runtime identity = %d:%d/%v", uid, gid, err)
	}
}

func testRuntimeIdentity() (uint32, uint32) {
	if os.Geteuid() == 0 && os.Getegid() == 0 {
		return 1, 1
	}
	return uint32(os.Geteuid()), uint32(os.Getegid())
}

func assertOwnership(t *testing.T, path string, uid, gid uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid {
		t.Fatalf("ownership of %s = %v, want %d:%d", filepath.Base(path), info.Sys(), uid, gid)
	}
}

func privateDirectory(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
