package restore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/upstream"
	_ "github.com/mattn/go-sqlite3"
)

type fakeRepository struct {
	selected     restic.Snapshot
	template     string
	resolveError error
	restoreError error
	resolvedWith string
	resolveCalls int
	restoreCalls int
}

func (repository *fakeRepository) ResolveSnapshot(ctx context.Context, _ config.Target, requested string) (restic.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return restic.Snapshot{}, err
	}
	repository.resolveCalls++
	repository.resolvedWith = requested
	if repository.resolveError != nil {
		return restic.Snapshot{}, repository.resolveError
	}
	return repository.selected, nil
}

func (repository *fakeRepository) Restore(ctx context.Context, _ config.Target, snapshotID, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repository.restoreCalls++
	if repository.restoreError != nil {
		return repository.restoreError
	}
	if snapshotID != repository.selected.ID {
		return errors.New("unexpected snapshot")
	}
	return copyTree(repository.template, destination)
}

func recoveryTemplate(t *testing.T) (string, snapshot.Manifest) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "point")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(data, "db.sqlite3")
	database, err := sql.Open("sqlite3", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := upstream.VaultwardenSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{"PRAGMA journal_mode=DELETE", "PRAGMA foreign_keys=ON"}
	for _, object := range contract.Objects {
		statements = append(statements, object.SQL)
	}
	statements = append(statements, "INSERT INTO __diesel_schema_migrations(version) VALUES ('"+contract.DatabaseMigration+"')")
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		t.Fatal(err)
	}
	databaseBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(databaseBytes)
	manifest, err := snapshot.NewManifest([]snapshot.File{{
		Path: "data/db.sqlite3", Size: int64(len(databaseBytes)), SHA256: hex.EncodeToString(digest[:]),
	}})
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Verify(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	return root, manifest
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}

func privateRestoreRoot(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restore")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunPublishesOneVerifiedDirectory(t *testing.T) {
	template, manifest := recoveryTemplate(t)
	restoreRoot := privateRestoreRoot(t)
	output := filepath.Join(restoreRoot, "prepared")
	repository := &fakeRepository{
		template: template,
		selected: restic.Snapshot{
			ID: strings.Repeat("a", 64), ContentID: manifest.ContentID,
		},
	}
	result, err := Run(context.Background(), repository, Request{
		Target: config.Target{Name: "primary"}, Output: output, RestoreRoot: restoreRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != ResultSchemaVersion || result.Target != "primary" || result.SnapshotID != repository.selected.ID || result.ContentID != manifest.ContentID || result.Output != output {
		t.Fatalf("result = %#v", result)
	}
	if repository.resolveCalls != 1 || repository.restoreCalls != 1 || repository.resolvedWith != "" {
		t.Fatalf("repository calls = %#v", repository)
	}
	if _, err := snapshot.Verify(context.Background(), output); err != nil {
		t.Fatal(err)
	}
	assertNoRestoreTemps(t, restoreRoot)
}

func TestExplicitSnapshotIsPassedWithoutFallback(t *testing.T) {
	template, manifest := recoveryTemplate(t)
	restoreRoot := privateRestoreRoot(t)
	requested := strings.Repeat("b", 64)
	repository := &fakeRepository{
		template: template,
		selected: restic.Snapshot{ID: requested, ContentID: manifest.ContentID},
	}
	if _, err := Run(context.Background(), repository, Request{
		Target: config.Target{Name: "primary"}, SnapshotID: requested,
		Output: filepath.Join(restoreRoot, "old"), RestoreRoot: restoreRoot,
	}); err != nil {
		t.Fatal(err)
	}
	if repository.resolvedWith != requested || repository.resolveCalls != 1 || repository.restoreCalls != 1 {
		t.Fatalf("repository calls = %#v", repository)
	}
}

func TestLatestVerificationFailureDoesNotFallbackOrPublish(t *testing.T) {
	template, manifest := recoveryTemplate(t)
	if err := os.WriteFile(filepath.Join(template, "data", "db.sqlite3"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	restoreRoot := privateRestoreRoot(t)
	output := filepath.Join(restoreRoot, "prepared")
	repository := &fakeRepository{
		template: template,
		selected: restic.Snapshot{ID: strings.Repeat("c", 64), ContentID: manifest.ContentID},
	}
	if _, err := Run(context.Background(), repository, Request{
		Target: config.Target{Name: "primary"}, Output: output, RestoreRoot: restoreRoot,
	}); err == nil {
		t.Fatal("invalid latest snapshot was accepted")
	}
	if repository.resolveCalls != 1 || repository.restoreCalls != 1 {
		t.Fatalf("latest failure attempted fallback: %#v", repository)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed restore published output")
	}
	assertNoRestoreTemps(t, restoreRoot)
}

func TestEveryFailureLeavesFinalPathAbsent(t *testing.T) {
	template, manifest := recoveryTemplate(t)
	for name, configure := range map[string]func(*fakeRepository){
		"resolve": func(repository *fakeRepository) { repository.resolveError = errors.New("resolve failed") },
		"restore": func(repository *fakeRepository) { repository.restoreError = errors.New("restore failed") },
		"content tag": func(repository *fakeRepository) {
			repository.selected.ContentID = "sha256:" + strings.Repeat("f", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			restoreRoot := privateRestoreRoot(t)
			output := filepath.Join(restoreRoot, "prepared")
			repository := &fakeRepository{
				template: template,
				selected: restic.Snapshot{ID: strings.Repeat("d", 64), ContentID: manifest.ContentID},
			}
			configure(repository)
			if _, err := Run(context.Background(), repository, Request{
				Target: config.Target{Name: "primary"}, Output: output, RestoreRoot: restoreRoot,
			}); err == nil {
				t.Fatal("failure was accepted")
			}
			if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failure published output")
			}
			assertNoRestoreTemps(t, restoreRoot)
		})
	}
}

func TestValidateOutputRequiresAbsentContainedPathAndReal0700Parents(t *testing.T) {
	restoreRoot := privateRestoreRoot(t)
	valid := filepath.Join(restoreRoot, "prepared")
	if err := validateOutput(restoreRoot, valid); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{restoreRoot, filepath.Dir(restoreRoot), "relative"} {
		if err := validateOutput(restoreRoot, invalid); err == nil {
			t.Fatalf("unsafe output accepted: %s", invalid)
		}
	}
	if err := os.Mkdir(valid, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateOutput(restoreRoot, valid); err == nil {
		t.Fatal("existing output was accepted")
	}
	if err := os.Remove(valid); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(restoreRoot, "link")
	if err := os.Symlink(restoreRoot, link); err != nil {
		t.Fatal(err)
	}
	if err := validateOutput(restoreRoot, filepath.Join(link, "prepared")); err == nil {
		t.Fatal("symlink parent was accepted")
	}
	if err := os.Chmod(restoreRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := validateOutput(restoreRoot, valid); err == nil {
		t.Fatal("non-private restore root was accepted")
	}
}

func assertNoRestoreTemps(t *testing.T, restoreRoot string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(restoreRoot, ".vaultwarden-less-restore-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("restore temporaries = %#v, %v", matches, err)
	}
}
