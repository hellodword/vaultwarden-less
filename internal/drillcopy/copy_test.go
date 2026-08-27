package drillcopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

func TestCopyManifestFilesCopiesOnlyListedStableFiles(t *testing.T) {
	source := privateDirectory(t, "source")
	destination := privateDirectory(t, "destination")
	data := []byte("database")
	writeFile(t, filepath.Join(source, "db.sqlite3"), data)
	digest := sha256.Sum256(data)
	files := []snapshot.File{{
		Path: "db.sqlite3", Size: int64(len(data)), SHA256: "sha256:" + hex.EncodeToString(digest[:]),
	}}
	if err := copyManifestFiles(context.Background(), source, destination, files); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(destination, "db.sqlite3"))
	if err != nil || string(copied) != string(data) {
		t.Fatalf("copied data = %q/%v", copied, err)
	}
	if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("destination mode = %v/%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("manifest was copied into drill data")
	}
}

func TestCopyManifestFilesRejectsUnsafeDestinationAndSource(t *testing.T) {
	t.Run("nonempty destination", func(t *testing.T) {
		source := privateDirectory(t, "source")
		destination := privateDirectory(t, "destination")
		writeFile(t, filepath.Join(destination, "existing"), []byte("data"))
		if err := copyManifestFiles(context.Background(), source, destination, []snapshot.File{}); err == nil {
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
		if err := copyManifestFiles(context.Background(), source, destination, files); err == nil {
			t.Fatal("symlink source was accepted")
		}
	})
	t.Run("path escape", func(t *testing.T) {
		source := privateDirectory(t, "source")
		destination := privateDirectory(t, "destination")
		files := []snapshot.File{{Path: "../escape", Size: 0, SHA256: "sha256:" + strings.Repeat("0", 64)}}
		if err := copyManifestFiles(context.Background(), source, destination, files); err == nil {
			t.Fatal("path escape was accepted")
		}
	})
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
