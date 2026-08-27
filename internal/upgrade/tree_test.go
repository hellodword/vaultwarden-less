package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRollbackTreeIdentityGoldenAndStableCopy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(source, "a"), []byte("x"))
	identity, err := rollbackTreeID(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "sha256:9f8e26f31b80de718cd3966cf14e28a9ccf7e1e36fdc62890fb17bd91d943a70"
	if identity != golden {
		t.Fatalf("rollback identity = %s, want %s", identity, golden)
	}
	destination := filepath.Join(t.TempDir(), "copy")
	if err := copyPrivateTree(context.Background(), source, destination); err != nil {
		t.Fatal(err)
	}
	copied, err := rollbackTreeID(context.Background(), destination)
	if err != nil || copied != identity {
		t.Fatalf("copied identity = %s/%v", copied, err)
	}
	writePrivateFile(t, filepath.Join(source, "a"), []byte("y"))
	changed, err := rollbackTreeID(context.Background(), source)
	if err != nil || changed == identity {
		t.Fatalf("changed identity = %s/%v", changed, err)
	}
}

func TestRollbackTreeRejectsUnsafeEntries(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "tree")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "target")
		writePrivateFile(t, target, []byte("value"))
		if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := rollbackTreeID(context.Background(), root); err == nil {
			t.Fatal("symlink entry was accepted")
		}
	})
	t.Run("hard link", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "tree")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		first := filepath.Join(root, "first")
		writePrivateFile(t, first, []byte("value"))
		if err := os.Link(first, filepath.Join(root, "second")); err != nil {
			t.Fatal(err)
		}
		if _, err := rollbackTreeID(context.Background(), root); err == nil {
			t.Fatal("hard-linked entries were accepted")
		}
	})
	t.Run("insecure mode", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "tree")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "file")
		writePrivateFile(t, path, []byte("value"))
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := rollbackTreeID(context.Background(), root); err == nil {
			t.Fatal("group-readable entry was accepted")
		}
	})
}

func TestStrictUpgradeJSONRejectsDuplicateUnknownAndTrailingData(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "state.json")
	for _, document := range []string{
		`{"schema_version":1,"schema_version":1}`,
		`{"schema_version":1,"unknown":true}`,
		`{"schema_version":1} {"schema_version":1}`,
		`{ "schema_version": 1 }`,
	} {
		writePrivateFile(t, path, []byte(document+"\n"))
		var destination struct {
			SchemaVersion int `json:"schema_version"`
		}
		if err := readStrictJSON(path, &destination); err == nil {
			t.Fatalf("invalid state JSON was accepted: %s", document)
		}
	}
}
